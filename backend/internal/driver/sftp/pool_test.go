package sftp_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	drv "refity/backend/internal/driver/sftp"
	"refity/backend/internal/driver/sftp/sftptest"
)

func putRemote(t *testing.T, srv *sftptest.Server, path string, data []byte, mtime time.Time) {
	t.Helper()
	p := srv.File(path)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if !mtime.IsZero() {
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
}

// After an outage longer than any fixed retry budget the pool must recover on its own, and calls made
// during the outage must fail fast instead of waiting forever for a connection.
func TestPoolRecoversAfterLongOutageAndFailsFast(t *testing.T) {
	srv := sftptest.New(t)
	putRemote(t, srv, "x/file", []byte("hello"), time.Time{})
	d := srv.PoolWith(t, drv.PoolOptions{
		Size:              2,
		AcquireTimeout:    200 * time.Millisecond,
		RefillBackoffBase: 5 * time.Millisecond,
		RefillBackoffMax:  20 * time.Millisecond,
	})
	ctx := context.Background()

	srv.SetRefuse(true)
	srv.DropAll()
	before := srv.DialAttempts()
	for i := 0; i < 3; i++ {
		start := time.Now()
		_, err := d.Stat(ctx, "x/file")
		if err == nil {
			t.Fatal("Stat should fail while the remote is down")
		}
		if el := time.Since(start); el > 2*time.Second {
			t.Fatalf("call during outage took %v; must fail fast", el)
		}
	}
	// Well past the old "10 attempts per slot, then give up" refill budget.
	waitFor(t, 10*time.Second, "many refill attempts", func() bool { return srv.DialAttempts()-before >= 40 })

	srv.SetRefuse(false)
	waitFor(t, 5*time.Second, "pool recovery", func() bool { return d.Pool.Alive() == 2 })
	if _, err := d.Stat(ctx, "x/file"); err != nil {
		t.Fatalf("Stat after recovery: %v", err)
	}
}

// Repeated authentication failures open a circuit breaker: dials pause instead of piling up failed
// logins, then resume on their own.
func TestPoolAuthCircuitBreaker(t *testing.T) {
	srv := sftptest.New(t)
	const pause = 600 * time.Millisecond
	d := srv.PoolWith(t, drv.PoolOptions{
		Size:              1,
		AcquireTimeout:    100 * time.Millisecond,
		RefillBackoffBase: 5 * time.Millisecond,
		RefillBackoffMax:  10 * time.Millisecond,
		AuthFailLimit:     3,
		AuthPause:         pause,
	})
	srv.SetAuthFail(true)
	srv.DropAll()
	before := srv.DialAttempts()
	if _, err := d.Stat(context.Background(), "x"); err == nil {
		t.Fatal("expected failure while authentication is rejected")
	}
	waitFor(t, 3*time.Second, "three auth failures", func() bool { return srv.DialAttempts()-before >= 3 })
	tripped := srv.DialAttempts()
	time.Sleep(pause / 2)
	if n := srv.DialAttempts(); n != tripped {
		t.Fatalf("dialing continued during the breaker pause: %d -> %d attempts", tripped, n)
	}
	waitFor(t, 3*pause, "dials to resume after the pause", func() bool { return srv.DialAttempts() > tripped })

	srv.SetAuthFail(false)
	waitFor(t, 5*time.Second, "pool recovery", func() bool { return d.Pool.Alive() == 1 })
}

// Downloads may hold at most poolSize-1 connections, so metadata calls always find one free.
func TestReaderSlotsLeaveAConnectionFree(t *testing.T) {
	srv := sftptest.New(t)
	putRemote(t, srv, "x/a", make([]byte, 1<<20), time.Time{})
	putRemote(t, srv, "x/b", make([]byte, 1<<20), time.Time{})
	d := srv.PoolWith(t, drv.PoolOptions{Size: 2, AcquireTimeout: time.Second})
	ctx := context.Background()

	ra, err := d.Reader(ctx, "x/a", 0)
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	if rb, err := d.Reader(waitCtx, "x/b", 0); err == nil {
		rb.Close()
		t.Fatal("second concurrent download should wait for a read slot")
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unexpected error: %v", err)
	}
	start := time.Now()
	if _, err := d.Stat(ctx, "x/b"); err != nil || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("metadata call while a download runs: err=%v after %v", err, time.Since(start))
	}
	ra.Close()
	rb, err := d.Reader(ctx, "x/b", 0)
	if err != nil {
		t.Fatalf("download after the slot was released: %v", err)
	}
	rb.Close()
}

