// Package readcache is a size-bounded, LRU, read-through disk cache for immutable (digest-addressed)
// objects fetched from slow remote storage.
//
// Concurrent cold reads of one key share a single remote download ("fill"): the fill writes into a temp
// file and every reader tails that file as it grows, so the first bytes reach each client immediately
// instead of after the whole object. A fill only becomes a cache entry after its size and sha256 match;
// the final byte is withheld from readers until then, so a corrupt or truncated download can never be
// delivered to a client as a complete, valid-length body.
//
// Layout: <dir>/<sha256(group)>/<sha256(key)>. Grouping (the registry groups by repository) lets a whole
// group be dropped, also after a restart when only hashed names are on disk. The cache only ever indexes,
// evicts or deletes names of exactly that shape, so pointing it at a directory with other files in it
// cannot destroy them.
package readcache

import (
	"container/list"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	godigest "github.com/opencontainers/go-digest"
)

// ErrTooLarge means the object can never fit in the budget; stream it without caching.
var ErrTooLarge = errors.New("readcache: object larger than cache budget")

// ErrNoRoom means concurrent fills already reserve the budget; stream this one without caching.
var ErrNoRoom = errors.New("readcache: budget reserved by in-progress fills")

// ErrCorrupt means the source bytes did not match the digest they are stored under. The object on the
// remote is damaged, not the cache, so the caller may delete it and let the next push rewrite it.
var ErrCorrupt = errors.New("readcache: source does not match its digest")

var errDropped = errors.New("readcache: group dropped during fill")

var (
	hashNameRe = regexp.MustCompile(`^[0-9a-f]{64}$`)
	ownTempRe  = regexp.MustCompile(`^[0-9a-f]{64}\.[0-9a-f]{12}\.tmp$`)
)

// OpenFunc opens the remote object at offset. It is called again after a failure to resume from the
// current position.
type OpenFunc func(ctx context.Context, offset int64) (io.ReadCloser, error)

type item struct {
	name string // "<group hash>/<key hash>", relative to dir
	size int64
}

type Cache struct {
	dir string
	max int64

	// Group maps a key to its group (default: one group for everything). Set before first use.
	Group func(key string) string

	mu       sync.Mutex
	items    map[string]*list.Element // name -> element holding *item
	lru      *list.List               // front = most recently used
	size     int64                    // bytes in promoted entries
	reserved int64                    // bytes reserved by in-progress fills
	fills    map[string]*fill

	// ResumeAttempts is how many consecutive failed remote opens/reads a fill tolerates (default 5).
	ResumeAttempts int
	wg             sync.WaitGroup
}

// New opens (or creates) a cache in dir with a budget of maxBytes, indexing entries left by a previous run.
func New(dir string, maxBytes int64) (*Cache, error) {
	if maxBytes <= 0 {
		return nil, errors.New("readcache: budget must be positive")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	c := &Cache{dir: dir, max: maxBytes, items: map[string]*list.Element{}, lru: list.New(), fills: map[string]*fill{}, ResumeAttempts: 5}
	groups, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	type found struct {
		name  string
		size  int64
		mtime time.Time
	}
	var fs []found
	for _, g := range groups {
		if !g.IsDir() || !hashNameRe.MatchString(g.Name()) {
			continue
		}
		ents, err := os.ReadDir(filepath.Join(dir, g.Name()))
		if err != nil {
			continue
		}
		for _, de := range ents {
			name := de.Name()
			if ownTempRe.MatchString(name) {
				_ = os.Remove(filepath.Join(dir, g.Name(), name))
				continue
			}
			if !hashNameRe.MatchString(name) {
				continue
			}
			info, err := de.Info()
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			fs = append(fs, found{g.Name() + "/" + name, info.Size(), info.ModTime()})
		}
	}
	// Oldest first, each pushed to the front, so the most recent ends up at the front.
	sort.Slice(fs, func(i, j int) bool { return fs[i].mtime.Before(fs[j].mtime) })
	for _, f := range fs {
		c.items[f.name] = c.lru.PushFront(&item{name: f.name, size: f.size})
		c.size += f.size
	}
	c.mu.Lock()
	c.evictLocked()
	c.mu.Unlock()
	return c, nil
}

func hashHex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func (c *Cache) groupOf(key string) string {
	if c.Group == nil {
		return ""
	}
	return c.Group(key)
}

func (c *Cache) nameOf(key string) string {
	return hashHex(c.groupOf(key)) + "/" + hashHex(key)
}

func (c *Cache) path(name string) string { return filepath.Join(c.dir, filepath.FromSlash(name)) }

func (c *Cache) tempPath(name string) string { return c.path(name) + "." + randSuffix() + ".tmp" }

// Max is the configured byte budget.
func (c *Cache) Max() int64 { return c.max }

// Size is the bytes currently held, including space reserved by in-progress fills.
func (c *Cache) Size() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.size + c.reserved
}

