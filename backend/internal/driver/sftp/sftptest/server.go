// Package sftptest runs an in-process SFTP server (pkg/sftp request server rooted at a temp dir) over
// net.Pipe, with per-connection bandwidth throttling and fault injection, for tests that need a slow or
// misbehaving remote without any network.
package sftptest

import (
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pkgsftp "github.com/pkg/sftp"
	"refity/backend/internal/config"
	drv "refity/backend/internal/driver/sftp"
)

// Server is a fake remote. Configure fields before the first Dial; counters are safe to read anytime.
type Server struct {
	Root string
	// BytesPerSec throttles each connection in each direction (0 = unlimited). Per connection, not
	// global: that matches the real limiter (one SSH window per TCP flow), so parallel uploads help.
	BytesPerSec int64

	// FailOpen, when set, can reject a write-open (flags are os.O_* values).
	FailOpen func(path string, flags int) error
	// FailWriteAt, when set, can fail individual writes after the file was opened (and truncated).
	FailWriteAt func(path string, off int64) error

	// HandshakeDelay makes each login take this long (a real SSH login is not instant, so concurrent
	// dials overlap).
	HandshakeDelay time.Duration

	// IgnoreSetstat makes Setstat a silent no-op (servers that do not honour mtime changes).
	IgnoreSetstat bool
	// ClockAhead stamps renamed files with server time = now + ClockAhead (a skewed server clock).
	ClockAhead time.Duration

	authOKLeft   atomic.Int64 // >0: that many more logins succeed, then all are rejected (see AuthFailAfter)
	writeDelay   atomic.Int64 // nanoseconds added to every client write (slow link without failure)
	readDelay    atomic.Int64 // nanoseconds added to every client read (slow download without failure)
	refuse       atomic.Bool
	authFail     atomic.Bool
	dialAttempts atomic.Int64

	mu        sync.Mutex
	dials     int
	readOpens atomic.Int64
	renames   map[string]int
	conns     []*FaultConn

	// stallOnceWrite/Read: the first connection to have sent/received more than N bytes goes silent.
	stallOnceWrite atomic.Int64
	stallOnceRead  atomic.Int64
	stallOnceUsed  atomic.Bool
	// stallAllWrite: every connection goes silent after sending N bytes.
	stallAllWrite atomic.Int64
	// stallAllRead: every connection goes silent after receiving N bytes.
	stallAllRead atomic.Int64
	stalls       atomic.Int64
}

func New(t testing.TB) *Server {
	return &Server{Root: t.TempDir(), renames: map[string]int{}}
}

// StallOnceAfterWrite makes the first connection that sends more than n bytes stop responding forever.
func (s *Server) StallOnceAfterWrite(n int64) {
	s.stallOnceUsed.Store(false)
	s.stallOnceWrite.Store(n)
}

// StallOnceAfterRead makes the first connection that receives more than n bytes stop responding forever.
func (s *Server) StallOnceAfterRead(n int64) { s.stallOnceUsed.Store(false); s.stallOnceRead.Store(n) }

// StallAllAfterWrite makes every connection stop responding after sending n bytes (0 = off).
func (s *Server) StallAllAfterWrite(n int64) { s.stallAllWrite.Store(n) }

// StallAllAfterRead makes every connection stop responding after receiving n bytes (0 = off).
func (s *Server) StallAllAfterRead(n int64) { s.stallAllRead.Store(n) }

func (s *Server) Dials() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dials
}

// ReadOpens counts files opened for reading (to prove a response came from a local copy).
func (s *Server) ReadOpens() int64 { return s.readOpens.Load() }

// Writes counts bytes the client has sent to this server, so a test can wait until an upload is really
// under way rather than merely started.
func (s *Server) Writes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for _, c := range s.conns {
		n += c.written.Load()
	}
	return n
}

// Stalls counts connections that went silent.
func (s *Server) Stalls() int64 { return s.stalls.Load() }

// Renames returns how many renames targeted path (slash form, relative to Root).
func (s *Server) Renames(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.renames["/"+path]
}

// File returns the server-side location of a remote path.
func (s *Server) File(path string) string { return filepath.Join(s.Root, filepath.FromSlash(path)) }

// AuthFailAfter lets the next n logins succeed and rejects every login after that.
func (s *Server) AuthFailAfter(n int64) { s.authOKLeft.Store(n + 1) }

// SetWriteDelay delays every client->server write by d (0 = off): a slow but healthy link.
func (s *Server) SetWriteDelay(d time.Duration) { s.writeDelay.Store(int64(d)) }

// SetReadDelay delays every server->client read by d (0 = off): a slow but healthy download.
func (s *Server) SetReadDelay(d time.Duration) { s.readDelay.Store(int64(d)) }

// SetRefuse makes new dials fail like a refused TCP connection.
func (s *Server) SetRefuse(v bool) { s.refuse.Store(v) }

