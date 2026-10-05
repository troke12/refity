package registry

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"refity/backend/internal/spool"
)

// spoolTempRe matches remote temp names of spool uploads (token = spool job id); direct writes use a
// different token shape.
var spoolTempRe = regexp.MustCompile(`\.uploading-\d{8}T\d{6}-[0-9a-f]{24}$`)

// blockSpoolUploads makes every spool upload fail at the remote temp-file open until the returned
// release func is called. Direct (sync) writes still work.
func blockSpoolUploads(e *testEnv) (release func()) {
	var blocked atomic.Bool
	blocked.Store(true)
	e.srv.FailOpen = func(path string, flags int) error {
		if blocked.Load() && spoolTempRe.MatchString(path) {
			return errors.New("injected: spool uploads blocked")
		}
		return nil
	}
	return func() { blocked.Store(false) }
}

func (e *testEnv) putManifest(repo, ref string, m []byte) int {
	e.t.Helper()
	resp := e.do(http.MethodPut, "/v2/"+repo+"/manifests/"+ref, m, map[string]string{"Content-Type": "application/vnd.oci.image.manifest.v1+json"})
	readAll(e.t, resp)
	return resp.StatusCode
}

func (e *testEnv) getManifest(repo, ref string) []byte {
	e.t.Helper()
	resp := e.do(http.MethodGet, "/v2/"+repo+"/manifests/"+ref, nil, nil)
	b := readAll(e.t, resp)
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("GET manifest %s: %d", ref, resp.StatusCode)
	}
	return b
}

// A tag must never regress to an older version: not when the spool is over budget for the newer push,
// and not when the instance is switched to sync mode while older jobs are still queued.
func TestTagNeverRegressesBehindSpool(t *testing.T) {
	v1 := []byte(`{"schemaVersion":2,"layers":[],"annotations":{"v":"1"}}`)
	v2 := []byte(`{"schemaVersion":2,"layers":[],"annotations":{"v":"2"}}`)
	cases := []struct {
		name string
		// between runs after v1 is spooled and before v2 is pushed.
		between func(t *testing.T, e *testEnv)
	}{
		{
			name: "spool over budget",
			between: func(t *testing.T, e *testEnv) {
				// Fill the spool to just under its budget so the next push would not fit.
				blob, digest := randomBlob(t, 1860) // + two ~57-byte v1 copies = 1974: fits, but one more manifest does not
				if _, code := e.pushBlob("grp/app", blob, digest); code != http.StatusCreated {
					t.Fatalf("blob commit: %d", code)
				}
			},
		},
		{
			name: "switched to sync mode",
			between: func(t *testing.T, e *testEnv) {
				e.cfg.SFTPSyncUpload = true
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			old := syncRetryBackoff
			syncRetryBackoff = 10 * time.Millisecond
			t.Cleanup(func() { syncRetryBackoff = old })
			e := newEnv(t, envOpts{spoolMax: 2000})
			release := blockSpoolUploads(e)
			if code := e.putManifest("grp/app", "latest", v1); code != http.StatusCreated {
				t.Fatalf("PUT v1: %d", code)
			}
			tc.between(t, e)
			if code := e.putManifest("grp/app", "latest", v2); code != http.StatusCreated {
				t.Fatalf("PUT v2: %d", code)
			}
			if got := e.getManifest("grp/app", "latest"); !bytes.Equal(got, v2) {
				t.Fatalf("GET right after pushing v2 returned %s", got)
			}
			release()
			e.waitDrained(15 * time.Second)
			if got := e.getManifest("grp/app", "latest"); !bytes.Equal(got, v2) {
				t.Fatalf("GET after drain returned %s", got)
			}
			got, err := os.ReadFile(e.srv.File("registry/grp/app/manifests/latest"))
			if err != nil || !bytes.Equal(got, v2) {
				t.Fatalf("remote tag ended on %s (err %v), want v2", got, err)
			}
		})
	}
}

