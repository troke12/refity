package registry

import (
	"net/http"
	"refity/internal/config"
	"refity/internal/driver/sftp"
)

var (
	storageDriver sftp.StorageDriver
)

func NewRouterWithDeps(driver sftp.StorageDriver, c *config.Config) http.Handler {
	storageDriver = driver
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", RegistryHandler)
	return mux
} 