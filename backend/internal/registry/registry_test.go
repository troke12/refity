package registry

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	godigest "github.com/opencontainers/go-digest"
	"refity/backend/internal/config"
	"refity/backend/internal/driver/local"
	"refity/backend/internal/driver/sftp"
	"refity/backend/internal/driver/sftp/sftptest"
)

// These tests drive RegistryHandler over real HTTP against an in-process, throttled SFTP server.
// They share the package-level registry state, so they must not run in parallel.

type testEnv struct {
	t     *testing.T
	srv   *sftptest.Server
	drv   *sftp.PoolStorageDriver
	cfg   *config.Config
	url   string
	spool string
}

type envOpts struct {
	sync           bool
	bytesPerSec    int64
	stall          time.Duration
	parallel       int64
	spoolMax       int64
	readCacheBytes int64
	poolSize       int           // default 4
	acquire        time.Duration // pool acquire timeout (default: driver default)
	writeTimeout   time.Duration // STREAM_WRITE_TIMEOUT
	minFree        int64         // SPOOL_MIN_FREE_BYTES
	// smallSendBuffer shrinks the server's TCP send buffer so a client that stops reading blocks writes.
	smallSendBuffer bool
	authFailAtStart bool          // the SFTP server rejects every login from the start
	authPause       time.Duration // pool AuthPause (AuthPauseMax = 2x)
	cacheDir        string        // pre-populated READ_CACHE_DIR (default: fresh temp dir)
	// spoolDir reuses an existing spool directory, so a test can simulate a restart that keeps its
	// durable state (the corrupt marks).
	spoolDir string
}

func newEnv(t *testing.T, o envOpts) *testEnv {
	t.Helper()
	return newEnvWithRemoteRoot(t, o, "")
}

// newEnvWithRemoteRoot is newEnv with control over where the fake Storage Box lives. Passing the root
// of a previous env models a restart against the same box (only the process is new); passing "" gives
// each env a box of its own.
func newEnvWithRemoteRoot(t *testing.T, o envOpts, remoteRoot string) *testEnv {
	t.Helper()
	// Start from an empty in-memory set: the only marks that survive into this env are the ones
	// initStorage reloads from disk, which is exactly what a real restart does.
	corrupt.Range(func(k, _ any) bool { corrupt.Delete(k); return true })
	healing.Range(func(k, _ any) bool { healing.Delete(k); return true })
	srv := sftptest.New(t)
	if remoteRoot != "" {
		srv.Root = remoteRoot
	}
	srv.BytesPerSec = o.bytesPerSec
	if o.authFailAtStart {
		srv.SetAuthFail(true)
	}
	size := o.poolSize
	if size == 0 {
		size = 4
	}
	drv := srv.PoolWith(t, sftp.PoolOptions{
		Size: size, StallTimeout: o.stall, ParallelThreshold: o.parallel, AcquireTimeout: o.acquire,
		RefillBackoffBase: 10 * time.Millisecond, RefillBackoffMax: 200 * time.Millisecond,
		AuthPause: o.authPause, AuthPauseMax: 2 * o.authPause,
	})
	cacheDir := o.cacheDir
	if cacheDir == "" {
		cacheDir = t.TempDir()
	}
	spoolDir := o.spoolDir
	if spoolDir == "" {
		spoolDir = t.TempDir()
	}
	e := &testEnv{t: t, srv: srv, drv: drv, spool: spoolDir}
	e.cfg = &config.Config{
		JWTSecret:          "test-secret",
		SFTPSyncUpload:     o.sync,
		SpoolDir:           e.spool,
		SpoolMaxBytes:      o.spoolMax,
		UploadWorkers:      4,
		ReadCacheDir:       cacheDir,
		ReadCacheBytes:     o.readCacheBytes,
		StreamWriteTimeout: o.writeTimeout,
		SpoolMinFreeBytes:  o.minFree,
	}
	NewRouterWithDeps(local.NewDriver(t.TempDir()), drv, e.cfg, nil, nil)
	ts := httptest.NewUnstartedServer(http.HandlerFunc(RegistryHandler))
	if o.smallSendBuffer {
		ts.Config.ConnState = func(c net.Conn, s http.ConnState) {
			if tc, ok := c.(*net.TCPConn); ok && s == http.StateNew {
				_ = tc.SetWriteBuffer(4096)
			}
		}
	}
	ts.Start()
	t.Cleanup(ts.Close)
	t.Cleanup(Shutdown) // runs before the pool and temp dirs are torn down
	e.url = ts.URL
	return e
}

