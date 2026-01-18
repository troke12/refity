package api

import (
	"net/http"
	"strings"
	"refity/backend/internal/driver/sftp"
	"refity/backend/internal/database"
	"refity/backend/internal/auth"
	"refity/backend/internal/config"
)

type APIRouter struct {
	apiHandler  *APIHandler
	authHandler *AuthHandler
}

func NewAPIRouter(sftpDriver sftp.StorageDriver, db *database.Database, cfg *config.Config) *APIRouter {
	return &APIRouter{
		apiHandler:  NewAPIHandler(sftpDriver, db, cfg),
		authHandler: NewAuthHandler(db),
	}
}

func (r *APIRouter) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	path := req.URL.Path

	// Auth routes (no JWT required)
	if path == "/api/auth/login" && req.Method == http.MethodPost {
		r.authHandler.LoginHandler(w, req)
		return
	}

	if path == "/api/auth/logout" && req.Method == http.MethodPost {
		r.authHandler.LogoutHandler(w, req)
		return
	}

	// Protected routes (require JWT)
	if path == "/api/auth/me" && req.Method == http.MethodGet {
		auth.JWTMiddleware(http.HandlerFunc(r.authHandler.MeHandler)).ServeHTTP(w, req)
		return
	}

	if path == "/api/dashboard" && req.Method == http.MethodGet {
		auth.JWTMiddleware(http.HandlerFunc(r.apiHandler.DashboardHandler)).ServeHTTP(w, req)
		return
	}

	if path == "/api/ftp/usage" && req.Method == http.MethodGet {
		auth.JWTMiddleware(http.HandlerFunc(r.apiHandler.FTPUsageHandler)).ServeHTTP(w, req)
		return
	}

	// Groups routes (require JWT)
	if path == "/api/groups" {
		if req.Method == http.MethodGet {
			auth.JWTMiddleware(http.HandlerFunc(r.apiHandler.GetGroupsHandler)).ServeHTTP(w, req)
			return
		}
		if req.Method == http.MethodPost {
			auth.JWTMiddleware(http.HandlerFunc(r.apiHandler.CreateGroupHandler)).ServeHTTP(w, req)
			return
		}
	}

	// Group repositories routes (require JWT)
	if strings.HasPrefix(path, "/api/groups/") && strings.HasSuffix(path, "/repositories") && req.Method == http.MethodGet {
		auth.JWTMiddleware(http.HandlerFunc(r.apiHandler.GetRepositoriesByGroupHandler)).ServeHTTP(w, req)
		return
	}

	// Repository tags routes (require JWT)
	if strings.HasPrefix(path, "/api/groups/") && strings.HasSuffix(path, "/tags") && req.Method == http.MethodGet {
		auth.JWTMiddleware(http.HandlerFunc(r.apiHandler.GetTagsByRepositoryHandler)).ServeHTTP(w, req)
		return
	}

	// Repository routes (require JWT) - for backward compatibility and create/delete
	if strings.HasPrefix(path, "/api/repositories") {
		// Check if it's a specific repository endpoint
		repoPath := strings.TrimPrefix(path, "/api/repositories")
		if repoPath == "" {
			// POST /api/repositories (create)
			if req.Method == http.MethodPost {
				auth.JWTMiddleware(http.HandlerFunc(r.apiHandler.CreateRepositoryHandler)).ServeHTTP(w, req)
				return
			}
		} else if strings.Contains(repoPath, "/tags/") && req.Method == http.MethodDelete {
			// DELETE /api/repositories/{repo}/tags/{tag}
			auth.JWTMiddleware(http.HandlerFunc(r.apiHandler.DeleteTagHandler)).ServeHTTP(w, req)
			return
		} else if req.Method == http.MethodDelete {
			// DELETE /api/repositories/{repo}
			auth.JWTMiddleware(http.HandlerFunc(r.apiHandler.DeleteRepositoryHandler)).ServeHTTP(w, req)
			return
		}
	}

	// 404 for unknown routes
	http.NotFound(w, req)
}

