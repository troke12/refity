package web

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"refity/internal/driver/sftp"
	"refity/internal/database"
	"log"
	"sync"
	"time"
)

type WebHandler struct {
	sftpDriver sftp.StorageDriver
	db         *database.Database
	cache      map[string]cachedData
	cacheMutex sync.RWMutex
	lastUpdate time.Time
}

type cachedData struct {
	data      DashboardData
	timestamp time.Time
}

const cacheDuration = 30 * time.Second // Cache for 30 seconds

func NewWebHandler(sftpDriver sftp.StorageDriver, db *database.Database) *WebHandler {
	return &WebHandler{
		sftpDriver: sftpDriver,
		db:         db,
		cache:      make(map[string]cachedData),
	}
}



type Tag struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

type Repository struct {
	Name string `json:"name"`
	Tags []Tag  `json:"tags"`
}

type DashboardData struct {
	Repositories []Repository `json:"repositories"`
	TotalImages  int          `json:"total_images"`
	TotalSize    int64        `json:"total_size"`
}

func (h *WebHandler) DashboardHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Check cache first
	h.cacheMutex.RLock()
	if cached, exists := h.cache["dashboard"]; exists && time.Since(cached.timestamp) < cacheDuration {
		h.cacheMutex.RUnlock()
		h.renderDashboard(w, cached.data)
		return
	}
	h.cacheMutex.RUnlock()

	// Get fresh data from SFTP
	data := h.getDashboardData()

	// Cache the result
	h.cacheMutex.Lock()
	h.cache["dashboard"] = cachedData{
		data:      data,
		timestamp: time.Now(),
	}
	h.cacheMutex.Unlock()

	h.renderDashboard(w, data)
}

func (h *WebHandler) renderDashboard(w http.ResponseWriter, data DashboardData) {
	// Parse and execute template with custom functions
	tmpl, err := template.New("dashboard").Funcs(template.FuncMap{
		"formatBytes": func(bytes int64) string {
			if bytes == 0 {
				return "0 Bytes"
			}
			const unit = 1024
			if bytes < unit {
				return fmt.Sprintf("%d Bytes", bytes)
			}
			div, exp := int64(unit), 0
			for n := bytes / unit; n >= unit; n /= unit {
				div *= unit
				exp++
			}
			return fmt.Sprintf("%.2f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
		},
	}).Parse(dashboardTemplate)
	if err != nil {
		log.Printf("Failed to parse template: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	err = tmpl.Execute(w, data)
	if err != nil {
		log.Printf("Failed to execute template: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
	}
}

func (h *WebHandler) getDashboardData() DashboardData {
	// Get statistics from database
	totalImages, totalSize, err := h.db.GetStatistics()
	if err != nil {
		log.Printf("Failed to get statistics: %v", err)
		return DashboardData{}
	}

	// Get repositories from database
	repoNames, err := h.db.GetRepositories()
	if err != nil {
		log.Printf("Failed to get repositories: %v", err)
		return DashboardData{}
	}

	repositories := []Repository{}
	for _, repoName := range repoNames {
		// Get images for this repository
		images, err := h.db.GetImagesByRepository(repoName)
		if err != nil {
			log.Printf("Failed to get images for repository %s: %v", repoName, err)
			continue
		}

		// Convert images to tags
		var tags []Tag
		for _, img := range images {
			tags = append(tags, Tag{
				Name: img.Tag,
				Size: img.Size,
			})
		}

		repository := Repository{
			Name: repoName,
			Tags: tags,
		}
		repositories = append(repositories, repository)
	}

	return DashboardData{
		Repositories: repositories,
		TotalImages:  totalImages,
		TotalSize:    totalSize,
	}
}

func (h *WebHandler) APIRepositoriesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Check cache first
	h.cacheMutex.RLock()
	if cached, exists := h.cache["dashboard"]; exists && time.Since(cached.timestamp) < cacheDuration {
		h.cacheMutex.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"repositories": cached.data.Repositories,
			"total":        len(cached.data.Repositories),
		})
		return
	}
	h.cacheMutex.RUnlock()

	// Get fresh data
	data := h.getDashboardData()

	// Cache the result
	h.cacheMutex.Lock()
	h.cache["dashboard"] = cachedData{
		data:      data,
		timestamp: time.Now(),
	}
	h.cacheMutex.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"repositories": data.Repositories,
		"total":        len(data.Repositories),
	})
}

func (h *WebHandler) APIDeleteTagHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse URL: /api/repositories/{repo}/tags/{tag}
	path := strings.TrimPrefix(r.URL.Path, "/api/repositories/")
	parts := strings.Split(path, "/tags/")
	if len(parts) != 2 {
		http.Error(w, "Invalid URL format", http.StatusBadRequest)
		return
	}

	repo := parts[0]
	tag := parts[1]

	// Delete from database
	err := h.db.DeleteImage(repo, tag)
	if err != nil {
		log.Printf("Failed to delete tag %s for repo %s: %v", tag, repo, err)
		http.Error(w, "Failed to delete tag", http.StatusInternalServerError)
		return
	}

	// Invalidate cache
	h.cacheMutex.Lock()
	delete(h.cache, "dashboard")
	h.cacheMutex.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": fmt.Sprintf("Tag %s deleted from repository %s", tag, repo),
	})
}

func (h *WebHandler) APIDeleteRepositoryHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse URL: /api/repositories/{repo}
	repo := strings.TrimPrefix(r.URL.Path, "/api/repositories/")
	if repo == "" {
		http.Error(w, "Repository name required", http.StatusBadRequest)
		return
	}

	// Delete from database
	err := h.db.DeleteRepository(repo)
	if err != nil {
		log.Printf("Failed to delete repository %s: %v", repo, err)
		http.Error(w, "Failed to delete repository", http.StatusInternalServerError)
		return
	}

	// Invalidate cache
	h.cacheMutex.Lock()
	delete(h.cache, "dashboard")
	h.cacheMutex.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": fmt.Sprintf("Repository %s deleted", repo),
	})
}
