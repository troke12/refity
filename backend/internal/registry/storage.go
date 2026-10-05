package registry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	godigest "github.com/opencontainers/go-digest"
	"refity/backend/internal/config"
	"refity/backend/internal/driver/sftp"
	"refity/backend/internal/readcache"
	"refity/backend/internal/spool"
)

// Read/write path around the slow remote:
//
//	write: client -> local staging -> digest check -> spool (durable, 201 now) -> workers -> remote
//	read:  spool (just pushed) -> read cache (verified copies) -> remote (streamed, Range-aware)
//
// The spool is always opened, even in sync mode, so jobs left by an earlier async run still drain.
var (
	// The spool and cache are swapped atomically: handlers may still be running when Shutdown (after the
	// HTTP drain timeout) clears them, so every use takes one snapshot via blobSpool()/blobCache() and
	// works on that local value.
	spoolPtr atomic.Pointer[spool.Spool]
	cachePtr atomic.Pointer[readcache.Cache]
	spoolDir string

	// storageCtx is cancelled by Shutdown; background goroutines watch it. storageMu guards the pair so
	// goBackground never adds to storageWG once Shutdown has started waiting.
	storageMu     sync.Mutex
	storageCtx    context.Context
	storageCancel context.CancelFunc
	storageWG     sync.WaitGroup

	// syncRetryBackoff is the first pause between synchronous upload attempts (doubles each time).
	syncRetryBackoff = time.Second
	// spoolHealthInterval is how often a stuck spool is reported.
	spoolHealthInterval = time.Minute
	// ociCopyRuns counts background OCI digest-copy runs (observability for tests and debugging).
	ociCopyRuns atomic.Int64
	// spoolNow is the spool's clock (replaceable in tests to simulate a host clock step).
	spoolNow = time.Now
	// diskFree reports free bytes on the filesystem holding dir (-1 if unknown). Replaceable in tests.
	diskFree = freeBytes
	// healing tracks remote digest paths already found corrupt, so a run of pulls deletes each once.
	healing sync.Map
	// corrupt marks remote digest paths that were found to hold the wrong bytes and have been removed,
	// so the next push of that digest is written even though the size matches what was there before.
	corrupt sync.Map
)

const (
	syncUploadAttempts = 3
	// remoteResumeAttempts bounds consecutive failed reopen attempts while streaming one remote blob.
	remoteResumeAttempts = 5
	// purgeWait bounds how long a repository delete waits for its in-flight uploads to stop.
	purgeWait = 30 * time.Second
	// spoolStuckAge: a spool whose oldest job is older than this is logged as a warning.
	spoolStuckAge = 10 * time.Minute
	// staleTempAge: remote "*.uploading-*" files older than this are leftovers from crashed uploads.
	staleTempAge = 24 * time.Hour
	// defaultStreamWriteTimeout applies when the config leaves STREAM_WRITE_TIMEOUT at zero.
	defaultStreamWriteTimeout = 5 * time.Minute
	// healVerifyMaxBytes is the largest blob the read-path heal will re-hash. Above it the re-read would
	// cost a full download while holding a pooled connection and the path lock, so those are left to the
	// offline audit instead (which has no request to stall and no lock to block).
	healVerifyMaxBytes = 64 << 20
	// healVerifyTimeout bounds that re-read on top of the size cap, so a stalled link ends the attempt.
	healVerifyTimeout = 2 * time.Minute
)

func blobSpool() *spool.Spool     { return spoolPtr.Load() }
func blobCache() *readcache.Cache { return cachePtr.Load() }

// fileUploader is implemented by the pooled SFTP driver (atomic temp+rename, parallel, stall-aware).
type fileUploader interface {
	UploadFile(ctx context.Context, localPath, remotePath, token string, skipIfSameSize bool) error
}

type poolSizer interface{ PoolSize() int }

type tempSweeper interface {
	SweepStaleTemps(ctx context.Context, root string, olderThan time.Duration) (int, error)
}

