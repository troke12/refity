package registry

import (
	"bytes"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// A re-pushed tag must reach the remote whatever the clocks do: a Storage Box clock ahead of ours (with
// setstat ignored), or this host's clock stepping backwards while the first version is still uploading.
// The two versions overlap on purpose and differ in length, so neither the size shortcut nor a lucky
// ordering can make this pass by accident.
//
// This is the end-to-end half of the guarantee. The queue order itself is Seq, which only matters when
// the spool is recovered from disk, so the part that pins Seq lives in the spool package
// (TestRestartKeepsLastAddedVersionDespiteClockStep); this test shows the whole request path agrees.
func TestTagRepushSurvivesClockSkew(t *testing.T) {
	cases := []struct {
		name  string
		setup func(e *testEnv)
		// between the two puts, while v1 is still pending.
		between func(e *testEnv)
	}{
		{
			name: "server clock ahead, setstat ignored",
			setup: func(e *testEnv) {
				e.srv.IgnoreSetstat = true
				e.srv.ClockAhead = 5 * time.Second
			},
			between: func(e *testEnv) {},
		},
		{
			name:  "host clock steps backwards",
			setup: func(e *testEnv) {},
			between: func(e *testEnv) {
				back := time.Now().Add(-time.Minute)
				spoolNow = func() time.Time { return back }
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			oldNow := spoolNow
			t.Cleanup(func() { spoolNow = oldNow })
			e := newEnv(t, envOpts{bytesPerSec: 32 << 10})
			tc.setup(e)
			// v1 is large enough to still be uploading when v2 is spooled, so the two genuinely overlap.
			v1 := manifestWithPadding("1", 256<<10)
			v2 := manifestWithPadding("2", 300<<10)
			if len(v1) == len(v2) {
				t.Fatal("the two manifests must differ in length for this test to mean anything")
			}
			if code := e.putManifest("grp/app", "latest", v1); code != http.StatusCreated {
				t.Fatalf("PUT v1: %d", code)
			}
			tc.between(e)
			if code := e.putManifest("grp/app", "latest", v2); code != http.StatusCreated {
				t.Fatalf("PUT v2: %d", code)
			}
			e.waitDrained(20 * time.Second)
			e.assertRemoteContent(e.srv.File("registry/grp/app/manifests/latest"), v2)
		})
	}
}

// HEAD of a blob that is not available locally while storage is down answers 404 (push-safe: the client
// uploads it into the spool), GET answers 503, and the push is accepted and later uploaded.
func TestBlobHeadIs404DuringOutage(t *testing.T) {
	e := newEnv(t, envOpts{acquire: 2 * time.Second})
	data, digest := randomBlob(t, 3000)
	placeRemote(t, e, "grp/app", data, digest) // exists remotely, unknown locally
	e.srv.SetRefuse(true)
	e.srv.DropAll()

	resp := e.do(http.MethodHead, "/v2/grp/app/blobs/"+digest, nil, nil)
	readAll(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("HEAD blob during outage: %d, want 404", resp.StatusCode)
	}
	resp = e.do(http.MethodGet, "/v2/grp/app/blobs/"+digest, nil, nil)
	readAll(t, resp)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("GET blob during outage: %d, want 503", resp.StatusCode)
	}
	fresh, freshDigest := randomBlob(t, 2500)
	if _, code := e.pushBlob("grp/app", fresh, freshDigest); code != http.StatusCreated {
		t.Fatalf("push during outage: %d", code)
	}
	e.srv.SetRefuse(false)
	e.waitDrained(15 * time.Second)
	e.assertRemoteContent(e.remoteBlob("grp/app", freshDigest), fresh)
	if _, err := os.Stat(e.remoteBlob("grp/app", digest)); err != nil {
		t.Fatal("pre-existing remote blob disappeared")
	}
	if got := readAll(t, e.do(http.MethodGet, "/v2/grp/app/blobs/"+digest, nil, nil)); !bytes.Equal(got, data) {
		t.Fatal("remote blob not served after recovery")
	}
}

// manifestWithPadding builds a manifest of roughly the requested size, tagged so two calls are
// distinguishable and never equal in length.
func manifestWithPadding(version string, size int) []byte {
	head := `{"schemaVersion":2,"layers":[],"annotations":{"v":"` + version + `","pad":"`
	tail := `"}}`
	return []byte(head + strings.Repeat("x", size-len(head)-len(tail)) + tail)
}