// Open returns the cached object for key and marks it recently used.
func (c *Cache) Open(key string) (*os.File, int64, bool) {
	name := c.nameOf(key)
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[name]
	if !ok {
		return nil, 0, false
	}
	f, err := os.Open(c.path(name))
	if err != nil {
		c.removeLocked(el)
		return nil, 0, false
	}
	c.lru.MoveToFront(el)
	// Persist recency for the next process start; best effort.
	now := time.Now()
	_ = os.Chtimes(c.path(name), now, now)
	return f, el.Value.(*item).size, true
}

// Has reports whether key is cached (without touching recency).
func (c *Cache) Has(key string) (int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[c.nameOf(key)]; ok {
		return el.Value.(*item).size, true
	}
	return 0, false
}

// Adopt moves an already-verified local file (e.g. a just-uploaded spool copy) into the cache. The file
// is consumed either way.
func (c *Cache) Adopt(key, srcPath string, size int64) {
	if size > c.max {
		_ = os.Remove(srcPath)
		return
	}
	name := c.nameOf(key)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(c.path(name)), 0o755); err != nil {
		_ = os.Remove(srcPath)
		return
	}
	if err := os.Rename(srcPath, c.path(name)); err != nil {
		_ = os.Remove(srcPath)
		return
	}
	c.insertLocked(name, size)
}

// PutBytes caches a small object after checking it matches digest.
func (c *Cache) PutBytes(key string, data []byte, digest string) error {
	if int64(len(data)) > c.max {
		return ErrTooLarge
	}
	d, err := godigest.Parse(digest)
	if err != nil {
		return err
	}
	if d.Algorithm().FromBytes(data) != d {
		return fmt.Errorf("readcache: digest mismatch for %s", key)
	}
	name := c.nameOf(key)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(c.path(name)), 0o755); err != nil {
		return err
	}
	tmp := c.tempPath(name)
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, c.path(name)); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	c.insertLocked(name, int64(len(data)))
	return nil
}

func (c *Cache) insertLocked(name string, size int64) {
	if el, ok := c.items[name]; ok {
		c.size -= el.Value.(*item).size
		el.Value.(*item).size = size
		c.size += size
		c.lru.MoveToFront(el)
	} else {
		c.items[name] = c.lru.PushFront(&item{name: name, size: size})
		c.size += size
	}
	c.evictLocked()
}

func (c *Cache) removeLocked(el *list.Element) {
	it := el.Value.(*item)
	c.lru.Remove(el)
	delete(c.items, it.name)
	c.size -= it.size
}

// evictLocked drops least-recently-used entries until promoted entries plus reservations fit the budget.
// Readers that already opened an evicted file keep reading it: unlinking does not invalidate open
// descriptors.
func (c *Cache) evictLocked() {
	for c.size+c.reserved > c.max && c.lru.Len() > 0 {
		el := c.lru.Back()
		it := el.Value.(*item)
		c.removeLocked(el)
		_ = os.Remove(c.path(it.name))
	}
}

// DropGroup removes every entry of group and cancels its in-progress fills (a deleted repository).
func (c *Cache) DropGroup(group string) {
	g := hashHex(group)
	c.mu.Lock()
	defer c.mu.Unlock()
	for name, el := range c.items {
		if filepath.Dir(filepath.FromSlash(name)) == g {
			c.removeLocked(el)
			_ = os.Remove(c.path(name))
		}
	}
	for key, f := range c.fills {
		if c.groupOf(key) == group {
			f.dropped = true
			delete(c.fills, key)
			f.cancel()
		}
	}
}

