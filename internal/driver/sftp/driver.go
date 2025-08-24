package sftp

import (
	"context"
	"io"
	"net/http"
	"os"
	pathpkg "path"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"refity/internal/config"
	"errors"
	"fmt"
	"log"
)

// TODO: Ganti import berikut jika sudah tahu path module Go yang benar
// import storagedriver "distribution/registry/storage/driver"

// Sementara, definisikan interface StorageDriver minimal agar tidak error

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

// Tambahkan pool SFTP client

type DriverPool struct {
	clients chan *sftp.Client
	cfg     *config.Config
}

func NewDriverPool(cfg *config.Config, poolSize int) (*DriverPool, error) {
	clients := make(chan *sftp.Client, poolSize)
	for i := 0; i < poolSize; i++ {
		addr := cfg.FTPHost + ":" + cfg.FTPPort
		user := cfg.FTPUsername
		pass := cfg.FTPPassword
		sshConfig := &ssh.ClientConfig{
			User: user,
			Auth: []ssh.AuthMethod{
				ssh.Password(pass),
			},
			HostKeyCallback: ssh.InsecureIgnoreHostKey(),
			Timeout: 10 * time.Second,
		}
		conn, err := ssh.Dial("tcp", addr, sshConfig)
		if err != nil {
			return nil, err
		}
		client, err := sftp.NewClient(conn)
		if err != nil {
			return nil, err
		}
		clients <- client
	}
	return &DriverPool{clients: clients, cfg: cfg}, nil
}

func (p *DriverPool) getClient() *sftp.Client {
	return <-p.clients
}

func (p *DriverPool) putClient(c *sftp.Client) {
	p.clients <- c
}

// Implementasi StorageDriver

type PoolStorageDriver struct {
	Pool *DriverPool
}

func (d *PoolStorageDriver) Name() string { return "sftp-pool" }

func (d *PoolStorageDriver) GetContent(ctx context.Context, path string) ([]byte, error) {
	client := d.Pool.getClient()
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
	defer d.Pool.putClient(client)
	dir := pathpkg.Dir(path)
	group := groupFolder(dir)
	if group != "" {
		if _, err := client.Stat(group); err != nil {
			return ErrRepoNotFound
		}
	}
	if err := ensureDirWithClient(client, dir); err != nil {
		return err
	}
	f, err := client.Create(path)
	if err != nil {
		return err
	}
	var writeErr error
	defer func() {
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
				} else {
					fmt.Printf("[SFTP] StatVFS error: %v\n", ferr)
				}
			}
			fmt.Printf("[SFTP] SSH_FX_FAILURE: path=%s, size=%d, error=%v\n", path, len(content), writeErr)
			time.Sleep(1 * time.Second)
		}
		checkNoSpace(client, dir, int64(len(content)), writeErr)
		return writeErr
	}
	return nil
}

// CreateRepositoryFolder creates the complete folder structure for a repository
func (d *PoolStorageDriver) CreateRepositoryFolder(ctx context.Context, repoName string) error {
	client := d.Pool.getClient()
	defer d.Pool.putClient(client)
	
	// Create the main repository folder
	repoPath := "registry/" + repoName
	if err := createDirRecursiveWithClient(client, repoPath); err != nil {
		return fmt.Errorf("failed to create repository folder: %v", err)
	}
	
	// Create subdirectories for blobs, manifests, etc.
	subdirs := []string{
		repoPath + "/blobs",
		repoPath + "/blobs/uploads",
		repoPath + "/manifests",
	}
	
	for _, subdir := range subdirs {
		if err := createDirRecursiveWithClient(client, subdir); err != nil {
			return fmt.Errorf("failed to create subdirectory %s: %v", subdir, err)
		}
	}
	
	return nil
}

// DeleteRepositoryFolder removes the complete folder structure for a repository
func (d *PoolStorageDriver) DeleteRepositoryFolder(ctx context.Context, repoName string) error {
	client := d.Pool.getClient()
	defer d.Pool.putClient(client)
	
	// Delete the main repository folder
	repoPath := "registry/" + repoName
	if err := deleteDirRecursiveWithClient(client, repoPath); err != nil {
		return fmt.Errorf("failed to delete repository folder: %v", err)
	}
	
	return nil
}

