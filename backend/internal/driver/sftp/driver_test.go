package sftp_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"refity/backend/internal/driver/sftp/sftptest"
)

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func writeLocal(t *testing.T, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "blob")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func assertNoTemps(t *testing.T, dir string) {
	t.Helper()
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && strings.Contains(info.Name(), ".uploading-") {
			t.Errorf("leftover temp file %s", p)
		}
		return nil
	})
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestUploadFile(t *testing.T) {
	data := randomBytes(t, 1<<20)
	cases := []struct {
		name      string
		threshold int64
		// failOpen rejects some write-opens on the server.
		failOpen func(path string, flags int) error
		// minWriters is the minimum number of distinct write-opens expected (parallel => several).
		minWriters int
	}{
		{name: "single stream", threshold: 0, minWriters: 1},
		{name: "parallel", threshold: 64 << 10, minWriters: 4},
		{
			// Every range writer (opened without O_TRUNC) fails: parallel gives up, single stream wins.
			name:      "parallel falls back to single stream",
			threshold: 64 << 10,
			failOpen: func(path string, flags int) error {
				if flags&os.O_TRUNC == 0 {
					return errors.New("injected range failure")
				}
				return nil
			},
			minWriters: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := sftptest.New(t)
			var writers atomic.Int64
			srv.FailOpen = func(path string, flags int) error {
				writers.Add(1)
				if tc.failOpen != nil {
					return tc.failOpen(path, flags)
				}
				return nil
			}
			d := srv.Pool(t, 4, 0, tc.threshold)
			local := writeLocal(t, data)
			remote := "registry/g/r/blobs/sha256:abc"
			if err := d.UploadFile(context.Background(), local, remote, "tok", true); err != nil {
				t.Fatalf("UploadFile: %v", err)
			}
			got, err := os.ReadFile(srv.File(remote))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, data) {
				t.Fatalf("remote content differs (len %d vs %d)", len(got), len(data))
			}
			if int(writers.Load()) < tc.minWriters {
				t.Fatalf("expected at least %d write-opens, got %d", tc.minWriters, writers.Load())
			}
			if n := srv.Renames(remote); n != 1 {
				t.Fatalf("expected exactly one rename into place, got %d", n)
			}
			assertNoTemps(t, srv.Root)

			// Same size already there: skipped for content-addressed paths, rewritten otherwise.
			if err := d.UploadFile(context.Background(), local, remote, "tok2", true); err != nil {
				t.Fatal(err)
			}
			if n := srv.Renames(remote); n != 1 {
				t.Fatalf("existing blob should be skipped, renames=%d", n)
			}
			if err := d.UploadFile(context.Background(), local, remote, "tok3", false); err != nil {
				t.Fatal(err)
			}
			if n := srv.Renames(remote); n != 2 {
				t.Fatalf("without skipIfSameSize the file must be rewritten, renames=%d", n)
			}
		})
	}
}

func TestUploadFileParallelBeatsSingleStreamOnThrottledLink(t *testing.T) {
	data := randomBytes(t, 512<<10)
	elapsed := func(threshold int64) time.Duration {
		srv := sftptest.New(t)
		srv.BytesPerSec = 1 << 20
		d := srv.Pool(t, 4, 0, threshold)
		start := time.Now()
		if err := d.UploadFile(context.Background(), writeLocal(t, data), "x/blob", "t", false); err != nil {
			t.Fatal(err)
		}
		return time.Since(start)
	}
	single, parallel := elapsed(0), elapsed(64<<10)
	t.Logf("single=%v parallel=%v", single, parallel)
	if parallel > single*3/4 {
		t.Fatalf("parallel upload (%v) not meaningfully faster than single stream (%v)", parallel, single)
	}
}

func TestUploadFileSurvivesStalledConnection(t *testing.T) {
	for _, threshold := range []int64{0, 64 << 10} {
		name := "single"
		if threshold > 0 {
			name = "parallel"
		}
		t.Run(name, func(t *testing.T) {
			srv := sftptest.New(t)
			srv.StallOnceAfterWrite(96 << 10)
			const stall = 300 * time.Millisecond
			d := srv.Pool(t, 4, stall, threshold)
			data := randomBytes(t, 512<<10)
			remote := "registry/g/r/blobs/sha256:def"
			start := time.Now()
			// Single-stream mode has no internal retry: the caller retries, as the spool and sync paths do.
			var err error
			for attempt := 0; attempt < 3; attempt++ {
				if err = d.UploadFile(context.Background(), writeLocal(t, data), remote, "tok", true); err == nil {
					break
				}
			}
			if err != nil {
				t.Fatalf("upload never succeeded: %v", err)
			}
			if el := time.Since(start); el > stall+3*time.Second {
				t.Fatalf("took %v, expected roughly the stall timeout", el)
			}
			if srv.Stalls() != 1 {
				t.Fatalf("expected the injected stall to fire once, got %d", srv.Stalls())
			}
			got, _ := os.ReadFile(srv.File(remote))
			if !bytes.Equal(got, data) {
				t.Fatal("remote content differs after stall recovery")
			}
			assertNoTemps(t, srv.Root)
			// The stalled client is discarded and replaced.
			waitFor(t, 5*time.Second, "pool refill", func() bool { return d.Pool.Alive() == 4 && srv.Dials() > 4 })
		})
	}
}

func TestReaderStreamsFromOffsetAndAbortsOnStall(t *testing.T) {
	srv := sftptest.New(t)
	data := randomBytes(t, 1<<20)
	if err := os.MkdirAll(filepath.Dir(srv.File("f/blob")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(srv.File("f/blob"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	d := srv.Pool(t, 2, 300*time.Millisecond, 0)

	rc, err := d.Reader(context.Background(), "f/blob", 1000)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || !bytes.Equal(got, data[1000:]) {
		t.Fatalf("offset read mismatch: err=%v len=%d", err, len(got))
	}

	srv.StallOnceAfterRead(200 << 10)
	rc, err = d.Reader(context.Background(), "f/blob", 0)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = io.ReadAll(rc)
	rc.Close()
	if err == nil {
		t.Fatal("expected the stalled read to fail")
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("stalled read took %v to abort", el)
	}
	if !strings.Contains(err.Error(), "connection lost") {
		t.Fatalf("stall error should be classified as connection lost: %v", err)
	}
}