// within reports whether p is base or below it.
func within(p, base string) bool {
	rel, err := filepath.Rel(base, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// validateDirs refuses layouts where the spool or cache could sweep or evict files it does not own: the
// two must be separate, non-nested directories, and neither may be (or contain) the data root that holds
// refity.db. Living inside the data root (the default) is fine.
func validateDirs(spoolDir, cacheDir, dataDir string) error {
	abs := func(p string) string {
		if p == "" {
			return ""
		}
		a, err := filepath.Abs(p)
		if err != nil {
			return filepath.Clean(p)
		}
		return a
	}
	s, c, d := abs(spoolDir), abs(cacheDir), abs(dataDir)
	if s != "" && c != "" && (within(s, c) || within(c, s)) {
		return fmt.Errorf("SPOOL_DIR (%s) and READ_CACHE_DIR (%s) must be separate, non-nested directories", s, c)
	}
	if d != "" {
		if s != "" && within(d, s) {
			return fmt.Errorf("SPOOL_DIR (%s) must not be or contain the data directory (%s)", s, d)
		}
		if c != "" && within(d, c) {
			return fmt.Errorf("READ_CACHE_DIR (%s) must not be or contain the data directory (%s)", c, d)
		}
	}
	return nil
}

func initStorage(c *config.Config) error {
	Shutdown()
	if c == nil {
		c = &config.Config{}
	}
	dir := c.SpoolDir
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "refity-spool")
	}
	cacheDir := ""
	if c.ReadCacheBytes > 0 {
		cacheDir = c.ReadCacheDir
		if cacheDir == "" {
			cacheDir = filepath.Join(os.TempDir(), "refity-cache")
		}
	}
	if err := validateDirs(dir, cacheDir, c.DataDir); err != nil {
		return err
	}
	// Reload the corrupt marks before anything can consult them, so a push that happens right after
	// startup still knows which digests must not be trusted on size alone.
	bindCorruptStore(dir)
	if cacheDir != "" {
		rc, err := readcache.New(cacheDir, c.ReadCacheBytes)
		if err != nil {
			return fmt.Errorf("read cache: %w", err)
		}
		rc.Group = repoOf
		cachePtr.Store(rc)
	}
	workers := c.UploadWorkers
	if workers < 1 {
		workers = 4
	}
	if ps, ok := sftpDriver.(poolSizer); ok && workers > ps.PoolSize() {
		workers = ps.PoolSize()
	}
	sp, err := spool.Open(spool.Options{
		Dir:      dir,
		Now:      func() time.Time { return spoolNow() },
		MaxBytes: c.SpoolMaxBytes,
		Workers:  workers,
		Lock: func(remote string) func() {
			l := pathLock(remote)
			l.Lock()
			return l.Unlock
		},
		// The spool holds the path lock (Lock above) around this call, so no extra locking here.
		Upload: func(ctx context.Context, localPath string, job spool.Job) error {
			return uploadLocalFile(ctx, localPath, job.Remote, job.ID)
		},
		OnDone: func(job spool.Job, localPath string) { retireLocal(localPath, job.Remote, job.Size, job.Mutable) },
	})
	if err != nil {
		if rc := cachePtr.Swap(nil); rc != nil {
			rc.Close()
		}
		return fmt.Errorf("upload spool: %w", err)
	}
	sweepDirectTemps(dir)
	spoolPtr.Store(sp)
	spoolDir = dir

	storageMu.Lock()
	storageCtx, storageCancel = context.WithCancel(context.Background())
	storageMu.Unlock()
	goBackground(func(ctx context.Context) { watchSpoolHealth(ctx, sp) })
	if sw, ok := sftpDriver.(tempSweeper); ok {
		goBackground(func(ctx context.Context) {
			n, err := sw.SweepStaleTemps(ctx, "registry", staleTempAge)
			if err != nil && ctx.Err() == nil {
				log.Printf("[SFTP] Stale temp sweep: %v (removed %d)", err, n)
			} else if n > 0 {
				log.Printf("[SFTP] Stale temp sweep: removed %d leftover upload temp file(s) older than %v", n, staleTempAge)
			}
		})
	}
	return nil
}

var (
	ociCopyMu       sync.Mutex
	ociCopyInFlight = map[string]bool{}
)

// goOCICopy writes the OCI digest copy of a manifest in the background, at most one goroutine per path:
// during an outage each check can wait up to the acquire timeout, and every GET would otherwise add one.
func goOCICopy(remotePath string, data []byte) {
	ociCopyMu.Lock()
	if ociCopyInFlight[remotePath] {
		ociCopyMu.Unlock()
		return
	}
	ociCopyInFlight[remotePath] = true
	ociCopyMu.Unlock()
	started := false
	defer func() {
		if !started {
			ociCopyMu.Lock()
			delete(ociCopyInFlight, remotePath)
			ociCopyMu.Unlock()
		}
	}()
	started = goBackground(func(ctx context.Context) {
		defer func() {
			ociCopyMu.Lock()
			delete(ociCopyInFlight, remotePath)
			ociCopyMu.Unlock()
		}()
		ociCopyRuns.Add(1)
		exists, err := objectExistsErr(ctx, remotePath)
		if err != nil || exists {
			return
		}
		_ = persistManifest(data, remotePath)
	})
}

// goBackground runs fn in a goroutine that Shutdown cancels and waits for. It is a no-op once Shutdown
// has begun or before initStorage; the result says whether fn was started.
func goBackground(fn func(ctx context.Context)) bool {
	storageMu.Lock()
	ctx := storageCtx
	if ctx == nil || ctx.Err() != nil {
		storageMu.Unlock()
		return false
	}
	storageWG.Add(1)
	storageMu.Unlock()
	go func() {
		defer storageWG.Done()
		fn(ctx)
	}()
	return true
}

// Shutdown stops the upload workers (pending jobs stay on disk and resume on next start), cache fills and
// background maintenance. Safe to call when nothing was initialised.
func Shutdown() {
	storageMu.Lock()
	cancel := storageCancel
	storageCancel = nil
	if cancel != nil {
		cancel()
	}
	storageMu.Unlock()
	if cancel != nil {
		storageWG.Wait()
	}
	if sp := spoolPtr.Swap(nil); sp != nil {
		sp.Close()
	}
	if rc := cachePtr.Swap(nil); rc != nil {
		rc.Close()
	}
}

// sweepDirectTemps removes local temp files of direct manifest writes interrupted by a crash.
func sweepDirectTemps(dir string) {
	matches, _ := filepath.Glob(filepath.Join(dir, "manifest-*.direct.tmp"))
	for _, m := range matches {
		_ = os.Remove(m)
	}
}

// PurgeRepo forgets everything held locally for a repository that is being deleted: pending uploads are
// dropped (and in-flight ones stopped, so they cannot re-create the remote folder), cached objects are
// removed, and any corrupt mark is cleared. Call it before deleting the remote folder.
func PurgeRepo(name string) {
	prefix := "registry/" + strings.Trim(name, "/") + "/"
	if sp := blobSpool(); sp != nil {
		if n := sp.ForgetPrefix(prefix, purgeWait); n > 0 {
			log.Printf("[SPOOL] Repository %s deleted: dropped %d pending upload(s)", name, n)
		}
	}
	if c := blobCache(); c != nil {
		c.DropGroup(strings.Trim(name, "/"))
	}
	// The remote folder is about to go away, so a mark saying "do not trust the size here" has nothing
	// left to protect. Keeping it would follow the repository into its next life and force a full
	// re-upload of every digest that was ever marked there.
	if n := forgetCorruptPrefix(prefix); n > 0 {
		log.Printf("[CORRUPT] Repository %s deleted: cleared %d corrupt mark(s)", name, n)
	}
}

