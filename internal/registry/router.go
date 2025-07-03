package registry

import (
	"net/http"
	"refity/internal/ftp"
	"refity/internal/config"
)

var (
	ftpClient *ftp.FTPClient
	cfg       *config.Config
)

func NewRouterWithDeps(f *ftp.FTPClient, c *config.Config) http.Handler {
	ftpClient = f
	cfg = c
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", RegistryHandler)
	return mux
} 