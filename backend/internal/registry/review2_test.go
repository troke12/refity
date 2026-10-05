package registry

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// failAllUploads makes every remote temp-file open fail (spool and direct writes) until released.
func failAllUploads(e *testEnv) (release func()) {
	var blocked atomic.Bool
	blocked.Store(true)
	e.srv.FailOpen = func(path string, flags int) error {
		if blocked.Load() && strings.Contains(path, ".uploading-") {
			return errors.New("injected: remote rejects uploads")
		}
		return nil
	}
	return func() { blocked.Store(false) }
}

// An acknowledged (201) blob must survive a later failed direct write of the same blob, whether the
// direct write comes from the free-space floor or from switching the instance to sync mode.
func TestAckedBlobSurvivesFailedDirectWrite(t *testing.T) {
	cases := []struct {
		name   string
		before func(e *testEnv)
	}{
		{"free-space floor", func(e *testEnv) { diskFree = func(string) int64 { return 0 } }},
		{"switched to sync mode", func(e *testEnv) { e.cfg.SFTPSyncUpload = true }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			old := syncRetryBackoff
			syncRetryBackoff = 10 * time.Millisecond
			t.Cleanup(func() { syncRetryBackoff = old })
			oldFree := diskFree
			t.Cleanup(func() { diskFree = oldFree })
			e := newEnv(t, envOpts{minFree: 2000})
			release := failAllUploads(e)
			data, digest := randomBlob(t, 4096)
			if _, code := e.pushBlob("grp/app", data, digest); code != http.StatusCreated {
				t.Fatalf("first push: %d", code)
			}
			tc.before(e)
			e.pushBlob("grp/app", data, digest) // a second CI job pushes the same layer; outcome may vary

			if _, ok := blobSpool().Lookup("registry/grp/app/blobs/" + digest); !ok {
				t.Fatal("acknowledged blob was dropped from the spool")
			}
			if got := readAll(t, e.do(http.MethodGet, "/v2/grp/app/blobs/"+digest, nil, nil)); !bytes.Equal(got, data) {
				t.Fatal("acknowledged blob no longer retrievable")
			}
			release()
			e.waitDrained(15 * time.Second)
			e.assertRemoteContent(e.remoteBlob("grp/app", digest), data)
		})
	}
}

// A failed direct tag write must leave the older spooled version in place (served and later uploaded).
func TestFailedDirectTagWriteKeepsSpooledVersion(t *testing.T) {
	old := syncRetryBackoff
	syncRetryBackoff = 10 * time.Millisecond
	t.Cleanup(func() { syncRetryBackoff = old })
	e := newEnv(t, envOpts{})
	release := failAllUploads(e)
	v1 := []byte(`{"schemaVersion":2,"layers":[],"annotations":{"v":"1"}}`)
	v2 := []byte(`{"schemaVersion":2,"layers":[],"annotations":{"v":"2"}}`)
	if code := e.putManifest("grp/app", "latest", v1); code != http.StatusCreated {
		t.Fatalf("PUT v1: %d", code)
	}
	e.cfg.SFTPSyncUpload = true
	if code := e.putManifest("grp/app", "latest", v2); code != http.StatusInternalServerError {
		t.Fatalf("PUT v2 with failing remote: %d, want 500", code)
	}
	if got := e.getManifest("grp/app", "latest"); !bytes.Equal(got, v1) {
		t.Fatalf("after failed v2 write GET returned %s, want v1", got)
	}
	release()
	e.waitDrained(15 * time.Second)
	e.assertRemoteContent(e.srv.File("registry/grp/app/manifests/latest"), v1)
}

// A failed synchronous commit was never acknowledged: it must not leave a local copy behind or a job in
// the spool (the client retries the push).
func TestFailedSyncCommitLeavesNothingBehind(t *testing.T) {
	old := syncRetryBackoff
	syncRetryBackoff = 10 * time.Millisecond
	t.Cleanup(func() { syncRetryBackoff = old })
	e := newEnv(t, envOpts{sync: true})
	failAllUploads(e)
	data, digest := randomBlob(t, 2048)
	if _, code := e.pushBlob("grp/app", data, digest); code != http.StatusInternalServerError {
		t.Fatalf("commit with failing remote: %d, want 500", code)
	}
	if blobSpool().PendingBytes() != 0 {
		t.Fatal("unacknowledged blob was spooled")
	}
	if _, err := os.Stat(e.remoteBlob("grp/app", digest)); !os.IsNotExist(err) {
		t.Fatal("nothing may appear at the final remote path")
	}
}

