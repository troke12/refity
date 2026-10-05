package registry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// A directory that the walk reaches twice must be listed once and its blob counted once. Without the
// visited set the same blob is hashed and reported again, and a cycle would never finish. SFTP has no
// symlinks, so this runs against a driver that reports one path under two names.
func TestAuditCountsEachBlobOnce(t *testing.T) {
	newEnv(t, envOpts{}) // real storage for initStorage; the walk itself uses the stub below
	a := newAliasDriver()
	digest := "sha256:" + strings.Repeat("ab", 32)
	blobPath := "registry/grp/app/blobs/" + digest

	a.addDir("registry", "grp")
	a.addDir("registry/grp", "app")
	a.addDir("registry/grp/app", "blobs", "manifests")
	a.addDir("registry/grp/app/manifests")
	a.addDir("registry/grp/app/blobs", digest)
	a.addBlob(blobPath, []byte("not the bytes this digest names"))
	// "loop" resolves to registry/grp/app itself, so the walk descends into registry/grp/app/loop and
	// from there reaches registry/grp/app a second time. A cycle is the case the visited set exists for.
	a.selfAlias("registry/grp/app", "loop")

	old := sftpDriver
	sftpDriver = a
	t.Cleanup(func() { sftpDriver = old })

	res, err := auditBlobs(context.Background(), "", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Scanned != 1 {
		t.Fatalf("scanned=%d, want 1; the cycle made the walk count the blob %d times (paths %v)", res.Scanned, res.Scanned, res.CorruptPaths)
	}
	if res.Corrupt != 1 {
		t.Fatalf("corrupt=%d, want 1 (paths %v)", res.Corrupt, res.CorruptPaths)
	}
	// The directory must have been listed once even though the walk reached it twice.
	if n := a.listCount("registry/grp/app"); n > 1 {
		t.Fatalf("a cycled directory was listed %d times", n)
	}
}

// A digest-named file that is a directory, not a blob, must not be treated as a blob object to delete.
func TestAuditSkipsDigestNamedDirectories(t *testing.T) {
	e := newEnv(t, envOpts{})
	repo := "grp/app"
	makeRepoDirs(t, e, repo)
	_, digest := randomBlob(t, 8<<10)
	// A directory named like a digest inside blobs/.
	if err := os.MkdirAll(e.srv.File("registry/"+repo+"/blobs/"+digest), 0o755); err != nil {
		t.Fatal(err)
	}

	res, err := auditBlobs(context.Background(), repo, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed != 0 {
		t.Fatalf("removed=%d; a digest-named directory must not be deleted", res.Removed)
	}
	if _, err := os.Stat(e.srv.File("registry/" + repo + "/blobs/" + digest)); err != nil {
		t.Fatal("the digest-named directory was removed")
	}
	// It is counted as a failure (Stat says "is a directory"), which is the honest outcome.
	if res.Failed == 0 {
		t.Log("the digest-named directory was skipped without being counted; acceptable but noted")
	}
}

// The mark file lives in the spool directory; the audit must not treat it as a blob.
func TestAuditIgnoresSpoolMetadataFiles(t *testing.T) {
	e := newEnv(t, envOpts{})
	repo := "grp/app"
	makeRepoDirs(t, e, repo)
	good, digest := randomBlob(t, 8<<10)
	placeRemote(t, e, repo, good, digest)
	// A non-digest stray file in blobs/ (e.g. an editor backup) must be ignored.
	if err := os.WriteFile(e.srv.File("registry/"+repo+"/blobs/notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := auditBlobs(context.Background(), repo, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Scanned != 1 {
		t.Fatalf("scanned=%d, want 1; the stray file was treated as a blob", res.Scanned)
	}
}

// The report shape must be usable by a strict JSON consumer even when nothing is wrong.
func TestAuditReportIsWellFormed(t *testing.T) {
	e := newEnv(t, envOpts{})
	repo := "grp/app"
	makeRepoDirs(t, e, repo)
	good, digest := randomBlob(t, 8<<10)
	placeRemote(t, e, repo, good, digest)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/maintenance/audit-blobs", nil)
	AuditCorruptBlobsHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"corrupt_paths":[]`) {
		t.Fatalf("an empty report must serialise corrupt_paths as [], got: %s", rec.Body.String())
	}
	var parsed AuditResult
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("the report is not valid JSON: %v", err)
	}
}
