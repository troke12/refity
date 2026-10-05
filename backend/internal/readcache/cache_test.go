package readcache_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	godigest "github.com/opencontainers/go-digest"
	"refity/backend/internal/readcache"
)

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// slowSource serves data in small steps so fills are observably in progress.
type slowSource struct {
	data  []byte
	opens atomic.Int32
	step  time.Duration
	// failAt makes the first open fail with an error after this many bytes (0 = never).
	failAt int64
}

func (s *slowSource) open(ctx context.Context, off int64) (io.ReadCloser, error) {
	n := s.opens.Add(1)
	r := &slowReader{data: s.data, pos: off, step: s.step}
	if n == 1 && s.failAt > 0 {
		r.failAt = s.failAt
	}
	return r, nil
}

type slowReader struct {
	data   []byte
	pos    int64
	step   time.Duration
	failAt int64
}

func (r *slowReader) Read(p []byte) (int, error) {
	if r.pos >= int64(len(r.data)) {
		return 0, io.EOF
	}
	if r.failAt > 0 && r.pos >= r.failAt {
		return 0, errors.New("injected connection loss")
	}
	time.Sleep(r.step)
	if len(p) > 16<<10 {
		p = p[:16<<10]
	}
	end := r.pos + int64(len(p))
	if r.failAt > 0 && end > r.failAt {
		end = r.failAt
	}
	n := copy(p, r.data[r.pos:min(end, int64(len(r.data)))])
	r.pos += int64(n)
	return n, nil
}

func (r *slowReader) Close() error { return nil }

func newCache(t *testing.T, max int64) *readcache.Cache {
	t.Helper()
	c, err := readcache.New(t.TempDir(), max)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func TestFetchCoalescesVerifiesAndPromotes(t *testing.T) {
	data := randomBytes(t, 256<<10)
	digest := godigest.FromBytes(data).String()
	src := &slowSource{data: data, step: time.Millisecond}
	c := newCache(t, 10<<20)

	var wg sync.WaitGroup
	results := make([][]byte, 4)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rc, err := c.Fetch("k", int64(len(data)), digest, src.open)
			if err != nil {
				t.Error(err)
				return
			}
			defer rc.Close()
			results[i], _ = io.ReadAll(rc)
		}(i)
	}
	wg.Wait()
	for i, r := range results {
		if !bytes.Equal(r, data) {
			t.Fatalf("reader %d got %d bytes, want identical content", i, len(r))
		}
	}
	if n := src.opens.Load(); n != 1 {
		t.Fatalf("concurrent cold reads should share one remote read, got %d opens", n)
	}
	if _, ok := c.Has("k"); !ok {
		t.Fatal("verified fill should be promoted")
	}
	// Warm read: no new remote open.
	rc, err := c.Fetch("k", int64(len(data)), digest, src.open)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, data) || src.opens.Load() != 1 {
		t.Fatal("warm read should come from disk")
	}
}

func TestFetchResumesAfterInterruption(t *testing.T) {
	data := randomBytes(t, 200<<10)
	src := &slowSource{data: data, failAt: 70 << 10}
	c := newCache(t, 10<<20)
	rc, err := c.Fetch("k", int64(len(data)), godigest.FromBytes(data).String(), src.open)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("resumed fill mismatch: err=%v len=%d", err, len(got))
	}
	if src.opens.Load() != 2 {
		t.Fatalf("expected one resume (2 opens), got %d", src.opens.Load())
	}
}

func TestCorruptFillIsNotPromotedOrDeliveredComplete(t *testing.T) {
	data := randomBytes(t, 64<<10)
	wrong := godigest.FromBytes([]byte("something else")).String()
	c := newCache(t, 10<<20)
	rc, err := c.Fetch("k", int64(len(data)), wrong, (&slowSource{data: data}).open)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err == nil {
		t.Fatal("reader must fail on digest mismatch")
	}
	if len(got) >= len(data) {
		t.Fatalf("a corrupt object must not be delivered complete (got %d of %d bytes)", len(got), len(data))
	}
	if _, ok := c.Has("k"); ok {
		t.Fatal("corrupt fill must not be cached")
	}
}