// repoOf extracts the repository from a storage key ("registry/<repo>/blobs/<digest>"), used to group
// cache entries so a deleted repository can be dropped even after a restart.
func repoOf(key string) string {
	k := strings.TrimPrefix(key, "registry/")
	for _, sep := range []string{"/blobs/", "/manifests/"} {
		if i := strings.Index(k, sep); i >= 0 {
			return k[:i]
		}
	}
	return ""
}

// SpoolHealth is the upload backlog as shown on the dashboard.
type SpoolHealth struct {
	Pending          int        `json:"pending"`
	PendingBytes     int64      `json:"pending_bytes"`
	OldestAgeSeconds int64      `json:"oldest_age_seconds"`
	LastError        string     `json:"last_error,omitempty"`
	LastErrorAt      *time.Time `json:"last_error_at,omitempty"`
	LastSuccessAt    *time.Time `json:"last_success_at,omitempty"`
}

// SpoolStats reports the upload backlog; ok is false when the registry storage is not initialised.
func SpoolStats() (SpoolHealth, bool) {
	sp := blobSpool()
	if sp == nil {
		return SpoolHealth{}, false
	}
	st := sp.Stats()
	h := SpoolHealth{Pending: st.Pending, PendingBytes: st.PendingBytes, OldestAgeSeconds: int64(st.OldestAge / time.Second), LastError: st.LastError}
	if !st.LastErrorAt.IsZero() {
		t := st.LastErrorAt
		h.LastErrorAt = &t
	}
	if !st.LastSuccessAt.IsZero() {
		t := st.LastSuccessAt
		h.LastSuccessAt = &t
	}
	return h, true
}

// CorruptHealth reports digest paths that were found holding the wrong bytes. Each mark means "this path
// was corrupt once and its size must not be trusted". In practice the corrupt copy is removed right
// after, so a marked path is normally missing from the remote and pulls of it answer 404 until somebody
// pushes that digest again — which makes this the operator's list of "images that need a re-push".
// It is not a live probe, though: a blob restored by something that bypasses the normal upload path
// (a manual copy, for instance) stays listed until that path is uploaded once.
type CorruptHealth struct {
	// Removed is the number of digest paths still carrying a corrupt mark.
	Removed int `json:"removed"`
	// Paths lists up to corruptReportCap of them, sorted so the output is stable between calls.
	Paths []string `json:"paths,omitempty"`
	// OldestAgeSeconds is how long the longest-standing mark has been outstanding. It reads 0 for marks
	// reloaded from disk, since the time they were found is not persisted.
	OldestAgeSeconds int64 `json:"oldest_age_seconds"`
}

// corruptRemovedAt records when each damaged path was removed, so the report can say how long it has
// been missing. Separate from the mark itself: the mark says "do not trust the size here", this says
// "and it is not there at all". Not persisted — losing it only makes the reported age unknown.
var corruptRemovedAt sync.Map // remote path -> time.Time

const corruptReportCap = 50

// CorruptStats reports the outstanding missing blobs. ok is false when storage is not initialised.
func CorruptStats() (CorruptHealth, bool) {
	if cfg == nil {
		return CorruptHealth{}, false
	}
	h := CorruptHealth{}
	var all []string
	var oldest time.Time
	corrupt.Range(func(k, _ any) bool {
		p, ok := k.(string)
		if !ok {
			return true
		}
		h.Removed++
		all = append(all, p)
		if t, ok := corruptRemovedAt.Load(p); ok {
			if ts, ok := t.(time.Time); ok && (oldest.IsZero() || ts.Before(oldest)) {
				oldest = ts
			}
		}
		return true
	})
	if len(all) > 0 {
		// Sort before capping so the reported subset is deterministic and is the same set on every call.
		sort.Strings(all)
		if len(all) > corruptReportCap {
			all = all[:corruptReportCap]
		}
		h.Paths = all
	}
	if !oldest.IsZero() {
		h.OldestAgeSeconds = int64(time.Since(oldest).Seconds())
	}
	return h, true
}

// spoolHealthWarning returns the log line for a stuck backlog, or "" when the spool is healthy.
func spoolHealthWarning(st spool.Stats) string {
	if st.Pending == 0 || st.OldestAge < spoolStuckAge {
		return ""
	}
	msg := fmt.Sprintf("WARN [SPOOL] %d upload(s) pending (%d bytes), oldest for %v", st.Pending, st.PendingBytes, st.OldestAge.Round(time.Second))
	if st.LastError != "" {
		msg += fmt.Sprintf("; last error %s ago: %s", time.Since(st.LastErrorAt).Round(time.Second), st.LastError)
	}
	return msg
}

// corruptHealthWarning returns the log line for blobs that are missing until they are pushed again, or
// "" when there are none. This is the operator's reminder: the damage is already repaired (the corrupt
// copy is gone), but the image is not usable again until somebody re-pushes that digest.
func corruptHealthWarning(h CorruptHealth) string {
	if h.Removed == 0 {
		return ""
	}
	oldest := time.Duration(h.OldestAgeSeconds) * time.Second
	msg := fmt.Sprintf("WARN [CORRUPT] %d blob(s) removed as corrupt and missing until re-pushed", h.Removed)
	if oldest > 0 {
		msg += fmt.Sprintf("; oldest missing for %v", oldest.Round(time.Second))
	}
	if len(h.Paths) > 0 {
		msg += fmt.Sprintf("; e.g. %s", h.Paths[0])
	}
	return msg
}