func randSuffix() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Close cancels running fills and waits for them, so the directory can be removed afterwards.
func (c *Cache) Close() {
	c.mu.Lock()
	for _, f := range c.fills {
		f.cancel()
	}
	c.mu.Unlock()
	c.wg.Wait()
}

// ---------------------------------------------------------------------------
// Coalesced read-through fills
// ---------------------------------------------------------------------------

type fill struct {
	key    string
	name   string
	tmp    string
	size   int64
	digest godigest.Digest
	cancel context.CancelFunc

	// Guarded by Cache.mu: membership decisions (join, abandon, drop) are made atomically with the
	// fills map so a new reader can never join a fill that is being cancelled.
	readers int
	dropped bool

	mu      sync.Mutex
	cond    *sync.Cond
	written int64
	done    bool
	err     error
}

// Fetch returns a reader for the whole object, filling the cache from open on the way. Concurrent calls
// for the same key share one download. size and digest describe the expected object (the digest is the
// blob name). Returns ErrTooLarge or ErrNoRoom when the object should be streamed without caching.
func (c *Cache) Fetch(key string, size int64, digest string, open OpenFunc) (io.ReadCloser, error) {
	if size > c.max {
		return nil, ErrTooLarge
	}
	d, err := godigest.Parse(digest)
	if err != nil {
		return nil, err
	}
	if f, _, ok := c.Open(key); ok {
		return f, nil
	}
	name := c.nameOf(key)
	c.mu.Lock()
	if fl, ok := c.fills[key]; ok {
		r, err := fl.attach(c)
		c.mu.Unlock()
		if err != nil {
			// The fill just promoted its temp file; the object is now a regular entry.
			if f, _, ok := c.Open(key); ok {
				return f, nil
			}
			return nil, err
		}
		return r, nil
	}
	// Reserve the whole object up front so the budget holds while it downloads, not only once promoted.
	if c.reserved+size > c.max {
		c.mu.Unlock()
		return nil, ErrNoRoom
	}
	c.reserved += size
	c.evictLocked()
	unreserve := func() { c.reserved -= size }
	if err := os.MkdirAll(filepath.Dir(c.path(name)), 0o755); err != nil {
		unreserve()
		c.mu.Unlock()
		return nil, err
	}
	tmp := c.tempPath(name)
	tf, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		unreserve()
		c.mu.Unlock()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	fl := &fill{key: key, name: name, tmp: tmp, size: size, digest: d, cancel: cancel}
	fl.cond = sync.NewCond(&fl.mu)
	r, err := fl.attach(c)
	if err != nil {
		unreserve()
		c.mu.Unlock()
		cancel()
		tf.Close()
		_ = os.Remove(tmp)
		return nil, err
	}
	c.fills[key] = fl
	c.wg.Add(1)
	c.mu.Unlock()
	go c.run(ctx, fl, tf, open)
	return r, nil
}

// attach must be called with c.mu held.
func (f *fill) attach(c *Cache) (*tailReader, error) {
	fd, err := os.Open(f.tmp)
	if err != nil {
		return nil, err
	}
	f.readers++
	return &tailReader{c: c, f: f, file: fd}, nil
}

func (c *Cache) run(ctx context.Context, f *fill, tf *os.File, open OpenFunc) {
	defer c.wg.Done()
	err := c.download(ctx, f, tf, open)
	cerr := tf.Close()
	if err == nil {
		err = cerr
	}
	c.mu.Lock()
	c.reserved -= f.size
	if err == nil && f.dropped {
		err = errDropped
	}
	if err == nil {
		if rerr := os.Rename(f.tmp, c.path(f.name)); rerr != nil {
			err = rerr
		} else {
			c.insertLocked(f.name, f.size)
		}
	}
	if c.fills[f.key] == f {
		delete(c.fills, f.key)
	}
	c.mu.Unlock()
	if err != nil {
		_ = os.Remove(f.tmp)
		if !errors.Is(err, context.Canceled) && !errors.Is(err, errDropped) {
			log.Printf("[CACHE] Fill of %s discarded: %v", f.key, err)
		}
	}
	f.mu.Lock()
	f.done = true
	f.err = err
	f.cond.Broadcast()
	f.mu.Unlock()
}

