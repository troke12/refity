package web

import (
	"log"
	"net/http"
	"os"
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

	// Static files
	if strings.HasPrefix(path, "/static/") {
		// Debug: log the request
		log.Printf("Static file request: %s", path)
		
		// Check if file exists
		filePath := strings.TrimPrefix(path, "/static/")
		fullPath := "static/" + filePath
		
		// Try to open the file to check if it exists
		if _, err := os.Stat(fullPath); os.IsNotExist(err) {
			log.Printf("File not found: %s", fullPath)
			http.NotFound(w, req)
			return
		}
		
		// Create a custom file server with proper MIME types
		fs := http.FileServer(http.Dir("static"))
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Set proper MIME types for static files
			if strings.HasSuffix(r.URL.Path, ".css") {
				w.Header().Set("Content-Type", "text/css")
			} else if strings.HasSuffix(r.URL.Path, ".js") {
				w.Header().Set("Content-Type", "application/javascript")
			} else if strings.HasSuffix(r.URL.Path, ".png") {
				w.Header().Set("Content-Type", "image/png")
			} else if strings.HasSuffix(r.URL.Path, ".jpg") || strings.HasSuffix(r.URL.Path, ".jpeg") {
				w.Header().Set("Content-Type", "image/jpeg")
			} else if strings.HasSuffix(r.URL.Path, ".svg") {
				w.Header().Set("Content-Type", "image/svg+xml")
			}
			fs.ServeHTTP(w, r)
		})
		http.StripPrefix("/static/", handler).ServeHTTP(w, req)
		return
	}

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
			r.handler.APIGetRepositoriesHandler(w, req)
			return
		} else if req.Method == http.MethodPost {
			// POST /api/repositories (create new repository)
			r.handler.APICreateRepositoryHandler(w, req)
			return
		}
	}

	// 404 for unknown routes
	http.NotFound(w, req)
}