func watchSpoolHealth(ctx context.Context, sp *spool.Spool) {
	t := time.NewTicker(spoolHealthInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if msg := spoolHealthWarning(sp.Stats()); msg != "" {
				log.Print(msg)
			}
			if h, ok := CorruptStats(); ok {
				if msg := corruptHealthWarning(h); msg != "" {
					log.Print(msg)
				}
			}
		}
	}
}

func pathLock(p string) *sync.Mutex {
	l, _ := sftpPathLocks.LoadOrStore(p, &sync.Mutex{})
	return l.(*sync.Mutex)
}

// remoteBlobIsCorrect reports whether the blob already at remotePath hashes to the digest it is named
// after, so the caller can skip the upload. verifyUnchecked is reported as false (do not skip): an
// unverified blob must never be treated as stored, which is the whole point of checking.
func remoteBlobIsCorrect(ctx context.Context, remotePath string) bool {
	// A verified local copy settles it without touching the remote at all. Re-pushing an image re-sends
	// every unchanged layer, and re-downloading each one just to hash it would be the largest cost of
	// this check; the cache and spool already hold digest-verified bytes for exactly these paths.
	if cachedBlobMatches(remotePath) {
		return true
	}
	want, err := godigest.Parse(remoteBase(remotePath))
	if err != nil {
		return false
	}
	return remoteMatchesDigestWithin(ctx, remotePath, want, healVerifyTimeout) == verifyMatches
}

// cachedBlobMatches reports whether a digest-verified copy of remotePath is already on local disk, which
// settles a re-push without touching the remote.
//
// Only the read cache counts. A spool job is not a settled copy: it is the upload that has not happened
// yet, so treating it as proof would make every first push skip itself. The read cache only ever holds
// bytes that were hashed to the name they are filed under, and it is filled from the spool copy after a
// successful upload, so it is exactly the "this digest is stored and verified" signal wanted here.
func cachedBlobMatches(remotePath string) bool {
	if !isDigestPath(remotePath) {
		return false
	}
	if rc := blobCache(); rc != nil {
		if _, _, ok := rc.Open(remotePath); ok {
			return true
		}
	}
	return false
}

// uploadLocalFile writes localPath to remotePath now (temp + verify + rename). The caller holds the
// per-path lock.
//
// Same size on the remote means done only for content-addressed paths, never for tags. A path that
// was found corrupt and removed is written even when the sizes match: the corrupt copy had exactly
// that size, so trusting it again is what left the damage in place.
func uploadLocalFile(ctx context.Context, localPath, remotePath, token string) error {
	skip := isDigestPath(remotePath) && !knownCorrupt(remotePath)
	// Size alone is not proof. A corrupt copy has exactly the size the correct one would have, so
	// trusting the size alone lets a same-size corrupt blob survive forever — and if the heal or audit
	// later removes it, the client that was already answered 201 has nothing left. So for a blob we are
	// about to call done, confirm the remote bytes really do hash to the path's digest. That is one
	// extra download, but only on the path where we would otherwise skip the upload entirely.
	//
	// Anything short of a confirmed match means write. That includes a blob too large to re-read here
	// (the verification cap) and a remote that could not be read: neither is evidence that what is
	// stored is the bytes the client just pushed.
	if skip && !remoteBlobIsCorrect(ctx, remotePath) {
		log.Printf("[SPOOL] %s is not verified as matching its digest; writing it again", remotePath)
		skip = false
	}
	if u, ok := sftpDriver.(fileUploader); ok {
		return u.UploadFile(ctx, localPath, remotePath, token, skip)
	}
	// Generic path for drivers without UploadFile: same temp+verify+rename contract, single stream.
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if skip {
		if fi, err := sftpDriver.Stat(ctx, remotePath); err == nil && fileSize(fi) == st.Size() {
			return nil
		}
	}
	tmp := remotePath + ".uploading-" + token
	w, err := sftpDriver.Writer(ctx, tmp, false)
	if err != nil {
		return err
	}
	_, cerr := io.Copy(w, f)
	if err := w.Close(); cerr == nil {
		cerr = err
	}
	if cerr == nil {
		if fi, err := sftpDriver.Stat(ctx, tmp); err != nil || fileSize(fi) != st.Size() {
			cerr = fmt.Errorf("uploaded size mismatch for %s", tmp)
		}
	}
	if cerr == nil {
		_ = sftpDriver.Delete(ctx, remotePath)
		cerr = sftpDriver.Move(ctx, tmp, remotePath)
	}
	if cerr != nil {
		_ = sftpDriver.Delete(ctx, tmp)
	}
	return cerr
}

func fileSize(fi interface{}) int64 {
	if s, ok := fi.(interface{ Size() int64 }); ok {
		return s.Size()
	}
	return -1
}

func randToken() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// retireLocal disposes of a local copy whose upload finished: immutable objects feed the read cache so
// the next pull is local, everything else is deleted.
func retireLocal(localPath, remotePath string, size int64, mutable bool) {
	// Reaching here means the upload finished, so whatever is at remotePath now hashes to its own name
	// and any recorded damage is resolved. This is cleared for every digest path, not just the ones the
	// read cache takes: with the cache disabled (READ_CACHE_BYTES=0) or for a blob larger than the
	// budget, the mark would otherwise pin that digest to a full re-upload on every future push.
	if isDigestPath(remotePath) {
		clearCorrupt(remotePath)
	}
	if rc := blobCache(); !mutable && rc != nil && isDigestPath(remotePath) {
		rc.Adopt(remotePath, localPath, size)
		return
	}
	_ = os.Remove(localPath)
}

// isDigestPath: only a strictly valid digest name is content-addressed (and so safe to cache and to
// treat "same size on the remote" as "already uploaded").
func isDigestPath(p string) bool { return validDigest.MatchString(path.Base(p)) }

