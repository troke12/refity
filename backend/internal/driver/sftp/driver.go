package sftp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	pathpkg "path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	"refity/backend/internal/config"
)

type StorageDriver interface {
	Name() string
	GetContent(ctx context.Context, path string) ([]byte, error)
	PutContent(ctx context.Context, path string, content []byte, progressCb ...func(written, total int64)) error
	Reader(ctx context.Context, path string, offset int64) (io.ReadCloser, error)
	Writer(ctx context.Context, path string, append bool) (FileWriter, error)
	Stat(ctx context.Context, path string) (FileInfo, error)
	List(ctx context.Context, path string) ([]string, error)
	Move(ctx context.Context, sourcePath string, destPath string) error
	Delete(ctx context.Context, path string) error
	RedirectURL(r *http.Request, path string) (string, error)
	Walk(ctx context.Context, path string, f WalkFn, options ...func(*WalkOptions)) error
	CreateRepositoryFolder(ctx context.Context, repoName string) error
	DeleteRepositoryFolder(ctx context.Context, repoName string) error
	CreateGroupFolder(ctx context.Context, groupName string) error
}

type FileWriter interface {
	io.WriteCloser
	Size() int64
	Cancel(context.Context) error
	Commit(context.Context) error
}

type FileInfo interface{}
type WalkFn func(fileInfo FileInfo) error
type WalkOptions struct{}

var ErrUnsupportedMethod = func(driverName string) error { return &unsupportedMethodError{DriverName: driverName} }

type unsupportedMethodError struct{ DriverName string }

func (e *unsupportedMethodError) Error() string { return e.DriverName + ": unsupported method" }

var ErrRepoNotFound = errors.New("repository not found")

// ---------------------------------------------------------------------------
// Connection Pool with keepalive, auto-reconnect, and safe client lifecycle
// ---------------------------------------------------------------------------

type DriverPool struct {
	clients  chan *sftp.Client
	cfg      *config.Config
	poolSize int
	alive    atomic.Int32
	stopOnce sync.Once
	stopCh   chan struct{}

	opts  PoolOptions
	conns sync.Map // *sftp.Client -> *watchedConn
	// long is one budget of poolSize-1 (min 1) slots shared by everything that holds a connection for a
	// long time: upload parts and download streams. With separate budgets per kind, uploads plus
	// downloads could still occupy every connection, and HEADs, Stats and folder creation would queue
	// behind them for minutes. Sharing one budget guarantees a free connection for short calls.
	long chan struct{}

	breakerMu   sync.Mutex
	authFails   int
	pausedUntil time.Time
	pauseLevel  int       // >0 after a breaker trip until a dial succeeds; drives probe mode and backoff
	probing     bool      // a single probe dial is in flight while recovering from a pause
	lastDialErr time.Time // when the most recent dial failed (zero after a success)
}

// PoolOptions tunes a DriverPool. Zero values keep the historical behaviour (no watchdog, no parallel upload).
type PoolOptions struct {
	Size int
	// StallTimeout aborts a connection whose in-flight operation has received no bytes from the server for
	// this long. A half-dead TCP path over a high-RTT link otherwise blocks an SFTP call forever: neither
	// pkg/sftp nor x/crypto/ssh has per-request deadlines. 0 disables the watchdog.
	StallTimeout time.Duration
	// ParallelThreshold: UploadFile splits files at least this large across several pooled connections.
	// 0 disables parallel uploads.
	ParallelThreshold int64
	// Dial returns the raw transport. nil dials TCP to cfg.FTPHost:cfg.FTPPort.
	Dial func() (net.Conn, error)
	// Handshake turns a transport into an SFTP client. nil runs the SSH handshake from cfg.
	Handshake func(conn net.Conn, opts ...sftp.ClientOption) (*sftp.Client, error)
	// AcquireTimeout bounds how long a driver call waits for a pooled connection (default 30s). When the
	// remote is down every call fails after this instead of queueing forever.
	AcquireTimeout time.Duration
	// RefillBackoffBase/Max pace reconnect attempts per missing slot (defaults 5s / 5m). Refilling never
	// gives up; it stops only when the pool is closed.
	RefillBackoffBase time.Duration
	RefillBackoffMax  time.Duration
	// After AuthFailLimit consecutive authentication failures (default 3) all dials pause for AuthPause
	// (default 5m). Generic resilience for a genuine auth failure: a reconnect storm only adds failed
	// logins, and Storage Boxes are reported to throttle repeated failed password logins.
	AuthFailLimit int
	AuthPause     time.Duration
	// RecentFailWindow: with no live connection and a dial failure this recent, calls fail at once
	// instead of waiting the acquire timeout (default 10s). Covers network outages, where the auth
	// breaker never pauses.
	RecentFailWindow time.Duration
	// AuthPauseMax caps the pause as it doubles after each failed probe (default 15m: 5, 10, 15, 15...).
	// Kept short so the pool is not left empty long after a block lifts.
	AuthPauseMax time.Duration
}

const (
	dialTimeout = 10 * time.Second
	// handshakeTimeout bounds SSH key exchange + auth + SFTP init. ssh.ClientConfig.Timeout only covers the
	// TCP connect, so a peer that accepts TCP but never speaks would hang newClient (and fillPool) forever.
	handshakeTimeout = 30 * time.Second
)

// ErrUnavailable is returned when no pooled connection could be obtained within the acquire timeout.
var ErrUnavailable = errors.New("SFTP unavailable: no connection available")

// ErrDialPaused is returned by dial attempts while the authentication circuit breaker is open.
var ErrDialPaused = errors.New("sftp: dialing paused after repeated authentication failures")

// ErrStalled marks an operation aborted by the stall watchdog. Its text contains "connection lost" so
// IsConnectionLost treats it as transient.
var ErrStalled = errors.New("sftp: connection stalled (connection lost)")

// clientOptions enables request pipelining. x/crypto/ssh uses a fixed 2 MiB per-channel window
// (channelWindowSize, not configurable through ClientConfig), so one SSH connection can never move more
// than ~window/RTT; on a 0.2-0.5 s path that is the observed ~1 MB/s ceiling. Concurrent requests keep that
// window full; going beyond it needs more TCP connections, which is what UploadFile's parallel mode does.
func clientOptions() []sftp.ClientOption {
	return []sftp.ClientOption{
		sftp.UseConcurrentWrites(true),
		sftp.UseConcurrentReads(true),
		sftp.MaxConcurrentRequestsPerFile(64),
	}
}

// watchedConn counts bytes received from the server and, while any operation is in flight, closes the
// transport when nothing has arrived for the stall timeout. Closing the transport (not the sftp.Client) is
// what unblocks a stuck call: sftp.Client.Close needs the session write lock, which a write blocked on a
// full SSH window holds forever, whereas a closed socket makes every pending request fail immediately.
type watchedConn struct {
	net.Conn
	timeout   time.Duration
	lastRx    atomic.Int64 // unix nanos of the last received byte (or of the idle->busy transition)
	busy      atomic.Int32
	stalled   atomic.Bool
	closeOnce sync.Once
	done      chan struct{}
}

func newWatchedConn(c net.Conn, timeout time.Duration) *watchedConn {
	w := &watchedConn{Conn: c, timeout: timeout, done: make(chan struct{})}
	w.lastRx.Store(time.Now().UnixNano())
	return w
}

func (w *watchedConn) Read(p []byte) (int, error) {
	n, err := w.Conn.Read(p)
	if n > 0 {
		w.lastRx.Store(time.Now().UnixNano())
	}
	return n, err
}

func (w *watchedConn) Close() error {
	var err error
	w.closeOnce.Do(func() {
		close(w.done)
		err = w.Conn.Close()
	})
	return err
}

