package api

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	godigest "github.com/opencontainers/go-digest"
	"refity/backend/internal/config"
	"refity/backend/internal/database"
	"refity/backend/internal/driver/local"
	"refity/backend/internal/driver/sftp/sftptest"
	"refity/backend/internal/registry"
)

func pushBlob(t *testing.T, base, repo string, data []byte, digest string) {
	t.Helper()
	do := func(method, url string, body []byte) *http.Response {
		req, _ := http.NewRequest(method, url, bytes.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp
	}
	resp := do(http.MethodPost, base+"/v2/"+repo+"/blobs/uploads/", nil)
	resp = do(http.MethodPatch, base+resp.Header.Get("Location"), data)
	resp = do(http.MethodPut, base+resp.Header.Get("Location")+"&digest="+digest, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("push: %d", resp.StatusCode)
	}
}

// A repository delete that fails in the database must not destroy the repository's pending
// (acknowledged) pushes: the repository still exists.
func TestDeleteRepositoryPurgesOnlyAfterDBDelete(t *testing.T) {
	srv := sftptest.New(t)
	srv.FailOpen = func(path string, flags int) error {
		if strings.Contains(path, ".uploading-") {
			return errors.New("injected: uploads blocked")
		}
		return nil
	}
	drv := srv.Pool(t, 2, 0, 0)
	cfg := &config.Config{JWTSecret: "x", SpoolDir: t.TempDir(), UploadWorkers: 1}
	registry.NewRouterWithDeps(local.NewDriver(t.TempDir()), drv, cfg, nil, nil)
	t.Cleanup(registry.Shutdown)
	ts := httptest.NewServer(http.HandlerFunc(registry.RegistryHandler))
	t.Cleanup(ts.Close)

	data := make([]byte, 1024)
	_, _ = rand.Read(data)
	pushBlob(t, ts.URL, "grp/app", data, godigest.FromBytes(data).String())
	if st, _ := registry.SpoolStats(); st.Pending != 1 {
		t.Fatalf("precondition: %d pending", st.Pending)
	}

	db, err := database.NewDatabase(filepath.Join(t.TempDir(), "refity.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.Close() // every DB call now fails
	h := NewAPIHandler(drv, db, cfg)
	rec := httptest.NewRecorder()
	h.DeleteRepositoryHandler(rec, httptest.NewRequest(http.MethodDelete, "/api/repositories/grp%2Fapp", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("delete with failing DB: %d, want 500", rec.Code)
	}
	// The repository still exists, so its pending (already acknowledged) pushes must survive: the purge
	// runs only after the DB rows are really gone. (The remote folder is untouched here too, but that is
	// already covered by the successful-delete test below, which has a folder to remove.)
	if st, _ := registry.SpoolStats(); st.Pending != 1 {
		t.Fatalf("failed delete dropped the repository's pending push (%d pending)", st.Pending)
	}
}

// A repository delete that succeeds really does purge: the repository's remote folder goes. This is the
// positive half, so the ordering test above cannot pass simply because nothing happens either way.
func TestDeleteRepositoryPurgesAfterDBDelete(t *testing.T) {
	srv := sftptest.New(t)
	drv := srv.Pool(t, 2, 0, 0)
	cfg := &config.Config{JWTSecret: "x", SpoolDir: t.TempDir(), UploadWorkers: 2,
		ReadCacheDir: t.TempDir(), ReadCacheBytes: 8 << 20}
	registry.NewRouterWithDeps(local.NewDriver(t.TempDir()), drv, cfg, nil, nil)
	t.Cleanup(registry.Shutdown)
	ts := httptest.NewServer(http.HandlerFunc(registry.RegistryHandler))
	t.Cleanup(ts.Close)

	// A real push, so the repository has a remote folder with content in it.
	data := make([]byte, 1024)
	_, _ = rand.Read(data)
	pushBlob(t, ts.URL, "grp/app", data, godigest.FromBytes(data).String())
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(srv.File("registry/grp/app/manifests")); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(srv.File("registry/grp/app")); err != nil {
		t.Fatalf("precondition: the push did not create the remote folder: %v", err)
	}

	db, err := database.NewDatabase(filepath.Join(t.TempDir(), "refity.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.CreateRepository("grp/app"); err != nil {
		t.Fatal(err)
	}
	h := NewAPIHandler(drv, db, cfg)
	rec := httptest.NewRecorder()
	h.DeleteRepositoryHandler(rec, httptest.NewRequest(http.MethodDelete, "/api/repositories/grp%2Fapp", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("successful delete: %d body=%s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(srv.File("registry/grp/app")); !os.IsNotExist(err) {
		t.Fatalf("a successful delete must remove the remote folder: %v", err)
	}
	if st, _ := registry.SpoolStats(); st.Pending != 0 {
		t.Fatalf("pending uploads survived the delete (%d)", st.Pending)
	}
}
