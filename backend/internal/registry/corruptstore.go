package registry

import (
	"bufio"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The corrupt marks have to outlive the process. Without this, a restart drops the memory of every
// digest we already found damaged, and the next push of one of those digests is skipped on size
// again — the exact failure the marks exist to prevent. So they are written to a small file next to the
// spool and reloaded at startup.
//
// The file is a cache of a fact that is also re-derivable (a pull of the blob detects it again), so
// losing or corrupting it is not data loss: it only means the next push of such a digest might be
// trusted on size until something reads it. Writes are therefore best-effort and failures are logged
// rather than propagated.

// corruptFileName holds one remote path per line. Only digest paths are ever written.
const corruptFileName = "corrupt-paths"

// maxCorruptLine bounds one line of the mark file. Real entries are a remote path, so this is generous;
// anything longer is not a mark and is skipped rather than allowed to end the scan.
const maxCorruptLine = 1024 * 1024

// corruptStore persists the corrupt marks. A zero value is ready to use.
type corruptStore struct {
	mu   sync.Mutex
	path string
}

var corruptState corruptStore

// persistCorrupt writes the current mark set to durable storage.
func (s *corruptStore) persist() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path == "" {
		return
	}
	tmp := s.path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		log.Printf("[CORRUPT] cannot write %s: %v", s.path, err)
		return
	}
	w := bufio.NewWriter(f)
	n := 0
	corrupt.Range(func(k, _ any) bool {
		if p, ok := k.(string); ok {
			if _, err := w.WriteString(p + "\n"); err == nil {
				n++
			}
		}
		return true
	})
	if err := w.Flush(); err != nil {
		log.Printf("[CORRUPT] cannot write %s: %v", s.path, err)
		f.Close()
		os.Remove(tmp)
		return
	}
	if err := f.Close(); err != nil {
		log.Printf("[CORRUPT] cannot write %s: %v", s.path, err)
		os.Remove(tmp)
		return
	}
	// Rename is atomic, so a crash mid-write cannot leave a half file that would drop marks on reload.
	if err := os.Rename(tmp, s.path); err != nil {
		log.Printf("[CORRUPT] cannot replace %s: %v", s.path, err)
		os.Remove(tmp)
		return
	}
	log.Printf("[CORRUPT] persisted %d corrupt path(s)", n)
}

// loadCorrupt reads the marks back. A missing file is normal (nothing has been marked yet); an
// unreadable one is logged and ignored, because dropping the marks is recoverable and refusing to start
// over them is not.
func (s *corruptStore) load() {
	s.mu.Lock()
	defer s.mu.Unlock()
	// A reload replaces the in-memory set wholesale: nothing from the previous run survives, so a
	// removedAt left behind by an earlier mark cannot be attributed to the entry we load now.
	corruptRemovedAt.Range(func(k, _ any) bool { corruptRemovedAt.Delete(k); return true })
	if s.path == "" {
		return
	}
	f, err := os.Open(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[CORRUPT] cannot read %s: %v", s.path, err)
		}
		return
	}
	defer f.Close()
	n, skipped := loadCorruptMarks(f)
	if skipped > 0 {
		log.Printf("[CORRUPT] %s: %d line(s) were too long to be marks and were ignored", s.path, skipped)
	}
	if n > 0 {
		log.Printf("[CORRUPT] reloaded %d corrupt path(s)", n)
	}
}

