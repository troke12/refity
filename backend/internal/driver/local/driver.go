package local

import (
	"context"
	"errors"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
)

var ErrPathTraversal = errors.New("path escapes root")

type StorageDriver interface {
	Name() string
	GetContent(ctx context.Context, path string) ([]byte, error)
	PutContent(ctx context.Context, path string, content []byte, progressCb ...func(written, total int64)) error
	Writer(ctx context.Context, path string) (io.WriteCloser, error)
	WriterAppend(ctx context.Context, path string) (io.WriteCloser, error)
	Size(ctx context.Context, path string) (int64, error)
	List(ctx context.Context, path string) ([]string, error)
	Move(ctx context.Context, sourcePath string, destPath string) error
	Delete(ctx context.Context, path string) error
}

type Driver struct {
	root string
}

func NewDriver(root string) *Driver {
	return &Driver{root: root}
}

func (d *Driver) Name() string { return "local" }

// fullPath returns path under d.root; returns error if p escapes root (path traversal).
func (d *Driver) fullPath(p string) (string, error) {
	cleaned := filepath.Clean(p)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", ErrPathTraversal
	}
	absRoot, _ := filepath.Abs(d.root)
	joined := filepath.Join(d.root, cleaned)
	absPath, _ := filepath.Abs(joined)
	rel, err := filepath.Rel(absRoot, absPath)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", ErrPathTraversal
	}
	return joined, nil
}

func (d *Driver) GetContent(ctx context.Context, path string) ([]byte, error) {
	fp, err := d.fullPath(path)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(fp)
}

func (d *Driver) PutContent(ctx context.Context, path string, content []byte, progressCb ...func(written, total int64)) error {
	fp, err := d.fullPath(path)
	if err != nil {
		return err
	}
	dir := filepath.Dir(fp)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(fp, content, 0o644)
}

func (d *Driver) Writer(ctx context.Context, path string) (io.WriteCloser, error) {
	fp, err := d.fullPath(path)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(fp)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return os.Create(fp)
}

// WriterAppend opens path for appending (creates if not exists). Use for chunked blob uploads (multiple PATCHes).
func (d *Driver) WriterAppend(ctx context.Context, path string) (io.WriteCloser, error) {
	fp, err := d.fullPath(path)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(fp)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(fp, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
}

// Size returns the size in bytes of the file at path. Returns 0, nil if file does not exist.
func (d *Driver) Size(ctx context.Context, path string) (int64, error) {
	fp, err := d.fullPath(path)
	if err != nil {
		return 0, err
	}
	fi, err := os.Stat(fp)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	return fi.Size(), nil
}

func (d *Driver) List(ctx context.Context, path string) ([]string, error) {
	fp, err := d.fullPath(path)
	if err != nil {
		return nil, err
	}
	fis, err := ioutil.ReadDir(fp)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, fi := range fis {
		out = append(out, fi.Name())
	}
	return out, nil
}

func (d *Driver) Move(ctx context.Context, sourcePath, destPath string) error {
	src, err := d.fullPath(sourcePath)
	if err != nil {
		return err
	}
	dst, err := d.fullPath(destPath)
	if err != nil {
		return err
	}
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.Rename(src, dst)
}

func (d *Driver) Delete(ctx context.Context, path string) error {
	fp, err := d.fullPath(path)
	if err != nil {
		return err
	}
	return os.RemoveAll(fp)
} 