// begin/end bracket work that expects server traffic. Idle time between operations (a pooled client, or a
// download reader waiting for a slow HTTP client) must not count as a stall.
func (w *watchedConn) begin() {
	if w.busy.Add(1) == 1 {
		w.lastRx.Store(time.Now().UnixNano())
	}
}

func (w *watchedConn) end() { w.busy.Add(-1) }

func (w *watchedConn) watch() {
	if w.timeout <= 0 {
		return
	}
	tick := w.timeout / 4
	if tick < 5*time.Millisecond {
		tick = 5 * time.Millisecond
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-w.done:
			return
		case <-t.C:
			if w.busy.Load() > 0 && time.Since(time.Unix(0, w.lastRx.Load())) > w.timeout {
				w.stalled.Store(true)
				log.Printf("[SFTP] Pool: connection stalled (no data for %v), aborting it", w.timeout)
				_ = w.Close()
				return
			}
		}
	}
}

func hostKeyCallback(cfg *config.Config) (ssh.HostKeyCallback, error) {
	if cfg.FTPKnownHosts != "" {
		return knownhosts.New(cfg.FTPKnownHosts)
	}
	log.Println("WARNING: FTP_KNOWN_HOSTS not set. SSH host key verification is DISABLED. This is vulnerable to MITM attacks. Set FTP_KNOWN_HOSTS in production.")
	return ssh.InsecureIgnoreHostKey(), nil
}

// NewDriverPool connects poolSize SSH/SFTP sessions using the stall/parallel settings from cfg.
func NewDriverPool(cfg *config.Config, poolSize int) (*DriverPool, error) {
	return NewDriverPoolWithOptions(cfg, PoolOptions{
		Size:              poolSize,
		StallTimeout:      cfg.SFTPStallTimeout,
		ParallelThreshold: cfg.SFTPParallelThreshold,
		AcquireTimeout:    cfg.SFTPAcquireTimeout,
	})
}

func NewDriverPoolWithOptions(cfg *config.Config, opts PoolOptions) (*DriverPool, error) {
	if cfg == nil {
		cfg = &config.Config{}
	}
	poolSize := opts.Size
	if poolSize < 1 {
		poolSize = 1
	}
	if opts.Dial == nil {
		addr := cfg.FTPHost + ":" + cfg.FTPPort
		opts.Dial = func() (net.Conn, error) { return net.DialTimeout("tcp", addr, dialTimeout) }
	}
	if opts.Handshake == nil {
		opts.Handshake = sshHandshake(cfg)
	}
	if opts.AcquireTimeout <= 0 {
		opts.AcquireTimeout = 30 * time.Second
	}
	if opts.RefillBackoffBase <= 0 {
		opts.RefillBackoffBase = 5 * time.Second
	}
	if opts.RefillBackoffMax <= 0 {
		opts.RefillBackoffMax = 5 * time.Minute
	}
	if opts.AuthFailLimit <= 0 {
		opts.AuthFailLimit = 3
	}
	if opts.AuthPause <= 0 {
		opts.AuthPause = 5 * time.Minute
	}
	if opts.AuthPauseMax <= 0 {
		opts.AuthPauseMax = 15 * time.Minute
	}
	if opts.RecentFailWindow <= 0 {
		opts.RecentFailWindow = 10 * time.Second
	}
	pool := &DriverPool{
		clients:  make(chan *sftp.Client, poolSize),
		cfg:      cfg,
		poolSize: poolSize,
		stopCh:   make(chan struct{}),
		opts:     opts,
		long:     make(chan struct{}, max(1, poolSize-1)),
	}

	connected := 0
	for i := 0; i < poolSize; i++ {
		client, err := pool.newClient()
		if err != nil {
			log.Printf("[SFTP] Pool: initial connection %d/%d failed: %v", i+1, poolSize, err)
			if isAuthError(err) {
				// Login rejected (e.g. wrong credentials). More attempts now would only add failed
				// logins: pause right away, keep whatever connected, and let the refill loop probe once
				// per pause.
				pool.tripBreaker(err)
				break
			}
			continue
		}
		if connected == 0 {
			logRenameMode(client)
		}
		pool.clients <- client
		connected++
	}

	// An empty pool is not fatal: the process must start (and serve its spool and cache) even while the
	// Storage Box rejects logins or is unreachable; the background refill connects once it can.
	if connected == 0 {
		log.Printf("[SFTP] Pool: no connection to %s:%s at startup; starting empty and retrying in the background", cfg.FTPHost, cfg.FTPPort)
	}

	pool.alive.Store(int32(connected))
	log.Printf("[SFTP] Pool: initialized %d/%d connections", connected, poolSize)

	// Fill remaining slots in background
	if connected < poolSize {
		go pool.fillPool(poolSize - connected)
	}

	// Start keepalive goroutine
	go pool.keepalive()

	return pool, nil
}

// logRenameMode reports once at startup how uploads will be moved into place.
func logRenameMode(c *sftp.Client) {
	if _, ok := c.HasExtension("posix-rename@openssh.com"); ok {
		log.Printf("[SFTP] Server supports posix-rename@openssh.com: uploads replace files atomically")
	} else {
		log.Printf("[SFTP] Server lacks posix-rename@openssh.com: uploads use remove+rename after the new temp file is verified (old version stays until then)")
	}
}

// isAuthError recognises SSH authentication rejections.
func isAuthError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "unable to authenticate") || strings.Contains(s, "permission denied")
}

// dialPausedFor reports how long the auth circuit breaker still blocks dialing.
func (p *DriverPool) dialPausedFor() time.Duration {
	p.breakerMu.Lock()
	defer p.breakerMu.Unlock()
	return time.Until(p.pausedUntil)
}

// beginDial decides whether a dial may go out now. While recovering from an auth pause only one probe
// dial is allowed at a time: when the pause ends, every refill loop wakes together, and letting them all
// dial would send a burst of failed logins (servers that throttle failed logins would keep throttling).
func (p *DriverPool) beginDial() error {
	p.breakerMu.Lock()
	defer p.breakerMu.Unlock()
	if d := time.Until(p.pausedUntil); d > 0 {
		return fmt.Errorf("%w (%v left)", ErrDialPaused, d.Round(time.Second))
	}
	if p.pauseLevel > 0 {
		if p.probing {
			return fmt.Errorf("%w (probe in progress)", ErrDialPaused)
		}
		p.probing = true
	}
	return nil
}

func (p *DriverPool) recordDial(err error) {
	p.breakerMu.Lock()
	defer p.breakerMu.Unlock()
	wasProbe := p.probing
	p.probing = false
	if err != nil {
		p.lastDialErr = time.Now()
	} else {
		p.lastDialErr = time.Time{}
	}
	if err == nil {
		if p.pauseLevel > 0 {
			log.Printf("[SFTP] Pool: probe login succeeded, resuming normal reconnects")
		}
		p.authFails = 0
		p.pauseLevel = 0
		return
	}
	if !isAuthError(err) {
		return
	}
	if wasProbe {
		// Probe rejected: logins still fail. Back off further (base, 2x, 4x, ... capped).
		p.pauseLevel++
		d := p.opts.AuthPause
		for i := 1; i < p.pauseLevel && d < p.opts.AuthPauseMax; i++ {
			d *= 2
		}
		if d > p.opts.AuthPauseMax {
			d = p.opts.AuthPauseMax
		}
		p.pausedUntil = time.Now().Add(d)
		log.Printf("[SFTP] Pool: probe login rejected (%v); pausing all dials for %v", err, d)
		return
	}
	p.authFails++
	if p.authFails >= p.opts.AuthFailLimit {
		p.tripLocked(err)
	}
}

func (p *DriverPool) tripBreaker(err error) {
	p.breakerMu.Lock()
	defer p.breakerMu.Unlock()
	p.tripLocked(err)
}

