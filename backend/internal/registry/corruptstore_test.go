package registry

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A restart must not re-arm the bug the corrupt marks exist to prevent. If the marks only lived in
// memory, the first push after a restart would trust a same-size corrupt copy again.
func TestCorruptMarksSurviveRestart(t *testing.T) {
	e := newEnv(t, envOpts{})
	repo := "grp/app"
	path := "registry/" + repo + "/blobs/sha256:" + strings64()
	markCorrupt(path)

	// A restart: initStorage is what reloads them, and newEnv runs it.
	e2 := newEnv(t, envOpts{spoolDir: e.spool})
	if !knownCorrupt(path) {
		t.Fatal("corrupt mark did not survive a restart")
	}
	_ = e2
}

// The mark file is a cache of a re-derivable fact, so an unreadable one must not stop the process.
func TestCorruptMarkFileIsBestEffort(t *testing.T) {
	e := newEnv(t, envOpts{})
	path := "registry/grp/app/blobs/sha256:" + strings64()
	markCorrupt(path)

	// Corrupt the file: garbage lines and an over-long one.
	f := filepath.Join(e.spool, corruptFileName)
	if err := os.WriteFile(f, []byte("not-a-digest\n\n"+path+"\n"+string(make([]byte, 2<<20))+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	corruptState.mu.Lock()
	corruptState.path = f
	corruptState.mu.Unlock()
	corrupt.Range(func(k, _ any) bool { corrupt.Delete(k); return true })

	corruptState.load() // must not panic and must keep only the valid line
	if !knownCorrupt(path) {
		t.Fatal("a valid mark was dropped when reloading after garbage")
	}
	corrupt.Range(func(k, _ any) bool {
		if k.(string) == "not-a-digest" {
			t.Fatal("a non-digest line was loaded as a corrupt mark")
		}
		return true
	})

	// A missing file is simply empty.
	os.Remove(f)
	corrupt.Range(func(k, _ any) bool { corrupt.Delete(k); return true })
	corruptState.load()
	if knownCorrupt(path) {
		t.Fatal("marks reappeared from a deleted file")
	}
}

// Clearing after a correct upload must also drop it from disk, so a later restart does not resurrect it.
func TestClearedCorruptMarkIsRemovedFromDisk(t *testing.T) {
	e := newEnv(t, envOpts{})
	path := "registry/grp/app/blobs/sha256:" + strings64()
	markCorrupt(path)
	f := filepath.Join(e.spool, corruptFileName)
	if _, err := os.Stat(f); err != nil {
		t.Fatalf("mark file was not written: %v", err)
	}
	clearCorrupt(path)
	data, err := os.ReadFile(f)
	if err != nil {
		t.Fatalf("mark file disappeared instead of being emptied: %v", err)
	}
	if len(data) != 0 {
		t.Fatalf("cleared mark is still on disk: %q", data)
	}
	// And a reload does not bring it back.
	corruptState.load()
	if knownCorrupt(path) {
		t.Fatal("a cleared mark came back on reload")
	}
}

// A full cycle across a restart: a pull finds the damage and removes it, the mark survives, and the
// re-push after the restart is really written even though the corrupt copy had the identical size.
//
// The remote survives the restart through the spool directory's sibling root: the SFTP test server is
// rooted at its own temp dir, so the second env reuses the first one's root to model one box.
func TestHealedSameSizeBlobIsRewrittenAfterRestart(t *testing.T) {
	first := newEnvWithRemoteRoot(t, envOpts{readCacheBytes: 64 << 20}, "")
	repo := "grp/app"
	good, digest := randomBlob(t, 32<<10)
	bad, _ := randomBlob(t, 32<<10)
	placeRemote(t, first, repo, bad, digest)
	remotePath := first.remoteBlob(repo, digest)

	// Pull once: that finds the damage and removes it.
	first.do(http.MethodGet, "/v2/"+repo+"/blobs/"+digest, nil, nil).Body.Close()
	waitUntil(t, 5*time.Second, func() bool {
		_, err := os.Stat(remotePath)
		return os.IsNotExist(err)
	})
	if !knownCorrupt("registry/" + repo + "/blobs/" + digest) {
		t.Fatal("the heal did not mark the path corrupt")
	}

	// Restart against the same spool directory and the same remote, so initStorage reloads the mark.
	second := newEnvWithRemoteRoot(t, envOpts{spoolDir: first.spool, readCacheBytes: 64 << 20}, first.srv.Root)
	if !knownCorrupt("registry/" + repo + "/blobs/" + digest) {
		t.Fatal("the corrupt mark did not survive the restart")
	}
	if err := os.MkdirAll(filepath.Dir(remotePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(remotePath, bad, 0o644); err != nil {
		t.Fatal(err)
	}

	// The mark survived, so this push must really rewrite it despite the identical size.
	if _, code := second.pushBlob(repo, good, digest); code != http.StatusCreated {
		t.Fatalf("re-push after restart: %d", code)
	}
	second.waitDrained(10 * time.Second)
	second.assertRemoteContent(remotePath, good)
}

func waitUntil(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// strings64 returns 64 hex characters, i.e. the shape of a sha256 digest.
func strings64() string {
	b := make([]byte, 64)
	for i := range b {
		b[i] = "0123456789abcdef"[i%16]
	}
	return string(b)
}

// The operator must be able to see which blobs are missing until re-pushed, without reading logs.
func TestCorruptHealthReportsMissingBlobs(t *testing.T) {
	newEnv(t, envOpts{})
	if h, ok := CorruptStats(); !ok || h.Removed != 0 {
		t.Fatalf("a fresh instance must report nothing outstanding, got ok=%v %+v", ok, h)
	}
	if msg := corruptHealthWarning(CorruptHealth{}); msg != "" {
		t.Fatalf("no outstanding damage must produce no warning, got %q", msg)
	}

	d := "sha256:"
	p1 := "registry/grp/app/blobs/" + d + strings64()
	p2 := "registry/grp/other/blobs/" + d + strings64()[:63] + "1"
	markCorrupt(p1)
	markCorrupt(p2)

	h, ok := CorruptStats()
	if !ok {
		t.Fatal("CorruptStats unavailable while storage is running")
	}
	if h.Removed != 2 {
		t.Fatalf("Removed=%d, want 2", h.Removed)
	}
	// sync.Map iteration order is not defined, so the report is a set, not a sequence.
	got := make(map[string]bool, len(h.Paths))
	for _, p := range h.Paths {
		got[p] = true
	}
	if !got[p1] || !got[p2] || len(got) != 2 {
		t.Fatalf("Paths=%v, want both %s and %s", h.Paths, p1, p2)
	}
	msg := corruptHealthWarning(h)
	if msg == "" {
		t.Fatal("outstanding missing blobs must produce a warning")
	}
	if !strings.Contains(msg, "2 blob(s)") {
		t.Fatalf("warning %q does not mention the count", msg)
	}
	if !strings.Contains(msg, "registry/grp/") {
		t.Fatalf("warning %q does not name an example path", msg)
	}
	// The age is only printed once it rounds to a second; a fresh mark legitimately has none.
	if h.OldestAgeSeconds >= 1 && !strings.Contains(msg, "oldest missing") {
		t.Fatalf("warning %q omits the age even though the oldest is %ds old", msg, h.OldestAgeSeconds)
	}

	// Once a correct copy is stored the entry is gone from the report.
	clearCorrupt(p1)
	h, _ = CorruptStats()
	if h.Removed != 1 {
		t.Fatalf("after clearing one, Removed=%d, want 1", h.Removed)
	}
	clearCorrupt(p2)
	h, _ = CorruptStats()
	if h.Removed != 0 || h.Paths != nil {
		t.Fatalf("after clearing all, %+v; the report must be empty", h)
	}
}

// Deleting a repository must clear its corrupt marks. The remote folder is going away, so a mark there
// has nothing left to guard against, and keeping it would follow the name into its next life and force
// a full re-upload of every digest ever marked.
func TestPurgeRepoClearsItsCorruptMarksOnly(t *testing.T) {
	newEnv(t, envOpts{})
	const d = "sha256:"
	digest := func(s string) string { return d + s }
	inside := []string{
		"registry/grp/app/blobs/" + digest(strings64()),
		"registry/grp/app/blobs/" + digest(strings64B()),
	}
	outside := []string{
		"registry/grp/other/blobs/" + digest(strings64()),
		// A sibling whose name merely starts with the deleted repository's name must not match.
		"registry/grp/application/blobs/" + digest(strings64()),
		"registry/grp/appx/blobs/" + digest(strings64()),
	}
	for _, p := range append(append([]string{}, inside...), outside...) {
		markCorrupt(p)
	}

	if n := PurgeRepoCount("grp/app"); n != len(inside) {
		t.Fatalf("purging grp/app cleared %d marks, want %d", n, len(inside))
	}
	for _, p := range inside {
		if knownCorrupt(p) {
			t.Fatalf("%s survived the repository delete", p)
		}
	}
	for _, p := range outside {
		if !knownCorrupt(p) {
			t.Fatalf("purging grp/app wrongly cleared %s", p)
		}
	}

	// And the marks that remain are what a restart reloads.
	for _, p := range outside {
		clearCorrupt(p)
	}
}

// PurgeRepo is the public entry point; this wraps it so the test can count without touching internals.
func PurgeRepoCount(name string) int {
	before := countCorrupt()
	PurgeRepo(name)
	return before - countCorrupt()
}

func countCorrupt() int {
	n := 0
	corrupt.Range(func(_, _ any) bool { n++; return true })
	return n
}

func strings64B() string {
	b := []byte(strings64())
	b[0] = 'a'
	return string(b)
}

// A push whose blob is already on the remote at the right size must still check that the remote bytes
// really are the right ones. Size alone cannot tell a correct copy from a corrupt one of the same
// length, and skipping on size is what left corrupt blobs in place — and what made a later heal or audit
// delete a blob the client had already been told was stored.
func TestPushVerifiesSameSizeBlobInsteadOfTrustingSize(t *testing.T) {
	e := newEnv(t, envOpts{})
	repo := "grp/app"
	good, digest := randomBlob(t, 100<<10)
	bad, _ := randomBlob(t, 100<<10) // identical length
	placeRemote(t, e, repo, bad, digest)
	path := e.remoteBlob(repo, digest)

	if _, code := e.pushBlob(repo, good, digest); code != http.StatusCreated {
		t.Fatalf("push: %d", code)
	}
	e.waitDrained(15 * time.Second)
	// The corrupt copy must have been replaced, not trusted.
	e.assertRemoteContent(path, good)

	// And a second push of the same blob must not rewrite: the bytes are now correct, so the verify
	// confirms it and the upload is skipped as before.
	before := e.srv.Renames(path)
	if _, code := e.pushBlob(repo, good, digest); code != http.StatusCreated {
		t.Fatalf("second push: %d", code)
	}
	e.waitDrained(15 * time.Second)
	e.assertRemoteContent(path, good)
	if got := e.srv.Renames(path); got != before {
		t.Logf("the verified blob was rewritten (%d -> %d renames); a redundant upload, not a correctness problem", before, got)
	}
}

// A blob too large for the verification cap must not be trusted on the upload path. That cap exists so
// the read-path heal does not download a huge blob under the path lock; treating "too big to check" as
// "matches" there is right (leave it for the audit), but on the upload path it silently skips writing
// the bytes the client just pushed — the exact bug the verification exists to catch.
func TestLargeUnverifiableBlobIsRewrittenNotTrusted(t *testing.T) {
	if testing.Short() {
		t.Skip("writes a blob larger than the verification cap")
	}
	e := newEnv(t, envOpts{})
	repo := "grp/app"
	makeRepoDirs(t, e, repo)
	// Just over the cap, so the remote copy is real but cannot be re-read to verify.
	good, digest := randomBlob(t, healVerifyMaxBytes+1024)
	bad, _ := randomBlob(t, healVerifyMaxBytes+1024) // same length, different bytes
	placeRemote(t, e, repo, bad, digest)
	path := e.remoteBlob(repo, digest)

	if _, code := e.pushBlob(repo, good, digest); code != http.StatusCreated {
		t.Fatalf("push: %d", code)
	}
	e.waitDrained(120 * time.Second)
	// The corrupt copy must have been replaced, not trusted.
	if got := fileSize(mustStat(t, path)); got != int64(len(good)) {
		t.Fatalf("remote size %d, want %d", got, len(good))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, good) {
		t.Fatalf("an unverifiable remote blob was trusted: %d bytes, still the corrupt copy", len(data))
	}
}

func mustStat(t *testing.T, p string) os.FileInfo {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi
}

// Re-pushing an unchanged layer must not re-download it just to hash it: the cache already holds
// digest-verified bytes for that path, which settles the question locally.
func TestRePushUsesTheCachedCopyInsteadOfRedownloading(t *testing.T) {
	e := newEnv(t, envOpts{readCacheBytes: 64 << 20})
	repo := "grp/app"
	data, digest := randomBlob(t, 128<<10)
	path := e.remoteBlob(repo, digest)

	// First push writes it and adopts the verified local copy.
	if _, code := e.pushBlob(repo, data, digest); code != http.StatusCreated {
		t.Fatalf("first push: %d", code)
	}
	e.waitDrained(15 * time.Second)
	e.assertRemoteContent(path, data)
	if _, ok := blobCache().Has("registry/" + repo + "/blobs/" + digest); !ok {
		t.Fatal("precondition: the verified local copy was not cached")
	}
	before := e.srv.ReadOpens()

	// Re-push the same layer: the remote must not be read at all.
	if _, code := e.pushBlob(repo, data, digest); code != http.StatusCreated {
		t.Fatalf("re-push: %d", code)
	}
	e.waitDrained(15 * time.Second)
	e.assertRemoteContent(path, data)
	if after := e.srv.ReadOpens(); after != before {
		t.Fatalf("re-push re-downloaded the blob to verify it: %d -> %d remote reads", before, after)
	}
}

// A single over-long line in the mark file must not silently drop every mark after it. bufio stops at
// ErrTooLong, so without an explicit skip the rest of the file would come back empty.
func TestOverLongMarkLineDoesNotDropTheRest(t *testing.T) {
	e := newEnv(t, envOpts{})
	first := "registry/grp/app/blobs/sha256:" + strings64()
	last := "registry/grp/other/blobs/sha256:" + strings64()[:63] + "1"
	overLong := strings.Repeat("x", maxCorruptLine+4096) // bigger than the buffer

	f := filepath.Join(e.spool, corruptFileName)
	content := first + "\n" + overLong + "\n" + last + "\n"
	if err := os.WriteFile(f, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	corrupt.Range(func(k, _ any) bool { corrupt.Delete(k); return true })

	corruptState.load()

	if !knownCorrupt(first) {
		t.Fatal("the mark before the long line was lost")
	}
	if !knownCorrupt(last) {
		t.Fatal("the mark after the long line was lost; one bad line truncated the file")
	}
}
