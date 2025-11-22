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

func NewWebRouter(sftpDriver sftp.StorageDriver, db *database.Database, username, password string) *WebRouter {
	return &WebRouter{
		handler: NewWebHandler(sftpDriver, db, username, password),
	}
}

func (r *WebRouter) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	path := req.URL.Path

	// Login routes
	if path == "/login" {
		r.handler.LoginHandler(w, req)
		return
	}

	// Logout route
	if path == "/logout" || path == "/api/logout" {
		r.handler.LogoutHandler(w, req)
		return
	}

	// API login route
	if path == "/api/login" && req.Method == http.MethodPost {
		r.handler.LoginHandler(w, req)
		return
	}

	// Dashboard routes (require auth)
	if path == "/" || path == "/dashboard" {
		// Check if user is logged in
		cookie, err := req.Cookie("refity_auth")
		if err != nil || cookie == nil || cookie.Value == "" {
			http.Redirect(w, req, "/login", http.StatusFound)
			return
		}
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