func TestInitStorageRejectsUnsafeDirs(t *testing.T) {
	e := newEnv(t, envOpts{})
	base := t.TempDir()
	data := filepath.Join(base, "data")
	cases := []struct {
		name               string
		spool, cache, data string
		ok                 bool
	}{
		{"default layout", filepath.Join(data, "spool"), filepath.Join(data, "cache"), data, true},
		{"spool is the data dir", data, filepath.Join(base, "cache"), data, false},
		{"spool contains the data dir", base, filepath.Join(data, "cache"), data, false},
		{"cache is the data dir", filepath.Join(data, "spool"), data, data, false},
		{"spool equals cache", filepath.Join(base, "x"), filepath.Join(base, "x"), data, false},
		{"cache nested in spool", filepath.Join(base, "s"), filepath.Join(base, "s", "c"), data, false},
		{"spool nested in cache", filepath.Join(base, "c", "s"), filepath.Join(base, "c"), data, false},
	}
	for _, c := range cases {
		cfg := *e.cfg
		cfg.SpoolDir, cfg.ReadCacheDir, cfg.DataDir, cfg.ReadCacheBytes = c.spool, c.cache, c.data, 1<<20
		err := initStorage(&cfg)
		if (err == nil) != c.ok {
			t.Errorf("%s: initStorage err = %v, want ok=%v", c.name, err, c.ok)
		}
		Shutdown()
	}

	// A spool dir that merely contains foreign files (data dir elsewhere) is accepted, and nothing in it
	// is touched.
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "refity.db"), []byte("db"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "other.bin"), []byte("x"), 0o644)
	cfg := *e.cfg
	cfg.SpoolDir, cfg.DataDir = dir, data
	if err := initStorage(&cfg); err != nil {
		t.Fatal(err)
	}
	Shutdown()
	for _, n := range []string{"refity.db", "other.bin"} {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Errorf("%s was removed from the spool dir", n)
		}
	}
}

// Downloads cannot take every pooled connection: a HEAD still answers while the pool is busy streaming.
func TestDownloadsLeaveConnectionForMetadata(t *testing.T) {
	e := newEnv(t, envOpts{poolSize: 2, bytesPerSec: 1 << 20})
	a, da := randomBlob(t, 2<<20)
	b, db := randomBlob(t, 2<<20)
	c, dc := randomBlob(t, 100)
	placeRemote(t, e, "grp/app", a, da)
	placeRemote(t, e, "grp/app", b, db)
	placeRemote(t, e, "grp/app", c, dc)
	done := make(chan []byte, 2)
	for _, d := range []string{da, db} {
		go func(d string) {
			resp, err := http.Get(e.url + "/v2/grp/app/blobs/" + d)
			if err != nil {
				done <- nil
				return
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			done <- body
		}(d)
	}
	time.Sleep(300 * time.Millisecond) // both downloads have started
	start := time.Now()
	resp := e.do(http.MethodHead, "/v2/grp/app/blobs/"+dc, nil, nil)
	readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD: %d", resp.StatusCode)
	}
	if el := time.Since(start); el > 800*time.Millisecond {
		t.Fatalf("HEAD took %v while downloads were running; a connection must stay free", el)
	}
	for i := 0; i < 2; i++ {
		if got := <-done; len(got) != 2<<20 {
			t.Fatalf("download %d returned %d bytes", i, len(got))
		}
	}
}

