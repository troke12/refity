package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"refity/backend/internal/driver/sftp"
	"refity/backend/internal/database"
	"log"
	"sync"
	"time"
)

type APIHandler struct {
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

func NewAPIHandler(sftpDriver sftp.StorageDriver, db *database.Database) *APIHandler {
	return &APIHandler{
		sftpDriver: sftpDriver,
		db:         db,
		cache:      make(map[string]cachedData),
		lastUpdate: time.Now(),
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

func (h *APIHandler) getDashboardData() DashboardData {
	// Get statistics from database
	totalImages, totalSize, err := h.db.GetStatistics()
	if err != nil {
		log.Printf("Failed to get statistics: %v", err)
		return DashboardData{}
	}

	// Get all repositories (both from images and manually created)
	repoNames, err := h.db.GetRepositories()
	if err != nil {
		log.Printf("Failed to get repositories: %v", err)
		return DashboardData{}
	}

	// Get manually created repositories
	manualRepos, err := h.db.GetAllRepositories()
	if err != nil {
		log.Printf("Failed to get manual repositories: %v", err)
		return DashboardData{}
	}

	// Create a map of all repository names
	repoMap := make(map[string]bool)
	for _, repoName := range repoNames {
		repoMap[repoName] = true
	}
	for _, repo := range manualRepos {
		repoMap[repo.Name] = true
	}

	repositories := []Repository{}
	for repoName := range repoMap {
		// Get images for this repository
		images, err := h.db.GetImagesByRepository(repoName)
		if err != nil {
			log.Printf("Failed to get images for repository %s: %v", repoName, err)
			// Even if no images, show the repository if it was manually created
			repositories = append(repositories, Repository{
				Name: repoName,
				Tags: []Tag{},
			})
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

func (h *APIHandler) DashboardHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Check cache first
	h.cacheMutex.RLock()
	if cached, exists := h.cache["dashboard"]; exists && time.Since(cached.timestamp) < cacheDuration {
		h.cacheMutex.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(cached.data)
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
	h.lastUpdate = time.Now()
	h.cacheMutex.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func (h *APIHandler) GetRepositoriesHandler(w http.ResponseWriter, r *http.Request) {
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

func (h *APIHandler) CreateRepositoryHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse JSON body
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	// Validate repository name
	if req.Name == "" {
		http.Error(w, "Repository name is required", http.StatusBadRequest)
		return
	}

	// Check if repository already exists
	if _, err := h.db.GetRepository(req.Name); err == nil {
		http.Error(w, "Repository already exists", http.StatusConflict)
		return
	}

	// Create repository in database
	repo, err := h.db.CreateRepository(req.Name)
	if err != nil {
		log.Printf("Failed to create repository in database: %v", err)
		http.Error(w, "Failed to create repository", http.StatusInternalServerError)
		return
	}

	// Create repository folder structure in SFTP
	err = h.sftpDriver.CreateRepositoryFolder(context.TODO(), req.Name)
	if err != nil {
		log.Printf("Failed to create repository folder in SFTP: %v", err)
		// Don't fail the request, just log the error
		// The folder will be created automatically when needed during upload
	} else {
		log.Printf("Successfully created repository folder structure for: %s", req.Name)
	}

	// Invalidate cache
	h.cacheMutex.Lock()
	delete(h.cache, "dashboard")
	h.cacheMutex.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":    true,
		"repository": repo,
		"message":    fmt.Sprintf("Repository %s created successfully", req.Name),
	})
}

func (h *APIHandler) DeleteRepositoryHandler(w http.ResponseWriter, r *http.Request) {
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
	
	// URL decode the repository name
	decodedRepo, err := url.QueryUnescape(repo)
	if err != nil {
		http.Error(w, "Invalid repository name encoding", http.StatusBadRequest)
		return
	}
	repo = decodedRepo

	// Delete from database
	err = h.db.DeleteRepository(repo)
	if err != nil {
		log.Printf("Failed to delete repository %s: %v", repo, err)
		http.Error(w, "Failed to delete repository", http.StatusInternalServerError)
		return
	}

	// Delete repository folder structure from SFTP
	err = h.sftpDriver.DeleteRepositoryFolder(context.TODO(), repo)
	if err != nil {
		log.Printf("Failed to delete repository folder in SFTP: %v", err)
		// Don't fail the request, just log the error
		// The folder can be deleted manually if needed
	} else {
		log.Printf("Successfully deleted repository folder structure for: %s", repo)
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

func (h *APIHandler) DeleteTagHandler(w http.ResponseWriter, r *http.Request) {
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

	repo, err1 := url.QueryUnescape(parts[0])
	if err1 != nil {
		http.Error(w, "Invalid repository name encoding", http.StatusBadRequest)
		return
	}
	
	tag, err2 := url.QueryUnescape(parts[1])
	if err2 != nil {
		http.Error(w, "Invalid tag name encoding", http.StatusBadRequest)
		return
	}

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

