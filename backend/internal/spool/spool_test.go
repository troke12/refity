package spool_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"refity/backend/internal/driver/sftp/sftptest"
	"refity/backend/internal/spool"
)

func randomFile(t *testing.T, n int) (string, []byte) {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "staged")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p, b
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

func spoolFiles(t *testing.T, dir string) []string {
	t.Helper()
	ents, _ := os.ReadDir(dir)
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func TestRestartResumesExactlyOnce(t *testing.T) {
	srv := sftptest.New(t)
	srv.BytesPerSec = 256 << 10 // 512 KiB takes ~2 s, so the first instance is stopped mid-upload
	d := srv.Pool(t, 2, 0, 0)
	dir := t.TempDir()
	remote := "registry/g/r/blobs/sha256:aaa"
	upload := func(ctx context.Context, local string, job spool.Job) error {
		return d.UploadFile(ctx, local, job.Remote, job.ID, true)
	}

	sp1, err := spool.Open(spool.Options{Dir: dir, Workers: 1, Upload: upload, BackoffBase: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	src, data := randomFile(t, 512<<10)
	if err := sp1.Add(src, spool.Job{Remote: remote, Digest: "sha256:aaa"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatal("Add should take ownership of the staged file")
	}
	// Wait until the temp file is being written remotely, then "crash".
	waitFor(t, 5*time.Second, "remote temp file", func() bool {
		m, _ := filepath.Glob(srv.File(remote) + ".uploading-*")
		if len(m) == 0 {
			return false
		}
		st, err := os.Stat(m[0])
		return err == nil && st.Size() > 0
	})
	sp1.Close()
	if _, err := os.Stat(srv.File(remote)); !os.IsNotExist(err) {
		t.Fatal("final remote path must not exist after an interrupted upload")
	}

	var uploads atomic.Int32
	sp2, err := spool.Open(spool.Options{Dir: dir, Workers: 2, BackoffBase: 10 * time.Millisecond,
		Upload: func(ctx context.Context, local string, job spool.Job) error {
			uploads.Add(1)
			return upload(ctx, local, job)
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer sp2.Close()
	waitFor(t, 15*time.Second, "resumed upload", func() bool { return sp2.PendingBytes() == 0 })

	got, err := os.ReadFile(srv.File(remote))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("remote content wrong after resume: err=%v len=%d", err, len(got))
	}
	if n := srv.Renames(remote); n != 1 {
		t.Fatalf("expected exactly one completed upload, got %d renames", n)
	}
	if uploads.Load() != 1 {
		t.Fatalf("expected the resumed job to run once, ran %d times", uploads.Load())
	}
	if left := spoolFiles(t, dir); len(left) != 0 {
		t.Fatalf("spool not empty after success: %v", left)
	}
	if m, _ := filepath.Glob(srv.File(remote) + ".uploading-*"); len(m) != 0 {
		t.Fatalf("temp files left: %v", m)
	}
}

func TestRetriesTransientFailures(t *testing.T) {
	srv := sftptest.New(t)
	var failures atomic.Int32
	srv.FailOpen = func(path string, flags int) error {
		if failures.Add(1) <= 3 {
			return errors.New("injected transient failure")
		}
		return nil
	}
	d := srv.Pool(t, 2, 0, 0)
	var attempts atomic.Int32
	sp, err := spool.Open(spool.Options{Dir: t.TempDir(), Workers: 1, BackoffBase: 10 * time.Millisecond, BackoffMax: 40 * time.Millisecond,
		Upload: func(ctx context.Context, local string, job spool.Job) error {
			attempts.Add(1)
			return d.UploadFile(ctx, local, job.Remote, job.ID, true)
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	src, data := randomFile(t, 64<<10)
	if err := sp.Add(src, spool.Job{Remote: "x/blob"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "upload after retries", func() bool { return sp.PendingBytes() == 0 })
	if attempts.Load() != 4 {
		t.Fatalf("expected 3 failures then success (4 attempts), got %d", attempts.Load())
	}
	if got, _ := os.ReadFile(srv.File("x/blob")); !bytes.Equal(got, data) {
		t.Fatal("content mismatch")
	}
}

func TestPersistentFailureKeepsData(t *testing.T) {
	dir := t.TempDir()
	var attempts atomic.Int32
	sp, err := spool.Open(spool.Options{Dir: dir, Workers: 1, BackoffBase: 5 * time.Millisecond, BackoffMax: 20 * time.Millisecond,
		Upload: func(ctx context.Context, local string, job spool.Job) error {
			attempts.Add(1)
			return errors.New("remote is down")
		}})
	if err != nil {
		t.Fatal(err)
	}
	src, data := randomFile(t, 4096)
	if err := sp.Add(src, spool.Job{Remote: "x/blob"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "several attempts", func() bool { return attempts.Load() >= 5 })
	sp.Close()

	var jobs, datas int
	for _, n := range spoolFiles(t, dir) {
		switch {
		case strings.HasSuffix(n, ".job"):
			jobs++
		case strings.HasSuffix(n, ".data"):
			datas++
			if got, _ := os.ReadFile(filepath.Join(dir, n)); !bytes.Equal(got, data) {
				t.Fatal("spooled data changed")
			}
		}
	}
	if jobs != 1 || datas != 1 {
		t.Fatalf("job and data must survive persistent failure, got jobs=%d data=%d", jobs, datas)
	}
	if sp.PendingBytes() != 4096 {
		t.Fatalf("pending bytes = %d", sp.PendingBytes())
	}
}

func TestBudgetAndDedup(t *testing.T) {
	block := make(chan struct{})
	sp, err := spool.Open(spool.Options{Dir: t.TempDir(), Workers: 1, MaxBytes: 10000,
		Upload: func(ctx context.Context, local string, job spool.Job) error {
			select {
			case <-block:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	defer close(block)

	a, _ := randomFile(t, 6000)
	if err := sp.Add(a, spool.Job{Remote: "blob/a"}); err != nil {
		t.Fatal(err)
	}
	b, _ := randomFile(t, 6000)
	if err := sp.Add(b, spool.Job{Remote: "blob/b"}); !errors.Is(err, spool.ErrFull) {
		t.Fatalf("expected ErrFull, got %v", err)
	}
	if _, err := os.Stat(b); err != nil {
		t.Fatal("rejected file must stay with the caller")
	}
	// Same immutable path again: deduplicated, not counted twice.
	a2, _ := randomFile(t, 6000)
	if err := sp.Add(a2, spool.Job{Remote: "blob/a"}); err != nil {
		t.Fatal(err)
	}
	if sp.PendingBytes() != 6000 {
		t.Fatalf("pending = %d, want 6000", sp.PendingBytes())
	}
	// Mutable path (tag): the newest version is what Open returns.
	t1 := filepath.Join(t.TempDir(), "t1")
	_ = os.WriteFile(t1, []byte("v1"), 0o644)
	t2 := filepath.Join(t.TempDir(), "t2")
	_ = os.WriteFile(t2, []byte("v2"), 0o644)
	if err := sp.Add(t1, spool.Job{Remote: "m/latest", Mutable: true}); err != nil {
		t.Fatal(err)
	}
	if err := sp.Add(t2, spool.Job{Remote: "m/latest", Mutable: true}); err != nil {
		t.Fatal(err)
	}
	f, _, ok := sp.Open("m/latest")
	if !ok {
		t.Fatal("tag should be pending")
	}
	got := make([]byte, 2)
	_, _ = f.Read(got)
	f.Close()
	if string(got) != "v2" {
		t.Fatalf("Open returned %q, want newest v2", got)
	}
	if n := len(sp.Pending("m/")); n != 1 {
		t.Fatalf("superseded tag version still listed: %d", n)
	}
}

// A SPOOL_DIR pointed at the wrong place must not destroy files the spool did not create.
func TestOpenOnlySweepsOwnFiles(t *testing.T) {
	dir := t.TempDir()
	foreign := map[string]string{
		"refity.db":            "database",
		"notes.txt":            "keep me",
		"backup.data":          "looks like ours but is not",
		"something.job":        "{}",
		"x.incoming.tmp":       "not our id shape",
		"20240101T000000.data": "malformed id",
	}
	for name, content := range foreign {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Debris the spool does own: orphaned data, temp job, incoming temp.
	own := []string{
		"20240101T000000-aaaaaaaaaaaaaaaaaaaaaaaa.data",
		"20240101T000000-bbbbbbbbbbbbbbbbbbbbbbbb.job.tmp",
		"20240101T000000-cccccccccccccccccccccccc.incoming.tmp",
	}
	for _, name := range own {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sp, err := spool.Open(spool.Options{Dir: dir, Upload: func(context.Context, string, spool.Job) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	sp.Close()
	for name, content := range foreign {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(got) != content {
			t.Errorf("foreign file %s was touched (err %v)", name, err)
		}
	}
	for _, name := range own {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("spool debris %s was not swept", name)
		}
	}
}

// Permanently failing jobs are requeued with a not-before time instead of pinning workers in a sleep.
func TestFailingJobsDoNotStarveQueue(t *testing.T) {
	var goodDone atomic.Bool
	sp, err := spool.Open(spool.Options{Dir: t.TempDir(), Workers: 2, BackoffBase: time.Minute, BackoffMax: time.Minute,
		Upload: func(ctx context.Context, local string, job spool.Job) error {
			if strings.HasPrefix(job.Remote, "bad/") {
				return errors.New("permanent failure")
			}
			goodDone.Store(true)
			return nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	for _, r := range []string{"bad/1", "bad/2"} {
		src, _ := randomFile(t, 100)
		if err := sp.Add(src, spool.Job{Remote: r}); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(100 * time.Millisecond) // both workers have failed once on the bad jobs
	src, _ := randomFile(t, 100)
	if err := sp.Add(src, spool.Job{Remote: "good/1"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "good job despite failing ones", goodDone.Load)
	// The upload callback returns before the spool records the success; wait for that bookkeeping.
	waitFor(t, 2*time.Second, "success recorded", func() bool {
		st := sp.Stats()
		return st.Pending == 2 && !st.LastSuccessAt.IsZero()
	})
	st := sp.Stats()
	if st.Pending != 2 || st.LastError == "" || st.LastErrorAt.IsZero() || st.LastSuccessAt.IsZero() {
		t.Fatalf("unexpected stats: %+v", st)
	}
	if st.OldestAge <= 0 {
		t.Fatalf("oldest age should be positive: %+v", st)
	}
}

// Forget drops a pending job (and cancels its in-flight upload) so a direct write wins.
func TestForgetCancelsInFlightUpload(t *testing.T) {
	started := make(chan struct{})
	var uploads atomic.Int32
	sp, err := spool.Open(spool.Options{Dir: t.TempDir(), Workers: 1, BackoffBase: 10 * time.Millisecond,
		Upload: func(ctx context.Context, local string, job spool.Job) error {
			uploads.Add(1)
			close(started)
			<-ctx.Done()
			return ctx.Err()
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	src, _ := randomFile(t, 100)
	if err := sp.Add(src, spool.Job{Remote: "m/latest", Mutable: true}); err != nil {
		t.Fatal(err)
	}
	<-started
	select {
	case <-sp.Forget("m/latest"):
	case <-time.After(2 * time.Second):
		t.Fatal("Forget did not stop the in-flight upload")
	}
	if _, ok := sp.Lookup("m/latest"); ok || sp.PendingBytes() != 0 {
		t.Fatal("forgotten job still pending")
	}
	time.Sleep(50 * time.Millisecond)
	if uploads.Load() != 1 {
		t.Fatalf("forgotten job was retried (%d uploads)", uploads.Load())
	}
}

// A superseded version that was already waiting on the caller's path lock must be skipped once it gets
// the lock, even when its upload function ignores cancellation.
func TestSupersededJobSkippedAfterLock(t *testing.T) {
	var pathMu sync.Mutex
	var mu sync.Mutex
	var uploaded []string
	sp, err := spool.Open(spool.Options{Dir: t.TempDir(), Workers: 2,
		Lock: func(string) func() { pathMu.Lock(); return pathMu.Unlock },
		Upload: func(_ context.Context, local string, job spool.Job) error {
			b, _ := os.ReadFile(local)
			mu.Lock()
			uploaded = append(uploaded, string(b))
			mu.Unlock()
			return nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	pathMu.Lock()
	if err := sp.AddBytes([]byte("v1"), spool.Job{Remote: "m/latest", Mutable: true}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // a worker holds v1 and waits on the path lock
	if err := sp.AddBytes([]byte("v2"), spool.Job{Remote: "m/latest", Mutable: true}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	pathMu.Unlock()
	waitFor(t, 2*time.Second, "drain", func() bool { return sp.PendingBytes() == 0 })
	mu.Lock()
	defer mu.Unlock()
	if len(uploaded) != 1 || uploaded[0] != "v2" {
		t.Fatalf("uploaded %v; only v2 may be uploaded", uploaded)
	}
}

// After a crash left two versions of one tag on disk, restart keeps the one added last, even if this
// host's clock stepped backwards between the two pushes.
func TestRestartKeepsLastAddedVersionDespiteClockStep(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	clock := func() time.Time { return now }
	block := func(context.Context, string, spool.Job) error { return errors.New("remote down") }
	sp, err := spool.Open(spool.Options{Dir: dir, Workers: 1, BackoffBase: time.Hour, Upload: block, Now: func() time.Time { return clock() }})
	if err != nil {
		t.Fatal(err)
	}
	if err := sp.AddBytes([]byte("v1"), spool.Job{Remote: "m/latest", Mutable: true}); err != nil {
		t.Fatal(err)
	}
	// Keep a copy of v1's files: they stand in for a crash before the superseded version was deleted.
	saved := map[string][]byte{}
	for _, n := range spoolFiles(t, dir) {
		b, _ := os.ReadFile(filepath.Join(dir, n))
		saved[n] = b
	}
	now = now.Add(-time.Minute) // clock steps back
	if err := sp.AddBytes([]byte("v2"), spool.Job{Remote: "m/latest", Mutable: true}); err != nil {
		t.Fatal(err)
	}
	sp.Close()
	for n, b := range saved {
		_ = os.WriteFile(filepath.Join(dir, n), b, 0o644)
	}
	sp, err = spool.Open(spool.Options{Dir: dir, Workers: 1, BackoffBase: time.Hour, Upload: block})
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	f, _, ok := sp.Open("m/latest")
	if !ok {
		t.Fatal("tag lost on restart")
	}
	got, _ := io.ReadAll(f)
	f.Close()
	if string(got) != "v2" {
		t.Fatalf("restart kept %q, want the last added version v2", got)
	}
}