func (p *DriverPool) tripLocked(err error) {
	p.pausedUntil = time.Now().Add(p.opts.AuthPause)
	p.pauseLevel = 1
	p.authFails = 0
	log.Printf("[SFTP] Pool: authentication rejected (%v); pausing all dials for %v, then one probe login at a time", err, p.opts.AuthPause)
}

func (p *DriverPool) newClient() (*sftp.Client, error) {
	if err := p.beginDial(); err != nil {
		return nil, err
	}
	raw, err := p.opts.Dial()
	if err != nil {
		p.recordDial(err)
		return nil, fmt.Errorf("sftp dial: %w", err)
	}
	wc := newWatchedConn(raw, p.opts.StallTimeout)
	_ = raw.SetDeadline(time.Now().Add(handshakeTimeout))
	client, err := p.opts.Handshake(wc, clientOptions()...)
	_ = raw.SetDeadline(time.Time{})
	p.recordDial(err)
	if err != nil {
		wc.Close()
		return nil, err
	}
	p.conns.Store(client, wc)
	go wc.watch()
	return client, nil
}

// sshHandshake runs SSH + SFTP setup over an already-dialed transport.
func sshHandshake(cfg *config.Config) func(net.Conn, ...sftp.ClientOption) (*sftp.Client, error) {
	return func(conn net.Conn, opts ...sftp.ClientOption) (*sftp.Client, error) {
		hk, err := hostKeyCallback(cfg)
		if err != nil {
			return nil, err
		}
		addr := cfg.FTPHost + ":" + cfg.FTPPort
		sshConfig := &ssh.ClientConfig{
			User:            cfg.FTPUsername,
			Auth:            []ssh.AuthMethod{ssh.Password(cfg.FTPPassword)},
			HostKeyCallback: hk,
			Timeout:         dialTimeout,
		}
		c, chans, reqs, err := ssh.NewClientConn(conn, addr, sshConfig)
		if err != nil {
			return nil, fmt.Errorf("ssh handshake %s: %w", addr, err)
		}
		sc := ssh.NewClient(c, chans, reqs)
		client, err := sftp.NewClient(sc, opts...)
		if err != nil {
			sc.Close()
			return nil, fmt.Errorf("sftp new client: %w", err)
		}
		return client, nil
	}
}

// dialSSHBounded is ssh.Dial with the handshake bounded too (see handshakeTimeout).
func dialSSHBounded(addr string, sshConfig *ssh.ClientConfig) (*ssh.Client, error) {
	conn, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, sshConfig)
	if err != nil {
		conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return ssh.NewClient(c, chans, reqs), nil
}

func (p *DriverPool) watched(c *sftp.Client) *watchedConn {
	if v, ok := p.conns.Load(c); ok {
		return v.(*watchedConn)
	}
	return nil
}

func (p *DriverPool) begin(c *sftp.Client) {
	if w := p.watched(c); w != nil {
		w.begin()
	}
}

func (p *DriverPool) end(c *sftp.Client) {
	if w := p.watched(c); w != nil {
		w.end()
	}
}

// annotate tags err with ErrStalled when the watchdog killed c, so logs say why the call failed.
func (p *DriverPool) annotate(c *sftp.Client, err error) error {
	if err == nil {
		return nil
	}
	if w := p.watched(c); w != nil && w.stalled.Load() {
		return fmt.Errorf("%w: %v", ErrStalled, err)
	}
	return err
}

// closeClient tears down the transport first (see watchedConn) and then the SFTP session.
func (p *DriverPool) closeClient(c *sftp.Client) {
	if v, ok := p.conns.LoadAndDelete(c); ok {
		_ = v.(*watchedConn).Close()
	}
	c.Close()
}

// setDeadline puts an absolute deadline on a pooled connection's transport, or clears it with the zero
// time. Used to bound a single operation that would otherwise be able to block indefinitely.
func (p *DriverPool) setDeadline(c *sftp.Client, t time.Time) error {
	v, ok := p.conns.Load(c)
	if !ok {
		return ErrUnavailable // not tracked: nothing to set a deadline on
	}
	return v.(*watchedConn).SetDeadline(t)
}

// Size is the configured number of pooled connections.
func (p *DriverPool) Size() int { return p.poolSize }

// Parked reports how many idle connections are currently queued for checkout. Tests use it to see the
// pool's inventory directly: Alive counts dials, so it cannot tell a healthy pool from one where a
// connection was handed back twice.
func (p *DriverPool) Parked() int { return len(p.clients) }

// fillPool brings count missing slots back. It never gives up: after an outage of any length the pool
// recovers on its own. Attempts back off exponentially per slot (capped), and wait out an open auth
// circuit breaker instead of hammering the server.
func (p *DriverPool) fillPool(count int) {
	for i := 0; i < count; i++ {
		wait := p.opts.RefillBackoffBase
		for attempt := 1; ; attempt++ {
			select {
			case <-p.stopCh:
				return
			default:
			}
			client, err := p.newClient()
			if err == nil {
				if !p.park(client) {
					return
				}
				p.alive.Add(1)
				log.Printf("[SFTP] Pool: background fill succeeded, pool at %d", p.alive.Load())
				break
			}
			sleep := wait
			if d := p.dialPausedFor(); d > sleep {
				sleep = d
			}
			if attempt == 1 || attempt%10 == 0 {
				log.Printf("[SFTP] Pool: background fill attempt %d failed: %v (next in %v)", attempt, err, sleep.Round(time.Millisecond))
			}
			t := time.NewTimer(sleep)
			select {
			case <-t.C:
			case <-p.stopCh:
				t.Stop()
				return
			}
			if wait *= 2; wait > p.opts.RefillBackoffMax {
				wait = p.opts.RefillBackoffMax
			}
		}
	}
}

// keepalive pings all connections periodically to prevent idle timeout.
// Hetzner Storage Box typically drops idle SSH after ~15 min.
func (p *DriverPool) keepalive() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			p.healthCheckAll()
		}
	}
}

// healthCheckAll drains the pool, pings each client, reconnects dead ones, puts them back.
func (p *DriverPool) healthCheckAll() {
	count := len(p.clients)
	if count == 0 {
		return
	}

	var healthy []*sftp.Client
	var dead int

	// Drain up to 'count' clients (non-blocking)
	for i := 0; i < count; i++ {
		select {
		case c := <-p.clients:
			p.begin(c)
			_, err := c.Getwd()
			if err != nil {
				p.closeClient(c)
				dead++
			} else {
				p.end(c)
				healthy = append(healthy, c)
			}
		default:
			break
		}
	}

	// Put healthy ones back
	for _, c := range healthy {
		p.clients <- c
	}

	// Replace dead ones
	if dead > 0 {
		log.Printf("[SFTP] Pool keepalive: %d dead connections, reconnecting...", dead)
		p.alive.Add(-int32(dead))
		go p.fillPool(dead)
	}
}

func (p *DriverPool) getClient() *sftp.Client {
	return p.getClientCtx(context.Background())
}

// getClientCtx checks out a healthy client, waiting at most AcquireTimeout (or until ctx ends); nil means
// none was available and callers return ErrUnavailable. A stale client found on checkout is discarded and
// handed to fillPool for replacement instead of being reconnected inline: inline reconnects with sleeps
// made every caller pay for an outage, while the refill loop paces itself and honours the auth breaker.
// A checked-out client counts as busy for the stall watchdog until putClient.
func (p *DriverPool) getClientCtx(ctx context.Context) *sftp.Client {
	if p.unavailableNow() {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, p.opts.AcquireTimeout)
	defer cancel()
	// While waiting, re-check periodically: the stale clients this caller discards trigger refill dials,
	// and if those fail the wait is over long before the acquire timeout.
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		var client *sftp.Client
		select {
		case client = <-p.clients:
		case <-ctx.Done():
			return nil
		case <-p.stopCh:
			return nil
		case <-tick.C:
			if p.unavailableNow() {
				return nil
			}
			continue
		}
		p.begin(client)
		// The probe runs aside so a connection that hangs silently (no stall watchdog, or not yet
		// fired) cannot hold the caller past its acquire deadline; closing the client unblocks it.
		probe := make(chan error, 1)
		go func(c *sftp.Client) { _, err := c.Getwd(); probe <- err }(client)
		select {
		case err := <-probe:
			if err != nil {
				p.discardStale(client, err)
				continue
			}
			return client
		case <-ctx.Done():
			// The caller gave up, not the connection. Closing it here would throw away a healthy
			// session (and every replacement is a login, possibly a rejected one), so the probe result
			// decides its fate in the background: park it if healthy, discard it only if it failed.
			go p.settleProbe(client, probe)
			return nil
		}
	}
}