func (c *Cache) download(ctx context.Context, f *fill, tf *os.File, open OpenFunc) error {
	h := f.digest.Algorithm().Hash()
	buf := make([]byte, 256<<10)
	var off int64
	failures := 0
	var lastErr error
	for off < f.size {
		if err := ctx.Err(); err != nil {
			return err
		}
		if failures > c.ResumeAttempts {
			return fmt.Errorf("giving up after %d failed attempts: %w", failures, lastErr)
		}
		if failures > 0 {
			t := time.NewTimer(time.Duration(failures) * 100 * time.Millisecond)
			select {
			case <-t.C:
			case <-ctx.Done():
				t.Stop()
				return ctx.Err()
			}
		}
		rc, err := open(ctx, off)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			failures++
			lastErr = err
			continue
		}
		progressed := false
		var rerr error
		lr := io.LimitReader(rc, f.size-off)
		for {
			if ctx.Err() != nil {
				rerr = ctx.Err()
				break
			}
			n, err := lr.Read(buf)
			if n > 0 {
				if _, werr := tf.Write(buf[:n]); werr != nil {
					rc.Close()
					return werr
				}
				h.Write(buf[:n])
				off += int64(n)
				progressed = true
				f.publish(off)
			}
			if err != nil {
				rerr = err
				break
			}
		}
		rc.Close()
		if errors.Is(rerr, context.Canceled) {
			return rerr
		}
		if rerr == io.EOF && off < f.size {
			return fmt.Errorf("remote object shorter than expected: %d of %d bytes", off, f.size)
		}
		if rerr != io.EOF && rerr != nil {
			if progressed {
				failures = 0
			}
			failures++
			lastErr = rerr
			log.Printf("[CACHE] Fill of %s interrupted at %d/%d: %v; resuming", f.key, off, f.size, rerr)
		}
	}
	if got := godigest.NewDigest(f.digest.Algorithm(), h); got != f.digest {
		return fmt.Errorf("%w: got %s", ErrCorrupt, got)
	}
	return nil
}

func (f *fill) publish(off int64) {
	f.mu.Lock()
	f.written = off
	f.cond.Broadcast()
	f.mu.Unlock()
}

type tailReader struct {
	c      *Cache
	f      *fill
	file   *os.File
	pos    int64
	closed bool
}

func (t *tailReader) Read(p []byte) (int, error) {
	f := t.f
	f.mu.Lock()
	var limit int64
	for {
		limit = f.written
		if f.done && f.err == nil {
			limit = f.size
		} else if limit >= f.size {
			limit = f.size - 1 // hold back the last byte until the digest is verified
		}
		if t.pos < limit {
			break
		}
		if f.done {
			err := f.err
			f.mu.Unlock()
			if err == nil {
				return 0, io.EOF
			}
			return 0, err
		}
		f.cond.Wait()
	}
	f.mu.Unlock()
	if avail := limit - t.pos; int64(len(p)) > avail {
		p = p[:avail]
	}
	n, err := t.file.ReadAt(p, t.pos)
	t.pos += int64(n)
	if err == io.EOF && n > 0 {
		err = nil
	}
	return n, err
}

// Close detaches the reader; when the last reader leaves an unfinished fill, the fill is removed from the
// map and cancelled in one step under the cache lock, so a reader arriving right after starts a fresh
// download instead of joining a cancelled one. Nothing unverified is ever cached.
func (t *tailReader) Close() error {
	if t.closed {
		return nil
	}
	t.closed = true
	f, c := t.f, t.c
	c.mu.Lock()
	f.readers--
	f.mu.Lock()
	done := f.done
	f.mu.Unlock()
	if f.readers == 0 && !done {
		if c.fills[f.key] == f {
			delete(c.fills, f.key)
		}
		f.cancel()
	}
	c.mu.Unlock()
	return t.file.Close()
}