// remoteBase exists because handlers name their request-path parameter "path", shadowing the package.
func remoteBase(p string) string { return path.Base(p) }

// isNotFound distinguishes "the object does not exist" (404, permanent for Docker) from storage
// failures (503, retried by Docker).
func isNotFound(err error) bool {
	return errors.Is(err, os.ErrNotExist) || sftp.IsNotExist(err)
}

// isNotDir reports a "this path is a file, not a directory" failure. The audit walks by descending into
// every name it has not recognised yet, so it legitimately asks the server to list tag manifests, and
// needs to tell that expected answer apart from a directory it genuinely could not read.
//
// Deliberately narrow: only the message the SFTP server actually produces counts. A broader match (a
// generic "failure" that happens to mention a file) would swallow real storage errors and make an
// unreadable tree look like a clean audit.
func isNotDir(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "not a directory")
}

func storageUnavailable(w http.ResponseWriter, r *http.Request, err error) {
	log.Printf("[REGISTRY] %s %s: storage unavailable: %v", r.Method, r.URL.Path, err)
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	registryError(w, "UNAVAILABLE", "storage backend temporarily unavailable", http.StatusServiceUnavailable)
}

// uploadSync uploads before returning (sync mode, or async when the spool cannot take the blob). Attempts
// are bounded so a dead remote turns into an error response rather than a hung request.
func uploadSync(localPath, remotePath string, size int64, mutable bool) error {
	sftpSemaphore <- struct{}{}
	defer func() { <-sftpSemaphore }()
	err := uploadDirect(localPath, remotePath)
	if err == nil {
		retireLocal(localPath, remotePath, size, mutable)
		return nil
	}
	_ = os.Remove(localPath)
	return err
}

// uploadDirect writes localPath to remotePath now (temp + verify + rename).
//
// A spooled version of the same path is only dropped after the direct write succeeded, never before:
// that version was already acknowledged to a client, and if the direct write fails it is the only copy.
//   - Digest paths are never dropped at all: the spooled bytes are identical, so letting the worker
//     finish (its same-size check makes it a no-op) is always safe.
//   - Tag paths: the older spooled version is forgotten while the path lock is still held. The spool
//     re-checks under that same lock that its job is current, so the old version cannot slip in after
//     this write. Only the version that was pending when this write started is forgotten; a newer one
//     spooled meanwhile must still win.
func uploadDirect(localPath, remotePath string) error {
	sp := blobSpool()
	mutable := !isDigestPath(remotePath)
	var older spool.Job
	var hasOlder bool
	if sp != nil && mutable {
		older, hasOlder = sp.Lookup(remotePath)
	}
	ctx := context.Background()
	token := randToken()
	var err error
	wait := syncRetryBackoff
	for attempt := 1; attempt <= syncUploadAttempts; attempt++ {
		l := pathLock(remotePath)
		l.Lock()
		err = uploadLocalFile(ctx, localPath, remotePath, token)
		if err == nil && hasOlder {
			sp.ForgetID(remotePath, older.ID)
		}
		l.Unlock()
		if err == nil {
			return nil
		}
		log.Printf("[SFTP] Sync upload of %s attempt %d/%d failed: %v", remotePath, attempt, syncUploadAttempts, err)
		if attempt < syncUploadAttempts {
			time.Sleep(wait)
			wait *= 2
		}
	}
	return err
}

// spoolHasRoom checks the free-space floor: refity.db shares the volume, so the spool must not fill it
// even when SPOOL_MAX_BYTES would allow more.
func spoolHasRoom(size int64) bool {
	if cfg == nil || cfg.SpoolMinFreeBytes <= 0 {
		return true
	}
	free := diskFree(spoolDir)
	if free < 0 {
		return true // unknown on this platform
	}
	return free-size >= cfg.SpoolMinFreeBytes
}

// persistBlob makes a digest-verified local file durable remotely. Returns "async" when spooled (the
// upload continues in the background) or "sync" when it was uploaded before returning.
func persistBlob(localPath, remotePath string, size int64, digest string) (string, error) {
	sp := blobSpool()
	if (cfg == nil || !cfg.SFTPSyncUpload) && sp != nil {
		if job, ok := sp.Lookup(remotePath); ok && job.Size == size {
			// The same verified blob is already spooled (two jobs pushing one layer): nothing to add, and
			// no reason to fall back to a direct write just because the spool volume is low on space.
			_ = os.Remove(localPath)
			return "async", nil
		}
		if !spoolHasRoom(size) {
			log.Printf("[SPOOL] Free space on the spool volume below SPOOL_MIN_FREE_BYTES; uploading %s synchronously", remotePath)
		} else {
			err := sp.Add(localPath, spool.Job{Remote: remotePath, Size: size, Digest: digest})
			if err == nil {
				return "async", nil
			}
			log.Printf("[SPOOL] Not spooling %s (%v); uploading synchronously", remotePath, err)
		}
	}
	return "sync", uploadSync(localPath, remotePath, size, false)
}

// persistManifest stores a manifest at remotePath. In async mode it is always spooled, outside the byte
// budget: a manifest is tiny, and a newer tag version written directly while an older one sits in the
// spool would later be overwritten by it. Sync mode (or a spool error) writes it directly and atomically.
func persistManifest(data []byte, remotePath string) error {
	mutable := !isDigestPath(remotePath)
	if sp := blobSpool(); (cfg == nil || !cfg.SFTPSyncUpload) && sp != nil {
		err := sp.AddBytes(data, spool.Job{Remote: remotePath, Digest: godigest.FromBytes(data).String(), Mutable: mutable, Unbudgeted: true})
		if err == nil {
			return nil
		}
		log.Printf("[SPOOL] Not spooling manifest %s (%v); uploading synchronously", remotePath, err)
	}
	return putManifestDirect(data, remotePath)
}

