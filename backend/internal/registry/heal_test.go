package registry

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	godigest "github.com/opencontainers/go-digest"
)

// A digest-named blob whose remote copy is the wrong content (a partial or interrupted write from
// before the spool existed) must be healed: the pull fails instead of returning foreign bytes, and the
// corrupt file is removed from the remote so the next push of that digest actually writes it.
//
// The corrupt copy deliberately has the SAME SIZE as the correct one, because that is exactly the case
// the "same size means already uploaded" shortcut silently trusts. Without the path being marked as
// corrupt, the re-push at the end would be skipped and the wrong bytes would stay stored.
func TestCorruptRemoteBlobIsHealedOnPull(t *testing.T) {
	e := newEnv(t, envOpts{readCacheBytes: 64 << 20})
	repo := "grp/app"
	good, digest := randomBlob(t, 32<<10)
	bad, _ := randomBlob(t, 32<<10) // same length, different bytes
	placeRemote(t, e, repo, bad, digest)

	remotePath := e.remoteBlob(repo, digest)
	if fi, err := os.Stat(remotePath); err != nil || fi.Size() != int64(len(bad)) {
		t.Fatalf("precondition: want a %d-byte corrupt file at the digest path", len(bad))
	}

	// The pull must not hand back the corrupt bytes as a complete body: either it fails part-way, or what
	// it delivers hashes to the digest that was asked for.
	resp := e.do(http.MethodGet, "/v2/"+repo+"/blobs/"+digest, nil, nil)
	got, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err == nil {
		if bytes.Equal(got, bad) {
			t.Fatal("corrupt remote blob delivered as a complete 200 body")
		}
		if godigest.FromBytes(got).String() != digest {
			t.Fatalf("complete body does not hash to the requested digest: %s", godigest.FromBytes(got))
		}
	}

	// The corrupt copy must be gone, so the next push of that digest is written for real.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(remotePath); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("corrupt remote blob was never removed")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, ok := blobCache().Has("registry/" + repo + "/blobs/" + digest); ok {
		t.Fatal("corrupt blob was cached")
	}
	// The path must stay marked as corrupt until a correct copy is stored, otherwise the push below
	// would trust the matching size and skip the write entirely.
	if !knownCorrupt("registry/" + repo + "/blobs/" + digest) {
		t.Fatal("healed path was not marked corrupt, so a same-size re-push would be skipped")
	}

	// And the push path now restores it correctly, despite the size being identical.
	if _, code := e.pushBlob(repo, good, digest); code != http.StatusCreated {
		t.Fatalf("re-push of the healed digest: %d", code)
	}
	e.waitDrained(10 * time.Second)
	e.assertRemoteContent(remotePath, good)
	if knownCorrupt("registry/" + repo + "/blobs/" + digest) {
		t.Fatal("path still marked corrupt after a correct copy was stored there")
	}
}

// The same-size shortcut trusts the remote file's size, so a corrupt copy that was NOT removed would
// survive every re-push. This is the case that makes the corrupt marking necessary: the heal is
// interrupted (storage goes away right after it marked the path), the file is somehow still there, and
// the re-push must still rewrite it.
func TestSameSizeRepushRewritesKnownCorruptPath(t *testing.T) {
	e := newEnv(t, envOpts{readCacheBytes: 64 << 20})
	repo := "grp/app"
	good, digest := randomBlob(t, 32<<10)
	bad, _ := randomBlob(t, 32<<10) // identical size
	placeRemote(t, e, repo, bad, digest)
	remotePath := e.remoteBlob(repo, digest)
	path := "registry/" + repo + "/blobs/" + digest

	// Simulate the state the heal leaves behind: the path is known corrupt but the bad file is still
	// sitting there (the delete did not get through). This is what the marking exists for.
	markCorrupt(path)
	if !knownCorrupt(path) {
		t.Fatal("markCorrupt did not record the path")
	}
	if _, code := e.pushBlob(repo, good, digest); code != http.StatusCreated {
		t.Fatalf("re-push: %d", code)
	}
	e.waitDrained(10 * time.Second)
	e.assertRemoteContent(remotePath, good)
}
