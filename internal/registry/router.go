package registry

import (
	"net/http"
	"refity/internal/ftp"
	"refity/internal/config"
)

var (
	sftpClient *ftp.SFTPClient
)

func NewRouterWithDeps(f *ftp.SFTPClient, c *config.Config) http.Handler {
	sftpClient = f
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", RegistryHandler)
	return mux
} 