// putManifestDirect writes a manifest through the same local-temp, remote-temp, verify, rename path as
// blobs, so a failed or interrupted write never truncates the existing version.
func putManifestDirect(data []byte, remotePath string) error {
	dir := spoolDir
	if dir == "" {
		dir = os.TempDir()
	}
	f, err := os.CreateTemp(dir, "manifest-*.direct.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	_, werr := f.Write(data)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return werr
	}
	return uploadDirect(tmp, remotePath)
}

// stageBody appends an (optional) request body to the local staging file — the final chunk of a chunked
// upload, or the whole blob for a monolithic one — and returns the staging file's location on disk.
func stageBody(stagingPath string, body io.Reader) (string, error) {
	full, err := localDriver.Path(stagingPath)
	if err != nil {
		return "", err
	}
	if body == nil {
		return full, nil
	}
	w, err := localDriver.WriterAppend(context.TODO(), stagingPath)
	if err != nil {
		return "", err
	}
	_, cerr := io.CopyBuffer(w, body, make([]byte, 1024*1024))
	if err := w.Close(); cerr == nil {
		cerr = err
	}
	return full, cerr
}

// hashFile streams a file through sha256 instead of loading it into memory.
func hashFile(p string) (godigest.Digest, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	d := godigest.Canonical.Digester()
	n, err := io.CopyBuffer(d.Hash(), f, make([]byte, 1024*1024))
	if err != nil {
		return "", 0, err
	}
	return d.Digest(), n, nil
}

// objectExists checks spool, cache and remote, cheapest first. A storage error counts as "missing";
// use objectExistsErr where that distinction matters.
func objectExists(ctx context.Context, remotePath string) bool {
	ok, _ := objectExistsErr(ctx, remotePath)
	return ok
}

// objectExistsErr is objectExists that reports storage failures (other than not-found) as errors.
func objectExistsErr(ctx context.Context, remotePath string) (bool, error) {
	if sp := blobSpool(); sp != nil {
		if _, ok := sp.Lookup(remotePath); ok {
			return true, nil
		}
	}
	if rc := blobCache(); rc != nil {
		if _, ok := rc.Has(remotePath); ok {
			return true, nil
		}
	}
	_, err := sftpDriver.Stat(ctx, remotePath)
	if err == nil {
		return true, nil
	}
	if isNotFound(err) {
		return false, nil
	}
	return false, err
}

// readManifest returns manifest bytes from spool, cache (digest refs only) or remote.
func readManifest(ctx context.Context, remotePath string) ([]byte, error) {
	if sp := blobSpool(); sp != nil {
		if f, _, ok := sp.Open(remotePath); ok {
			defer f.Close()
			return io.ReadAll(f)
		}
	}
	digestRef := isDigestPath(remotePath)
	rc := blobCache()
	if digestRef && rc != nil {
		if f, _, ok := rc.Open(remotePath); ok {
			defer f.Close()
			return io.ReadAll(f)
		}
	}
	data, err := sftpDriver.GetContent(ctx, remotePath)
	if err == nil && digestRef && rc != nil {
		_ = rc.PutBytes(remotePath, data, path.Base(remotePath))
	}
	return data, err
}

// openLocalBlob finds a complete local copy: spool first (newest data), then the read cache.
func openLocalBlob(remotePath string) (*os.File, int64, bool) {
	if sp := blobSpool(); sp != nil {
		if f, job, ok := sp.Open(remotePath); ok {
			return f, job.Size, true
		}
	}
	if rc := blobCache(); rc != nil {
		if f, size, ok := rc.Open(remotePath); ok {
			return f, size, true
		}
	}
	return nil, 0, false
}

func setBlobHeaders(w http.ResponseWriter, digest string) {
	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Etag", `"`+digest+`"`)
}

func streamWriteTimeout() time.Duration {
	if cfg != nil && cfg.StreamWriteTimeout > 0 {
		return cfg.StreamWriteTimeout
	}
	return defaultStreamWriteTimeout
}

// deadlineWriter re-arms the connection write deadline before every write, so a client that stops
// reading fails the stream after the timeout instead of holding a pooled SFTP connection forever.
type deadlineWriter struct {
	w       io.Writer
	rc      *http.ResponseController
	timeout time.Duration
}

func (d deadlineWriter) Write(p []byte) (int, error) {
	_ = d.rc.SetWriteDeadline(time.Now().Add(d.timeout))
	return d.w.Write(p)
}

