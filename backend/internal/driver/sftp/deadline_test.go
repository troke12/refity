package sftp_test

import (
	"context"
	"io"
	"testing"
	"time"

	drv "refity/backend/internal/driver/sftp"
	"refity/backend/internal/driver/sftp/sftptest"
)

// A read that is slower than its budget must fail instead of running to completion. The heal and the
// audit both hold a path lock while they read, so "cannot give up" is not an option for them. A slow
// link models this just as well as a dead one: without the deadline this read takes several seconds
// and holds the lock for all of them.
func TestReaderWithDeadlineEndsASlowRead(t *testing.T) {
	srv := sftptest.New(t)
	putRemote(t, srv, "registry/g/app/blobs/x", make([]byte, 200_000), time.Time{})
	// No StallTimeout: nothing else would interrupt this read.
	d := srv.PoolWith(t, drv.PoolOptions{Size: 2, AcquireTimeout: 10 * time.Second})
	srv.SetReadDelay(250 * time.Millisecond) // every read is slow

	rc, err := d.ReaderWithDeadline(context.Background(), "registry/g/app/blobs/x", 0, 600*time.Millisecond)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer rc.Close()

	buf := make([]byte, 4096)
	start := time.Now()
	var lastErr error
	for lastErr == nil {
		if _, err := rc.Read(buf); err != nil {
			lastErr = err
		}
		if time.Since(start) > 8*time.Second {
			t.Fatal("the read ran far past its budget; the deadline did not end it")
		}
	}
	el := time.Since(start)
	if lastErr == nil {
		t.Fatal("a read past its deadline returned no error")
	}
	if el > 4*time.Second {
		t.Fatalf("the read ran for %v; the deadline did not cut it short", el)
	}
	t.Logf("slow read ended after %v with: %v", el.Round(time.Millisecond), lastErr)
}

// Without a deadline the same slow read runs to completion, which is what the deadline is protecting
// against. Guards against the test above passing because the read simply got fast.
func TestReaderWithoutDeadlineCompletesTheSlowRead(t *testing.T) {
	srv := sftptest.New(t)
	putRemote(t, srv, "registry/g/app/blobs/s", make([]byte, 40_000), time.Time{})
	d := srv.PoolWith(t, drv.PoolOptions{Size: 2, AcquireTimeout: 10 * time.Second})
	srv.SetReadDelay(100 * time.Millisecond)

	start := time.Now()
	got, err := d.GetContent(context.Background(), "registry/g/app/blobs/s")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	el := time.Since(start)
	if len(got) != 40_000 {
		t.Fatalf("read %d bytes, want 40000", len(got))
	}
	if el < 100*time.Millisecond {
		t.Fatalf("the slow read finished in %v; the delay was not in effect, so the deadline test proves nothing", el)
	}
	t.Logf("unbounded slow read took %v", el.Round(time.Millisecond))
}

// The deadline must be cleared on close, so the connection goes back to the pool usable. The check waits
// past the original deadline: a deadline left armed would only fire later, so asserting immediately
// after Close would pass even with the clearing removed.
func TestReaderWithDeadlineRestoresTheConnection(t *testing.T) {
	srv := sftptest.New(t)
	payload := []byte("hello refity")
	putRemote(t, srv, "registry/g/app/blobs/y", payload, time.Time{})
	// A deadline short enough to wait out, so "it still works afterwards" really proves it was cleared.
	const deadline = 700 * time.Millisecond
	d := srv.PoolWith(t, drv.PoolOptions{Size: 2, AcquireTimeout: 10 * time.Second})

	rc, err := d.ReaderWithDeadline(context.Background(), "registry/g/app/blobs/y", 0, deadline)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("content: got %q want %q", got, payload)
	}

	// Wait past the deadline the connection was opened under. If Close had not cleared it, every
	// operation on this connection now fails.
	time.Sleep(deadline + 300*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i := 0; i < 3; i++ {
		again, err := d.GetContent(ctx, "registry/g/app/blobs/y")
		if err != nil {
			t.Fatalf("read %d after the deadline passed: %v; the connection was left with a deadline", i, err)
		}
		if string(again) != string(payload) {
			t.Fatalf("read %d: got %q want %q", i, again, payload)
		}
	}
}