// A client that stops reading is cut off after STREAM_WRITE_TIMEOUT and its connection is released.
func TestStuckClientReleasesConnection(t *testing.T) {
	e := newEnv(t, envOpts{poolSize: 2, writeTimeout: 300 * time.Millisecond, smallSendBuffer: true, acquire: 2 * time.Second})
	big, dbig := randomBlob(t, 8<<20)
	small, dsmall := randomBlob(t, 1000)
	placeRemote(t, e, "grp/app", big, dbig)
	placeRemote(t, e, "grp/app", small, dsmall)
	host := strings.TrimPrefix(e.url, "http://")
	for i := 0; i < 2; i++ {
		conn, err := net.Dial("tcp", host)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_ = conn.(*net.TCPConn).SetReadBuffer(4096)
		if _, err := conn.Write([]byte("GET /v2/grp/app/blobs/" + dbig + " HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(300 * time.Millisecond)
	client := &http.Client{Timeout: 10 * time.Second}
	start := time.Now()
	resp, err := client.Get(e.url + "/v2/grp/app/blobs/" + dsmall)
	if err != nil {
		t.Fatalf("GET while two clients are stuck: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Equal(got, small) {
		t.Fatalf("GET: %d, %d bytes", resp.StatusCode, len(got))
	}
	t.Logf("served after %v", time.Since(start))
}

// Deleting a repository while its push is still spooled must stick: nothing is re-created remotely and
// nothing stays pullable from the spool or the cache.
func TestPurgeRepoWhileSpooled(t *testing.T) {
	e := newEnv(t, envOpts{readCacheBytes: 64 << 20})
	cached, dcached := randomBlob(t, 4096)
	placeRemote(t, e, "grp/app", cached, dcached)
	if got := readAll(t, e.do(http.MethodGet, "/v2/grp/app/blobs/"+dcached, nil, nil)); !bytes.Equal(got, cached) {
		t.Fatal("warm-up GET")
	}
	release := blockSpoolUploads(e)
	blob, digest := randomBlob(t, 8192)
	if _, code := e.pushBlob("grp/app", blob, digest); code != http.StatusCreated {
		t.Fatalf("commit: %d", code)
	}
	if code := e.putManifest("grp/app", "v1", []byte(`{"schemaVersion":2,"layers":[]}`)); code != http.StatusCreated {
		t.Fatalf("manifest: %d", code)
	}

	PurgeRepo("grp/app")
	if err := e.drv.DeleteRepositoryFolder(context.Background(), "grp/app"); err != nil {
		t.Fatal(err)
	}
	release()
	time.Sleep(2500 * time.Millisecond) // past the spool's first retry backoffs

	if _, err := os.Stat(e.srv.File("registry/grp/app")); !os.IsNotExist(err) {
		t.Fatal("remote repository folder was re-created after delete")
	}
	for _, d := range []string{digest, dcached} {
		resp := e.do(http.MethodHead, "/v2/grp/app/blobs/"+d, nil, nil)
		readAll(t, resp)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("HEAD %s after delete: %d", d, resp.StatusCode)
		}
	}
	resp := e.do(http.MethodGet, "/v2/grp/app/manifests/v1", nil, nil)
	readAll(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET manifest after delete: %d", resp.StatusCode)
	}
}

// A storage outage is reported as 503 (retryable), never as 404 (permanent "unknown").
func TestStorageOutageIs503Not404(t *testing.T) {
	e := newEnv(t, envOpts{acquire: 200 * time.Millisecond})
	data, digest := randomBlob(t, 1000)
	placeRemote(t, e, "grp/app", data, digest)
	_ = os.WriteFile(e.srv.File("registry/grp/app/manifests/v1"), []byte(`{"schemaVersion":2}`), 0o644)

	e.srv.SetRefuse(true)
	e.srv.DropAll()
	// Blob HEAD is deliberately 404 during an outage (push-safe, see serveBlob and
	// TestBlobHeadIs404DuringOutage); blob GET and manifest reads stay 503.
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/v2/grp/app/blobs/" + digest},
		{http.MethodGet, "/v2/grp/app/manifests/v1"},
		{http.MethodHead, "/v2/grp/app/manifests/v1"},
	} {
		resp := e.do(c.method, c.path, nil, nil)
		body := readAll(t, resp)
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("%s %s during outage: %d %s", c.method, c.path, resp.StatusCode, body)
		}
		if c.method == http.MethodGet && !strings.Contains(string(body), "UNAVAILABLE") {
			t.Fatalf("%s %s: missing registry error body: %s", c.method, c.path, body)
		}
	}
	e.srv.SetRefuse(false)
	deadline := time.Now().Add(5 * time.Second)
	for e.drv.Pool.Alive() < 1 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	_, missing := randomBlob(t, 10)
	resp := e.do(http.MethodHead, "/v2/grp/app/blobs/"+missing, nil, nil)
	readAll(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing blob with storage up: %d, want 404", resp.StatusCode)
	}
}

// An older tag version waiting on the path lock must not upload after a newer one was spooled. The lock
// is held across both pushes and released only once the spool holds both, so the worker really does pick
// up v1 and then find it superseded: without that the test passes even with the supersede check removed,
// because v1 would simply have finished before v2 was added.
func TestSupersededJobRecheckedUnderPathLock(t *testing.T) {
	e := newEnv(t, envOpts{})
	tagPath := "registry/grp/app/manifests/latest"
	v1 := []byte(`{"schemaVersion":2,"layers":[],"annotations":{"v":"1"}}`)
	v2 := []byte(`{"schemaVersion":2,"layers":[],"annotations":{"v":"2"}}`)
	l := pathLock(tagPath)
	l.Lock()
	if code := e.putManifest("grp/app", "latest", v1); code != http.StatusCreated {
		l.Unlock()
		t.Fatalf("PUT v1: %d", code)
	}
	if code := e.putManifest("grp/app", "latest", v2); code != http.StatusCreated {
		l.Unlock()
		t.Fatalf("PUT v2: %d", code)
	}
	// Both are spooled now and the lock is still held, so no upload can have completed.
	if n := blobSpool().PendingBytes(); n == 0 {
		l.Unlock()
		t.Fatalf("nothing pending while the path lock is held; the test would not exercise superseding")
	}
	l.Unlock()
	e.waitDrained(10 * time.Second)
	e.assertRemoteContent(e.srv.File(tagPath), v2)
	if n := e.srv.Renames(tagPath); n != 1 {
		t.Fatalf("tag uploaded %d times; the superseded v1 must be skipped", n)
	}
}

// Sync-mode manifest writes are atomic: a failing write never truncates or removes the old version.
func TestSyncManifestWriteIsAtomic(t *testing.T) {
	old := syncRetryBackoff
	syncRetryBackoff = 10 * time.Millisecond
	t.Cleanup(func() { syncRetryBackoff = old })
	e := newEnv(t, envOpts{sync: true})
	v1 := []byte(`{"schemaVersion":2,"layers":[],"annotations":{"v":"1"}}`)
	if code := e.putManifest("grp/app", "latest", v1); code != http.StatusCreated {
		t.Fatalf("PUT v1: %d", code)
	}
	e.srv.FailWriteAt = func(path string, off int64) error {
		if strings.Contains(path, "manifests/latest") {
			return errors.New("injected write failure")
		}
		return nil
	}
	if code := e.putManifest("grp/app", "latest", []byte(`{"schemaVersion":2,"layers":[],"annotations":{"v":"2"}}`)); code != http.StatusInternalServerError {
		t.Fatalf("PUT v2 with failing storage: %d, want 500", code)
	}
	e.assertRemoteContent(e.srv.File("registry/grp/app/manifests/latest"), v1)
	e.assertNoRemoteTemps()
}

// Below SPOOL_MIN_FREE_BYTES of free space the commit uploads synchronously instead of spooling.
func TestFreeSpaceFloorFallsBackToSync(t *testing.T) {
	oldFree := diskFree
	diskFree = func(string) int64 { return 1000 }
	t.Cleanup(func() { diskFree = oldFree })
	e := newEnv(t, envOpts{minFree: 2000})
	data, digest := randomBlob(t, 4096)
	if _, code := e.pushBlob("grp/app", data, digest); code != http.StatusCreated {
		t.Fatalf("commit: %d", code)
	}
	e.assertRemoteContent(e.remoteBlob("grp/app", digest), data) // already uploaded: it was synchronous
	if blobSpool().PendingBytes() != 0 {
		t.Fatal("blob must not be spooled below the free-space floor")
	}
}

func TestSpoolHealthReporting(t *testing.T) {
	e := newEnv(t, envOpts{})
	blockSpoolUploads(e)
	data, digest := randomBlob(t, 500)
	if _, code := e.pushBlob("grp/app", data, digest); code != http.StatusCreated {
		t.Fatalf("commit: %d", code)
	}
	h, ok := SpoolStats()
	if !ok || h.Pending != 1 || h.PendingBytes != 500 {
		t.Fatalf("SpoolStats = %+v, ok=%v", h, ok)
	}
	if msg := spoolHealthWarning(spool.Stats{Pending: 1, OldestAge: time.Minute}); msg != "" {
		t.Fatalf("young backlog should not warn: %q", msg)
	}
	msg := spoolHealthWarning(spool.Stats{Pending: 3, PendingBytes: 10, OldestAge: 11 * time.Minute, LastError: "boom", LastErrorAt: time.Now()})
	if !strings.Contains(msg, "WARN") || !strings.Contains(msg, "boom") {
		t.Fatalf("stuck backlog warning: %q", msg)
	}
}

func TestIsDigestPathIsStrict(t *testing.T) {
	valid := "sha256:" + strings.Repeat("a", 64)
	for p, want := range map[string]bool{
		"registry/g/r/blobs/" + valid:                              true,
		"registry/g/r/manifests/" + valid:                          true,
		"registry/g/r/manifests/sha256:short":                      false,
		"registry/g/r/manifests/sha256:" + strings.Repeat("A", 64): false,
		"registry/g/r/manifests/latest":                            false,
		"registry/g/r/manifests/" + valid + ".uploading-x":         false,
	} {
		if got := isDigestPath(p); got != want {
			t.Errorf("isDigestPath(%q) = %v, want %v", p, got, want)
		}
	}
}

// The harder supersede case: a worker has already picked up the older version and is inside its upload
// when the newer one is spooled. The in-flight upload must then be abandoned, not committed, so the tag
// ends up holding the newer version only. Holding the path lock gets the worker as far as "picked up and
// waiting"; the write delay gives it time to actually start.
func TestSupersededInFlightUploadIsAbandoned(t *testing.T) {
	e := newEnv(t, envOpts{bytesPerSec: 16 << 10})
	tagPath := "registry/grp/app/manifests/latest"
	v1 := manifestWithPadding("1", 64<<10)
	v2 := manifestWithPadding("2", 70<<10)

	// Start v1 on a slow link and let a worker pick it up.
	if code := e.putManifest("grp/app", "latest", v1); code != http.StatusCreated {
		t.Fatalf("PUT v1: %d", code)
	}
	waitForCond(t, 10*time.Second, "the v1 upload to start", func() bool {
		return e.srv.Writes() > 0
	})
	// While it is in flight, spool the newer version of the same tag.
	if code := e.putManifest("grp/app", "latest", v2); code != http.StatusCreated {
		t.Fatalf("PUT v2: %d", code)
	}
	e.waitDrained(30 * time.Second)
	// Whatever the interleaving, the tag must end on the newer version.
	e.assertRemoteContent(e.srv.File(tagPath), v2)
}

// waitForCond polls cond until it holds or the timeout expires, naming what it was waiting for.
func waitForCond(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", d, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The strongest form of the supersede guarantee: the older version must not be committed to the remote
// after the newer one was spooled, even though its upload had already started. Asserting the final
// content is not enough on its own — when the newer upload simply overwrites the older one the outcome
// looks the same — so this also asserts the tag was only ever renamed into place once.
func TestSupersededUploadNeverReachesTheRemote(t *testing.T) {
	e := newEnv(t, envOpts{bytesPerSec: 8 << 10})
	tagPath := "registry/grp/app/manifests/latest"
	v1 := manifestWithPadding("1", 96<<10)
	v2 := manifestWithPadding("2", 100<<10)

	if code := e.putManifest("grp/app", "latest", v1); code != http.StatusCreated {
		t.Fatalf("PUT v1: %d", code)
	}
	waitForCond(t, 15*time.Second, "the v1 upload to reach the remote", func() bool {
		return e.srv.Writes() > 0
	})
	if code := e.putManifest("grp/app", "latest", v2); code != http.StatusCreated {
		t.Fatalf("PUT v2: %d", code)
	}
	e.waitDrained(40 * time.Second)

	// Once v2 was spooled, the older version must not have completed: its temp file would have been
	// renamed into place, and the newer upload is a different temp name.
	e.assertRemoteContent(e.srv.File(tagPath), v2)
	if n := e.srv.Renames(tagPath); n > 1 {
		t.Fatalf("the tag was renamed into place %d times; a superseded version was committed", n)
	}
}