// discardStale drops a client whose probe failed and schedules its replacement.
func (p *DriverPool) discardStale(c *sftp.Client, err error) {
	log.Printf("[SFTP] Pool: stale connection on checkout (%v), replacing in background", err)
	p.closeClient(c)
	p.alive.Add(-1)
	go p.fillPool(1)
}

// settleProbe waits for a checkout probe the caller abandoned. A silent connection is ended by the stall
// watchdog when configured; without one, a probe that never answers is capped at handshakeTimeout so the
// slot cannot leak.
func (p *DriverPool) settleProbe(c *sftp.Client, probe <-chan error) {
	var err error
	if p.opts.StallTimeout > 0 {
		err = <-probe
	} else {
		t := time.NewTimer(handshakeTimeout)
		select {
		case err = <-probe:
			t.Stop()
		case <-t.C:
			err = fmt.Errorf("checkout probe unanswered after %v", handshakeTimeout)
		}
	}
	if err != nil {
		p.discardStale(c, err)
		return
	}
	p.end(c)
	p.park(c)
}

// unavailableNow reports that waiting for a client is pointless: no connection is alive and either the
// auth breaker is holding all dials back, or the latest dial failed moments ago (network outage).
func (p *DriverPool) unavailableNow() bool {
	if p.alive.Load() > 0 {
		return false
	}
	p.breakerMu.Lock()
	defer p.breakerMu.Unlock()
	if time.Until(p.pausedUntil) > 0 {
		return true
	}
	return !p.lastDialErr.IsZero() && time.Since(p.lastDialErr) < p.opts.RecentFailWindow
}

func (p *DriverPool) putClient(c *sftp.Client) {
	if c == nil {
		// nil client from failed getClient — try to restore pool
		go p.fillPool(1)
		return
	}

	if _, err := c.Getwd(); err != nil {
		log.Printf("[SFTP] Pool: dead connection on return, discarding")
		p.closeClient(c)
		p.alive.Add(-1)
		go p.fillPool(1)
		return
	}

	p.end(c)
	p.park(c)
}

// park returns a healthy client to the pool, or closes it if the pool is closed. Close may drain the
// channel between our check and our send, so after sending the stop flag is checked again and anything
// left in the channel is closed: no connection outlives Close.
func (p *DriverPool) park(c *sftp.Client) bool {
	select {
	case <-p.stopCh:
		p.closeClient(c)
		return false
	default:
	}
	select {
	case p.clients <- c:
	case <-p.stopCh:
		p.closeClient(c)
		return false
	}
	select {
	case <-p.stopCh:
		p.drain()
		return false
	default:
		return true
	}
}

func (p *DriverPool) drain() {
	for {
		select {
		case c := <-p.clients:
			p.closeClient(c)
		default:
			return
		}
	}
}

func (p *DriverPool) Close() {
	p.stopOnce.Do(func() {
		close(p.stopCh)
		p.drain()
	})
}

func (p *DriverPool) Alive() int {
	return int(p.alive.Load())
}

// ---------------------------------------------------------------------------
// PoolStorageDriver — uses pool for all operations
// ---------------------------------------------------------------------------

type PoolStorageDriver struct {
	Pool *DriverPool
}

func (d *PoolStorageDriver) Name() string { return "sftp-pool" }

// PoolSize lets callers size worker pools to the available connections.
func (d *PoolStorageDriver) PoolSize() int { return d.Pool.Size() }