func TestAbandonedFillIsDiscarded(t *testing.T) {
	data := randomBytes(t, 512<<10)
	dir := t.TempDir()
	c, err := readcache.New(dir, 10<<20)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := c.Fetch("k", int64(len(data)), godigest.FromBytes(data).String(), (&slowSource{data: data, step: 2 * time.Millisecond}).open)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1000)
	if _, err := io.ReadFull(rc, buf); err != nil {
		t.Fatal(err)
	}
	rc.Close() // client aborts
	c.Close()  // waits for the fill goroutine
	if _, ok := c.Has("k"); ok {
		t.Fatal("abandoned fill must not be cached")
	}
	var left []string
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			left = append(left, p)
		}
		return nil
	})
	if len(left) != 0 {
		t.Fatalf("partial files left: %v", left)
	}
}

// Eviction must respect the budget including space reserved by fills in progress. Checking only the
// promoted size would let the cache evict entries while a download is in flight, so the download's
// reservation has to count toward the ceiling it evicts against.
func TestEvictionRespectsBudget(t *testing.T) {
	dir := t.TempDir()
	c, err := readcache.New(dir, 1000)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	adopt := func(key string, n int) {
		p := filepath.Join(t.TempDir(), key)
		if err := os.WriteFile(p, make([]byte, n), 0o644); err != nil {
			t.Fatal(err)
		}
		c.Adopt(key, p, int64(n))
	}
	adopt("a", 400)
	adopt("b", 400)
	if f, _, ok := c.Open("a"); ok { // touch a so b becomes least recently used
		f.Close()
	}
	adopt("c", 400)
	if c.Size() > 1000 {
		t.Fatalf("cache size %d over budget", c.Size())
	}
	if _, ok := c.Has("b"); ok {
		t.Fatal("least recently used entry should have been evicted")
	}
	for _, k := range []string{"a", "c"} {
		if _, ok := c.Has(k); !ok {
			t.Fatalf("%s should still be cached", k)
		}
	}
	if _, err := c.Fetch("big", 2000, godigest.FromBytes(nil).String(), nil); !errors.Is(err, readcache.ErrTooLarge) {
		t.Fatalf("expected ErrTooLarge, got %v", err)
	}

	// A fill that reserves most of the budget must make the next Adopt evict, not sit alongside it.
	data := randomBytes(t, 700)
	rc, err := c.Fetch("inflight", int64(len(data)), godigest.FromBytes(data).String(), (&slowSource{data: data, step: 2 * time.Millisecond}).open)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	adopt("d", 400)
	if u := diskUsage(t, dir); u > 1000 {
		t.Fatalf("disk usage %d over the 1000 budget while a fill is in flight", u)
	}
	if c.Size() > 1000 {
		t.Fatalf("accounted size %d over budget while a fill is in flight", c.Size())
	}
}

func diskUsage(t *testing.T, dir string) int64 {
	t.Helper()
	var n int64
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			n += info.Size()
		}
		return nil
	})
	return n
}

