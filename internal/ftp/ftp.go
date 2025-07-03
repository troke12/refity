package ftp

import (
	"github.com/jlaffaye/ftp"
	"time"
	"bytes"
	"io"
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

func (f *FTPClient) Upload(path string, data []byte) error {
	return f.conn.Stor(path, bytes.NewReader(data))
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