func (d *PoolStorageDriver) GetContent(ctx context.Context, path string) ([]byte, error) {
	client := d.Pool.getClientCtx(ctx)
	if client == nil {
		return nil, ErrUnavailable
	}
	defer d.Pool.putClient(client)
	f, err := client.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

func (d *PoolStorageDriver) PutContent(ctx context.Context, path string, content []byte, progressCb ...func(written, total int64)) error {
	client := d.Pool.getClientCtx(ctx)
	if client == nil {
		return ErrUnavailable
	}
	defer d.Pool.putClient(client)
	dir := pathpkg.Dir(path)
	if err := ensureDirWithClient(client, dir); err != nil {
		return err
	}
	f, err := client.Create(path)
	if err != nil {
		return err
	}
	var writeErr error
	defer func() {
		f.Close()
		if writeErr != nil {
			_ = client.Remove(path)
		}
	}()
	total := int64(len(content))
	written := int64(0)
	chunk := int64(256 * 1024)
	nextPercent := int64(10)
	for written < total {
		toWrite := chunk
		if total-written < chunk {
			toWrite = total - written
		}
		n, err := f.Write(content[written : written+toWrite])
		if err != nil {
			writeErr = err
			break
		}
		written += int64(n)
		if len(progressCb) > 0 && progressCb[0] != nil {
			percent := written * 100 / total
			if percent >= nextPercent || written == total {
				progressCb[0](written, total)
				nextPercent += 10
			}
		}
	}
	if writeErr != nil {
		if se, ok := writeErr.(*sftp.StatusError); ok && se.Code == uint32(sftp.ErrSSHFxFailure) {
			if _, hasExt := client.HasExtension("statvfs@openssh.com"); hasExt {
				fsinfo, ferr := client.StatVFS(dir)
				if ferr == nil {
					fmt.Printf("[SFTP] StatVFS: Free=%d, Favail=%d, Files=%d\n", fsinfo.FreeSpace(), fsinfo.Favail, fsinfo.Files)
					if fsinfo.Favail == 0 || fsinfo.Frsize*fsinfo.Bavail < uint64(len(content)) {
						return fmt.Errorf("SFTP: no space left on device (ENOSPC)")
					}
				}
			}
		}
		checkNoSpace(client, dir, int64(len(content)), writeErr)
		return writeErr
	}
	return nil
}

func (d *PoolStorageDriver) CreateRepositoryFolder(ctx context.Context, repoName string) error {
	return retrySFTPOperation(fmt.Sprintf("CreateRepositoryFolder(%s)", repoName), 5, func() error {
		client := d.Pool.getClientCtx(ctx)
		if client == nil {
			return ErrUnavailable
		}
		defer d.Pool.putClient(client)
		repoPath := "registry/" + repoName
		if err := createDirRecursiveWithClient(client, repoPath); err != nil {
			return fmt.Errorf("create repo folder: %w", err)
		}
		for _, sub := range []string{"/blobs", "/blobs/uploads", "/manifests"} {
			if err := createDirRecursiveWithClient(client, repoPath+sub); err != nil {
				return fmt.Errorf("create %s: %w", sub, err)
			}
		}
		return nil
	})
}

func (d *PoolStorageDriver) CreateGroupFolder(ctx context.Context, groupName string) error {
	return retrySFTPOperation(fmt.Sprintf("CreateGroupFolder(%s)", groupName), 5, func() error {
		client := d.Pool.getClientCtx(ctx)
		if client == nil {
			return ErrUnavailable
		}
		defer d.Pool.putClient(client)
		return createDirRecursiveWithClient(client, "registry/"+groupName)
	})
}

// DeleteRepositoryFolder holds a connection for one round trip per file (minutes for a big repository on
// a high-latency link), so it runs inside the long-operation budget like uploads and downloads: it can
// never take the last connection away from HEADs and manifest reads.
func (d *PoolStorageDriver) DeleteRepositoryFolder(ctx context.Context, repoName string) error {
	return d.withBulkClient(ctx, func(c *sftp.Client) error {
		return deleteDirRecursiveWithClient(c, "registry/"+repoName)
	})
}

func (d *PoolStorageDriver) Stat(ctx context.Context, path string) (FileInfo, error) {
	client := d.Pool.getClientCtx(ctx)
	if client == nil {
		return nil, ErrUnavailable
	}
	defer d.Pool.putClient(client)
	return client.Stat(path)
}

func (d *PoolStorageDriver) List(ctx context.Context, path string) ([]string, error) {
	client := d.Pool.getClientCtx(ctx)
	if client == nil {
		return nil, ErrUnavailable
	}
	defer d.Pool.putClient(client)
	fis, err := client.ReadDir(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, fi := range fis {
		out = append(out, fi.Name())
	}
	return out, nil
}

func (d *PoolStorageDriver) Move(ctx context.Context, sourcePath string, destPath string) error {
	client := d.Pool.getClientCtx(ctx)
	if client == nil {
		return ErrUnavailable
	}
	defer d.Pool.putClient(client)
	if err := ensureDirWithClient(client, pathpkg.Dir(destPath)); err != nil {
		return err
	}
	return client.Rename(sourcePath, destPath)
}

func (d *PoolStorageDriver) Delete(ctx context.Context, path string) error {
	client := d.Pool.getClientCtx(ctx)
	if client == nil {
		return ErrUnavailable
	}
	defer d.Pool.putClient(client)
	return client.Remove(path)
}

func (d *PoolStorageDriver) RedirectURL(r *http.Request, path string) (string, error) {
	return "", nil
}

func (d *PoolStorageDriver) Walk(ctx context.Context, path string, f WalkFn, options ...func(*WalkOptions)) error {
	client := d.Pool.getClientCtx(ctx)
	if client == nil {
		return ErrUnavailable
	}
	defer d.Pool.putClient(client)
	return walkRecursiveWithClient(client, path, f)
}

// Reader checks out a client and returns a pipelined reader starting at offset; the client returns to the
// pool on Close. Reads keep several requests in flight (see prefetchReader) instead of one round trip per
// io.Copy buffer.
func (d *PoolStorageDriver) Reader(ctx context.Context, path string, offset int64) (io.ReadCloser, error) {
	// A read slot is waited for as long as the caller is (a queued download just starts later); the
	// connection itself is still bounded by the acquire timeout.
	select {
	case d.Pool.long <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	release := func() { <-d.Pool.long }
	client := d.Pool.getClientCtx(ctx)
	if client == nil {
		release()
		return nil, ErrUnavailable
	}
	f, err := client.Open(path)
	if err != nil {
		err = d.Pool.annotate(client, err)
		d.Pool.putClient(client)
		release()
		return nil, err
	}
	// Idle until the first Read: the consumer (an HTTP client) may be slow, which is not a server stall.
	d.Pool.end(client)
	return &prefetchReader{file: f, pool: d.Pool, client: client, next: offset, release: release}, nil
}

const (
	// One SFTP packet per chunk (pkg/sftp's default max packet). The server answers requests in arrival
	// order, so multi-packet chunks issued concurrently interleave and the first chunk completes late;
	// single-packet chunks issued in order give the first byte after one round trip.
	prefetchChunk = 32 << 10
	prefetchDepth = 64 // 2 MiB in flight: one full x/crypto SSH channel window
)

// prefetchReader streams a remote file with up to prefetchDepth chunk reads outstanding, delivered in order.
// Why not plain File.Read: each call is a blocking round trip, so with io.Copy's 32 KiB buffer a 0.5 s RTT
// caps throughput at ~64 KB/s. Why not File.WriteTo: it pipelines well but pushes straight into the
// destination, so a slow HTTP client would stall it and look like a dead server to the watchdog, and the
// first byte waits for a whole batch. Small chunks with a bounded window give fast first bytes, a full SSH
// window, and in-flight accounting per request.
type prefetchReader struct {
	file   *sftp.File
	pool   *DriverPool
	client *sftp.Client

	next    int64 // offset of the next chunk to request
	queue   []*prefetchChunkReq
	turn    chan struct{} // closed when the previously issued chunk has started its request
	cur     *prefetchChunkReq
	curPos  int
	err     error
	wg      sync.WaitGroup
	closed  bool
	release func()
}

type prefetchChunkReq struct {
	buf  []byte
	n    int
	err  error
	done chan struct{}
}

func (r *prefetchReader) fill() {
	for len(r.queue) < prefetchDepth {
		req := &prefetchChunkReq{buf: make([]byte, prefetchChunk), done: make(chan struct{})}
		off := r.next
		r.next += prefetchChunk
		r.queue = append(r.queue, req)
		prev := r.turn
		mine := make(chan struct{})
		r.turn = mine
		r.wg.Add(1)
		r.pool.begin(r.client)
		go func() {
			defer r.wg.Done()
			defer r.pool.end(r.client)
			// Keep request order close to offset order (see prefetchChunk).
			if prev != nil {
				<-prev
			}
			close(mine)
			req.n, req.err = r.file.ReadAt(req.buf, off)
			close(req.done)
		}()
	}
}

func (r *prefetchReader) Read(p []byte) (int, error) {
	for {
		if r.cur != nil {
			if r.curPos < r.cur.n {
				n := copy(p, r.cur.buf[r.curPos:r.cur.n])
				r.curPos += n
				return n, nil
			}
			// A short chunk (or any error, including io.EOF) ends the stream after its data is delivered.
			if r.cur.err != nil || r.cur.n < len(r.cur.buf) {
				r.err = r.cur.err
				if r.err == nil {
					r.err = io.EOF
				}
				r.err = r.pool.annotate(r.client, r.err)
			}
			r.cur = nil
		}
		if r.err != nil {
			return 0, r.err
		}
		r.fill()
		r.cur, r.queue = r.queue[0], r.queue[1:]
		r.curPos = 0
		<-r.cur.done
	}
}

func (r *prefetchReader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	// Outstanding chunk reads must finish before the client goes back to the pool; they end quickly
	// because they either complete or fail once the watchdog closes a stalled transport.
	r.wg.Wait()
	r.pool.begin(r.client)
	err := r.file.Close()
	r.pool.putClient(r.client)
	r.release()
	return err
}

// Writer checks out a client and wraps it so client returns to pool on Close/Commit.
func (d *PoolStorageDriver) Writer(ctx context.Context, path string, appendMode bool) (FileWriter, error) {
	client := d.Pool.getClientCtx(ctx)
	if client == nil {
		return nil, ErrUnavailable
	}
	dir := strings.TrimSuffix(path, "/"+filepathBase(path))
	if err := ensureDirWithClient(client, dir); err != nil {
		d.Pool.putClient(client)
		return nil, err
	}
	flag := os.O_WRONLY | os.O_CREATE
	if appendMode {
		flag |= os.O_APPEND
	} else {
		flag |= os.O_TRUNC
	}
	f, err := client.OpenFile(path, flag)
	if err != nil {
		d.Pool.putClient(client)
		return nil, err
	}
	// Idle between Writes: the caller may be feeding us from a slow HTTP body.
	d.Pool.end(client)
	return &poolFileWriter{file: f, pool: d.Pool, client: client}, nil
}

// poolFileWriter returns the SFTP client to the pool when closed.
type poolFileWriter struct {
	file   *sftp.File
	pool   *DriverPool
	client *sftp.Client
	closed bool
}

func (fw *poolFileWriter) Write(p []byte) (int, error) {
	fw.pool.begin(fw.client)
	defer fw.pool.end(fw.client)
	n, err := fw.file.Write(p)
	return n, fw.pool.annotate(fw.client, err)
}
func (fw *poolFileWriter) Size() int64 {
	fw.pool.begin(fw.client)
	defer fw.pool.end(fw.client)
	fi, err := fw.file.Stat()
	if err != nil {
		return 0
	}
	return fi.Size()
}
func (fw *poolFileWriter) Close() error {
	if fw.closed {
		return nil
	}
	fw.closed = true
	fw.pool.begin(fw.client)
	err := fw.file.Close()
	fw.pool.putClient(fw.client)
	return err
}

// ---------------------------------------------------------------------------
// UploadFile — atomic, optionally multi-connection upload of a local file
// ---------------------------------------------------------------------------

// maxParallelParts caps how many connections one blob may use. The effective count is also bounded by the
// pool's shared long-operation slots (pool size - 1), so one connection always stays free for metadata calls.
const maxParallelParts = 4

// partAttempts is how often one range of a parallel upload is retried (each time on whatever connection
// the pool hands out, so a stalled one is replaced) before the whole upload falls back to a single stream.
const partAttempts = 3

// UploadFile copies localPath to remotePath without ever exposing a partial file there: data is written to
// remotePath+".uploading-"+token, its size is verified, and only then is it renamed into place. A stable
// token (the spool job id) lets a resumed job overwrite its own leftover temp file instead of leaking a new
// one. With skipIfSameSize, an existing remotePath of the same size counts as done; only pass it for
// content-addressed paths (a re-pushed tag can have the same length and different bytes).
// Callers retry on error; the temp file is rewritten from scratch each time.
func (d *PoolStorageDriver) UploadFile(ctx context.Context, localPath, remotePath, token string, skipIfSameSize bool) error {
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	size := st.Size()

	if skipIfSameSize {
		var existing int64 = -1
		_ = d.withClient(ctx, func(c *sftp.Client) error {
			if fi, err := c.Stat(remotePath); err == nil {
				existing = fi.Size()
			}
			return nil
		})
		if existing == size {
			return nil
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	tmp := remotePath + ".uploading-" + token
	parts := cap(d.Pool.long)
	if parts > maxParallelParts {
		parts = maxParallelParts
	}
	thr := d.Pool.opts.ParallelThreshold
	uploaded := false
	if thr > 0 && size >= thr && parts > 1 {
		if err := d.uploadParallel(ctx, f, size, tmp, parts); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("[SFTP] Parallel upload of %s failed (%v), falling back to a single stream", remotePath, err)
		} else {
			uploaded = true
		}
	}
	if !uploaded {
		if err := d.uploadSingle(ctx, f, size, tmp); err != nil {
			d.removeQuiet(ctx, tmp)
			return err
		}
	}
	if err := d.finalize(ctx, tmp, remotePath, size); err != nil {
		d.removeQuiet(ctx, tmp)
		return err
	}
	return nil
}

// withBulkClient is withClient for upload traffic, bounded by the pool's shared long-operation slots.
func (d *PoolStorageDriver) withBulkClient(ctx context.Context, fn func(*sftp.Client) error) error {
	select {
	case d.Pool.long <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-d.Pool.long }()
	return d.withClient(ctx, fn)
}

// withClient runs fn on a pooled client, honouring ctx while waiting for one.
func (d *PoolStorageDriver) withClient(ctx context.Context, fn func(*sftp.Client) error) error {
	c := d.Pool.getClientCtx(ctx)
	if c == nil {
		if err := ctx.Err(); err != nil {
			return err
		}
		return ErrUnavailable
	}
	err := d.Pool.annotate(c, fn(c))
	d.Pool.putClient(c)
	return err
}

// ReaderWithDeadline is Reader for a background scan that must not be able to hang: the underlying
// transport gets a read deadline that stays in force for as long as the returned reader is open, so a
// link that stops delivering fails the read instead of blocking forever. Reader itself cannot promise
// that — its context is only honoured while waiting for a pooled connection, and a stall mid-stream is
// only caught when a watchdog is configured.
//
// It counts against the pool's shared long-operation budget, exactly as Reader does. A verification scan
// is bulk traffic like any other: without this, a few concurrent heals (or an audit, which runs for
// hours) could take every connection and leave HEADs, Stats and folder creation timing out.
func (d *PoolStorageDriver) ReaderWithDeadline(ctx context.Context, path string, offset int64, timeout time.Duration) (io.ReadCloser, error) {
	select {
	case d.Pool.long <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	release := func() { <-d.Pool.long }

	c := d.Pool.getClientCtx(ctx)
	if c == nil {
		release()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, ErrUnavailable
	}
	if timeout > 0 {
		if err := d.Pool.setDeadline(c, time.Now().Add(timeout)); err != nil {
			release()
			d.Pool.putClient(c)
			return nil, err
		}
	}
	f, err := c.Open(path)
	if err != nil {
		_ = d.Pool.setDeadline(c, time.Time{})
		release()
		d.Pool.putClient(c)
		return nil, d.Pool.annotate(c, err)
	}
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			f.Close()
			_ = d.Pool.setDeadline(c, time.Time{})
			release()
			d.Pool.putClient(c)
			return nil, err
		}
	}
	return &deadlineReader{ReadCloser: f, pool: d.Pool, client: c, armed: timeout > 0, release: release}, nil
}

// deadlineReader keeps the deadline in place while the file is being read and clears it before the
// connection goes back to the pool, so a caller that forgets to close does not poison the pool. The long
// budget slot is handed back exactly once, with the connection.
type deadlineReader struct {
	io.ReadCloser
	pool    *DriverPool
	client  *sftp.Client
	armed   bool
	closed  bool
	release func()
}

func (r *deadlineReader) Read(p []byte) (int, error) { return r.ReadCloser.Read(p) }

func (r *deadlineReader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	err := r.ReadCloser.Close()
	if r.armed {
		_ = r.pool.setDeadline(r.client, time.Time{})
	}
	r.pool.putClient(r.client)
	// The long slot goes back last, so the connection is already home when the budget frees up.
	if r.release != nil {
		r.release()
	}
	return err
}

// openForWrite opens path, creating parent directories only when the first attempt says they are missing:
// probing every path component up front costs one round trip each, which is seconds on a slow link.
func openForWrite(c *sftp.Client, path string, flags int) (*sftp.File, error) {
	f, err := c.OpenFile(path, flags)
	if err == nil || flags&os.O_CREATE == 0 || !isNotExist(err) {
		return f, err
	}
	if err := ensureDirWithClient(c, pathpkg.Dir(path)); err != nil {
		return nil, err
	}
	return c.OpenFile(path, flags)
}

// ctxSectionReader stops an in-progress ReadFrom when ctx ends. It keeps Size() so pkg/sftp still sizes
// its concurrent-write window from it.
type ctxSectionReader struct {
	ctx context.Context
	*io.SectionReader
}

func (r ctxSectionReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.SectionReader.Read(p)
}

func (d *PoolStorageDriver) uploadSingle(ctx context.Context, src *os.File, size int64, tmp string) error {
	return d.withBulkClient(ctx, func(c *sftp.Client) error {
		rf, err := openForWrite(c, tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
		if err != nil {
			return err
		}
		_, werr := rf.ReadFrom(ctxSectionReader{ctx, io.NewSectionReader(src, 0, size)})
		cerr := rf.Close()
		if werr != nil {
			return werr
		}
		return cerr
	})
}

// uploadParallel writes size bytes as `parts` ranges, each through its own pooled connection, into one temp
// file. This is the throughput lever: every SSH connection is capped near window/RTT (see clientOptions),
// while the network path itself carries several such flows.
func (d *PoolStorageDriver) uploadParallel(ctx context.Context, src *os.File, size int64, tmp string, parts int) error {
	// Create/truncate once up front; the range writers open without O_TRUNC so they cannot wipe each other.
	if err := d.withBulkClient(ctx, func(c *sftp.Client) error {
		rf, err := openForWrite(c, tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
		if err != nil {
			return err
		}
		return rf.Close()
	}); err != nil {
		return err
	}

	partSize := (size + int64(parts) - 1) / int64(parts)
	errs := make([]error, parts)
	var wg sync.WaitGroup
	for i := 0; i < parts; i++ {
		off := int64(i) * partSize
		n := partSize
		if off+n > size {
			n = size - off
		}
		if n <= 0 {
			continue
		}
		wg.Add(1)
		go func(i int, off, n int64) {
			defer wg.Done()
			var err error
			for attempt := 1; attempt <= partAttempts; attempt++ {
				if ctx.Err() != nil {
					err = ctx.Err()
					break
				}
				err = d.withBulkClient(ctx, func(c *sftp.Client) error {
					rf, err := c.OpenFile(tmp, os.O_WRONLY)
					if err != nil {
						return err
					}
					if _, err := rf.Seek(off, io.SeekStart); err != nil {
						rf.Close()
						return err
					}
					_, werr := rf.ReadFrom(ctxSectionReader{ctx, io.NewSectionReader(src, off, n)})
					cerr := rf.Close()
					if werr != nil {
						return werr
					}
					return cerr
				})
				if err == nil {
					break
				}
				log.Printf("[SFTP] Parallel part %d/%d of %s (offset %d) attempt %d failed: %v", i+1, parts, tmp, off, attempt, err)
			}
			errs[i] = err
		}(i, off, n)
	}
	wg.Wait()
	return errors.Join(errs...)
}

// finalize verifies the temp file size and renames it over remotePath.
func (d *PoolStorageDriver) finalize(ctx context.Context, tmp, remotePath string, size int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return d.withBulkClient(ctx, func(c *sftp.Client) error {
		fi, err := c.Stat(tmp)
		if err != nil {
			return err
		}
		if fi.Size() != size {
			return fmt.Errorf("uploaded size mismatch for %s: remote %d, local %d", tmp, fi.Size(), size)
		}
		// posix-rename replaces the target atomically. Plain SFTP rename refuses to overwrite, so without
		// the extension the old file is removed first (a reader can briefly miss it, never see a partial).
		if _, ok := c.HasExtension("posix-rename@openssh.com"); ok {
			if err := c.PosixRename(tmp, remotePath); err == nil {
				return nil
			}
		}
		if err := c.Remove(remotePath); err != nil && !isNotExist(err) {
			return err
		}
		return c.Rename(tmp, remotePath)
	})
}

func (d *PoolStorageDriver) removeQuiet(ctx context.Context, p string) {
	if ctx.Err() != nil {
		// Shutting down: leave the temp file; a resumed job with the same token truncates it.
		return
	}
	_ = d.withClient(ctx, func(c *sftp.Client) error { return c.Remove(p) })
}
func (fw *poolFileWriter) Cancel(ctx context.Context) error { return fw.Close() }
func (fw *poolFileWriter) Commit(ctx context.Context) error { return nil }

// ---------------------------------------------------------------------------
// Single-connection Driver (legacy, non-pooled)
// ---------------------------------------------------------------------------

type Driver struct {
	client *sftp.Client
}

func NewDriverWithConfig(cfg *config.Config) (*Driver, error) {
	hk, err := hostKeyCallback(cfg)
	if err != nil {
		return nil, fmt.Errorf("FTP_KNOWN_HOSTS: %w", err)
	}
	addr := cfg.FTPHost + ":" + cfg.FTPPort
	sshConfig := &ssh.ClientConfig{
		User:            cfg.FTPUsername,
		Auth:            []ssh.AuthMethod{ssh.Password(cfg.FTPPassword)},
		HostKeyCallback: hk,
		Timeout:         dialTimeout,
	}
	conn, err := dialSSHBounded(addr, sshConfig)
	if err != nil {
		return nil, err
	}
	client, err := sftp.NewClient(conn)
	if err != nil {
		return nil, err
	}
	return &Driver{client: client}, nil
}

func NewDriver() (*Driver, error) {
	return NewDriverWithConfig(&config.Config{
		FTPHost: "localhost", FTPPort: "22", FTPUsername: "user", FTPPassword: "password",
	})
}

func (d *Driver) Name() string                                             { return "sftp" }
func (d *Driver) RedirectURL(r *http.Request, path string) (string, error) { return "", nil }
func (d *Driver) Stat(ctx context.Context, path string) (FileInfo, error)  { return d.client.Stat(path) }
func (d *Driver) Delete(ctx context.Context, path string) error            { return d.client.Remove(path) }
func (d *Driver) Move(ctx context.Context, src string, dst string) error {
	d.ensureDir(pathpkg.Dir(dst))
	return d.client.Rename(src, dst)
}

func (d *Driver) GetContent(ctx context.Context, path string) ([]byte, error) {
	f, err := d.client.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

func (d *Driver) PutContent(ctx context.Context, path string, content []byte, progressCb ...func(written, total int64)) error {
	dir := pathpkg.Dir(path)
	if err := d.ensureDir(dir); err != nil {
		return err
	}
	f, err := d.client.Create(path)
	if err != nil {
		return err
	}
	var writeErr error
	defer func() {
		f.Close()
		if writeErr != nil {
			_ = d.client.Remove(path)
		}
	}()
	total := int64(len(content))
	written := int64(0)
	chunk := int64(256 * 1024)
	nextPercent := int64(10)
	for written < total {
		toWrite := chunk
		if total-written < chunk {
			toWrite = total - written
		}
		n, err := f.Write(content[written : written+toWrite])
		if err != nil {
			writeErr = err
			break
		}
		written += int64(n)
		if len(progressCb) > 0 && progressCb[0] != nil {
			percent := written * 100 / total
			if percent >= nextPercent || written == total {
				progressCb[0](written, total)
				nextPercent += 10
			}
		}
	}
	if writeErr != nil {
		checkNoSpace(d.client, dir, int64(len(content)), writeErr)
		return writeErr
	}
	return nil
}

func (d *Driver) Reader(ctx context.Context, path string, offset int64) (io.ReadCloser, error) {
	f, err := d.client.Open(path)
	if err != nil {
		return nil, err
	}
	if offset > 0 {
		if _, err = f.Seek(offset, io.SeekStart); err != nil {
			f.Close()
			return nil, err
		}
	}
	return f, nil
}

func (d *Driver) Writer(ctx context.Context, path string, appendMode bool) (FileWriter, error) {
	dir := strings.TrimSuffix(path, "/"+filepathBase(path))
	if err := d.ensureDir(dir); err != nil {
		return nil, err
	}
	flag := os.O_WRONLY | os.O_CREATE
	if appendMode {
		flag |= os.O_APPEND
	} else {
		flag |= os.O_TRUNC
	}
	f, err := d.client.OpenFile(path, flag)
	if err != nil {
		return nil, err
	}
	return &sftpFileWriter{file: f}, nil
}

func (d *Driver) List(ctx context.Context, path string) ([]string, error) {
	fis, err := d.client.ReadDir(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, fi := range fis {
		out = append(out, fi.Name())
	}
	return out, nil
}

func (d *Driver) Walk(ctx context.Context, path string, f WalkFn, options ...func(*WalkOptions)) error {
	return d.walkRecursive(path, f)
}

func (d *Driver) CreateRepositoryFolder(ctx context.Context, repoName string) error {
	repoPath := "registry/" + repoName
	if err := d.createDirRecursive(repoPath); err != nil {
		return fmt.Errorf("create repo folder: %w", err)
	}
	for _, sub := range []string{"/blobs", "/blobs/uploads", "/manifests"} {
		if err := d.createDirRecursive(repoPath + sub); err != nil {
			return fmt.Errorf("create %s: %w", sub, err)
		}
	}
	return nil
}

func (d *Driver) CreateGroupFolder(ctx context.Context, groupName string) error {
	return d.createDirRecursive("registry/" + groupName)
}

func (d *Driver) DeleteRepositoryFolder(ctx context.Context, repoName string) error {
	return d.deleteDirRecursive("registry/" + repoName)
}

func (d *Driver) walkRecursive(path string, fn WalkFn) error {
	fis, err := d.client.ReadDir(path)
	if err != nil {
		return err
	}
	for _, fi := range fis {
		full := path + "/" + fi.Name()
		if err := fn(fi); err != nil {
			return err
		}
		if fi.IsDir() {
			if err := d.walkRecursive(full, fn); err != nil {
				return err
			}
		}
	}
	return nil
}

func (d *Driver) deleteDirRecursive(dir string) error {
	if dir == "" || dir == "." || dir == "/" {
		return nil
	}
	fis, err := d.client.ReadDir(dir)
	if err != nil {
		if isNotExist(err) {
			return nil
		}
		return err
	}
	for _, fi := range fis {
		fullPath := dir + "/" + fi.Name()
		if fi.IsDir() {
			if err := d.deleteDirRecursive(fullPath); err != nil {
				return err
			}
		} else {
			if err := d.client.Remove(fullPath); err != nil && !isNotExist(err) {
				return err
			}
		}
	}
	if err := d.client.Remove(dir); err != nil && !isNotExist(err) {
		return err
	}
	return nil
}

func (d *Driver) createDirRecursive(dir string) error {
	return createDirRecursiveWithClient(d.client, dir)
}

func (d *Driver) ensureDir(dir string) error {
	return ensureDirWithClient(d.client, dir)
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

type sftpFileWriter struct {
	file   *sftp.File
	closed bool
}

func (fw *sftpFileWriter) Write(p []byte) (int, error) { return fw.file.Write(p) }
func (fw *sftpFileWriter) Size() int64 {
	fi, err := fw.file.Stat()
	if err != nil {
		return 0
	}
	return fi.Size()
}
func (fw *sftpFileWriter) Close() error {
	fw.closed = true
	return fw.file.Close()
}
func (fw *sftpFileWriter) Cancel(ctx context.Context) error { return fw.Close() }
func (fw *sftpFileWriter) Commit(ctx context.Context) error { return nil }

// IsNotExist reports whether err means the remote path does not exist (as opposed to a storage failure).
func IsNotExist(err error) bool { return isNotExist(err) }

// SweepStaleTemps removes "*.uploading-*" files under root whose modification time is older than
// olderThan: leftovers of uploads that crashed before renaming. Live uploads keep touching their temp
// file, and a resumed spool job truncates its own, so an age of hours is safe. Each directory listing
// takes a pooled connection only briefly, so the sweep never monopolises the pool.
func (d *PoolStorageDriver) SweepStaleTemps(ctx context.Context, root string, olderThan time.Duration) (int, error) {
	cutoff := time.Now().Add(-olderThan)
	removed := 0
	dirs := []string{root}
	for len(dirs) > 0 {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		dir := dirs[len(dirs)-1]
		dirs = dirs[:len(dirs)-1]
		var fis []os.FileInfo
		err := d.withClient(ctx, func(c *sftp.Client) error {
			var err error
			fis, err = c.ReadDir(dir)
			return err
		})
		if err != nil {
			if isNotExist(err) {
				continue
			}
			return removed, err
		}
		for _, fi := range fis {
			full := dir + "/" + fi.Name()
			if fi.IsDir() {
				dirs = append(dirs, full)
				continue
			}
			if strings.Contains(fi.Name(), ".uploading-") && fi.ModTime().Before(cutoff) {
				if err := d.withClient(ctx, func(c *sftp.Client) error { return c.Remove(full) }); err == nil {
					removed++
				}
			}
		}
	}
	return removed, nil
}

func isNotExist(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "does not exist") || strings.Contains(s, "no such file")
}

func createDirRecursiveWithClient(client *sftp.Client, dir string) error {
	if dir == "" || dir == "." || dir == "/" {
		return nil
	}
	parts := strings.Split(dir, "/")
	current := ""
	for _, p := range parts {
		if p == "" {
			continue
		}
		current = pathpkg.Join(current, p)
		if _, err := client.Stat(current); err != nil {
			if err := client.Mkdir(current); err != nil && !strings.Contains(err.Error(), "file exists") {
				return err
			}
		}
	}
	return nil
}

func ensureDirWithClient(client *sftp.Client, dir string) error {
	return createDirRecursiveWithClient(client, dir)
}

func deleteDirRecursiveWithClient(client *sftp.Client, dir string) error {
	if dir == "" || dir == "." || dir == "/" {
		return nil
	}
	fis, err := client.ReadDir(dir)
	if err != nil {
		if isNotExist(err) {
			return nil
		}
		return err
	}
	for _, fi := range fis {
		fullPath := dir + "/" + fi.Name()
		if fi.IsDir() {
			if err := deleteDirRecursiveWithClient(client, fullPath); err != nil {
				return err
			}
		} else {
			if err := client.Remove(fullPath); err != nil && !isNotExist(err) {
				return err
			}
		}
	}
	if err := client.Remove(dir); err != nil && !isNotExist(err) {
		return err
	}
	return nil
}

func walkRecursiveWithClient(client *sftp.Client, path string, fn WalkFn) error {
	fis, err := client.ReadDir(path)
	if err != nil {
		return err
	}
	for _, fi := range fis {
		full := path + "/" + fi.Name()
		if err := fn(fi); err != nil {
			return err
		}
		if fi.IsDir() {
			if err := walkRecursiveWithClient(client, full, fn); err != nil {
				return err
			}
		}
	}
	return nil
}

func filepathBase(p string) string {
	if p == "" {
		return ""
	}
	parts := strings.Split(p, "/")
	return parts[len(parts)-1]
}

// IsConnectionLost returns true if the error is a transient SFTP connection failure.
func IsConnectionLost(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "connection lost") ||
		strings.Contains(s, "connection refused") ||
		strings.Contains(s, "EOF") ||
		strings.Contains(s, "i/o timeout") ||
		strings.Contains(s, "ssh: connection closed") ||
		strings.Contains(s, "use of closed network connection") ||
		strings.Contains(s, "request failed")
}

// retrySFTPOperation wraps an SFTP operation with retry on transient failures.
// If all retries fail, the last error is returned.
func retrySFTPOperation(desc string, maxRetries int, fn func() error) error {
	var lastErr error
	for i := 0; i < maxRetries; i++ {
		err := fn()
		if err == nil {
			return nil
		}
		lastErr = err
		if !IsConnectionLost(err) {
			return err
		}
		backoff := time.Duration(1<<i) * time.Second
		if backoff > 16*time.Second {
			backoff = 16 * time.Second
		}
		log.Printf("[SFTP] Retry %d/%d %s failed: %v, retrying in %v", i+1, maxRetries, desc, err, backoff)
		time.Sleep(backoff)
	}
	return fmt.Errorf("%s: all %d retries failed: %w", desc, maxRetries, lastErr)
}

func checkNoSpace(client *sftp.Client, dir string, size int64, origErr error) {
	e, ok := origErr.(*sftp.StatusError)
	if !ok || e.Code != uint32(sftp.ErrSSHFxFailure) {
		return
	}
	if _, hasExt := client.HasExtension("statvfs@openssh.com"); !hasExt {
		return
	}
	fsinfo, ferr := client.StatVFS(dir)
	if ferr != nil {
		return
	}
	if fsinfo.Favail == 0 || fsinfo.Frsize*fsinfo.Bavail < uint64(size) {
		log.Printf("[SFTP] ENOSPC: dir=%s, needed=%d, free=%d", dir, size, fsinfo.FreeSpace())
	}
}
