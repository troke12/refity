package registry

import (
	"bytes"
	"io"
	"net/http"
	"testing"
	"time"
)

// A pull's cache fill keeps an open handle to the file it started reading, so a push that renames a
// correct copy over that path in the meantime leaves the fill hashing bytes that are already unlinked.
// The heal must not act on that stale verdict: deleting by path would destroy the good copy the client
// was just told had been stored, leaving nothing anywhere.
//
// The corrupt and correct blobs differ in size on purpose: a same-size re-push is skipped by the
// "same size means done" shortcut, so the push would not replace anything and the race would never
// happen.
func TestHealDoesNotDeleteBlobReplacedDuringTheFill(t *testing.T) {
	e := newEnv(t, envOpts{readCacheBytes: 64 << 20})
	repo := "grp/app"
	good, digest := randomBlob(t, 512<<10)
	bad, _ := randomBlob(t, 300<<10) // different length, so the push is a real write
	placeRemote(t, e, repo, bad, digest)
	remotePath := e.remoteBlob(repo, digest)

	// Slow the read so the push lands while the fill is still hashing the old copy.
	e.srv.SetReadDelay(60 * time.Millisecond)

	pullDone := make(chan struct{})
	go func() {
		defer close(pullDone)
		resp := e.do(http.MethodGet, "/v2/"+repo+"/blobs/"+digest, nil, nil)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	// While that pull runs, push the correct bytes for the same digest.
	if _, code := e.pushBlob(repo, good, digest); code != http.StatusCreated {
		t.Fatalf("re-push: %d", code)
	}
	e.waitDrained(15 * time.Second)

	// Precondition: the push really replaced the corrupt copy. Without this the test would pass even
	// if the heal deleted the correct blob, because nothing correct was ever stored.
	e.assertRemoteContent(remotePath, good)

	<-pullDone
	waitHeal(t, remotePath)

	// The correct copy must have survived: a stale-verdict heal would have removed it.
	e.assertRemoteContent(remotePath, good)
	if got := readAll(t, e.do(http.MethodGet, "/v2/"+repo+"/blobs/"+digest, nil, nil)); !bytes.Equal(got, good) {
		t.Fatal("pull after the concurrent push does not return the correct bytes")
	}
}

// waitHeal gives a heal for remotePath time to run and finish. It is deliberately tolerant: if the
// pull finished so fast that the heal already completed, the assertions after it are what matter.
func waitHeal(t *testing.T, remotePath string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for healingHas(remotePath) {
		if time.Now().After(deadline) {
			t.Fatal("heal did not finish within 30s")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Let a late heal goroutine that has not registered yet register and run.
	time.Sleep(500 * time.Millisecond)
}

func healingHas(remotePath string) bool {
	_, busy := healing.Load(remotePath)
	return busy
}
