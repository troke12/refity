package web

import (
	"net/http"
	"strings"
	"refity/internal/driver/sftp"
	"refity/internal/database"
)

type WebRouter struct {
	handler *WebHandler
}

func NewWebRouter(sftpDriver sftp.StorageDriver, db *database.Database) *WebRouter {
	return &WebRouter{
		handler: NewWebHandler(sftpDriver, db),
	}
}

func (r *WebRouter) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	path := req.URL.Path

	// Dashboard routes
	if path == "/" || path == "/dashboard" {
		r.handler.DashboardHandler(w, req)
		return
	}

	// API routes
	if strings.HasPrefix(path, "/api/repositories") {
		if strings.Contains(path, "/tags/") && req.Method == http.MethodDelete {
			// DELETE /api/repositories/{repo}/tags/{tag}
			r.handler.APIDeleteTagHandler(w, req)
			return
		} else if req.Method == http.MethodDelete {
			// DELETE /api/repositories/{repo}
			r.handler.APIDeleteRepositoryHandler(w, req)
			return
		} else if req.Method == http.MethodGet {
			// GET /api/repositories
			r.handler.APIRepositoriesHandler(w, req)
			return
		}
	}

	// 404 for unknown routes
	http.NotFound(w, req)
}