// Closing twice must not hand the connection back to the pool twice. A double putClient would let two
// callers check out the same connection and silently corrupt the pool. Alive() counts dials and cannot
// see this, so the check is that the pool still has exactly its configured number of parked connections
// afterwards — a double return leaves an extra one queued.
func TestReaderWithDeadlineDoubleCloseIsSafe(t *testing.T) {
	srv := sftptest.New(t)
	putRemote(t, srv, "registry/g/app/blobs/z", []byte("x"), time.Time{})
	const size = 2
	d := srv.PoolWith(t, drv.PoolOptions{Size: size, AcquireTimeout: 5 * time.Second})
	parked := func() int { return d.Pool.Parked() }

	rc, err := d.ReaderWithDeadline(context.Background(), "registry/g/app/blobs/z", 0, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(rc); err != nil {
		t.Fatal(err)
	}
	if got := parked(); got != size-1 {
		t.Fatalf("while a reader is open the pool should hold %d connections, holds %d", size-1, got)
	}
	if err := rc.Close(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "the connection back in the pool", func() bool { return parked() == size })
	if err := rc.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	// A second Close must not queue a second copy of the same connection.
	if got := parked(); got != size {
		t.Fatalf("parked connections after a double close: %d, want %d; the connection was returned twice", got, size)
	}

	// And the pool still hands out distinct, working connections.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i := 0; i < size; i++ {
		if _, err := d.GetContent(ctx, "registry/g/app/blobs/z"); err != nil {
			t.Fatalf("checkout %d after a double close: %v", i, err)
		}
	}
}

// An offset is honoured, so a resumed read does not start from the beginning.
func TestReaderWithDeadlineHonoursOffset(t *testing.T) {
	srv := sftptest.New(t)
	payload := []byte("0123456789abcdef")
	putRemote(t, srv, "registry/g/app/blobs/w", payload, time.Time{})
	d := srv.PoolWith(t, drv.PoolOptions{Size: 2, AcquireTimeout: 5 * time.Second})
	rc, err := d.ReaderWithDeadline(context.Background(), "registry/g/app/blobs/w", 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "abcdef" {
		t.Fatalf("offset read: got %q want %q", got, "abcdef")
	}
}

// A background verification read is bulk traffic and must count against the shared long-operation
// budget, like every other reader. Without this, a few concurrent heals (or an audit, which runs for
// hours) take every connection and leave HEADs, Stats and folder creation timing out — the exact thing
// the budget exists to prevent.
func TestReaderWithDeadlineRespectsTheLongOpBudget(t *testing.T) {
	srv := sftptest.New(t)
	putRemote(t, srv, "registry/g/app/blobs/b", make([]byte, 64<<10), time.Time{})
	const size = 4
	d := srv.PoolWith(t, drv.PoolOptions{Size: size, AcquireTimeout: 2 * time.Second})

	// Open readers until one is refused. The budget is poolSize-1, so the pool must still have one
	// connection left over for metadata calls. A bounded context, because waiting for a budget slot
	// blocks and Background would hang here forever.
	var open []io.ReadCloser
	refused := false
	for i := 0; i < size+2; i++ {
		attemptCtx, cancelAttempt := context.WithTimeout(context.Background(), 300*time.Millisecond)
		rc, err := d.ReaderWithDeadline(attemptCtx, "registry/g/app/blobs/b", 0, time.Minute)
		cancelAttempt()
		if err != nil {
			if len(open) < size-1 {
				t.Fatalf("open %d: a verification read was refused after only %d of the %d budget slots were used: %v",
					len(open), len(open), size-1, err)
			}
			refused = true
			break
		}
		open = append(open, rc)
	}
	if !refused {
		for _, rc := range open {
			rc.Close()
		}
		t.Fatalf("opened %d verification readers on a pool of %d; the long budget is not applied", len(open), size)
	}
	if len(open) != size-1 {
		for _, rc := range open {
			rc.Close()
		}
		t.Fatalf("opened %d readers, want %d (pool size minus the reserved connection)", len(open), size-1)
	}

	// With the budget full, a metadata call still works.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := d.Stat(ctx, "registry/g/app/blobs/b"); err != nil {
		t.Fatalf("a Stat was starved by open verification reads: %v", err)
	}

	// Closing them hands the budget back.
	for _, rc := range open {
		rc.Close()
	}
	rc, err := d.ReaderWithDeadline(context.Background(), "registry/g/app/blobs/b", 0, time.Minute)
	if err != nil {
		t.Fatalf("after closing them the budget was not released: %v", err)
	}
	rc.Close()
}