func randomBlob(t *testing.T, n int) ([]byte, string) {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b, godigest.FromBytes(b).String()
}

func (e *testEnv) do(method, path string, body []byte, hdr map[string]string) *http.Response {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	u := path
	if !strings.HasPrefix(path, "http") {
		u = e.url + path
	}
	req, err := http.NewRequest(method, u, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func readAll(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	return b
}

// pushBlob does POST + PATCH + PUT like the docker client and returns the commit duration.
func (e *testEnv) pushBlob(repo string, data []byte, digest string) (time.Duration, int) {
	e.t.Helper()
	resp := e.do(http.MethodPost, "/v2/"+repo+"/blobs/uploads/", nil, nil)
	readAll(e.t, resp)
	if resp.StatusCode != http.StatusAccepted {
		e.t.Fatalf("POST: %d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	resp = e.do(http.MethodPatch, loc, data, map[string]string{"Content-Type": "application/octet-stream"})
	readAll(e.t, resp)
	if resp.StatusCode != http.StatusAccepted {
		e.t.Fatalf("PATCH: %d", resp.StatusCode)
	}
	loc = resp.Header.Get("Location")
	start := time.Now()
	resp = e.do(http.MethodPut, loc+"&digest="+digest, nil, nil)
	readAll(e.t, resp)
	return time.Since(start), resp.StatusCode
}

func (e *testEnv) remoteBlob(repo, digest string) string {
	return e.srv.File("registry/" + repo + "/blobs/" + digest)
}

func (e *testEnv) waitDrained(timeout time.Duration) {
	e.t.Helper()
	deadline := time.Now().Add(timeout)
	for blobSpool().PendingBytes() != 0 {
		if time.Now().After(deadline) {
			e.t.Fatalf("spool not drained after %v (%d bytes pending)", timeout, blobSpool().PendingBytes())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (e *testEnv) assertNoRemoteTemps() {
	e.t.Helper()
	_ = filepath.Walk(e.srv.Root, func(p string, info os.FileInfo, err error) error {
		if err == nil && strings.Contains(info.Name(), ".uploading") {
			e.t.Errorf("leftover remote temp file %s", p)
		}
		return nil
	})
}

func (e *testEnv) assertRemoteContent(path string, want []byte) {
	e.t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		e.t.Fatalf("remote %s: %v", path, err)
	}
	if !bytes.Equal(got, want) {
		e.t.Fatalf("remote %s: content differs (%d vs %d bytes)", path, len(got), len(want))
	}
}

func manifestFor(cfgDigest string, cfgSize int, layerDigest string, layerSize int) []byte {
	m := map[string]interface{}{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config":        map[string]interface{}{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": cfgDigest, "size": cfgSize},
		"layers":        []interface{}{map[string]interface{}{"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip", "digest": layerDigest, "size": layerSize}},
	}
	b, _ := json.Marshal(m)
	return b
}

// Requirements 1+2: async push returns long before the throttled upload could finish, the image is
// pullable immediately from the spool, and the background upload lands correctly afterwards.
func TestAsyncPushThenImmediatePull(t *testing.T) {
	e := newEnv(t, envOpts{bytesPerSec: 512 << 10, parallel: 1 << 20, readCacheBytes: 64 << 20})
	repo := "grp/app"
	layer, layerDigest := randomBlob(t, 4<<20) // >= 2 s remotely even with 4 parallel connections
	cfgBlob, cfgDigest := randomBlob(t, 2048)

	elapsed, code := e.pushBlob(repo, layer, layerDigest)
	if code != http.StatusCreated {
		t.Fatalf("commit: %d", code)
	}
	if elapsed > time.Second {
		t.Fatalf("async commit took %v; it must not wait for the remote upload", elapsed)
	}
	if _, code := e.pushBlob(repo, cfgBlob, cfgDigest); code != http.StatusCreated {
		t.Fatalf("config commit: %d", code)
	}
	manifest := manifestFor(cfgDigest, len(cfgBlob), layerDigest, len(layer))
	manifestDigest := godigest.FromBytes(manifest).String()
	resp := e.do(http.MethodPut, "/v2/"+repo+"/manifests/v1", manifest, map[string]string{"Content-Type": "application/vnd.oci.image.manifest.v1+json"})
	readAll(t, resp)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("manifest PUT: %d", resp.StatusCode)
	}

	if _, err := os.Stat(e.remoteBlob(repo, layerDigest)); !os.IsNotExist(err) {
		t.Fatal("test precondition: layer should still be uploading")
	}

	// Pull while the upload is still running.
	resp = e.do(http.MethodHead, "/v2/"+repo+"/blobs/"+layerDigest, nil, nil)
	readAll(t, resp)
	if resp.StatusCode != http.StatusOK || resp.ContentLength != int64(len(layer)) {
		t.Fatalf("HEAD blob: %d len=%d", resp.StatusCode, resp.ContentLength)
	}
	resp = e.do(http.MethodGet, "/v2/"+repo+"/blobs/"+layerDigest, nil, nil)
	if got := readAll(t, resp); resp.StatusCode != http.StatusOK || !bytes.Equal(got, layer) {
		t.Fatalf("GET blob from spool: %d, %d bytes", resp.StatusCode, len(got))
	}
	for _, ref := range []string{"v1", manifestDigest} {
		for _, method := range []string{http.MethodHead, http.MethodGet} {
			resp = e.do(method, "/v2/"+repo+"/manifests/"+ref, nil, nil)
			body := readAll(t, resp)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("%s manifest %s: %d", method, ref, resp.StatusCode)
			}
			if method == http.MethodGet && !bytes.Equal(body, manifest) {
				t.Fatalf("manifest %s content differs", ref)
			}
		}
	}
	resp = e.do(http.MethodGet, "/v2/"+repo+"/tags/list", nil, nil)
	if body := string(readAll(t, resp)); !strings.Contains(body, `"v1"`) {
		t.Fatalf("tags/list should show the spooled tag: %s", body)
	}
	resp = e.do(http.MethodGet, "/v2/_catalog", nil, nil)
	if body := string(readAll(t, resp)); !strings.Contains(body, `"grp"`) {
		t.Fatalf("catalog should show the spooled repo: %s", body)
	}
	if _, err := os.Stat(e.remoteBlob(repo, layerDigest)); !os.IsNotExist(err) {
		t.Fatal("pull assertions above must have run while the upload was in progress")
	}

	// Requirement 2: upload completes, nothing partial or pending left.
	e.waitDrained(20 * time.Second)
	e.assertRemoteContent(e.remoteBlob(repo, layerDigest), layer)
	e.assertRemoteContent(e.remoteBlob(repo, cfgDigest), cfgBlob)
	e.assertRemoteContent(e.srv.File("registry/"+repo+"/manifests/v1"), manifest)
	e.assertRemoteContent(e.srv.File("registry/"+repo+"/manifests/"+manifestDigest), manifest)
	e.assertNoRemoteTemps()
	if left, _ := filepath.Glob(filepath.Join(e.spool, "*")); len(left) != 0 {
		t.Fatalf("spool not empty: %v", left)
	}
	// The uploaded layer moved into the read cache: pulling it again does not touch the remote.
	before := e.srv.ReadOpens()
	resp = e.do(http.MethodGet, "/v2/"+repo+"/blobs/"+layerDigest, nil, nil)
	if got := readAll(t, resp); !bytes.Equal(got, layer) || e.srv.ReadOpens() != before {
		t.Fatal("post-upload pull should be served from the read cache")
	}
}

func placeRemote(t *testing.T, e *testEnv, repo string, data []byte, digest string) {
	t.Helper()
	p := e.remoteBlob(repo, digest)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Requirement 5: remote GET streams (fast first byte), supports Range/416/HEAD.
func TestStreamingRemoteGet(t *testing.T) {
	for _, cache := range []int64{0, 64 << 20} {
		t.Run(fmt.Sprintf("cache=%d", cache), func(t *testing.T) {
			e := newEnv(t, envOpts{bytesPerSec: 1 << 20, readCacheBytes: cache})
			repo := "grp/app"
			data, digest := randomBlob(t, 2<<20) // ~2 s to transfer at 1 MiB/s
			placeRemote(t, e, repo, data, digest)
			blobURL := "/v2/" + repo + "/blobs/" + digest

			// Ranges first, while nothing is cached, so the remote range path is exercised.
			resp := e.do(http.MethodGet, blobURL, nil, map[string]string{"Range": "bytes=100-199"})
			body := readAll(t, resp)
			if resp.StatusCode != http.StatusPartialContent || resp.Header.Get("Content-Range") != fmt.Sprintf("bytes 100-199/%d", len(data)) || !bytes.Equal(body, data[100:200]) {
				t.Fatalf("range: %d %q %d bytes", resp.StatusCode, resp.Header.Get("Content-Range"), len(body))
			}
			resp = e.do(http.MethodGet, blobURL, nil, map[string]string{"Range": "bytes=-10"})
			if body := readAll(t, resp); resp.StatusCode != http.StatusPartialContent || !bytes.Equal(body, data[len(data)-10:]) {
				t.Fatalf("suffix range: %d", resp.StatusCode)
			}
			resp = e.do(http.MethodGet, blobURL, nil, map[string]string{"Range": fmt.Sprintf("bytes=%d-", len(data)+5)})
			readAll(t, resp)
			if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
				t.Fatalf("unsatisfiable range: %d", resp.StatusCode)
			}
			resp = e.do(http.MethodHead, blobURL, nil, nil)
			readAll(t, resp)
			if resp.StatusCode != http.StatusOK || resp.ContentLength != int64(len(data)) || resp.Header.Get("Docker-Content-Digest") != digest {
				t.Fatalf("HEAD: %d len=%d", resp.StatusCode, resp.ContentLength)
			}

			start := time.Now()
			resp = e.do(http.MethodGet, blobURL, nil, nil)
			if resp.ContentLength != int64(len(data)) {
				t.Fatalf("Content-Length %d", resp.ContentLength)
			}
			first := make([]byte, 1)
			if _, err := io.ReadFull(resp.Body, first); err != nil {
				t.Fatal(err)
			}
			ttfb := time.Since(start)
			rest := readAll(t, resp)
			total := time.Since(start)
			got := append(first, rest...)
			if godigest.FromBytes(got).String() != digest {
				t.Fatal("streamed body digest mismatch")
			}
			t.Logf("ttfb=%v total=%v", ttfb, total)
			if ttfb > total/4 || ttfb > time.Second {
				t.Fatalf("first byte after %v of %v: response is not streamed", ttfb, total)
			}
		})
	}
}

// Requirement 6: cache hits avoid the remote; corrupt remote data is never cached.
func TestReadCache(t *testing.T) {
	e := newEnv(t, envOpts{readCacheBytes: 64 << 20})
	repo := "grp/app"
	data, digest := randomBlob(t, 512<<10)
	placeRemote(t, e, repo, data, digest)
	url := "/v2/" + repo + "/blobs/" + digest
	if got := readAll(t, e.do(http.MethodGet, url, nil, nil)); !bytes.Equal(got, data) {
		t.Fatal("first GET content")
	}
	before := e.srv.ReadOpens()
	if got := readAll(t, e.do(http.MethodGet, url, nil, nil)); !bytes.Equal(got, data) {
		t.Fatal("second GET content")
	}
	if e.srv.ReadOpens() != before {
		t.Fatal("second GET touched the remote")
	}

	// Remote bytes that do not match their digest name.
	bad, _ := randomBlob(t, 256<<10)
	_, badDigest := randomBlob(t, 10)
	placeRemote(t, e, repo, bad, badDigest)
	resp := e.do(http.MethodGet, "/v2/"+repo+"/blobs/"+badDigest, nil, nil)
	got, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err == nil && len(got) == len(bad) {
		t.Fatal("corrupt blob delivered as a complete body")
	}
	if _, ok := blobCache().Has("registry/" + repo + "/blobs/" + badDigest); ok {
		t.Fatal("corrupt blob was cached")
	}
}

// Requirement 7: over the spool budget a commit uploads synchronously and still succeeds.
func TestSpoolBackpressureFallsBackToSync(t *testing.T) {
	e := newEnv(t, envOpts{spoolMax: 1000})
	data, digest := randomBlob(t, 64<<10)
	if _, code := e.pushBlob("grp/app", data, digest); code != http.StatusCreated {
		t.Fatalf("commit: %d", code)
	}
	e.assertRemoteContent(e.remoteBlob("grp/app", digest), data) // already there: it was synchronous
	if blobSpool().PendingBytes() != 0 {
		t.Fatal("over-budget blob must not be spooled")
	}
	if got := readAll(t, e.do(http.MethodGet, "/v2/grp/app/blobs/"+digest, nil, nil)); !bytes.Equal(got, data) {
		t.Fatal("GET after sync fallback")
	}
}

// Requirement 9: SFTP_SYNC_UPLOAD=true answers only after the remote write.
func TestSyncModeWaitsForRemote(t *testing.T) {
	e := newEnv(t, envOpts{sync: true, bytesPerSec: 512 << 10})
	data, digest := randomBlob(t, 512<<10) // ~1 s remotely
	elapsed, code := e.pushBlob("grp/app", data, digest)
	if code != http.StatusCreated {
		t.Fatalf("commit: %d", code)
	}
	e.assertRemoteContent(e.remoteBlob("grp/app", digest), data)
	if elapsed < 700*time.Millisecond {
		t.Fatalf("sync commit returned after %v, before the throttled upload could finish", elapsed)
	}
	manifest := manifestFor(digest, len(data), digest, len(data))
	resp := e.do(http.MethodPut, "/v2/grp/app/manifests/v1", manifest, map[string]string{"Content-Type": "application/vnd.oci.image.manifest.v1+json"})
	readAll(t, resp)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("manifest PUT: %d", resp.StatusCode)
	}
	e.assertRemoteContent(e.srv.File("registry/grp/app/manifests/v1"), manifest)
	if blobSpool().PendingBytes() != 0 {
		t.Fatal("sync mode must not spool")
	}
	e.assertNoRemoteTemps()
}

// Stall G1 + G3: a connection that goes silent mid-upload is abandoned and replaced; the spooled upload
// completes on another connection with no partial file at the final name.
func TestSpoolUploadRecoversFromStall(t *testing.T) {
	const stall = 400 * time.Millisecond
	e := newEnv(t, envOpts{stall: stall})
	data, digest := randomBlob(t, 512<<10)
	e.srv.StallOnceAfterWrite(128 << 10)
	start := time.Now()
	if _, code := e.pushBlob("grp/app", data, digest); code != http.StatusCreated {
		t.Fatalf("commit: %d", code)
	}
	// stall detection + the spool's first 1 s backoff + the upload itself
	e.waitDrained(stall + 4*time.Second)
	t.Logf("drained after %v", time.Since(start))
	if e.srv.Stalls() != 1 {
		t.Fatalf("expected exactly one injected stall, got %d", e.srv.Stalls())
	}
	e.assertRemoteContent(e.remoteBlob("grp/app", digest), data)
	e.assertNoRemoteTemps()
	deadline := time.Now().Add(5 * time.Second)
	for e.drv.Pool.Alive() != 4 || e.srv.Dials() <= 4 {
		if time.Now().After(deadline) {
			t.Fatalf("stalled client not replaced: alive=%d dials=%d", e.drv.Pool.Alive(), e.srv.Dials())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Stall G2: a streaming GET survives a mid-stream stall and still delivers identical bytes.
func TestStreamingGetSurvivesStall(t *testing.T) {
	for _, cache := range []int64{0, 64 << 20} {
		t.Run(fmt.Sprintf("cache=%d", cache), func(t *testing.T) {
			e := newEnv(t, envOpts{stall: 300 * time.Millisecond, readCacheBytes: cache})
			data, digest := randomBlob(t, 1<<20)
			placeRemote(t, e, "grp/app", data, digest)
			e.srv.StallOnceAfterRead(300 << 10)
			resp := e.do(http.MethodGet, "/v2/grp/app/blobs/"+digest, nil, nil)
			got := readAll(t, resp)
			if resp.StatusCode != http.StatusOK || !bytes.Equal(got, data) {
				t.Fatalf("GET across stall: %d, %d bytes", resp.StatusCode, len(got))
			}
			if e.srv.Stalls() != 1 {
				t.Fatalf("expected the injected stall to fire, got %d", e.srv.Stalls())
			}
		})
	}
}

// Stall G4: in sync mode, when every connection stalls, the commit fails instead of hanging.
func TestSyncCommitFailsWhenAllConnectionsStall(t *testing.T) {
	old := syncRetryBackoff
	syncRetryBackoff = 50 * time.Millisecond
	t.Cleanup(func() { syncRetryBackoff = old })
	e := newEnv(t, envOpts{sync: true, stall: 400 * time.Millisecond})
	data, digest := randomBlob(t, 256<<10)

	resp := e.do(http.MethodPost, "/v2/grp/app/blobs/uploads/", nil, nil)
	readAll(t, resp)
	resp = e.do(http.MethodPatch, resp.Header.Get("Location"), data, nil)
	readAll(t, resp)
	e.srv.StallAllAfterWrite(64 << 10)
	start := time.Now()
	resp = e.do(http.MethodPut, resp.Header.Get("Location")+"&digest="+digest, nil, nil)
	readAll(t, resp)
	elapsed := time.Since(start)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500 when the remote is unusable, got %d", resp.StatusCode)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("commit took %v; must fail within bounded retries", elapsed)
	}
	if _, err := os.Stat(e.remoteBlob("grp/app", digest)); !os.IsNotExist(err) {
		t.Fatal("no file may appear at the final remote path")
	}
	t.Logf("failed after %v with %d stalls", elapsed, e.srv.Stalls())
}

// A tag re-pushed with a same-length but different manifest must replace the remote copy (the
// "same size means done" shortcut is only valid for content-addressed paths).
func TestRepushedTagWithSameSizeIsUploaded(t *testing.T) {
	e := newEnv(t, envOpts{})
	put := func(m []byte) {
		resp := e.do(http.MethodPut, "/v2/grp/app/manifests/latest", m, map[string]string{"Content-Type": "application/vnd.oci.image.manifest.v1+json"})
		readAll(t, resp)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("manifest PUT: %d", resp.StatusCode)
		}
	}
	v1 := []byte(`{"schemaVersion":2,"layers":[],"annotations":{"v":"1"}}`)
	v2 := []byte(`{"schemaVersion":2,"layers":[],"annotations":{"v":"2"}}`)
	put(v1)
	e.waitDrained(5 * time.Second)
	put(v2)
	if got := readAll(t, e.do(http.MethodGet, "/v2/grp/app/manifests/latest", nil, nil)); !bytes.Equal(got, v2) {
		t.Fatalf("GET right after re-push returned %s", got)
	}
	e.waitDrained(5 * time.Second)
	e.assertRemoteContent(e.srv.File("registry/grp/app/manifests/latest"), v2)
}

// Monolithic POST ?digest= still works in both modes.
func TestMonolithicUpload(t *testing.T) {
	for _, sync := range []bool{false, true} {
		t.Run(fmt.Sprintf("sync=%v", sync), func(t *testing.T) {
			e := newEnv(t, envOpts{sync: sync})
			data, digest := randomBlob(t, 10<<10)
			resp := e.do(http.MethodPost, "/v2/grp/app/blobs/uploads/?digest="+digest, data, nil)
			readAll(t, resp)
			if resp.StatusCode != http.StatusCreated || resp.Header.Get("Docker-Content-Digest") != digest {
				t.Fatalf("monolithic: %d", resp.StatusCode)
			}
			if !sync {
				e.waitDrained(5 * time.Second)
			}
			e.assertRemoteContent(e.remoteBlob("grp/app", digest), data)
			_, wrong := randomBlob(t, 5)
			resp = e.do(http.MethodPost, "/v2/grp/app/blobs/uploads/?digest="+wrong, data, nil)
			readAll(t, resp)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("digest mismatch should be 400, got %d", resp.StatusCode)
			}
		})
	}
}

func TestParseRange(t *testing.T) {
	cases := []struct {
		h          string
		start, end int64
		ok         bool
	}{
		{"bytes=0-9", 0, 9, true},
		{"bytes=5-", 5, 99, true},
		{"bytes=-10", 90, 99, true},
		{"bytes=-500", 0, 99, true},
		{"bytes=90-500", 90, 99, true},
		{"bytes=100-", 0, 0, false},
		{"bytes=9-5", 0, 0, false},
		{"bytes=0-1,5-6", 0, 0, false},
		{"items=0-1", 0, 0, false},
		{"bytes=abc", 0, 0, false},
	}
	for _, c := range cases {
		s, e, ok := parseRange(c.h, 100)
		if ok != c.ok || (ok && (s != c.start || e != c.end)) {
			t.Errorf("parseRange(%q) = %d,%d,%v; want %d,%d,%v", c.h, s, e, ok, c.start, c.end, c.ok)
		}
	}
}