func TestSweepStaleTemps(t *testing.T) {
	srv := sftptest.New(t)
	old := time.Now().Add(-48 * time.Hour)
	putRemote(t, srv, "registry/g/r/blobs/sha256:aa.uploading-dead", []byte("x"), old)
	putRemote(t, srv, "registry/g/r/blobs/sha256:bb.uploading-live", []byte("x"), time.Time{})
	putRemote(t, srv, "registry/g/r/blobs/sha256:cc", []byte("x"), old)
	d := srv.Pool(t, 2, 0, 0)
	n, err := d.SweepStaleTemps(context.Background(), "registry", 24*time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("sweep removed %d (err %v), want 1", n, err)
	}
	for p, want := range map[string]bool{
		"registry/g/r/blobs/sha256:aa.uploading-dead": false,
		"registry/g/r/blobs/sha256:bb.uploading-live": true,
		"registry/g/r/blobs/sha256:cc":                true,
	} {
		_, err := os.Stat(srv.File(p))
		if (err == nil) != want {
			t.Errorf("%s exists=%v, want %v", p, err == nil, want)
		}
	}
}

// Uploads and downloads share one budget of poolSize-1 long-running connections, so a metadata call is
// served promptly even when both run at full tilt.
func TestUploadsAndDownloadsShareLongOpBudget(t *testing.T) {
	srv := sftptest.New(t)
	srv.BytesPerSec = 512 << 10
	putRemote(t, srv, "x/a", make([]byte, 1<<20), time.Time{})
	putRemote(t, srv, "x/b", make([]byte, 1<<20), time.Time{})
	d := srv.PoolWith(t, drv.PoolOptions{Size: 4, ParallelThreshold: 64 << 10, AcquireTimeout: 10 * time.Second})
	ctx := context.Background()
	local := writeLocal(t, make([]byte, 2<<20))
	done := make(chan error, 3)
	go func() { done <- d.UploadFile(ctx, local, "x/up", "tok", false) }()
	for _, p := range []string{"x/a", "x/b"} {
		go func(p string) {
			r, err := d.Reader(ctx, p, 0)
			if err == nil {
				_, err = io.Copy(io.Discard, r)
				r.Close()
			}
			done <- err
		}(p)
	}
	time.Sleep(300 * time.Millisecond) // upload parts and downloads are all running or queued
	start := time.Now()
	if _, err := d.Stat(ctx, "x/a"); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el > 800*time.Millisecond {
		t.Fatalf("Stat took %v while uploads and downloads ran; one connection must stay free", el)
	}
	for i := 0; i < 3; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

// After an auth pause exactly one probe dial goes out; a failed probe extends the pause with backoff,
// so the pool never produces a burst of failed logins.
func TestAuthPauseLetsOneProbeThrough(t *testing.T) {
	srv := sftptest.New(t)
	srv.HandshakeDelay = 100 * time.Millisecond // refill loops waking together would overlap their dials
	const pause = 400 * time.Millisecond
	d := srv.PoolWith(t, drv.PoolOptions{
		Size:              3,
		AcquireTimeout:    100 * time.Millisecond,
		RefillBackoffBase: 5 * time.Millisecond,
		RefillBackoffMax:  10 * time.Millisecond,
		AuthFailLimit:     3,
		AuthPause:         pause,
		AuthPauseMax:      4 * pause,
	})
	srv.SetAuthFail(true)
	srv.DropAll()
	before := srv.DialAttempts()
	_, _ = d.Stat(context.Background(), "x") // discovers the dead connections, starts refills
	waitFor(t, 3*time.Second, "breaker trip", func() bool { return srv.DialAttempts()-before >= 3 })
	tripped := srv.DialAttempts()
	time.Sleep(pause + 3*pause/4) // the first pause is over; one probe failed; the pause is now 2x
	if n := srv.DialAttempts() - tripped; n != 1 {
		t.Fatalf("after the pause %d dials went out, want exactly one probe", n)
	}
	time.Sleep(pause / 2) // still inside the doubled pause
	if n := srv.DialAttempts() - tripped; n != 1 {
		t.Fatalf("dials during the extended pause: %d, want still 1", n)
	}
	srv.SetAuthFail(false)
	waitFor(t, 5*time.Second, "recovery after a successful probe", func() bool { return d.Pool.Alive() == 3 })
}

// The health probe at checkout honours the acquire timeout even when a connection hangs silently. The
// caller giving up does not condemn the connection: it is replaced only once its probe actually fails
// (here: the stall watchdog fires), and then exactly once.
func TestCheckoutProbeHonoursAcquireTimeout(t *testing.T) {
	srv := sftptest.New(t)
	putRemote(t, srv, "x/f", []byte("hi"), time.Time{})
	const stall = 800 * time.Millisecond
	d := srv.PoolWith(t, drv.PoolOptions{
		Size:              1,
		AcquireTimeout:    300 * time.Millisecond,
		StallTimeout:      stall,
		RefillBackoffBase: 5 * time.Millisecond,
		RefillBackoffMax:  10 * time.Millisecond,
	})
	dials := srv.DialAttempts()
	srv.StallOnceAfterWrite(1) // the pooled connection goes silent on its next request
	errc := make(chan error, 1)
	start := time.Now()
	go func() { _, err := d.Stat(context.Background(), "x/f"); errc <- err }()
	select {
	case <-errc:
		if el := time.Since(start); el > 700*time.Millisecond {
			t.Fatalf("checkout took %v", el)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("checkout hung on a silent connection")
	}
	time.Sleep(200 * time.Millisecond) // a wrongful replacement would have dialed by now; the watchdog has not fired
	if n := srv.DialAttempts(); n != dials {
		t.Fatalf("connection replaced before its probe failed: %d -> %d dials", dials, n)
	}
	waitFor(t, 5*time.Second, "replacement connection", func() bool {
		_, err := d.Stat(context.Background(), "x/f")
		return err == nil
	})
	if n := srv.DialAttempts() - dials; n != 1 {
		t.Fatalf("dead connection replaced with %d dials, want 1", n)
	}
}

// A client returned after Close must be closed, not parked where nobody drains it, and a closed pool must
// not dial again to replace what it was handed. park() covers the race three ways (stop already signalled,
// stop while sending, stop just after sending), so this exercises the whole function rather than any one
// of those checks: it closes the pool while a reader is open, then returns that reader.
func TestPutClientAfterCloseDoesNotLeak(t *testing.T) {
	for i := 0; i < 20; i++ {
		srv := sftptest.New(t)
		putRemote(t, srv, "x/f", []byte("hi"), time.Time{})
		d := srv.PoolWith(t, drv.PoolOptions{Size: 2})
		before := srv.DialAttempts()
		r, err := d.Reader(context.Background(), "x/f", 0)
		if err != nil {
			t.Fatal(err)
		}
		d.Pool.Close()
		r.Close()
		waitFor(t, 2*time.Second, "all connections closed", func() bool { return srv.OpenConns() == 0 })
		// Closing a pool that then receives a late reader must not trigger a replacement dial: the pool
		// is closed, so there is nothing left to fill.
		time.Sleep(50 * time.Millisecond)
		if n := srv.DialAttempts(); n != before {
			t.Fatalf("iteration %d: a closed pool dialled again (%d -> %d)", i, before, n)
		}
	}
}

// A caller whose context ends while the checkout probe is still running must not cost the pool a
// healthy connection: no close, no replacement dial (every dial is a login, possibly a rejected one).
func TestCheckoutProbeKeepsHealthyConnectionWhenCallerGivesUp(t *testing.T) {
	srv := sftptest.New(t)
	putRemote(t, srv, "x/f", []byte("hi"), time.Time{})
	d := srv.PoolWith(t, drv.PoolOptions{Size: 1, AcquireTimeout: 5 * time.Second})
	dials := srv.DialAttempts()
	srv.SetWriteDelay(800 * time.Millisecond) // the probe now takes ~0.8 s but succeeds
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := d.Stat(ctx, "x/f"); err == nil {
		t.Fatal("expected the short-deadline call to give up")
	}
	if el := time.Since(start); el > 600*time.Millisecond {
		t.Fatalf("caller returned after %v; it must not wait for the probe", el)
	}
	srv.SetWriteDelay(0)
	time.Sleep(1500 * time.Millisecond) // let the probe finish
	if n := srv.DialAttempts(); n != dials {
		t.Fatalf("healthy connection was replaced: %d -> %d dials", dials, n)
	}
	if a := d.Pool.Alive(); a != 1 {
		t.Fatalf("pool size %d, want 1", a)
	}
	if _, err := d.Stat(context.Background(), "x/f"); err != nil {
		t.Fatalf("Stat on the kept connection: %v", err)
	}
	if n := srv.DialAttempts(); n != dials {
		t.Fatalf("Stat needed a new dial: %d -> %d", dials, n)
	}
}

// With no working login at all, the pool still starts (empty), costs a single login attempt per probe
// window instead of a burst, and fills on its own once logins work.
func TestPoolStartsEmptyWhenLoginRejected(t *testing.T) {
	srv := sftptest.New(t)
	srv.SetAuthFail(true)
	const pause = 300 * time.Millisecond
	d := srv.PoolWith(t, drv.PoolOptions{
		Size:              4,
		AcquireTimeout:    2 * time.Second,
		RefillBackoffBase: 5 * time.Millisecond,
		RefillBackoffMax:  10 * time.Millisecond,
		AuthPause:         pause,
		AuthPauseMax:      2 * pause,
	})
	if a := d.Pool.Alive(); a != 0 {
		t.Fatalf("alive=%d, want an empty pool", a)
	}
	start := time.Now()
	if _, err := d.Stat(context.Background(), "x"); err == nil {
		t.Fatal("expected unavailable")
	}
	if el := time.Since(start); el > 500*time.Millisecond {
		t.Fatalf("call against an empty, paused pool took %v; must fail fast", el)
	}
	time.Sleep(4 * pause)
	// Startup login + one probe per pause window (300, then 600 ms) — far from 4 slots x retries.
	if n := srv.DialAttempts(); n > 4 {
		t.Fatalf("%d login attempts during the outage; want at most one per probe window", n)
	}
	srv.SetAuthFail(false)
	waitFor(t, 5*time.Second, "pool fill after logins work", func() bool { return d.Pool.Alive() == 4 })
}

// A TCP-level outage (not an auth block) also fails fast once the pool is empty and dials are failing,
// instead of every call waiting the full acquire timeout.
func TestNetworkOutageFailsFast(t *testing.T) {
	srv := sftptest.New(t)
	d := srv.PoolWith(t, drv.PoolOptions{Size: 2, AcquireTimeout: 10 * time.Second})
	srv.SetRefuse(true)
	srv.DropAll()
	for i := 0; i < 4; i++ {
		start := time.Now()
		if _, err := d.Stat(context.Background(), "x"); err == nil {
			t.Fatal("expected unavailable")
		}
		if el := time.Since(start); el > 2*time.Second {
			t.Fatalf("call %d took %v during a network outage (acquire timeout 10s); must fail fast", i, el)
		}
	}
}

// Any login rejection during the initial dials trips the breaker, even after some logins worked.
func TestStartupTripsOnAnyAuthRejection(t *testing.T) {
	srv := sftptest.New(t)
	srv.AuthFailAfter(1)
	d := srv.PoolWith(t, drv.PoolOptions{Size: 4, AuthPause: time.Minute, RefillBackoffBase: 5 * time.Millisecond})
	if a := d.Pool.Alive(); a != 1 {
		t.Fatalf("alive=%d, want 1", a)
	}
	time.Sleep(200 * time.Millisecond)
	if n := srv.DialAttempts(); n != 2 {
		t.Fatalf("%d logins at startup; want 2 (one accepted, one rejected, then pause)", n)
	}
}

// A long recursive repository delete runs inside the long-operation budget, so it never takes the last
// free connection away from metadata reads.
func TestRepoDeleteUsesLongOpBudget(t *testing.T) {
	srv := sftptest.New(t)
	for i := 0; i < 150; i++ {
		putRemote(t, srv, fmt.Sprintf("registry/g/r/blobs/f%03d", i), []byte("x"), time.Time{})
	}
	putRemote(t, srv, "registry/g/other/manifests/v1", []byte("{}"), time.Time{})
	putRemote(t, srv, "x/big", make([]byte, 1<<20), time.Time{})
	d := srv.PoolWith(t, drv.PoolOptions{Size: 2, AcquireTimeout: 10 * time.Second})
	ctx := context.Background()
	r, err := d.Reader(ctx, "x/big", 0) // holds the only long-op slot
	if err != nil {
		t.Fatal(err)
	}
	srv.SetWriteDelay(10 * time.Millisecond) // ~150 removes take seconds
	done := make(chan error, 1)
	go func() { done <- d.DeleteRepositoryFolder(ctx, "g/r") }()
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	if _, err := d.GetContent(ctx, "registry/g/other/manifests/v1"); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el > 700*time.Millisecond {
		t.Fatalf("manifest read took %v during a repository delete", el)
	}
	r.Close()
	srv.SetWriteDelay(0)
	if err := <-done; err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := os.Stat(srv.File("registry/g/r")); !os.IsNotExist(err) {
		t.Fatal("repository folder not deleted")
	}
}

// Closing the pool while a connection is on its way back must not strand it: park() re-checks stopCh
// after handing the connection over and drains again if the close landed in between, because Close's own
// drain ran before the connection arrived.
//
// The window between the send and the re-check is small, so this runs many times with a single-slot pool
// (where park genuinely blocks on the send). It is a leak detector for the behaviour as a whole, not a
// proof of one particular line: reverting only the post-send drain does not make it fail.
func TestCloseDuringParkDoesNotStrandAConnection(t *testing.T) {
	leaked := 0
	for i := 0; i < 200; i++ {
		srv := sftptest.New(t)
		putRemote(t, srv, "x/f", []byte("hi"), time.Time{})
		// Size 1 so the send in park() has no other slot to land in.
		d := srv.PoolWith(t, drv.PoolOptions{Size: 1, AcquireTimeout: 5 * time.Second})
		r, err := d.Reader(context.Background(), "x/f", 0)
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			// Close while the reader is still open: park will run against a stopped pool.
			d.Pool.Close()
		}()
		r.Close()
		if !waitForOK(2*time.Second, func() bool { return srv.OpenConns() == 0 }) {
			leaked++
		}
		d.Pool.Close()
	}
	if leaked > 0 {
		t.Fatalf("%d of 200 runs stranded a connection after Close", leaked)
	}
}

// waitForOK is waitFor for a condition that may legitimately never hold: it reports whether it became
// true before the deadline.
func waitForOK(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}