// loadCorruptMarks reads the mark file, one path per line, and returns how many marks it stored and how
// many lines it had to skip.
//
// An over-long line is skipped rather than ending the scan. bufio stops at ErrTooLong, so one stray long
// line would otherwise silently drop every mark after it, turning a recoverable loss into a silent one —
// which is the exact thing this file is written to avoid.
func loadCorruptMarks(r io.Reader) (marks, skipped int) {
	const startBuf = 64 * 1024
	for {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, startBuf), maxCorruptLine)
		progressed := false
		for sc.Scan() {
			progressed = true
			// Only blob paths are ever marked, so nothing else is meaningful here. A hand-edited line
			// cannot be allowed to pin a re-upload of something that was never damaged.
			if isCorruptMarkable(sc.Text()) {
				corrupt.Store(sc.Text(), struct{}{})
				marks++
			}
		}
		if err := sc.Err(); !errors.Is(err, bufio.ErrTooLong) {
			if err != nil {
				log.Printf("[CORRUPT] cannot read the corrupt path file: %v", err)
			}
			return marks, skipped
		}
		// The scanner stopped on an over-long token and the rest of that line is still unread, so start a
		// new scanner over the same reader to pick up where it left off.
		skipped++
		if !progressed {
			// Nothing was consumed this round, so we would spin. Give up rather than loop.
			log.Printf("[CORRUPT] corrupt path file has an unreadable line after %d mark(s); stopping there", marks)
			return marks, skipped
		}
	}
}

// markCorrupt records that a digest path was found holding the wrong bytes and removed, so the next
// push of that digest is actually written instead of being skipped because the size matches what the
// corrupt copy had. Entries are dropped again once a correct copy has been stored at the path.
//
// Only blob paths are accepted. Every caller already filters for those, but the mark outlives the process
// and is consulted by the upload path, so a manifests/ entry (which isDigestPath accepts on its base name
// alone) would pin an unrelated re-upload if one ever got in. Enforcing it here means no future caller can.
//
// removedAt says when the damage was found, which is what the dashboard's "missing since" reports. It
// is not persisted: losing it only makes the reported age unknown, and the mark itself is what matters.
func markCorrupt(p string) {
	if !isCorruptMarkable(p) {
		log.Printf("[CORRUPT] ignoring mark for %s: not a blob path", p)
		return
	}
	if _, loaded := corrupt.LoadOrStore(p, struct{}{}); !loaded {
		corruptRemovedAt.Store(p, time.Now())
		corruptState.persist()
	}
}

// isCorruptMarkable reports whether p is a path the corrupt marks may cover: a digest-named object
// inside a repository's blobs directory.
func isCorruptMarkable(p string) bool {
	return isDigestPath(p) && strings.Contains(p, "/blobs/")
}

// knownCorrupt reports whether p was recorded as corrupt and no correct copy has been written since.
func knownCorrupt(p string) bool {
	_, bad := corrupt.Load(p)
	return bad
}

// clearCorrupt forgets the damage record for p after a correct copy was written there.
func clearCorrupt(p string) {
	corruptRemovedAt.Delete(p)
	if _, loaded := corrupt.LoadAndDelete(p); loaded {
		corruptState.persist()
	}
}

// forgetCorruptPrefix drops every mark under prefix (a "registry/<repo>/" key). Used when a repository is
// deleted: its remote folder is going away, so the marks have nothing left to guard against, and keeping
// them would force a full re-upload of every digest ever marked there if the name is reused.
//
// Only markable paths are touched, so a stray entry that got in some other way is left alone rather than
// silently dropped by a repository delete.
func forgetCorruptPrefix(prefix string) int {
	dropped := 0
	corrupt.Range(func(k, _ any) bool {
		p, ok := k.(string)
		if !ok || !strings.HasPrefix(p, prefix) || !isCorruptMarkable(p) {
			return true
		}
		corruptRemovedAt.Delete(p)
		if _, loaded := corrupt.LoadAndDelete(p); loaded {
			dropped++
		}
		return true
	})
	if dropped > 0 {
		corruptState.persist()
	}
	return dropped
}

// bindCorruptStore points persistence at dir, and loads whatever is already there. Called once from
// initStorage, before any upload or pull can consult the marks.
func bindCorruptStore(dir string) {
	if dir == "" {
		return
	}
	corruptState.mu.Lock()
	corruptState.path = filepath.Join(dir, corruptFileName)
	corruptState.mu.Unlock()
	corruptState.load()
}
