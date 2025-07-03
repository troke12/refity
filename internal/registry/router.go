package registry

import (
	"net/http"
	"refity/internal/ftp"
	"refity/internal/config"
)

var (
	ftpClient *ftp.FTPClient
)

func NewRouterWithDeps(f *ftp.FTPClient, c *config.Config) http.Handler {
	ftpClient = f
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", RegistryHandler)
	return mux
} 