// serveBlob answers GET/HEAD for a blob. Local copies go through http.ServeContent (Range, HEAD, 416 for
// free); remote ones are streamed with headers sent before the first byte is fetched.
func serveBlob(w http.ResponseWriter, r *http.Request, remotePath, digest string) {
	if f, _, ok := openLocalBlob(remotePath); ok {
		defer f.Close()
		setBlobHeaders(w, digest)
		http.ServeContent(w, r, "", time.Time{}, f)
		return
	}
	ctx := r.Context()
	fi, err := sftpDriver.Stat(ctx, remotePath)
	if err != nil {
		// Blob HEAD is the push-side existence check. Answering 404 while storage is unreachable is safe:
		// the client just uploads the blob (into the spool; an identical remote copy is skipped by size
		// later). A 503 here can abort a containerd-based push, while Docker only logs it. GET keeps 503:
		// it is a pull, and 404 would tell the client the blob is gone for good.
		if !isNotFound(err) && r.Method != http.MethodHead {
			storageUnavailable(w, r, err)
			return
		}
		if !isNotFound(err) {
			log.Printf("[REGISTRY] HEAD %s: storage unavailable (%v); answering 404 so the push uploads it", r.URL.Path, err)
		}
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		registryError(w, "BLOB_UNKNOWN", "blob not found", http.StatusNotFound)
		return
	}
	size := fileSize(fi)
	setBlobHeaders(w, digest)

	start, length := int64(0), size
	status := http.StatusOK
	if rh := r.Header.Get("Range"); rh != "" {
		s, e, ok := parseRange(rh, size)
		if !ok {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		start, length = s, e-s+1
		status = http.StatusPartialContent
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", s, e, size))
	}
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	if r.Method == http.MethodHead {
		w.WriteHeader(status)
		return
	}

	// Full cold reads go through the cache (coalesced, verified); ranges and oversized blobs stream
	// straight from the remote.
	var body io.ReadCloser
	if rc := blobCache(); status == http.StatusOK && rc != nil {
		body, err = rc.Fetch(remotePath, size, digest, func(ctx context.Context, off int64) (io.ReadCloser, error) {
			return sftpDriver.Reader(ctx, remotePath, off)
		})
		if err != nil {
			if errors.Is(err, readcache.ErrCorrupt) {
				// The remote copy does not hash to its own digest, so it is damaged for good: left in
				// place it would be served to every pull and re-pushed blobs would be skipped on size.
				healCorruptBlob(remotePath, digest, err)
			} else if !errors.Is(err, readcache.ErrTooLarge) && !errors.Is(err, readcache.ErrNoRoom) {
				log.Printf("[CACHE] Fill for %s unavailable (%v), streaming directly", remotePath, err)
			}
			body = nil
		}
	}
	rc := http.NewResponseController(w)
	// The deadline is absolute on the connection: clear it afterwards or a later request on the same
	// keep-alive connection would inherit an expired deadline.
	defer func() { _ = rc.SetWriteDeadline(time.Time{}) }()
	out := deadlineWriter{w: w, rc: rc, timeout: streamWriteTimeout()}
	w.WriteHeader(status)
	_ = rc.Flush()
	if body != nil {
		defer body.Close()
		if _, err := io.CopyBuffer(out, body, make([]byte, 256<<10)); err != nil {
			log.Printf("[REGISTRY] Blob %s: stream ended early: %v", remotePath, err)
			if errors.Is(err, readcache.ErrCorrupt) {
				// Same damage as the fetch-time check: the cached fill is discarded, so the wrong
				// bytes never reach the cache, but the remote file itself still needs removing.
				healCorruptBlob(remotePath, digest, err)
			}
		}
		return
	}
	if err := streamRemote(ctx, out, remotePath, start, length); err != nil {
		log.Printf("[REGISTRY] Blob %s: stream ended early: %v", remotePath, err)
	}
}

// healCorruptBlob deletes a remote blob whose bytes do not match the digest it is stored under. Without
// this the damage is permanent: every pull serves the wrong content, and the next push of that digest is
// skipped because the remote already holds a file of the same size.
//
// The verdict that gets us here can be stale. A pull's fill holds an open handle to the file it started
// reading, so a push that renamed a correct copy over the path in the meantime leaves the fill hashing
// bytes that are already unlinked. Deleting on that verdict would destroy the good copy the client was
// just told (201) had been stored, with no copy left anywhere. So the bytes are hashed again here, under
// the same path lock the writers hold across their whole upload, and only a file that still fails its
// digest is removed.
//
// The delete is deduplicated per path and runs in the background, so the pull that noticed the damage
// still fails fast instead of waiting on a slow round trip.
func healCorruptBlob(remotePath, digest string, cause error) {
	if !isDigestPath(remotePath) {
		return // only content-addressed paths are safe to delete on a content mismatch
	}
	// The digest we verify against has to be the one the path is named after. If a caller ever wires
	// these up wrongly, hashing against a digest the path does not encode would delete a good file.
	if remoteBase(remotePath) != digest {
		return
	}
	want, err := godigest.Parse(digest)
	if err != nil {
		return
	}
	if _, busy := healing.LoadOrStore(remotePath, struct{}{}); busy {
		return
	}
	if !goBackground(func(ctx context.Context) {
		defer healing.Delete(remotePath)
		l := pathLock(remotePath)
		l.Lock()
		defer l.Unlock()
		// The re-read costs a full download of the blob, so bound it in time. A blob too large to
		// re-verify, or one the remote cannot currently be read from, comes back unverified and is left
		// alone: the audit is what deals with the size class, and a storage blip must never read as
		// corruption.
		ctx, cancel := context.WithTimeout(ctx, healVerifyTimeout)
		defer cancel()
		switch remoteMatchesDigestWithin(ctx, remotePath, want, healVerifyTimeout) {
		case verifyMatches:
			// A concurrent push already replaced it with the correct bytes, so the verdict we were
			// given was about the copy this pull happened to read. Nothing to remove.
			log.Printf("[REGISTRY] Heal %s: skipped, the stored copy now matches its digest", remotePath)
			return
		case verifyUnchecked:
			log.Printf("[REGISTRY] Heal %s: could not verify the stored copy; leaving it for the audit", remotePath)
			return
		}
		// Mark before deleting, not after, exactly as the audit does. The mark says "do not trust the
		// size here", and between confirming the damage and removing the file the corrupt copy is still
		// sitting there at exactly the size a correct push would present — so a push landing in that
		// window would be skipped on size and end up with no blob at all.
		markCorrupt(remotePath)
		if err := sftpDriver.Delete(ctx, remotePath); err != nil && !isNotFound(err) {
			log.Printf("[REGISTRY] Heal %s: delete failed (%v); leaving it for a later pull", remotePath, err)
			clearCorrupt(remotePath) // nothing was removed, so the mark must not outlive it
			return
		}
		log.Printf("[REGISTRY] Heal %s: removed corrupt blob (digest %s, %v); the next push rewrites it", remotePath, digest, cause)
	}) {
		healing.Delete(remotePath) // shutting down, so nothing ran: do not leave the path marked
	}
}

// deadlineReader is implemented by the pooled SFTP driver: a Reader that cannot block forever.
type deadlineReader interface {
	ReaderWithDeadline(ctx context.Context, path string, offset int64, timeout time.Duration) (io.ReadCloser, error)
}

// openBoundedReader opens remotePath for a background scan that must be able to give up. Drivers that
// cannot bound a read fall back to Reader; the stall watchdog is the only backstop there.
func openBoundedReader(ctx context.Context, remotePath string, timeout time.Duration) (io.ReadCloser, error) {
	if dr, ok := sftpDriver.(deadlineReader); ok {
		return dr.ReaderWithDeadline(ctx, remotePath, 0, timeout)
	}
	return sftpDriver.Reader(ctx, remotePath, 0)
}

// verifyResult is what a remote-blob verification concluded.
//
// The three cases are distinct because the callers want different things from "too big to check": the
// heal should leave the blob alone (the audit can take as long as it needs), while the upload path must
// not treat an unverified blob as good — that is the exact bug the verification exists to catch.
type verifyResult int

const (
	verifyMatches   verifyResult = iota // the bytes on the remote hash to want
	verifyMismatch                      // they were read and they do not
	verifyUnchecked                     // could not be read: wrong size to hash, or storage failed
)

// remoteMatchesDigestWithin reports whether the blob at remotePath hashes to want. A file that is not
// there counts as matching: there is nothing to replace. A blob too large to re-read, or one that could
// not be read at all, comes back as verifyUnchecked with the reason in the log — neither matches nor
// mismatches, so callers decide.
func remoteMatchesDigestWithin(ctx context.Context, remotePath string, want godigest.Digest, timeout time.Duration) verifyResult {
	fi, err := sftpDriver.Stat(ctx, remotePath)
	if err != nil {
		if isNotFound(err) {
			return verifyMatches
		}
		log.Printf("[REGISTRY] verify %s: stat failed: %v", remotePath, err)
		return verifyUnchecked
	}
	if size := fileSize(fi); size >= 0 && size > healVerifyMaxBytes {
		log.Printf("[REGISTRY] verify %s: %d bytes exceeds the %d byte re-verify limit; not checked here", remotePath, size, healVerifyMaxBytes)
		return verifyUnchecked
	}
	rc, err := openBoundedReader(ctx, remotePath, timeout)
	if err != nil {
		if isNotFound(err) {
			return verifyMatches
		}
		log.Printf("[REGISTRY] verify %s: read failed: %v", remotePath, err)
		return verifyUnchecked
	}
	defer rc.Close()
	h := want.Algorithm().Hash()
	if _, err := io.CopyBuffer(h, rc, make([]byte, 256<<10)); err != nil {
		log.Printf("[REGISTRY] verify %s: hashing failed: %v", remotePath, err)
		return verifyUnchecked
	}
	if godigest.NewDigest(want.Algorithm(), h) != want {
		return verifyMismatch
	}
	return verifyMatches
}

type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) Write(p []byte) (int, error) {
	n, err := e.w.Write(p)
	if err != nil {
		e.err = err
	}
	return n, err
}

