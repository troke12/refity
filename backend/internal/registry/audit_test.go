package registry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The audit finds damage that the read path cannot: a corrupt blob nobody has pulled since, whose size
// matches the correct one, so the "same size means done" shortcut keeps trusting it forever.
func TestAuditFindsAndRemovesSameSizeCorruptBlobs(t *testing.T) {
	e := newEnv(t, envOpts{})
	repo := "grp/app"
	makeRepoDirs(t, e, repo)
	good1, digest1 := randomBlob(t, 8<<10)
	good2, digest2 := randomBlob(t, 12<<10)
	bad, _ := randomBlob(t, 8<<10) // same size as good1
	placeRemote(t, e, repo, good1, digest1)
	placeRemote(t, e, repo, good2, digest2)
	placeRemote(t, e, repo, bad, digest1)

	p1, p2 := e.remoteBlob(repo, digest1), e.remoteBlob(repo, digest2)

	// Report-only first: it must find the damage without touching anything.
	res, err := auditBlobs(context.Background(), repo, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Corrupt != 1 {
		t.Fatalf("report-only audit: corrupt=%d, want 1 (paths %v)", res.Corrupt, res.CorruptPaths)
	}
	if res.Removed != 0 {
		t.Fatalf("report-only audit removed %d files", res.Removed)
	}
	e.assertRemoteContent(p1, bad) // untouched

	// Now remove.
	res, err = auditBlobs(context.Background(), repo, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Corrupt != 1 || res.Removed != 1 {
		t.Fatalf("audit: corrupt=%d removed=%d, want 1/1", res.Corrupt, res.Removed)
	}
	if _, err := os.Stat(p1); !os.IsNotExist(err) {
		t.Fatalf("corrupt blob still present: %v", err)
	}
	e.assertRemoteContent(p2, good2) // the healthy one is untouched

	// The removed path is marked, so a re-push of the correct bytes is actually written.
	if !knownCorrupt("registry/" + repo + "/blobs/" + digest1) {
		t.Fatal("audit did not mark the removed path as corrupt")
	}
	if _, code := e.pushBlob(repo, good1, digest1); code != http.StatusCreated {
		t.Fatalf("re-push after audit: %d", code)
	}
	e.waitDrained(10 * time.Second)
	e.assertRemoteContent(p1, good1)
}

// The audit must never delete a blob a client is pushing right now. The remote starts out holding the
// WRONG bytes of that digest, so the audit really does want to delete it; the concurrent push replaces
// them with the correct ones. The audit's re-verify happens under the path lock the writers hold across
// their whole upload, so it must see the new copy and back off.
func TestAuditLeavesConcurrentlyPushedBlobAlone(t *testing.T) {
	e := newEnv(t, envOpts{})
	repo := "grp/app"
	good, digest := randomBlob(t, 200<<10)
	bad, _ := randomBlob(t, 200<<10) // same size, different bytes: the audit must want to remove this
	placeRemote(t, e, repo, bad, digest)
	path := e.remoteBlob(repo, digest)

	// Push the correct bytes for that same digest while the audit is walking. The push runs in a
	// goroutine and the audit only starts once the request is actually in flight, so the two overlap.
	pushed := make(chan struct{})
	go func() {
		defer close(pushed)
		if _, code := e.pushBlob(repo, good, digest); code != http.StatusCreated {
			t.Errorf("concurrent push: %d", code)
		}
		e.waitDrained(15 * time.Second)
	}()
	res, err := auditBlobs(context.Background(), repo, true, nil)
	<-pushed
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed != 0 {
		t.Fatalf("the audit deleted a blob a client was pushing (%d removed, paths %v)", res.Removed, res.CorruptPaths)
	}
	// The correct bytes must be what is stored, not the corrupt copy and not nothing at all.
	e.assertRemoteContent(path, good)
}

func TestAuditHandlerRejectsNonPost(t *testing.T) {
	newEnv(t, envOpts{})
	rec := httptest.NewRecorder()
	AuditCorruptBlobsHandler(rec, httptest.NewRequest(http.MethodGet, "/api/maintenance/audit-blobs", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET audit: %d, want 405", rec.Code)
	}
}

// A real repository always has a manifests/ directory full of tags. The audit must walk past those
// (they are files, and their bytes deliberately do not match their path name) and still audit the blobs.
func TestAuditHandlesTaggedRepository(t *testing.T) {
	e := newEnv(t, envOpts{})
	repo := "grp/app"
	good, digest := randomBlob(t, 8<<10)
	bad, _ := randomBlob(t, 8<<10)
	placeRemote(t, e, repo, good, digest)
	placeRemote(t, e, repo, bad, digest)
	// Ordinary tag files, plus the OCI digest copy handleManifest writes alongside them.
	if code := e.putManifest(repo, "latest", []byte(`{"schemaVersion":2,"layers":[]}`)); code != http.StatusCreated {
		t.Fatalf("put manifest: %d", code)
	}
	e.waitDrained(10 * time.Second)

	res, err := auditBlobs(context.Background(), repo, true, nil)
	if err != nil {
		t.Fatalf("audit must not fail on a tagged repository: %v", err)
	}
	if res.Scanned != 1 {
		t.Fatalf("scanned %d, want 1 (only the blob; manifests are not blob objects)", res.Scanned)
	}
	if res.Corrupt != 1 || res.Removed != 1 {
		t.Fatalf("corrupt=%d removed=%d, want 1/1", res.Corrupt, res.Removed)
	}
	if _, err := os.Stat(e.remoteBlob(repo, digest)); !os.IsNotExist(err) {
		t.Fatal("corrupt blob still present")
	}
	// The tag manifest must be untouched: it is not a blob object and its bytes are rewritten to OCI.
	if _, err := os.Stat(e.srv.File("registry/" + repo + "/manifests/latest")); err != nil {
		t.Fatalf("tag manifest was removed by the audit: %v", err)
	}
}

// A leaf that cannot be listed must not abort the whole run.
func TestAuditSkipsUnreadableDirectoryAndContinues(t *testing.T) {
	e := newEnv(t, envOpts{})
	repo := "grp/app"
	makeRepoDirs(t, e, repo)
	good, digest := randomBlob(t, 8<<10)
	placeRemote(t, e, repo, good, digest)
	// A plain file where a directory is expected.
	if err := os.WriteFile(e.srv.File("registry/"+repo+"/blobs/notadir"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := auditBlobs(context.Background(), repo, true, nil)
	if err != nil {
		t.Fatalf("audit failed instead of skipping: %v", err)
	}
	if res.Scanned != 1 || res.Corrupt != 0 {
		t.Fatalf("scanned=%d corrupt=%d, want 1/0", res.Scanned, res.Corrupt)
	}
	e.assertRemoteContent(e.remoteBlob(repo, digest), good)
}

// A group or repository literally named "blobs" must still be audited, and must still not make its
// manifests directory look like a blobs directory. The walk therefore recognises a repository by the
// presence of a manifests sibling, not by how deep it sits.
func TestAuditCoversReposNamedBlobs(t *testing.T) {
	for _, repo := range []string{"blobs/app", "grp/blobs/app", "grp/blobs", "grp/app", "grp/deep/nested/app"} {
		t.Run(repo, func(t *testing.T) {
			e := newEnv(t, envOpts{})
			makeRepoDirs(t, e, repo)
			good, digest := randomBlob(t, 8<<10)
			bad, _ := randomBlob(t, 8<<10)
			placeRemote(t, e, repo, good, digest)
			placeRemote(t, e, repo, bad, digest)
			blobPath := e.remoteBlob(repo, digest)

			res, err := auditBlobs(context.Background(), repo, true, nil)
			if err != nil {
				t.Fatal(err)
			}
			if res.Scanned != 1 {
				t.Fatalf("repo %q: scanned=%d corrupt=%d, want 1; the audit missed this repository", repo, res.Scanned, res.Corrupt)
			}
			if res.Corrupt != 1 || res.Removed != 1 {
				t.Fatalf("repo %q: corrupt=%d removed=%d, want 1/1", repo, res.Corrupt, res.Removed)
			}
			if _, err := os.Stat(blobPath); !os.IsNotExist(err) {
				t.Fatalf("repo %q: corrupt blob survived the audit", repo)
			}
		})
	}
}

// makeRepoDirs creates the directory layout the registry creates for a repository: blobs/ and
// manifests/ side by side. placeRemote on its own only creates the blobs path.
func makeRepoDirs(t *testing.T, e *testEnv, repo string) {
	t.Helper()
	for _, d := range []string{"blobs", "manifests"} {
		if err := os.MkdirAll(e.srv.File("registry/"+repo+"/"+d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// Tag files and other leaves produce "not a directory" listings by design. Those must not be counted as
// failures, or a healthy tagged repository looks broken in the report.
func TestAuditDoesNotCountLeafListingsAsFailures(t *testing.T) {
	e := newEnv(t, envOpts{})
	repo := "grp/app"
	good, digest := randomBlob(t, 8<<10)
	placeRemote(t, e, repo, good, digest)
	for _, tag := range []string{"latest", "v1", "v2"} {
		if code := e.putManifest(repo, tag, []byte(`{"schemaVersion":2,"layers":[],"annotations":{"t":"`+tag+`"}}`)); code != http.StatusCreated {
			t.Fatalf("put manifest %s: %d", tag, code)
		}
	}
	e.waitDrained(10 * time.Second)
	res, err := auditBlobs(context.Background(), repo, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Failed != 0 {
		t.Fatalf("failed=%d on a healthy tagged repository; tag listings must not count", res.Failed)
	}
	if res.Scanned != 1 || res.Corrupt != 0 {
		t.Fatalf("scanned=%d corrupt=%d, want 1/0", res.Scanned, res.Corrupt)
	}
}

// A repo-scoped audit rooted at a path that itself contains "blobs" must not reach outside that
// repository. Deciding candidacy by a "/blobs/" substring made every descendant of such a root look
// like a blob object, so a run with remove=true would audit — and delete — a sibling repository's
// manifest, because a manifest stored in Docker v2 form is deliberately not byte-identical to the
// digest it is stored under.
func TestScopedAuditNeverTouchesOtherRepositories(t *testing.T) {
	e := newEnv(t, envOpts{})
	// The audited repository, whose group is named "blobs".
	target, digest := randomBlob(t, 8<<10)
	placeRemote(t, e, "blobs/app", target, digest)

	// A sibling repository's digest-named object, planted directly with bytes that do NOT match that
	// digest. This is the exact shape the substring filter misclassified: it lives under
	// registry/blobs/other/... (so the path contains "/blobs/") but it is a manifest, not a blob.
	otherRepo := "blobs/other"
	if err := os.MkdirAll(e.srv.File("registry/"+otherRepo+"/manifests"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, manifestDigest := randomBlob(t, 4<<10)
	digestCopy := e.srv.File("registry/" + otherRepo + "/manifests/" + manifestDigest)
	if err := os.WriteFile(digestCopy, []byte("not the manifest bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := auditBlobs(context.Background(), "blobs", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Corrupt != 0 || res.Removed != 0 {
		t.Fatalf("corrupt=%d removed=%d, want 0/0; the audit classified a manifest as a blob (paths %v)", res.Corrupt, res.Removed, res.CorruptPaths)
	}
	if _, err := os.Stat(digestCopy); err != nil {
		t.Fatalf("the audit deleted another repository's manifest: %v", err)
	}
}

// A repository at registry level (no group) has its blobs directory directly under the root.
func TestAuditCoversRepositoryWithoutGroup(t *testing.T) {
	e := newEnv(t, envOpts{})
	makeRepoDirs(t, e, "solo")
	good, digest := randomBlob(t, 8<<10)
	bad, _ := randomBlob(t, 8<<10)
	placeRemote(t, e, "solo", good, digest)
	placeRemote(t, e, "solo", bad, digest)
	res, err := auditBlobs(context.Background(), "solo", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Scanned != 1 || res.Corrupt != 1 {
		t.Fatalf("scanned=%d corrupt=%d, want 1/1", res.Scanned, res.Corrupt)
	}
}

// The repository name becomes a path the walk lists and deletes from, so ".." must never be accepted:
// it would take the walk out of registry/ and delete matching files wherever it landed.
func TestAuditRejectsRepositoryTraversal(t *testing.T) {
	newEnv(t, envOpts{}) // real storage behind the handler; the walk itself must refuse the name
	for _, bad := range []string{
		"../outside",
		"..",
		"grp/../../etc",
		"grp/../other",
		"grp//app",
	} {
		t.Run(bad, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost,
				"/api/maintenance/audit-blobs?remove=true&repository="+url.QueryEscape(bad), nil)
			AuditCorruptBlobsHandler(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("repository=%q: status %d, want 400 (body %s)", bad, rec.Code, rec.Body.String())
			}
			if _, err := auditBlobs(context.Background(), bad, true, nil); err == nil {
				t.Fatalf("auditBlobs accepted %q directly", bad)
			}
		})
	}
	// A real name is still accepted, and a legitimate sibling is untouched.
	res, err := auditBlobs(context.Background(), "grp/app", true, nil)
	if err != nil {
		t.Fatalf("a valid repository name was rejected: %v", err)
	}
	if res.Repository != "grp/app" {
		t.Fatalf("repository=%q", res.Repository)
	}
}

// A file outside registry/ that looks like a blob must survive any audit, including a full run.
func TestAuditCannotReachOutsideRegistry(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "outside") // the walk must never reach here
	repo := filepath.Join(outside, "evil")
	if err := os.MkdirAll(filepath.Join(repo, "blobs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "manifests"), 0o755); err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("cd", 32)
	victim := filepath.Join(repo, "blobs", digest)
	if err := os.WriteFile(victim, []byte("not the bytes this digest names"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Reach it through the handler with a traversal name.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/api/maintenance/audit-blobs?remove=true&repository="+url.QueryEscape("../outside/evil"), nil)
	AuditCorruptBlobsHandler(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("the audit accepted a traversal and ran: %s", rec.Body.String())
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("a file outside registry/ was removed: %v", err)
	}
}

// Every shape of repository name the audit must accept and the ones it must refuse. The accepted list
// includes names deeper than "group/repo" on purpose: CreateRepositoryHandler only rejects an empty
// name, so those exist in production and the audit has to be able to reach them.
func TestSafeAuditRepoBoundsTheWalk(t *testing.T) {
	for _, ok := range []string{
		"", "grp/app", "a/b/c/d", "blobs", "grp/blobs/app", "app.x", "a-b_c", "A1",
	} {
		if !safeAuditRepo(ok) {
			t.Errorf("rejected a safe repository name: %q", ok)
		}
	}
	for _, bad := range []string{
		"..", "../x", "a/../b", "a//b", "a/./b", "./a", "/abs", `a\b`, "a b", "a/../../etc",
		"a/..", "../", "a/./", ".", "a\x00b", "a%2e%2e", "a?b", "a*b", "a:b",
	} {
		if safeAuditRepo(bad) {
			t.Errorf("accepted a repository name that can escape registry/: %q", bad)
		}
	}
}
