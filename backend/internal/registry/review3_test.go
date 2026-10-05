package registry

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"refity/backend/internal/readcache"
)

// The whole storage layer comes up and stays useful while every SFTP login is rejected: local content
// is served, async pushes are accepted, the rest is 503, login attempts follow the probe schedule, and
// everything catches up once logins work again.
func TestServesLocallyWhileLoginsRejected(t *testing.T) {
	// A blob cached by an earlier run.
	cacheDir := t.TempDir()
	cachedBlob, cachedDigest := randomBlob(t, 3000)
	rc, err := readcache.New(cacheDir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	rc.Group = repoOf
	src := filepath.Join(t.TempDir(), "c")
	_ = os.WriteFile(src, cachedBlob, 0o644)
	rc.Adopt("registry/grp/app/blobs/"+cachedDigest, src, int64(len(cachedBlob)))
	rc.Close()

	const pause = 300 * time.Millisecond
	e := newEnv(t, envOpts{authFailAtStart: true, authPause: pause, acquire: 5 * time.Second, readCacheBytes: 1 << 20, cacheDir: cacheDir})
	remoteOnly, remoteDigest := randomBlob(t, 2000)
	placeRemote(t, e, "grp/app", remoteOnly, remoteDigest)

	// Async push accepted into the spool; pushed blob and tag are served from it.
	pushed, pushedDigest := randomBlob(t, 4096)
	start := time.Now()
	if _, code := e.pushBlob("grp/app", pushed, pushedDigest); code != http.StatusCreated {
		t.Fatalf("push during outage: %d", code)
	}
	tag := []byte(`{"schemaVersion":2,"layers":[]}`)
	if code := e.putManifest("grp/app", "v1", tag); code != http.StatusCreated {
		t.Fatalf("tag push during outage: %d", code)
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("push took %v during the outage; must not wait on SFTP", el)
	}
	for d, want := range map[string][]byte{pushedDigest: pushed, cachedDigest: cachedBlob} {
		if got := readAll(t, e.do(http.MethodGet, "/v2/grp/app/blobs/"+d, nil, nil)); !bytes.Equal(got, want) {
			t.Fatalf("local blob %s not served during outage", d)
		}
	}
	if got := e.getManifest("grp/app", "v1"); !bytes.Equal(got, tag) {
		t.Fatal("spooled tag not served during outage")
	}
	if body := string(readAll(t, e.do(http.MethodGet, "/v2/grp/app/tags/list", nil, nil))); !strings.Contains(body, `"v1"`) {
		t.Fatalf("tag list during outage should include the spooled tag: %s", body)
	}
	// Remote-only content: 503 quickly, never 404 or a hang.
	start = time.Now()
	resp := e.do(http.MethodGet, "/v2/grp/app/blobs/"+remoteDigest, nil, nil)
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "UNAVAILABLE") {
		t.Fatalf("remote-only blob during outage: %d %s", resp.StatusCode, body)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("503 took %v", el)
	}

	time.Sleep(4 * pause)
	if n := e.srv.DialAttempts(); n > 4 {
		t.Fatalf("%d login attempts during the outage; want at most one per probe window", n)
	}
	if blobSpool().PendingBytes() == 0 {
		t.Fatal("nothing can have drained without a login")
	}

	e.srv.SetAuthFail(false)
	deadline := time.Now().Add(10 * time.Second)
	for e.drv.Pool.Alive() < 4 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if a := e.drv.Pool.Alive(); a != 4 {
		t.Fatalf("pool did not fill after logins work: %d", a)
	}
	e.waitDrained(15 * time.Second)
	e.assertRemoteContent(e.remoteBlob("grp/app", pushedDigest), pushed)
	e.assertRemoteContent(e.srv.File("registry/grp/app/manifests/v1"), tag)
	if got := readAll(t, e.do(http.MethodGet, "/v2/grp/app/blobs/"+remoteDigest, nil, nil)); !bytes.Equal(got, remoteOnly) {
		t.Fatal("previously unavailable blob not served after recovery")
	}
}

// While the remote is slow to answer, repeated GETs of a manifest needing an OCI digest copy start one
// background copy per path, not one per GET.
func TestOCICopyCoalescedPerPath(t *testing.T) {
	e := newEnv(t, envOpts{acquire: 5 * time.Second})
	blockSpoolUploads(e)
	m := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","layers":[]}`)
	resp := e.do(http.MethodPut, "/v2/grp/app/manifests/v1", m, map[string]string{"Content-Type": "application/vnd.docker.distribution.manifest.v2+json"})
	readAll(t, resp)
	// Alive but slow: each copy's remote existence check takes about a second, so copies overlap.
	e.srv.SetWriteDelay(time.Second)
	t.Cleanup(func() { e.srv.SetWriteDelay(0) })
	before := ociCopyRuns.Load()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := e.do(http.MethodGet, "/v2/grp/app/manifests/v1", nil, nil)
			readAll(t, resp)
		}()
	}
	wg.Wait()
	if n := ociCopyRuns.Load() - before; n != 1 {
		t.Fatalf("%d background OCI copies started for one path, want 1", n)
	}
}
