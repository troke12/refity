// Package spool is a durable local queue of files waiting to be uploaded to remote storage.
//
// Each entry is two files in Dir: <id>.data (the payload, already digest-verified by the caller) and
// <id>.job (a small JSON sidecar written atomically after the data is durable). An entry exists iff its
// .job file exists, so a crash at any point either leaves a complete, resumable job or garbage that Open
// sweeps away. Workers retry uploads forever with exponential backoff: once a push has been acknowledged
// the only way its data leaves the spool is a successful upload, or the caller explicitly forgetting it
// (a newer direct write of the same path, or a deleted repository).
package spool

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ErrFull is returned by Add when accepting the file would exceed MaxBytes; callers upload synchronously.
var ErrFull = errors.New("spool: pending bytes would exceed SPOOL_MAX_BYTES")

// Names the spool itself creates. Startup only ever deletes files matching these, so a SPOOL_DIR pointed
// at the wrong directory by mistake cannot destroy unrelated data.
var (
	ownFileRe     = regexp.MustCompile(`^\d{8}T\d{6}-[0-9a-f]{24}\.(data|job)(\.tmp)?$`)
	ownIncomingRe = regexp.MustCompile(`^\d{8}T\d{6}-[0-9a-f]{24}\.incoming\.tmp$`)
)

