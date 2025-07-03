package ftp

import (
	"github.com/jlaffaye/ftp"
	"time"
	"bytes"
	"io"
	"path"
	"strings"
	"log"
)

type FTPClient struct {
	conn *ftp.ServerConn
}

func NewFTPClient(addr, user, pass string) (*FTPClient, error) {
	c, err := ftp.Dial(addr, ftp.DialWithTimeout(5*time.Second))
	if err != nil {
		return nil, err
	}
	if err := c.Login(user, pass); err != nil {
		return nil, err
	}
	return &FTPClient{conn: c}, nil
}

func (f *FTPClient) ensureDir(dirPath string) error {
	if dirPath == "" || dirPath == "." || dirPath == "/" {
		return nil
	}
	parts := strings.Split(dirPath, "/")
	current := ""
	for _, p := range parts {
		if p == "" {
			continue
		}
		current = path.Join(current, p)
		if err := f.conn.MakeDir(current); err != nil && !strings.Contains(err.Error(), "File exists") {
			log.Printf("ensureDir: failed to create subfolder '%s': %v", current, err)
			return err
		}
	}
	return nil
}

func (f *FTPClient) Upload(filePath string, data []byte) error {
	dir := path.Dir(filePath)
	if err := f.ensureDir(dir); err != nil {
		return err
	}
	return f.conn.Stor(filePath, bytes.NewReader(data))
}

func (f *FTPClient) Download(path string) ([]byte, error) {
	r, err := f.conn.Retr(path)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

func (f *FTPClient) Close() error {
	return f.conn.Quit()
}

func (f *FTPClient) List(path string) ([]*ftp.Entry, error) {
	return f.conn.List(path)
}

func (f *FTPClient) Rename(from, to string) error {
	return f.conn.Rename(from, to)
} 