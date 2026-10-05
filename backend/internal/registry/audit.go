package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	godigest "github.com/opencontainers/go-digest"
)

// AuditCorruptBlobsHandler verifies every digest-named blob on the remote and removes the ones whose
// bytes do not hash to their own name.
//
// The read path heals this on demand, but only for blobs small enough to re-verify cheaply (see
// healVerifyMaxBytes) and only once something pulls them. This walks the whole registry instead, so
// damage left by the pre-spool upload path is found even for blobs nobody has pulled since, and even
// when the read cache keeps serving them locally.
//
// It streams each blob once and can run for a long time on a slow link, so it is deliberately not on
// any request path of a push or pull. Run it when the box is otherwise idle.
func AuditCorruptBlobsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// The scan is long: bound it so one stalled blob cannot hang the request forever.
	timeout := auditTimeoutDefault
	if v := r.URL.Query().Get("timeout"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			registryError(w, "UNSUPPORTED", "invalid timeout", http.StatusBadRequest)
			return
		}
		timeout = d
	}
	repo := strings.Trim(r.URL.Query().Get("repository"), "/")
	// The repository name becomes a path the walk lists and deletes from, so it has to stay inside
	// registry/: ".." would otherwise walk out of it and remove matching files wherever it landed.
	if !safeAuditRepo(repo) {
		registryError(w, "NAME_INVALID", "invalid repository name", http.StatusBadRequest)
		return
	}
	remove := r.URL.Query().Get("remove") == "true"

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	res, err := auditBlobs(ctx, repo, remove, auditProgress)
	if err != nil {
		log.Printf("[AUDIT] %v", err)
		registryError(w, "UNAVAILABLE", "audit failed: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(res)
}

// auditProgress logs periodically so a long scan shows it is alive.
func auditProgress(done, total int, repo string) {
	if done%25 == 0 || done == total {
		log.Printf("[AUDIT] %s: verified %d/%d blobs", repo, done, total)
	}
}

// AuditResult is the report of one audit run.
type AuditResult struct {
	Repository string `json:"repository,omitempty"`
	Scanned    int    `json:"scanned"`
	Corrupt    int    `json:"corrupt"`
	Removed    int    `json:"removed"`
	Failed     int    `json:"failed"`
	// CorruptPaths lists what was wrong, whether or not it was removed.
	CorruptPaths []string `json:"corrupt_paths"`
	// TooLarge counts blobs skipped because they exceed the audit's own size cap. These are the ones
	// the read-path heal deliberately leaves alone.
	TooLarge int      `json:"too_large"`
	TimedOut bool     `json:"timed_out"`
	Errors   []string `json:"errors,omitempty"`
}

// auditMaxBytes bounds what the audit will stream. Anything bigger is reported as skipped rather than
// read, so a single enormous layer cannot make the scan take hours.
const auditMaxBytes = 1 << 30 // 1 GiB
const auditTimeoutDefault = 2 * time.Hour

// isRepoBlobsDir reports whether child, found inside dir, is a repository's blobs directory. It is
// recognised structurally rather than by depth: a repository directory is one that holds both a blobs
// and a manifests directory, which is what the registry creates for every repository. Depth counting
// cannot work here, because a repository is one or two path segments below the root depending on
// whether it has a group, and CreateRepositoryFolder accepts arbitrary names — so a "blobs" directory
// at the wrong depth is a missed repository, while treating any "blobs" name as one makes a group
// literally called "blobs" look like a repository.
//
// entries are the names already listed for dir by the walk, so this costs nothing: the parent has just
// been read anyway.
func isRepoBlobsDir(entries []string, child string) bool {
	if child != "blobs" {
		return false
	}
	// The repository root must also hold a manifests directory; a group directory that merely happens
	// to be named "blobs" will not.
	for _, n := range entries {
		if n == "manifests" {
			return true
		}
	}
	return false
}

// safeAuditRepo reports whether repo can be used as the root of an audit walk. It is turned into a path
// the walk lists and deletes from, so what matters is that it stays inside registry/.
//
// This is deliberately looser than validateRepoName: CreateRepositoryHandler accepts any non-empty name,
// so a repository can legitimately be deeper than "group/repo" and the audit must still reach it. What
// it rejects is what would escape: "..", an empty segment, and anything outside the name character set.
func safeAuditRepo(repo string) bool {
	if repo == "" {
		return true // the whole registry
	}
	for _, seg := range strings.Split(repo, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
		for i := 0; i < len(seg); i++ {
			c := seg[i]
			ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
				c == '.' || c == '_' || c == '-'
			if !ok {
				return false
			}
		}
	}
	return true
}

// auditBlobs walks registry/<repo>/blobs/ and hashes every digest-named file.
//
// Removal happens under the same per-path lock the writers hold across an upload, and only after the
// bytes at the path are confirmed to still be wrong, so a blob a client pushed while the audit was
// walking is never deleted.
func auditBlobs(ctx context.Context, repo string, remove bool, progress func(done, total int, repo string)) (*AuditResult, error) {
	// The repository name is part of the path this walks and deletes from, so it must stay inside
	// registry/. Without this ".." would take the walk out of it and it would remove matching files
	// wherever it landed.
	if !safeAuditRepo(repo) {
		return nil, fmt.Errorf("invalid repository name %q", repo)
	}
	root := "registry"
	if repo != "" {
		root = "registry/" + repo
	}
	res := &AuditResult{Repository: repo, CorruptPaths: []string{}}

	// Only objects sitting directly in a blobs/ directory are blob objects. Whether the walk is inside
	// one is tracked as it descends rather than guessed from a path substring: a group or repository
	// literally named "blobs" would otherwise make every descendant path contain "/blobs/", and a scoped
	// run with remove=true could reach (and delete) another repository's manifests.
	type entry struct{ path, name string }
	type dirEntry struct {
		path string
		// blobs is set when path is the blobs directory of the repository that contains it, so its
		// digest-named children are blob objects.
		blobs bool
	}
	var toScan []entry
	stack := []dirEntry{{path: root}}
	visited := make(map[string]bool)
	queued := make(map[string]bool)
	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			res.TimedOut = true
			return res, nil
		}
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		dir := cur.path
		// Each distinct path is listed once, so a directory reachable under two names does not get its
		// blobs hashed and reported twice. This is keyed on the path string the driver returns, which is
		// all the walk has to identify a directory.
		if visited[dir] {
			continue
		}
		visited[dir] = true
		names, err := sftpDriver.List(ctx, dir)
		if err != nil {
			if isNotFound(err) {
				continue
			}
			if isNotDir(err) {
				// Expected: tag manifests and other non-directory files are descended into before we
				// know they are leaves. Counting those would make a healthy tagged repository look
				// broken, so only genuinely unreadable directories count as failures.
				continue
			}
			// Any other failure (a real directory we cannot read) is counted but not fatal: it just has
			// nothing to audit, and aborting the whole run over one bad directory would be worse.
			log.Printf("[AUDIT] cannot list %s (%v); skipping", dir, err)
			res.Failed++
			continue
		}
		for _, name := range names {
			if strings.Contains(name, ".uploading-") {
				continue // leftover temp file, not an object
			}
			full := dir + "/" + name
			if isDigestPath(name) {
				if cur.blobs && !queued[full] {
					queued[full] = true
					toScan = append(toScan, entry{full, name})
				}
				continue
			}
			// A blobs directory is only a repository's when its parent also holds manifests/.
			stack = append(stack, dirEntry{path: full, blobs: isRepoBlobsDir(names, name)})
		}
	}

	for i, e := range toScan {
		if err := ctx.Err(); err != nil {
			res.TimedOut = true
			log.Printf("[AUDIT] stopping after %d/%d blobs: %v", i, len(toScan), err)
			break
		}
		res.Scanned++
		ok, err := auditOne(ctx, e.path, e.name, remove)
		if progress != nil {
			progress(res.Scanned, len(toScan), repo)
		}
		switch {
		case err != nil:
			res.Failed++
			if len(res.Errors) < 20 {
				res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", e.path, err))
			}
		case ok == auditTooLarge:
			res.TooLarge++
		case ok == auditCorrupt:
			res.Corrupt++
			res.CorruptPaths = append(res.CorruptPaths, e.path)
			if remove {
				res.Removed++
			}
		}
	}
	return res, nil
}