// Job describes one pending upload.
type Job struct {
	ID     string `json:"id"`
	Remote string `json:"remote"`
	Size   int64  `json:"size"`
	Digest string `json:"digest,omitempty"`
	// Mutable marks paths whose content can change (manifest tags): a newer Add for the same Remote
	// replaces the older one instead of being deduplicated against it.
	Mutable bool `json:"mutable,omitempty"`
	// Unbudgeted jobs are accepted even over MaxBytes (manifests: tiny, and must keep their ordering
	// relative to older spooled versions of the same tag).
	Unbudgeted bool      `json:"unbudgeted,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	// Seq orders versions of the same path by the order they were added. Wall-clock CreatedAt cannot:
	// the host clock may step backwards, and then an older version would win on restart.
	Seq int64 `json:"seq,omitempty"`
}

// UploadFunc uploads localPath to job.Remote. It must be idempotent and should honour ctx.
type UploadFunc func(ctx context.Context, localPath string, job Job) error

// DoneFunc takes ownership of localPath after a successful upload (e.g. moves it into a read cache).
type DoneFunc func(job Job, localPath string)

type Options struct {
	Dir      string
	MaxBytes int64 // 0 = unlimited
	Workers  int
	Upload   UploadFunc
	OnDone   DoneFunc // nil deletes the local copy
	// Lock, if set, is held around each upload attempt (the caller's per-remote-path lock). The spool
	// re-checks that the job is still the current version of its path after acquiring it, so an older
	// version that was waiting on the lock can never overwrite a newer one.
	Lock func(remote string) (unlock func())
	// Now is the clock used for job timestamps (default time.Now; replaceable in tests).
	Now func() time.Time
	// Backoff between failed attempts: BackoffBase * 2^n, capped at BackoffMax. Defaults 1s / 5m.
	BackoffBase time.Duration
	BackoffMax  time.Duration
}

type entry struct {
	job  Job
	data string

	running     bool
	superseded  bool
	finished    bool
	released    bool // pending bytes already subtracted
	cancel      context.CancelFunc
	stopped     chan struct{} // closed when the entry leaves the spool
	attempts    int
	nextAttempt time.Time
}

// Stats is a snapshot for health reporting.
type Stats struct {
	Pending       int
	PendingBytes  int64
	OldestAge     time.Duration
	LastError     string
	LastErrorAt   time.Time
	LastSuccessAt time.Time
}

type Spool struct {
	opts Options

	mu       sync.Mutex
	cond     *sync.Cond
	byRemote map[string]*entry
	running  map[*entry]struct{}
	queue    []*entry
	pending  int64
	closed   bool
	seq      int64 // highest Job.Seq handed out or loaded

	lastErr       string
	lastErrAt     time.Time
	lastSuccessAt time.Time

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// Open loads existing jobs from opts.Dir and starts the workers.
func Open(opts Options) (*Spool, error) {
	if opts.Dir == "" {
		return nil, errors.New("spool: Dir is required")
	}
	if opts.Upload == nil {
		return nil, errors.New("spool: Upload is required")
	}
	if opts.Workers < 1 {
		opts.Workers = 1
	}
	if opts.BackoffBase <= 0 {
		opts.BackoffBase = time.Second
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.BackoffMax <= 0 {
		opts.BackoffMax = 5 * time.Minute
	}
	if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
		return nil, err
	}
	s := &Spool{opts: opts, byRemote: map[string]*entry{}, running: map[*entry]struct{}{}}
	s.cond = sync.NewCond(&s.mu)
	s.ctx, s.cancel = context.WithCancel(context.Background())
	if err := s.load(); err != nil {
		return nil, err
	}
	for i := 0; i < opts.Workers; i++ {
		s.wg.Add(1)
		go s.worker()
	}
	return s, nil
}

func (s *Spool) jobPath(id string) string  { return filepath.Join(s.opts.Dir, id+".job") }
func (s *Spool) dataPath(id string) string { return filepath.Join(s.opts.Dir, id+".data") }

func newEntry(job Job, data string) *entry {
	return &entry{job: job, data: data, stopped: make(chan struct{})}
}

func (s *Spool) load() error {
	names, err := os.ReadDir(s.opts.Dir)
	if err != nil {
		return err
	}
	var entries []*entry
	keep := map[string]bool{}
	for _, de := range names {
		name := de.Name()
		if !strings.HasSuffix(name, ".job") || !ownFileRe.MatchString(name) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.opts.Dir, name))
		var job Job
		if err == nil {
			err = json.Unmarshal(b, &job)
		}
		if err != nil || job.ID+".job" != name || job.Remote == "" {
			log.Printf("[SPOOL] Dropping unreadable job file %s: %v", name, err)
			_ = os.Remove(filepath.Join(s.opts.Dir, name))
			continue
		}
		data := s.dataPath(job.ID)
		st, err := os.Stat(data)
		if err != nil || st.Size() != job.Size {
			log.Printf("[SPOOL] Dropping job %s for %s: data file missing or wrong size", job.ID, job.Remote)
			_ = os.Remove(filepath.Join(s.opts.Dir, name))
			continue
		}
		entries = append(entries, newEntry(job, data))
	}
	// Add order: Seq when present (jobs written before Seq existed have 0 and fall back to CreatedAt).
	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i].job, entries[j].job
		if a.Seq != b.Seq {
			return a.Seq < b.Seq
		}
		return a.CreatedAt.Before(b.CreatedAt)
	})
	for _, e := range entries {
		if e.job.Seq > s.seq {
			s.seq = e.job.Seq
		}
	}
	for _, e := range entries {
		if old := s.byRemote[e.job.Remote]; old != nil {
			// Same remote twice: keep only the newest (identical for blobs, latest wins for tags).
			s.discardFiles(old)
			s.pending -= old.job.Size
			old.superseded = true
			delete(keep, old.job.ID)
		}
		s.byRemote[e.job.Remote] = e
		s.pending += e.job.Size
		keep[e.job.ID] = true
	}
	for _, e := range entries {
		if !e.superseded {
			s.queue = append(s.queue, e)
		}
	}
	// Remaining spool-owned names are debris from an interrupted Add (data without a job, temp files).
	// Anything that does not look like ours is left alone.
	for _, de := range names {
		name := de.Name()
		if !ownFileRe.MatchString(name) && !ownIncomingRe.MatchString(name) {
			continue
		}
		id := name[:strings.IndexByte(name, '.')]
		if keep[id] && (strings.HasSuffix(name, ".job") || strings.HasSuffix(name, ".data")) {
			continue
		}
		if strings.HasSuffix(name, ".job") {
			continue // handled (kept or dropped) above
		}
		_ = os.Remove(filepath.Join(s.opts.Dir, name))
	}
	if len(s.queue) > 0 {
		log.Printf("[SPOOL] Resuming %d pending upload(s), %d bytes", len(s.queue), s.pending)
	}
	return nil
}

func newID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic("spool: crypto/rand failed: " + err.Error())
	}
	return time.Now().UTC().Format("20060102T150405") + "-" + hex.EncodeToString(b)
}

// Add takes ownership of srcPath (moving it into the spool) and queues job. On error srcPath is left in
// place. ErrFull means nothing was taken and the caller should upload synchronously.
func (s *Spool) Add(srcPath string, job Job) error {
	if job.Remote == "" {
		return errors.New("spool: job without remote path")
	}
	if job.ID == "" {
		job.ID = newID()
	}
	if job.CreatedAt.IsZero() {
		job.CreatedAt = s.opts.Now().UTC()
	}
	st, err := os.Stat(srcPath)
	if err != nil {
		return err
	}
	job.Size = st.Size()

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("spool: closed")
	}
	if old := s.byRemote[job.Remote]; old != nil && !job.Mutable && !old.job.Mutable {
		// Content-addressed and already pending: identical bytes, nothing to do.
		s.mu.Unlock()
		_ = os.Remove(srcPath)
		return nil
	}
	if !job.Unbudgeted && s.opts.MaxBytes > 0 && s.pending+job.Size > s.opts.MaxBytes {
		s.mu.Unlock()
		return ErrFull
	}
	s.seq++
	job.Seq = s.seq
	s.pending += job.Size // reserve before the (slow) disk work so concurrent Adds respect the budget
	s.mu.Unlock()

	unreserve := func() {
		s.mu.Lock()
		s.pending -= job.Size
		s.mu.Unlock()
	}
	data := s.dataPath(job.ID)
	if err := moveDurable(srcPath, data); err != nil {
		unreserve()
		return err
	}
	if err := s.writeJob(job); err != nil {
		_ = os.Rename(data, srcPath)
		unreserve()
		return err
	}

	e := newEntry(job, data)
	s.mu.Lock()
	if old := s.byRemote[job.Remote]; old != nil {
		s.supersedeLocked(old)
	}
	s.byRemote[job.Remote] = e
	s.queue = append(s.queue, e)
	s.cond.Broadcast()
	s.mu.Unlock()
	return nil
}

// AddBytes spools an in-memory payload (manifests).
func (s *Spool) AddBytes(data []byte, job Job) error {
	tmp := filepath.Join(s.opts.Dir, newID()+".incoming.tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := s.Add(tmp, job); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// supersedeLocked takes e out of service: an idle entry is discarded now, a running upload is cancelled
// and discarded when its attempt returns.
func (s *Spool) supersedeLocked(e *entry) {
	e.superseded = true
	if s.byRemote[e.job.Remote] == e {
		delete(s.byRemote, e.job.Remote)
	}
	if e.running {
		if e.cancel != nil {
			e.cancel()
		}
		return
	}
	if !e.finished {
		s.discardFiles(e)
		s.releaseLocked(e)
		e.finished = true
		close(e.stopped)
	}
}

// Forget drops the pending version of remote (if any) and cancels its in-flight upload. Call it before
// writing remote directly, so an older spooled version cannot later overwrite the direct write. The
// returned channel closes once no upload of the forgotten entry is running any more.
func (s *Spool) Forget(remote string) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.byRemote[remote]
	if e == nil {
		done := make(chan struct{})
		close(done)
		return done
	}
	s.supersedeLocked(e)
	return e.stopped
}

// ForgetID is Forget, but only if the pending version of remote is still the job with this id (a caller
// that wrote a newer version directly must not drop an even newer one spooled in the meantime).
func (s *Spool) ForgetID(remote, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.byRemote[remote]; e != nil && e.job.ID == id {
		s.supersedeLocked(e)
	}
}

// ForgetPrefix forgets every pending job under prefix (a deleted repository) and waits up to timeout for
// their in-flight uploads to stop. Returns how many jobs were dropped.
func (s *Spool) ForgetPrefix(prefix string, timeout time.Duration) int {
	s.mu.Lock()
	var waits []<-chan struct{}
	n := 0
	for remote, e := range s.byRemote {
		if strings.HasPrefix(remote, prefix) {
			s.supersedeLocked(e)
			waits = append(waits, e.stopped)
			n++
		}
	}
	// Older superseded versions may still be mid-upload without being in byRemote.
	for e := range s.running {
		if strings.HasPrefix(e.job.Remote, prefix) {
			s.supersedeLocked(e)
			waits = append(waits, e.stopped)
		}
	}
	s.mu.Unlock()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for _, w := range waits {
		select {
		case <-w:
		case <-deadline.C:
			log.Printf("[SPOOL] ForgetPrefix(%s): uploads still stopping after %v", prefix, timeout)
			return n
		}
	}
	return n
}

// Open returns the newest pending copy of remote. The file stays readable even if the upload finishes
// and the spool hands it off meanwhile (open descriptors survive rename/unlink).
func (s *Spool) Open(remote string) (*os.File, Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.byRemote[remote]
	if e == nil {
		return nil, Job{}, false
	}
	f, err := os.Open(e.data)
	if err != nil {
		return nil, Job{}, false
	}
	return f, e.job, true
}

// Lookup reports whether remote is pending, without opening it.
func (s *Spool) Lookup(remote string) (Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.byRemote[remote]
	if e == nil {
		return Job{}, false
	}
	return e.job, true
}

// Pending lists jobs whose remote path starts with prefix.
func (s *Spool) Pending(prefix string) []Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Job
	for remote, e := range s.byRemote {
		if strings.HasPrefix(remote, prefix) {
			out = append(out, e.job)
		}
	}
	return out
}

// PendingBytes is the total size of not-yet-uploaded data.
func (s *Spool) PendingBytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pending
}

// Stats summarises the queue for health reporting.
func (s *Spool) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Stats{
		Pending:       len(s.byRemote),
		PendingBytes:  s.pending,
		LastError:     s.lastErr,
		LastErrorAt:   s.lastErrAt,
		LastSuccessAt: s.lastSuccessAt,
	}
	var oldest time.Time
	for _, e := range s.byRemote {
		if oldest.IsZero() || e.job.CreatedAt.Before(oldest) {
			oldest = e.job.CreatedAt
		}
	}
	if !oldest.IsZero() {
		st.OldestAge = time.Since(oldest)
	}
	return st
}

// Close stops the workers. In-flight uploads are cancelled; their jobs stay on disk and resume on the
// next Open.
func (s *Spool) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.cond.Broadcast()
	s.mu.Unlock()
	s.cancel()
	s.wg.Wait()
}

// nextLocked removes and returns the first queued entry that is due. A job that failed is requeued at the
// back with a not-before time instead of pinning a worker in a sleep, so a few permanently failing jobs
// cannot starve the rest of the queue.
func (s *Spool) nextLocked(now time.Time) (*entry, time.Time) {
	var earliest time.Time
	for i := 0; i < len(s.queue); i++ {
		e := s.queue[i]
		if e.finished || e.superseded {
			s.queue = append(s.queue[:i], s.queue[i+1:]...)
			i--
			continue
		}
		if !e.nextAttempt.After(now) {
			s.queue = append(s.queue[:i], s.queue[i+1:]...)
			return e, time.Time{}
		}
		if earliest.IsZero() || e.nextAttempt.Before(earliest) {
			earliest = e.nextAttempt
		}
	}
	return nil, earliest
}

func (s *Spool) worker() {
	defer s.wg.Done()
	for {
		s.mu.Lock()
		var e *entry
		for {
			if s.closed {
				s.mu.Unlock()
				return
			}
			var earliest time.Time
			e, earliest = s.nextLocked(time.Now())
			if e != nil {
				break
			}
			var t *time.Timer
			if !earliest.IsZero() {
				t = time.AfterFunc(time.Until(earliest), func() {
					s.mu.Lock()
					s.cond.Broadcast()
					s.mu.Unlock()
				})
			}
			s.cond.Wait()
			if t != nil {
				t.Stop()
			}
		}
		ctx, cancel := context.WithCancel(s.ctx)
		e.running = true
		e.cancel = cancel
		s.running[e] = struct{}{}
		s.mu.Unlock()
		s.attempt(ctx, e)
		cancel()
	}
}

func (s *Spool) backoff(attempt int) time.Duration {
	d := s.opts.BackoffBase
	for i := 0; i < attempt && d < s.opts.BackoffMax; i++ {
		d *= 2
	}
	if d > s.opts.BackoffMax {
		d = s.opts.BackoffMax
	}
	return d
}

// current reports whether e is still the version of its path that should be uploaded.
func (s *Spool) current(e *entry) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !e.superseded && s.byRemote[e.job.Remote] == e
}

func (s *Spool) attempt(ctx context.Context, e *entry) {
	var err error
	skipped := false
	start := time.Now()
	func() {
		if s.opts.Lock != nil {
			unlock := s.opts.Lock(e.job.Remote)
			defer unlock()
		}
		if !s.current(e) {
			skipped = true
			return
		}
		err = s.opts.Upload(ctx, e.data, e.job)
	}()
	switch {
	case skipped:
		s.finish(e, false)
	case err == nil && s.current(e):
		log.Printf("[SPOOL] Uploaded %s (%d bytes, attempt %d, %v)", e.job.Remote, e.job.Size, e.attempts+1, time.Since(start).Round(time.Millisecond))
		s.mu.Lock()
		s.lastSuccessAt = time.Now()
		s.mu.Unlock()
		s.finish(e, true)
	case err == nil:
		// Uploaded, but superseded meanwhile: the newer version is queued behind the path lock.
		s.finish(e, false)
	case s.ctx.Err() != nil:
		s.stopRunning(e) // shutting down: keep the job for the next start
	case !s.current(e):
		s.finish(e, false)
	default:
		if _, statErr := os.Stat(e.data); errors.Is(statErr, os.ErrNotExist) {
			log.Printf("[SPOOL] Dropping job %s for %s: local data vanished", e.job.ID, e.job.Remote)
			s.finish(e, false)
			return
		}
		s.mu.Lock()
		wait := s.backoff(e.attempts)
		e.attempts++
		e.nextAttempt = time.Now().Add(wait)
		e.running = false
		e.cancel = nil
		delete(s.running, e)
		s.lastErr = fmt.Sprintf("%s: %v", e.job.Remote, err)
		s.lastErrAt = time.Now()
		if !e.superseded {
			s.queue = append(s.queue, e)
		} else {
			s.discardFiles(e)
			s.releaseLocked(e)
			e.finished = true
			close(e.stopped)
		}
		s.cond.Broadcast()
		s.mu.Unlock()
		log.Printf("[SPOOL] Upload of %s failed (attempt %d): %v; retrying in %v", e.job.Remote, e.attempts, err, wait)
	}
}

func (s *Spool) stopRunning(e *entry) {
	s.mu.Lock()
	e.running = false
	e.cancel = nil
	delete(s.running, e)
	s.mu.Unlock()
}

func (s *Spool) releaseLocked(e *entry) {
	if !e.released {
		e.released = true
		s.pending -= e.job.Size
	}
}

// finish removes e from the spool. uploaded=true hands the data to OnDone (unless a newer version of the
// same path is pending, in which case the old bytes are just deleted).
func (s *Spool) finish(e *entry, uploaded bool) {
	_ = os.Remove(s.jobPath(e.job.ID))
	_ = syncDir(s.opts.Dir)
	s.mu.Lock()
	e.running = false
	e.cancel = nil
	delete(s.running, e)
	alreadyFinished := e.finished
	e.finished = true
	s.releaseLocked(e)
	if s.byRemote[e.job.Remote] == e {
		delete(s.byRemote, e.job.Remote)
	}
	handOff := uploaded && !e.superseded
	if !alreadyFinished {
		close(e.stopped)
	}
	s.mu.Unlock()
	if handOff && s.opts.OnDone != nil {
		s.opts.OnDone(e.job, e.data)
		return
	}
	_ = os.Remove(e.data)
}

// discardFiles deletes a job and syncs the directory: after a power loss a deleted .job must not come
// back (with its data) and re-upload a superseded tag version.
func (s *Spool) discardFiles(e *entry) {
	_ = os.Remove(s.jobPath(e.job.ID))
	_ = os.Remove(e.data)
	_ = syncDir(s.opts.Dir)
}

// writeJob persists the sidecar atomically: temp file, fsync, rename, fsync the directory.
func (s *Spool) writeJob(job Job) error {
	b, err := json.Marshal(job)
	if err != nil {
		return err
	}
	tmp := s.jobPath(job.ID) + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, s.jobPath(job.ID)); err != nil {
		os.Remove(tmp)
		return err
	}
	return syncDir(s.opts.Dir)
}

// moveDurable moves src to dst and makes the bytes durable. Rename is O(1) when both are on one
// filesystem; across filesystems (e.g. staging in /tmp, spool on a volume) it copies.
func moveDurable(src, dst string) error {
	if err := os.Rename(src, dst); err != nil {
		var linkErr *os.LinkError
		if !errors.As(err, &linkErr) || !errors.Is(linkErr.Err, syscall.EXDEV) {
			return err
		}
		if err := copyFile(src, dst); err != nil {
			return err
		}
		_ = os.Remove(src)
	}
	f, err := os.Open(dst)
	if err != nil {
		return err
	}
	serr := f.Sync()
	f.Close()
	if serr != nil {
		return fmt.Errorf("fsync %s: %w", dst, serr)
	}
	return syncDir(filepath.Dir(dst))
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	// Some filesystems (and Windows) refuse fsync on directories; the rename itself already happened.
	_ = d.Sync()
	return nil
}