// A READ_CACHE_DIR pointed at the wrong place must not index, evict or delete foreign files.
func TestNewIgnoresForeignFiles(t *testing.T) {
	dir := t.TempDir()
	big := make([]byte, 4096)
	for _, name := range []string{"refity.db", "notes.txt", "deadbeef"} {
		if err := os.WriteFile(filepath.Join(dir, name), big, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sub := filepath.Join(dir, "photos")
	_ = os.MkdirAll(sub, 0o755)
	_ = os.WriteFile(filepath.Join(sub, "a.jpg"), big, 0o644)
	// Even inside a directory whose name looks like one of ours, only hash-named files are indexed.
	hexDir := filepath.Join(dir, strings.Repeat("ab", 32))
	_ = os.MkdirAll(hexDir, 0o755)
	_ = os.WriteFile(filepath.Join(hexDir, "readme.txt"), big, 0o644)
	c, err := readcache.New(dir, 100) // far below the foreign bytes
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Size() != 0 {
		t.Fatalf("foreign files were indexed: size %d", c.Size())
	}
	c.Adopt("k", writeTemp(t, 50), 50) // triggers eviction logic
	for _, p := range []string{"refity.db", "notes.txt", "deadbeef", "photos/a.jpg", strings.Repeat("ab", 32) + "/readme.txt"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Errorf("foreign file %s was removed", p)
		}
	}
}

func writeTemp(t *testing.T, n int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, make([]byte, n), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// The budget holds while a fill is downloading, not only after promotion.
func TestFillReservesBudget(t *testing.T) {
	dir := t.TempDir()
	c, err := readcache.New(dir, 100<<10)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Adopt("a", writeTemp(t, 45<<10), 45<<10)
	c.Adopt("b", writeTemp(t, 45<<10), 45<<10)
	data := randomBytes(t, 60<<10)
	src := &slowSource{data: data, step: 5 * time.Millisecond}
	rc, err := c.Fetch("big", int64(len(data)), godigest.FromBytes(data).String(), src.open)
	if err != nil {
		t.Fatal(err)
	}
	half := make([]byte, 30<<10)
	if _, err := io.ReadFull(rc, half); err != nil {
		t.Fatal(err)
	}
	if u := diskUsage(t, dir); u > 100<<10 {
		t.Fatalf("disk usage %d exceeds the %d budget mid-fill", u, 100<<10)
	}
	if c.Size() > 100<<10 {
		t.Fatalf("accounted size %d exceeds budget mid-fill", c.Size())
	}
	_, _ = io.ReadAll(rc)
	rc.Close()
	// A second fill that cannot be reserved is refused (streamed uncached by the caller).
	other := randomBytes(t, 60<<10)
	slow := &slowSource{data: data, step: 5 * time.Millisecond}
	r1, err := c.Fetch("big2", int64(len(data)), godigest.FromBytes(data).String(), slow.open)
	if err != nil {
		t.Fatal(err)
	}
	defer r1.Close()
	if _, err := c.Fetch("other", int64(len(other)), godigest.FromBytes(other).String(), (&slowSource{data: other}).open); !errors.Is(err, readcache.ErrNoRoom) {
		t.Fatalf("expected ErrNoRoom while the budget is reserved, got %v", err)
	}
}

// A reader arriving right after the last one abandoned a fill must get a fresh, complete download,
// never the cancelled stream.
func TestAbandonThenRefetchGetsFreshDownload(t *testing.T) {
	data := randomBytes(t, 256<<10)
	digest := godigest.FromBytes(data).String()
	c := newCache(t, 10<<20)
	for i := 0; i < 30; i++ {
		key := fmt.Sprintf("k%d", i)
		src := &slowSource{data: data, step: 2 * time.Millisecond}
		rc, err := c.Fetch(key, int64(len(data)), digest, src.open)
		if err != nil {
			t.Fatal(err)
		}
		one := make([]byte, 1)
		if _, err := io.ReadFull(rc, one); err != nil {
			t.Fatal(err)
		}
		rc.Close()
		rc, err = c.Fetch(key, int64(len(data)), digest, src.open)
		if err != nil {
			t.Fatalf("iteration %d: refetch: %v", i, err)
		}
		got, err := io.ReadAll(rc)
		rc.Close()
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("iteration %d: refetch after abandon got %d bytes, err %v", i, len(got), err)
		}
	}
}

func TestDropGroup(t *testing.T) {
	dir := t.TempDir()
	group := func(key string) string { return strings.SplitN(key, "/", 2)[0] }
	c, err := readcache.New(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	c.Group = group
	c.Adopt("repo1/a", writeTemp(t, 10), 10)
	c.Adopt("repo2/b", writeTemp(t, 10), 10)
	c.Close()
	// After a restart only hashed names are on disk; the group must still be droppable.
	c, err = readcache.New(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Group = group
	c.DropGroup("repo1")
	if _, ok := c.Has("repo1/a"); ok {
		t.Fatal("dropped group still cached")
	}
	if _, ok := c.Has("repo2/b"); !ok {
		t.Fatal("other group was dropped too")
	}
}