type auditOutcome int

const (
	auditOK auditOutcome = iota
	auditCorrupt
	auditTooLarge
)

// auditHashTimeout bounds hashing one blob. The audit's own timeout ends the whole run, but a single
// blob must not be able to sit on a path lock for the entire run.
const auditHashTimeout = 5 * time.Minute

// auditOne hashes one remote blob and, when remove is set and the damage is confirmed, deletes it.
//
// In remove mode the hash runs under the path lock, which is what makes the deletion safe against a
// concurrent push, but it also means a slow blob holds that lock (and a pooled connection) for the
// length of one download. That only blocks other traffic for the same digest, which is exactly the
// traffic the lock is there to serialise, so it is a fair trade for never deleting a fresh push.
//
// In report-only mode the lock is deliberately not taken: nothing is deleted, and the audit should not
// stall pushes just to look.
func auditOne(ctx context.Context, remotePath, name string, remove bool) (auditOutcome, error) {
	want, err := godigest.Parse(name)
	if err != nil {
		return auditOK, err // not a digest we understand; the caller already filtered on shape
	}
	fi, err := sftpDriver.Stat(ctx, remotePath)
	if err != nil {
		if isNotFound(err) {
			return auditOK, nil // gone (pushed or healed elsewhere) between listing and now
		}
		return auditOK, err
	}
	if size := fileSize(fi); size >= 0 && size > auditMaxBytes {
		return auditTooLarge, nil
	}
	if !remove {
		// Report-only mode still has to read the bytes to know, but it must not take the path lock:
		// a push may be in flight and we do not want to block it just to look.
		switch remoteMatchesDigestWithin(ctx, remotePath, want, auditHashTimeout) {
		case verifyMatches:
			return auditOK, nil
		case verifyUnchecked:
			return auditOK, fmt.Errorf("could not read %s for verification", remotePath)
		}
		return auditCorrupt, nil
	}

	l := pathLock(remotePath)
	l.Lock()
	defer l.Unlock()
	switch remoteMatchesDigestWithin(ctx, remotePath, want, auditHashTimeout) {
	case verifyMatches:
		return auditOK, nil
	case verifyUnchecked:
		// Never delete something we could not read: a storage blip must not read as corruption.
		return auditOK, fmt.Errorf("could not read %s for verification", remotePath)
	}
	// Mark before deleting, not after. The mark says "do not trust the size here". Between confirming
	// the damage and removing the file there is still a corrupt copy of exactly the size a correct
	// push would present, so a client pushing at that moment would be skipped on size, never written,
	// and then left with a blob that is gone. Marking first closes that window: the push either happens
	// before the mark (and the re-verify above sees its bytes and backs off) or after it (and the mark
	// forces a real write).
	markCorrupt(remotePath)
	if err := sftpDriver.Delete(ctx, remotePath); err != nil && !isNotFound(err) {
		return auditOK, fmt.Errorf("delete: %w", err)
	}
	log.Printf("[AUDIT] removed corrupt blob %s (named %s)", remotePath, name)
	return auditCorrupt, nil
}