// streamRemote copies length bytes from offset, reopening at the current offset when the remote read
// fails (e.g. a connection killed by the stall watchdog) so the client keeps receiving one valid stream.
func streamRemote(ctx context.Context, w io.Writer, remotePath string, offset, length int64) error {
	ew := &errWriter{w: w}
	buf := make([]byte, 256<<10)
	failures := 0
	var lastErr error
	for length > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		if failures > remoteResumeAttempts {
			return fmt.Errorf("giving up at offset %d: %w", offset, lastErr)
		}
		if failures > 0 {
			time.Sleep(time.Duration(failures) * 100 * time.Millisecond)
		}
		rc, err := sftpDriver.Reader(ctx, remotePath, offset)
		if err != nil {
			failures++
			lastErr = err
			continue
		}
		n, err := io.CopyBuffer(ew, io.LimitReader(rc, length), buf)
		rc.Close()
		offset += n
		length -= n
		if ew.err != nil {
			return ew.err // client went away or stopped reading
		}
		if err == nil && length > 0 {
			return fmt.Errorf("remote blob shorter than expected (%d bytes missing)", length)
		}
		if err != nil {
			if n > 0 {
				failures = 0
			}
			failures++
			lastErr = err
			log.Printf("[REGISTRY] Remote read of %s interrupted at offset %d: %v; resuming", remotePath, offset, err)
		}
	}
	return nil
}

// parseRange handles a single "bytes=a-b", "bytes=a-" or "bytes=-n" range. Multiple ranges are not
// supported (registry clients never send them) and are reported as unsatisfiable, like malformed input.
func parseRange(h string, size int64) (int64, int64, bool) {
	spec, ok := strings.CutPrefix(strings.TrimSpace(h), "bytes=")
	if !ok || strings.Contains(spec, ",") {
		return 0, 0, false
	}
	a, b, ok := strings.Cut(strings.TrimSpace(spec), "-")
	if !ok {
		return 0, 0, false
	}
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == "" {
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil || n <= 0 || size == 0 {
			return 0, 0, false
		}
		if n > size {
			n = size
		}
		return size - n, size - 1, true
	}
	start, err := strconv.ParseInt(a, 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false
	}
	end := size - 1
	if b != "" {
		e, err := strconv.ParseInt(b, 10, 64)
		if err != nil || e < start {
			return 0, 0, false
		}
		if e < end {
			end = e
		}
	}
	return start, end, true
}