// SetAuthFail makes new handshakes fail like an SSH authentication rejection.
func (s *Server) SetAuthFail(v bool) { s.authFail.Store(v) }

// OpenConns counts connections the client side has not closed yet (leak detection).
func (s *Server) OpenConns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.conns {
		select {
		case <-c.closed:
		default:
			n++
		}
	}
	return n
}

// DialAttempts counts every dial, successful or not.
func (s *Server) DialAttempts() int64 { return s.dialAttempts.Load() }

// DropAll abruptly closes every open connection, like a network outage or server restart.
func (s *Server) DropAll() {
	s.mu.Lock()
	conns := append([]*FaultConn(nil), s.conns...)
	s.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

// Dial implements PoolOptions.Dial.
func (s *Server) Dial() (net.Conn, error) {
	s.dialAttempts.Add(1)
	if s.refuse.Load() {
		return nil, errors.New("dial tcp: connect: connection refused")
	}
	cli, srv := net.Pipe()
	rs := pkgsftp.NewRequestServer(srv, pkgsftp.Handlers{FileGet: s, FilePut: s, FileCmd: s, FileList: s})
	go func() {
		_ = rs.Serve()
		_ = rs.Close()
	}()
	fc := &FaultConn{Conn: cli, s: s, closed: make(chan struct{})}
	s.mu.Lock()
	s.dials++
	s.conns = append(s.conns, fc)
	s.mu.Unlock()
	return fc, nil
}

// Handshake implements PoolOptions.Handshake: SFTP straight over the pipe, no SSH layer.
func Handshake(conn net.Conn, opts ...pkgsftp.ClientOption) (*pkgsftp.Client, error) {
	return pkgsftp.NewClientPipe(conn, conn, opts...)
}

// Handshake is the package Handshake plus SetAuthFail fault injection. The error text matches what
// x/crypto/ssh reports for a rejected login.
func (s *Server) Handshake(conn net.Conn, opts ...pkgsftp.ClientOption) (*pkgsftp.Client, error) {
	if s.HandshakeDelay > 0 {
		time.Sleep(s.HandshakeDelay)
	}
	if left := s.authOKLeft.Load(); left > 0 {
		if left == 1 {
			s.authFail.Store(true) // allowance used up
		} else {
			s.authOKLeft.Add(-1)
		}
	}
	if s.authFail.Load() {
		return nil, errors.New("ssh: handshake failed: ssh: unable to authenticate, attempted methods [none password], no supported methods remain")
	}
	return Handshake(conn, opts...)
}

// PoolWith builds a pooled driver from explicit options (Dial/Handshake are filled in).
func (s *Server) PoolWith(t testing.TB, opts drv.PoolOptions) *drv.PoolStorageDriver {
	opts.Dial = s.Dial
	opts.Handshake = s.Handshake
	p, err := drv.NewDriverPoolWithOptions(&config.Config{}, opts)
	if err != nil {
		t.Fatalf("sftptest pool: %v", err)
	}
	t.Cleanup(p.Close)
	return &drv.PoolStorageDriver{Pool: p}
}

// Pool builds a pooled driver against this server and closes it when the test ends.
func (s *Server) Pool(t testing.TB, size int, stall time.Duration, parallelThreshold int64) *drv.PoolStorageDriver {
	return s.PoolWith(t, drv.PoolOptions{
		Size:              size,
		StallTimeout:      stall,
		ParallelThreshold: parallelThreshold,
	})
}

// FaultConn is the client end of one connection.
type FaultConn struct {
	net.Conn
	s         *Server
	written   atomic.Int64
	read      atomic.Int64
	stalled   atomic.Bool
	closeOnce sync.Once
	closed    chan struct{}
}

func (c *FaultConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

func (c *FaultConn) throttle(n int) {
	if bps := c.s.BytesPerSec; bps > 0 && n > 0 {
		time.Sleep(time.Duration(int64(n) * int64(time.Second) / bps))
	}
}

// goSilent blocks like a dead TCP path: no data, no error, until the client closes the connection.
func (c *FaultConn) goSilent() error {
	if c.stalled.CompareAndSwap(false, true) {
		c.s.stalls.Add(1)
	}
	<-c.closed
	return net.ErrClosed
}

func (c *FaultConn) shouldStall(counter int64, once *atomic.Int64, all int64) bool {
	if c.stalled.Load() {
		return true
	}
	if all > 0 && counter > all {
		return true
	}
	if n := once.Load(); n > 0 && counter > n && c.s.stallOnceUsed.CompareAndSwap(false, true) {
		return true
	}
	return false
}

func (c *FaultConn) Write(p []byte) (int, error) {
	if c.shouldStall(c.written.Load(), &c.s.stallOnceWrite, c.s.stallAllWrite.Load()) {
		return 0, c.goSilent()
	}
	if d := c.s.writeDelay.Load(); d > 0 {
		time.Sleep(time.Duration(d))
	}
	c.throttle(len(p))
	n, err := c.Conn.Write(p)
	c.written.Add(int64(n))
	return n, err
}

func (c *FaultConn) Read(p []byte) (int, error) {
	if c.shouldStall(c.read.Load(), &c.s.stallOnceRead, c.s.stallAllRead.Load()) {
		return 0, c.goSilent()
	}
	if d := c.s.readDelay.Load(); d > 0 {
		time.Sleep(time.Duration(d))
	}
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.read.Add(int64(n))
		c.throttle(n)
	}
	if c.stalled.Load() {
		return 0, c.goSilent()
	}
	return n, err
}

// ---------------------------------------------------------------------------
// pkg/sftp request-server handlers rooted at Root
// ---------------------------------------------------------------------------

func (s *Server) local(p string) string { return filepath.Join(s.Root, filepath.FromSlash(p)) }

func (s *Server) Fileread(r *pkgsftp.Request) (io.ReaderAt, error) {
	s.readOpens.Add(1)
	return os.Open(s.local(r.Filepath))
}

func (s *Server) Filewrite(r *pkgsftp.Request) (io.WriterAt, error) {
	pf := r.Pflags()
	flags := os.O_WRONLY
	if pf.Creat {
		flags |= os.O_CREATE
	}
	if pf.Trunc {
		flags |= os.O_TRUNC
	}
	if pf.Excl {
		flags |= os.O_EXCL
	}
	if s.FailOpen != nil {
		if err := s.FailOpen(r.Filepath, flags); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(s.local(r.Filepath), flags, 0o644)
	if err != nil || s.FailWriteAt == nil {
		if f != nil {
			return &countingWriter{File: f, s: s, path: r.Filepath}, nil
		}
		return f, err
	}
	return &faultWriter{
		countingWriter: countingWriter{File: f, s: s, path: r.Filepath},
		path:           r.Filepath,
		fail:           s.FailWriteAt,
	}, nil
}

// countingWriter wraps a remote file so writes are counted per connection, which is how a test waits
// for an upload to be really under way rather than merely started.
type countingWriter struct {
	*os.File
	s    *Server
	path string
}

func (w *countingWriter) Write(p []byte) (int, error) { return w.File.Write(p) }

type faultWriter struct {
	countingWriter
	path string
	fail func(path string, off int64) error
}

func (w *faultWriter) WriteAt(p []byte, off int64) (int, error) {
	if err := w.fail(w.path, off); err != nil {
		return 0, err
	}
	return w.File.WriteAt(p, off)
}

func (s *Server) Filecmd(r *pkgsftp.Request) error {
	switch r.Method {
	case "Mkdir":
		return os.Mkdir(s.local(r.Filepath), 0o755)
	case "Remove", "Rmdir":
		return os.Remove(s.local(r.Filepath))
	case "Rename":
		// SFTPv3 rename (and OpenSSH's) refuses to replace an existing target.
		if _, err := os.Stat(s.local(r.Target)); err == nil {
			return os.ErrExist
		}
		return s.rename(r)
	case "Setstat":
		if s.IgnoreSetstat {
			return nil
		}
		if r.AttrFlags().Acmodtime {
			a := r.Attributes()
			return os.Chtimes(s.local(r.Filepath), time.Unix(int64(a.Atime), 0), time.Unix(int64(a.Mtime), 0))
		}
		return nil
	}
	return errors.New("unsupported: " + r.Method)
}

func (s *Server) PosixRename(r *pkgsftp.Request) error { return s.rename(r) }

func (s *Server) rename(r *pkgsftp.Request) error {
	if err := os.Rename(s.local(r.Filepath), s.local(r.Target)); err != nil {
		return err
	}
	if s.ClockAhead != 0 {
		t := time.Now().Add(s.ClockAhead)
		_ = os.Chtimes(s.local(r.Target), t, t)
	}
	s.mu.Lock()
	s.renames[r.Target]++
	s.mu.Unlock()
	return nil
}

type listerAt []os.FileInfo

func (l listerAt) ListAt(ls []os.FileInfo, off int64) (int, error) {
	if off >= int64(len(l)) {
		return 0, io.EOF
	}
	n := copy(ls, l[off:])
	if n < len(ls) {
		return n, io.EOF
	}
	return n, nil
}

func (s *Server) Filelist(r *pkgsftp.Request) (pkgsftp.ListerAt, error) {
	p := s.local(r.Filepath)
	switch r.Method {
	case "List":
		ents, err := os.ReadDir(p)
		if err != nil {
			return nil, err
		}
		var out listerAt
		for _, e := range ents {
			if fi, err := e.Info(); err == nil {
				out = append(out, fi)
			}
		}
		return out, nil
	default: // Stat, Lstat
		fi, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		return listerAt{fi}, nil
	}
}
