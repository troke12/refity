package sftp

import (
	"context"
	"io"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"refity/internal/config"
)

// TODO: Ganti import berikut jika sudah tahu path module Go yang benar
// import storagedriver "distribution/registry/storage/driver"

// Sementara, definisikan interface StorageDriver minimal agar tidak error

type StorageDriver interface {
	Name() string
	GetContent(ctx context.Context, path string) ([]byte, error)
	PutContent(ctx context.Context, path string, content []byte) error
	Reader(ctx context.Context, path string, offset int64) (io.ReadCloser, error)
	Writer(ctx context.Context, path string, append bool) (FileWriter, error)
	Stat(ctx context.Context, path string) (FileInfo, error)
	List(ctx context.Context, path string) ([]string, error)
	Move(ctx context.Context, sourcePath string, destPath string) error
	Delete(ctx context.Context, path string) error
	RedirectURL(r *http.Request, path string) (string, error)
	Walk(ctx context.Context, path string, f WalkFn, options ...func(*WalkOptions)) error
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

func (d *Driver) PutContent(ctx context.Context, path string, content []byte) error {
	dir := strings.TrimSuffix(path, "/"+filepathBase(path))
	if err := d.ensureDir(dir); err != nil {
		return err
	}
	f, err := d.client.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(content)
	return err
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
	dir := strings.TrimSuffix(destPath, "/"+filepathBase(destPath))
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
	current := ""
	for _, p := range parts {
		if p == "" {
			continue
		}
		current = path.Join(current, p)
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