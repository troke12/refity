package local

import (
	"context"
	"io/ioutil"
	"os"
	"path/filepath"
)

type StorageDriver interface {
	Name() string
	GetContent(ctx context.Context, path string) ([]byte, error)
	PutContent(ctx context.Context, path string, content []byte, progressCb ...func(written, total int64)) error
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

func (d *Driver) fullPath(p string) string {
	return filepath.Join(d.root, p)
}

func (d *Driver) GetContent(ctx context.Context, path string) ([]byte, error) {
	return os.ReadFile(d.fullPath(path))
}

func (d *Driver) PutContent(ctx context.Context, path string, content []byte, progressCb ...func(written, total int64)) error {
	fp := d.fullPath(path)
	dir := filepath.Dir(fp)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(fp, content, 0o644)
}

func (d *Driver) List(ctx context.Context, path string) ([]string, error) {
	fp := d.fullPath(path)
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
	src := d.fullPath(sourcePath)
	dst := d.fullPath(destPath)
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.Rename(src, dst)
}

func (d *Driver) Delete(ctx context.Context, path string) error {
	return os.RemoveAll(d.fullPath(path))
} 