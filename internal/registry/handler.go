package registry

import (
	"net/http"
)

// Handler untuk endpoint Docker Registry API v2
func RegistryHandler(w http.ResponseWriter, r *http.Request) {
	// TODO: Implementasi push/pull image ke FTP
	w.WriteHeader(http.StatusNotImplemented)
	w.Write([]byte("Registry API not implemented yet"))
} 