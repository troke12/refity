package sftp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
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
}

func hostKeyCallback(cfg *config.Config) (ssh.HostKeyCallback, error) {
	if cfg.FTPKnownHosts != "" {
		return knownhosts.New(cfg.FTPKnownHosts)
	}
	log.Println("WARNING: FTP_KNOWN_HOSTS not set. SSH host key verification is DISABLED. This is vulnerable to MITM attacks. Set FTP_KNOWN_HOSTS in production.")
	return ssh.InsecureIgnoreHostKey(), nil
}

func NewDriverPool(cfg *config.Config, poolSize int) (*DriverPool, error) {
	pool := &DriverPool{
		clients:  make(chan *sftp.Client, poolSize),
		cfg:      cfg,
		poolSize: poolSize,
		stopCh:   make(chan struct{}),
	}

	connected := 0
	for i := 0; i < poolSize; i++ {
		client, err := pool.newClient()
		if err != nil {
			log.Printf("[SFTP] Pool: initial connection %d/%d failed: %v", i+1, poolSize, err)
			continue
		}
		pool.clients <- client
		connected++
	}

	if connected == 0 {
		return nil, fmt.Errorf("SFTP pool: no connections established to %s:%s", cfg.FTPHost, cfg.FTPPort)
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

func (p *DriverPool) newClient() (*sftp.Client, error) {
	hk, err := hostKeyCallback(p.cfg)
	if err != nil {
		return nil, err
	}
	addr := p.cfg.FTPHost + ":" + p.cfg.FTPPort
	sshConfig := &ssh.ClientConfig{
		User:            p.cfg.FTPUsername,
		Auth:            []ssh.AuthMethod{ssh.Password(p.cfg.FTPPassword)},
		HostKeyCallback: hk,
		Timeout:         10 * time.Second,
	}
	conn, err := ssh.Dial("tcp", addr, sshConfig)
	if err != nil {
		return nil, fmt.Errorf("ssh dial %s: %w", addr, err)
	}
	client, err := sftp.NewClient(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("sftp new client: %w", err)
	}
	return client, nil
}

// fillPool tries to bring the pool back to full capacity in background.
func (p *DriverPool) fillPool(count int) {
	for i := 0; i < count; i++ {
		for attempt := 1; attempt <= 10; attempt++ {
			select {
			case <-p.stopCh:
				return
			default:
			}
			client, err := p.newClient()
			if err != nil {
				log.Printf("[SFTP] Pool: background fill attempt %d failed: %v", attempt, err)
				time.Sleep(time.Duration(attempt) * 5 * time.Second)
				continue
			}
			p.clients <- client
			p.alive.Add(1)
			log.Printf("[SFTP] Pool: background fill succeeded, pool at %d", p.alive.Load())
			break
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
			if _, err := c.Getwd(); err != nil {
				c.Close()
				dead++
			} else {
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
	client := <-p.clients

	if _, err := client.Getwd(); err != nil {
		log.Printf("[SFTP] Pool: stale connection on checkout, reconnecting...")
		client.Close()
		p.alive.Add(-1)

		for attempt := 1; attempt <= 3; attempt++ {
			newClient, err := p.newClient()
			if err == nil {
				p.alive.Add(1)
				log.Printf("[SFTP] Pool: reconnected on attempt %d (pool: %d alive)", attempt, p.alive.Load())
				return newClient
			}
			log.Printf("[SFTP] Pool: reconnect attempt %d/3 failed: %v", attempt, err)
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
		}

		// All retries exhausted — one final attempt
		finalClient, err := p.newClient()
		if err != nil {
			log.Printf("[SFTP] Pool: all reconnects failed, pool degraded to %d", p.alive.Load())
			// Spawn background recovery
			go p.fillPool(1)
			// Return nil — callers must handle this
			return nil
		}
		p.alive.Add(1)
		return finalClient
	}

	return client
}

func (p *DriverPool) putClient(c *sftp.Client) {
	if c == nil {
		// nil client from failed getClient — try to restore pool
		go p.fillPool(1)
		return
	}

	if _, err := c.Getwd(); err != nil {
		log.Printf("[SFTP] Pool: dead connection on return, discarding")
		c.Close()
		p.alive.Add(-1)
		go p.fillPool(1)
		return
	}

	p.clients <- c
}

func (p *DriverPool) Close() {
	p.stopOnce.Do(func() {
		close(p.stopCh)
		for {
			select {
			case c := <-p.clients:
				c.Close()
			default:
				return
			}
		}
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

func (d *PoolStorageDriver) GetContent(ctx context.Context, path string) ([]byte, error) {
	client := d.Pool.getClient()
	if client == nil {
		return nil, fmt.Errorf("SFTP unavailable: no connections in pool")
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
	client := d.Pool.getClient()
	if client == nil {
		return fmt.Errorf("SFTP unavailable: no connections in pool")
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
	client := d.Pool.getClient()
	if client == nil {
		return fmt.Errorf("SFTP unavailable")
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
}

func (d *PoolStorageDriver) CreateGroupFolder(ctx context.Context, groupName string) error {
	client := d.Pool.getClient()
	if client == nil {
		return fmt.Errorf("SFTP unavailable")
	}
	defer d.Pool.putClient(client)
	return createDirRecursiveWithClient(client, "registry/"+groupName)
}

func (d *PoolStorageDriver) DeleteRepositoryFolder(ctx context.Context, repoName string) error {
	client := d.Pool.getClient()
	if client == nil {
		return fmt.Errorf("SFTP unavailable")
	}
	defer d.Pool.putClient(client)
	return deleteDirRecursiveWithClient(client, "registry/"+repoName)
}

func (d *PoolStorageDriver) Stat(ctx context.Context, path string) (FileInfo, error) {
	client := d.Pool.getClient()
	if client == nil {
		return nil, fmt.Errorf("SFTP unavailable")
	}
	defer d.Pool.putClient(client)
	return client.Stat(path)
}

func (d *PoolStorageDriver) List(ctx context.Context, path string) ([]string, error) {
	client := d.Pool.getClient()
	if client == nil {
		return nil, fmt.Errorf("SFTP unavailable")
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
	client := d.Pool.getClient()
	if client == nil {
		return fmt.Errorf("SFTP unavailable")
	}
	defer d.Pool.putClient(client)
	if err := ensureDirWithClient(client, pathpkg.Dir(destPath)); err != nil {
		return err
	}
	return client.Rename(sourcePath, destPath)
}

func (d *PoolStorageDriver) Delete(ctx context.Context, path string) error {
	client := d.Pool.getClient()
	if client == nil {
		return fmt.Errorf("SFTP unavailable")
	}
	defer d.Pool.putClient(client)
	return client.Remove(path)
}

func (d *PoolStorageDriver) RedirectURL(r *http.Request, path string) (string, error) {
	return "", nil
}

func (d *PoolStorageDriver) Walk(ctx context.Context, path string, f WalkFn, options ...func(*WalkOptions)) error {
	client := d.Pool.getClient()
	if client == nil {
		return fmt.Errorf("SFTP unavailable")
	}
	defer d.Pool.putClient(client)
	return walkRecursiveWithClient(client, path, f)
}

// Reader checks out a client and wraps it so client returns to pool on Close.
func (d *PoolStorageDriver) Reader(ctx context.Context, path string, offset int64) (io.ReadCloser, error) {
	client := d.Pool.getClient()
	if client == nil {
		return nil, fmt.Errorf("SFTP unavailable")
	}
	f, err := client.Open(path)
	if err != nil {
		d.Pool.putClient(client)
		return nil, err
	}
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			f.Close()
			d.Pool.putClient(client)
			return nil, err
		}
	}
	return &poolReadCloser{file: f, pool: d.Pool, client: client}, nil
}

// Writer checks out a client and wraps it so client returns to pool on Close/Commit.
func (d *PoolStorageDriver) Writer(ctx context.Context, path string, appendMode bool) (FileWriter, error) {
	client := d.Pool.getClient()
	if client == nil {
		return nil, fmt.Errorf("SFTP unavailable")
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
	return &poolFileWriter{file: f, pool: d.Pool, client: client}, nil
}

// poolReadCloser returns the SFTP client to the pool when the reader is closed.
type poolReadCloser struct {
	file   *sftp.File
	pool   *DriverPool
	client *sftp.Client
}

func (r *poolReadCloser) Read(p []byte) (int, error) { return r.file.Read(p) }
func (r *poolReadCloser) Close() error {
	err := r.file.Close()
	r.pool.putClient(r.client)
	return err
}

// poolFileWriter returns the SFTP client to the pool when closed.
type poolFileWriter struct {
	file   *sftp.File
	pool   *DriverPool
	client *sftp.Client
	closed bool
}

func (fw *poolFileWriter) Write(p []byte) (int, error) { return fw.file.Write(p) }
func (fw *poolFileWriter) Size() int64 {
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
	err := fw.file.Close()
	fw.pool.putClient(fw.client)
	return err
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
		Timeout:         10 * time.Second,
	}
	conn, err := ssh.Dial("tcp", addr, sshConfig)
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

func (d *Driver) Name() string                                                              { return "sftp" }
func (d *Driver) RedirectURL(r *http.Request, path string) (string, error)                  { return "", nil }
func (d *Driver) Stat(ctx context.Context, path string) (FileInfo, error)                   { return d.client.Stat(path) }
func (d *Driver) Delete(ctx context.Context, path string) error                             { return d.client.Remove(path) }
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