// The specific starvation the long budget prevents. ReaderWithDeadline used to skip the budget, so
// three concurrent verification reads (one per corrupt blob, plus an audit) took every connection and
// left HEADs and Stats timing out. With the budget only poolSize-1 can be open at once, so the pool
// still has a connection free for metadata — and the fourth verification read has to wait.
func TestVerificationReadsCannotTakeEveryConnection(t *testing.T) {
	srv := sftptest.New(t)
	putRemote(t, srv, "registry/g/app/blobs/b", make([]byte, 64<<10), time.Time{})
	const size = 4
	d := srv.PoolWith(t, drv.PoolOptions{Size: size, AcquireTimeout: 2 * time.Second})

	// Fill the budget: size-1 verification reads.
	var open []io.ReadCloser
	for i := 0; i < size-1; i++ {
		rc, err := d.ReaderWithDeadline(context.Background(), "registry/g/app/blobs/b", 0, time.Minute)
		if err != nil {
			t.Fatalf("opening verification read %d of %d: %v", i, size-1, err)
		}
		open = append(open, rc)
	}
	if got := d.Pool.Parked(); got != 1 {
		t.Fatalf("parked connections: %d, want 1 (the budget is %d)", got, size-1)
	}

	// The reserved connection serves metadata while the budget is full.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := d.Stat(ctx, "registry/g/app/blobs/b"); err != nil {
		t.Fatalf("a Stat was starved while the verification budget was full: %v", err)
	}

	// The next verification read must wait rather than take the last connection. Note this passes either
	// way here: the pool checkout itself would also refuse once every connection is out, so this test
	// documents the invariant but does not isolate the budget from the checkout limit.
	// The budget is still what guarantees it for a caller that holds a connection open indefinitely.
	extraCtx, cancelExtra := context.WithTimeout(context.Background(), 300*time.Millisecond)
	_, err := d.ReaderWithDeadline(extraCtx, "registry/g/app/blobs/b", 0, time.Minute)
	cancelExtra()
	if err == nil {
		t.Fatalf("a verification read took the reserved connection instead of waiting for the budget")
	}

	for _, rc := range open {
		rc.Close()
	}
	// With the readers closed, the budget is free again.
	if got := d.Pool.Parked(); got != size {
		t.Fatalf("parked connections after closing: %d, want %d", got, size)
	}
}

// Every failure mode must give the long-operation budget slot back, or the pool loses one connection's
// worth of capacity per attempt and eventually starves every metadata call.
func TestReaderWithDeadlineReleasesTheBudgetOnEveryPath(t *testing.T) {
	srv := sftptest.New(t)
	putRemote(t, srv, "a/b", []byte("hi"), time.Time{})
	d := srv.PoolWith(t, drv.PoolOptions{Size: 3, AcquireTimeout: time.Second})

	// Can the whole budget still be taken? The budget is poolSize-1, so two is all of it.
	fillBudget := func() bool {
		for i := 0; i < 2; i++ {
			rc, err := d.ReaderWithDeadline(context.Background(), "a/b", 0, time.Minute)
			if err != nil {
				return false
			}
			rc.Close()
		}
		return true
	}
	if !fillBudget() {
		t.Fatal("precondition: could not fill the long budget")
	}
	// A missing file fails after the slot is taken.
	if _, err := d.ReaderWithDeadline(context.Background(), "nope/missing", 0, time.Minute); err == nil {
		t.Fatal("expected an error for a missing file")
	}
	if !fillBudget() {
		t.Error("a failed open leaked a long budget slot")
	}
	// A double close must not hand the slot back twice, which would let more work run than the budget.
	rc, err := d.ReaderWithDeadline(context.Background(), "a/b", 0, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	rc.Close()
	if !fillBudget() {
		t.Error("a double close released the long budget twice")
	}
}
