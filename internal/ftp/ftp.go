package ftp

import (
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"github.com/pkg/sftp"
)

type SFTPClient struct {
	client *sftp.Client
}

func NewSFTPClient(host, port, user, pass string) (*SFTPClient, error) {
	addr := fmt.Sprintf("%s:%s", host, port)
	config := &ssh.ClientConfig{
		User: user,
		Auth: []ssh.AuthMethod{
			ssh.Password(pass),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout: 10 * time.Second,
	}
	conn, err := ssh.Dial("tcp", addr, config)
	if err != nil {
		return nil, fmt.Errorf("failed to dial SSH: %w", err)
	}
	client, err := sftp.NewClient(conn)
	if err != nil {
		return nil, fmt.Errorf("failed to create SFTP client: %w", err)
	}
	return &SFTPClient{client: client}, nil
}

func (f *SFTPClient) ensureDir(dirPath string) error {
	dirPath = strings.TrimLeft(dirPath, "/")
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
		current = strings.TrimLeft(current, "/")
		if _, err := f.client.Stat(current); os.IsNotExist(err) {
			if err := f.client.Mkdir(current); err != nil {
				log.Printf("ensureDir: failed to create subfolder '%s': %v", current, err)
				return err
			}
		}
	}
	return nil
}

func (f *SFTPClient) Upload(filePath string, data []byte) error {
	filePath = strings.TrimLeft(filePath, "/")
	dir := path.Dir(filePath)
	dir = strings.TrimLeft(dir, "/")
	if err := f.ensureDir(dir); err != nil {
		return err
	}
	file, err := f.client.Create(filePath)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.Write(data)
	return err
}

func (f *SFTPClient) Download(filePath string) ([]byte, error) {
	filePath = strings.TrimLeft(filePath, "/")
	file, err := f.client.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(file)
}

func (f *SFTPClient) List(pathStr string) ([]os.FileInfo, error) {
	pathStr = strings.TrimLeft(pathStr, "/")
	return f.client.ReadDir(pathStr)
}

func (f *SFTPClient) Rename(from, to string) error {
	from = strings.TrimLeft(from, "/")
	to = strings.TrimLeft(to, "/")
	return f.client.Rename(from, to)
}

func (f *SFTPClient) Close() error {
	return f.client.Close()
} 