// deleteDirRecursiveWithClient deletes directories recursively
func deleteDirRecursiveWithClient(client *sftp.Client, dir string) error {
	if dir == "" || dir == "." || dir == "/" {
		return nil
	}
	
	// First, delete all files and subdirectories
	fis, err := client.ReadDir(dir)
	if err != nil {
		// If directory doesn't exist, consider it already deleted
		return nil
	}
	
	for _, fi := range fis {
		fullPath := dir + "/" + fi.Name()
		if fi.IsDir() {
			// Recursively delete subdirectories
			if err := deleteDirRecursiveWithClient(client, fullPath); err != nil {
				return err
			}
		} else {
			// Delete files
			if err := client.Remove(fullPath); err != nil {
				return fmt.Errorf("failed to delete file %s: %v", fullPath, err)
			}
		}
	}
	
	// Finally, delete the directory itself
	if err := client.Remove(dir); err != nil {
		return fmt.Errorf("failed to delete directory %s: %v", dir, err)
	}
	
	return nil
}

// createDirRecursiveWithClient creates directories recursively without group folder checks
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

// Tambahkan ensureDirWithClient untuk pool
func ensureDirWithClient(client *sftp.Client, dir string) error {
	if dir == "" || dir == "." || dir == "/" {
		return nil
	}
	parts := strings.Split(dir, "/")
	if len(parts) >= 2 && parts[0] == "registry" {
		group := parts[0] + "/" + parts[1]
		if _, err := client.Stat(group); err != nil {
			return ErrRepoNotFound
		}
	}
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

// Implementasi driver SFTP

type Driver struct {
	client *sftp.Client
}

// NewDriverWithConfig membuat koneksi SFTP baru dengan konfigurasi dari config.Config
func NewDriverWithConfig(cfg *config.Config) (*Driver, error) {
	addr := cfg.FTPHost + ":" + cfg.FTPPort
	user := cfg.FTPUsername
	pass := cfg.FTPPassword
	sshConfig := &ssh.ClientConfig{
		User: user,
		Auth: []ssh.AuthMethod{
			ssh.Password(pass),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout: 10 * time.Second,
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

// Deprecated: gunakan NewDriverWithConfig
func NewDriver() (*Driver, error) {
	return NewDriverWithConfig(&config.Config{
		FTPHost: "localhost", FTPPort: "22", FTPUsername: "user", FTPPassword: "password",
	})
}

func (d *Driver) Name() string {
	return "sftp"
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
	group := groupFolder(dir)
	if group != "" {
		if _, err := d.client.Stat(group); err != nil {
			return ErrRepoNotFound
		}
	}
	if err := d.ensureDir(dir); err != nil {
		return err
	}
	f, err := d.client.Create(path)
	if err != nil {
		return err
	}
	var writeErr error
	defer func() {
		if writeErr != nil {
			_ = d.client.Remove(path) // hapus file broken jika gagal
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
		// Cek error SFTP Failure
		if se, ok := writeErr.(*sftp.StatusError); ok && se.Code == uint32(sftp.ErrSSHFxFailure) {
			if _, hasExt := d.client.HasExtension("statvfs@openssh.com"); hasExt {
				fsinfo, ferr := d.client.StatVFS(dir)
				if ferr == nil {
					fmt.Printf("[SFTP] StatVFS: Free=%d, Favail=%d, Files=%d\n", fsinfo.FreeSpace(), fsinfo.Favail, fsinfo.Files)
					if fsinfo.Favail == 0 || fsinfo.Frsize*fsinfo.Bavail < uint64(len(content)) {
						return fmt.Errorf("SFTP: no space left on device (ENOSPC)")
					}
				} else {
					fmt.Printf("[SFTP] StatVFS error: %v\n", ferr)
				}
			}
			fmt.Printf("[SFTP] SSH_FX_FAILURE: path=%s, size=%d, error=%v\n", path, len(content), writeErr)
			time.Sleep(1 * time.Second) // delay sebelum retry
		}
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
		_, err = f.Seek(offset, io.SeekStart)
		if err != nil {
			f.Close()
			return nil, err
		}
	}
	return f, nil
}

func (d *Driver) Writer(ctx context.Context, path string, append bool) (FileWriter, error) {
	dir := strings.TrimSuffix(path, "/"+filepathBase(path))
	if err := d.ensureDir(dir); err != nil {
		return nil, err
	}
	flag := os.O_WRONLY | os.O_CREATE
	if append {
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

func (d *Driver) Stat(ctx context.Context, path string) (FileInfo, error) {
	fi, err := d.client.Stat(path)
	if err != nil {
		return nil, err
	}
	return fi, nil
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

func (d *Driver) Move(ctx context.Context, sourcePath string, destPath string) error {
	dir := pathpkg.Dir(destPath)
	group := groupFolder(dir)
	if group != "" {
		if _, err := d.client.Stat(group); err != nil {
			return ErrRepoNotFound
		}
	}
	if err := d.ensureDir(dir); err != nil {
		return err
	}
	return d.client.Rename(sourcePath, destPath)
}

func (d *Driver) Delete(ctx context.Context, path string) error {
	return d.client.Remove(path)
}

func (d *Driver) RedirectURL(r *http.Request, path string) (string, error) {
	return "", nil // SFTP tidak support direct URL
}

func (d *Driver) Walk(ctx context.Context, path string, f WalkFn, options ...func(*WalkOptions)) error {
	return d.walkRecursive(path, f)
}

// CreateRepositoryFolder creates the complete folder structure for a repository
func (d *Driver) CreateRepositoryFolder(ctx context.Context, repoName string) error {
	// Create the main repository folder
	repoPath := "registry/" + repoName
	if err := d.createDirRecursive(repoPath); err != nil {
		return fmt.Errorf("failed to create repository folder: %v", err)
	}
	
	// Create subdirectories for blobs, manifests, etc.
	subdirs := []string{
		repoPath + "/blobs",
		repoPath + "/blobs/uploads",
		repoPath + "/manifests",
	}
	
	for _, subdir := range subdirs {
		if err := d.createDirRecursive(subdir); err != nil {
			return fmt.Errorf("failed to create subdirectory %s: %v", subdir, err)
		}
	}
	
	return nil
}

// DeleteRepositoryFolder removes the complete folder structure for a repository
func (d *Driver) DeleteRepositoryFolder(ctx context.Context, repoName string) error {
	// Delete the main repository folder
	repoPath := "registry/" + repoName
	if err := d.deleteDirRecursive(repoPath); err != nil {
		return fmt.Errorf("failed to delete repository folder: %v", err)
	}
	
	return nil
}

// deleteDirRecursive deletes directories recursively
func (d *Driver) deleteDirRecursive(dir string) error {
	if dir == "" || dir == "." || dir == "/" {
		return nil
	}
	
	// First, delete all files and subdirectories
	fis, err := d.client.ReadDir(dir)
	if err != nil {
		// If directory doesn't exist, consider it already deleted
		return nil
	}
	
	for _, fi := range fis {
		fullPath := dir + "/" + fi.Name()
		if fi.IsDir() {
			// Recursively delete subdirectories
			if err := d.deleteDirRecursive(fullPath); err != nil {
				return err
			}
		} else {
			// Delete files
			if err := d.client.Remove(fullPath); err != nil {
				return fmt.Errorf("failed to delete file %s: %v", fullPath, err)
			}
		}
	}
	
	// Finally, delete the directory itself
	if err := d.client.Remove(dir); err != nil {
		return fmt.Errorf("failed to delete directory %s: %v", dir, err)
	}
	
	return nil
}

// createDirRecursive creates directories recursively without group folder checks
func (d *Driver) createDirRecursive(dir string) error {
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
		if _, err := d.client.Stat(current); err != nil {
			if err := d.client.Mkdir(current); err != nil && !strings.Contains(err.Error(), "file exists") {
				return err
			}
		}
	}
	return nil
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

func (d *Driver) ensureDir(dir string) error {
	if dir == "" || dir == "." || dir == "/" {
		return nil
	}
	parts := strings.Split(dir, "/")
	if len(parts) >= 2 && parts[0] == "registry" {
		group := parts[0] + "/" + parts[1]
		if _, err := d.client.Stat(group); err != nil {
			return ErrRepoNotFound
		}
	}
	current := ""
	for _, p := range parts {
		if p == "" {
			continue
		}
		current = pathpkg.Join(current, p)
		if _, err := d.client.Stat(current); err != nil {
			if err := d.client.Mkdir(current); err != nil && !strings.Contains(err.Error(), "file exists") {
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

func groupFolder(dir string) string {
	parts := strings.Split(dir, "/")
	if len(parts) >= 2 && parts[0] == "registry" {
		return parts[0] + "/" + parts[1]
	}
	return ""
}

// FileWriter implementasi dasar untuk SFTP

type sftpFileWriter struct {
	file *sftp.File
	closed bool
}

func (fw *sftpFileWriter) Write(p []byte) (int, error) {
	return fw.file.Write(p)
}

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

func (fw *sftpFileWriter) Cancel(ctx context.Context) error {
	fw.Close()
	return nil // SFTP tidak punya cancel atomic, hapus file jika perlu
}

func (fw *sftpFileWriter) Commit(ctx context.Context) error {
	return nil // SFTP: file sudah tersimpan saat write/close
}

// Tambahkan implementasi method yang belum ada di PoolStorageDriver
func (d *PoolStorageDriver) Stat(ctx context.Context, path string) (FileInfo, error) {
	client := d.Pool.getClient()
	defer d.Pool.putClient(client)
	fi, err := client.Stat(path)
	if err != nil {
		return nil, err
	}
	return fi, nil
}

func (d *PoolStorageDriver) List(ctx context.Context, path string) ([]string, error) {
	client := d.Pool.getClient()
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
	defer d.Pool.putClient(client)
	dir := pathpkg.Dir(destPath)
	group := groupFolder(dir)
	if group != "" {
		if _, err := client.Stat(group); err != nil {
			return ErrRepoNotFound
		}
	}
	if err := ensureDirWithClient(client, dir); err != nil {
		return err
	}
	return client.Rename(sourcePath, destPath)
}

func (d *PoolStorageDriver) Delete(ctx context.Context, path string) error {
	client := d.Pool.getClient()
	defer d.Pool.putClient(client)
	return client.Remove(path)
}

func (d *PoolStorageDriver) RedirectURL(r *http.Request, path string) (string, error) {
	return "", nil // SFTP tidak support direct URL
}

func (d *PoolStorageDriver) Walk(ctx context.Context, path string, f WalkFn, options ...func(*WalkOptions)) error {
	client := d.Pool.getClient()
	defer d.Pool.putClient(client)
	return walkRecursiveWithClient(client, path, f)
}

func (d *PoolStorageDriver) Reader(ctx context.Context, path string, offset int64) (io.ReadCloser, error) {
	client := d.Pool.getClient()
	defer d.Pool.putClient(client)
	f, err := client.Open(path)
	if err != nil {
		return nil, err
	}
	if offset > 0 {
		_, err = f.Seek(offset, io.SeekStart)
		if err != nil {
			f.Close()
			return nil, err
		}
	}
	return f, nil
}

func (d *PoolStorageDriver) Writer(ctx context.Context, path string, append bool) (FileWriter, error) {
	client := d.Pool.getClient()
	defer d.Pool.putClient(client)
	dir := strings.TrimSuffix(path, "/"+filepathBase(path))
	if err := ensureDirWithClient(client, dir); err != nil {
		return nil, err
	}
	flag := os.O_WRONLY | os.O_CREATE
	if append {
		flag |= os.O_APPEND
	} else {
		flag |= os.O_TRUNC
	}
	f, err := client.OpenFile(path, flag)
	if err != nil {
		return nil, err
	}
	return &sftpFileWriter{file: f}, nil
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

// Tambahkan fungsi utilitas checkNoSpace
func checkNoSpace(client *sftp.Client, dir string, size int64, origErr error) {
	e, ok := origErr.(*sftp.StatusError)
	if !ok || e.Code != uint32(sftp.ErrSSHFxFailure) {
		return
	}
	if _, hasExt := client.HasExtension("statvfs@openssh.com"); !hasExt {
		return
	}
	fsinfo, err := client.StatVFS(dir)
	if err != nil {
		fmt.Printf("[SFTP] StatVFS returned %v\n", err)
		return
	}
	if fsinfo.Favail == 0 || fsinfo.Frsize*fsinfo.Bavail < uint64(size) {
		log.Printf("sftp: no space left on device")
	}
} 