// Storage failures are 503 for tag listing and for manifest-list child checks, not 404/400.
func TestTagsListAndManifestListReport503OnOutage(t *testing.T) {
	e := newEnv(t, envOpts{acquire: 200 * time.Millisecond})
	_ = os.MkdirAll(e.srv.File("registry/grp/app/manifests"), 0o755)
	_ = os.WriteFile(e.srv.File("registry/grp/app/manifests/v1"), []byte(`{}`), 0o644)
	e.srv.SetRefuse(true)
	e.srv.DropAll()

	resp := e.do(http.MethodGet, "/v2/grp/app/tags/list", nil, nil)
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "UNAVAILABLE") {
		t.Fatalf("tags/list during outage: %d %s", resp.StatusCode, body)
	}
	child := "sha256:" + strings.Repeat("c", 64)
	list := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"digest":"` + child + `"}]}`)
	resp = e.do(http.MethodPut, "/v2/grp/app/manifests/multi", list, map[string]string{"Content-Type": "application/vnd.oci.image.index.v1+json"})
	body = readAll(t, resp)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("manifest list PUT during outage: %d %s, want 503", resp.StatusCode, body)
	}
}

// Manifest GET/HEAD served from the spool must not wait on an unreachable remote for the OCI copy.
func TestManifestGetFastDuringOutage(t *testing.T) {
	e := newEnv(t, envOpts{acquire: 3 * time.Second})
	blockSpoolUploads(e)
	// A Docker v2 manifest: the OCI rewrite changes its bytes, so a digest copy would be written.
	m := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","layers":[]}`)
	resp := e.do(http.MethodPut, "/v2/grp/app/manifests/v1", m, map[string]string{"Content-Type": "application/vnd.docker.distribution.manifest.v2+json"})
	readAll(t, resp)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT: %d", resp.StatusCode)
	}
	e.srv.SetRefuse(true)
	e.srv.DropAll()
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		start := time.Now()
		resp := e.do(method, "/v2/grp/app/manifests/v1", nil, nil)
		readAll(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d", method, resp.StatusCode)
		}
		if el := time.Since(start); el > 1500*time.Millisecond {
			t.Fatalf("%s took %v during an outage; must not block on the remote", method, el)
		}
	}
}

// Leftover local temp files of interrupted direct manifest writes are removed at startup.
func TestStartupSweepsDirectWriteTemps(t *testing.T) {
	e := newEnv(t, envOpts{})
	leftover := filepath.Join(e.spool, "manifest-123456.direct.tmp")
	if err := os.WriteFile(leftover, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := initStorage(e.cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(leftover); !os.IsNotExist(err) {
		t.Fatal("direct-write temp file survived startup")
	}
}

// Handlers still running when Shutdown clears the storage must not panic or race.
func TestHandlersSurviveConcurrentShutdown(t *testing.T) {
	e := newEnv(t, envOpts{readCacheBytes: 1 << 20})
	blockSpoolUploads(e)
	data, digest := randomBlob(t, 2048)
	if _, code := e.pushBlob("grp/app", data, digest); code != http.StatusCreated {
		t.Fatalf("commit: %d", code)
	}
	if code := e.putManifest("grp/app", "v1", []byte(`{"schemaVersion":2,"layers":[]}`)); code != http.StatusCreated {
		t.Fatal(code)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for _, u := range []string{"/v2/grp/app/blobs/" + digest, "/v2/grp/app/manifests/v1", "/v2/grp/app/tags/list", "/v2/_catalog"} {
					if resp, err := http.Get(e.url + u); err == nil {
						_, _ = io.Copy(io.Discard, resp.Body)
						resp.Body.Close()
					}
				}
			}
		}()
	}
	time.Sleep(100 * time.Millisecond)
	Shutdown()
	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
}
