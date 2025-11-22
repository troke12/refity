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
	Groups       []Group      `json:"groups"`
	TotalImages  int          `json:"total_images"`
	TotalSize    int64        `json:"total_size"`
}

type Group struct {
	Name         string `json:"name"`
	Repositories int    `json:"repositories"`
}

func (h *APIHandler) getDashboardData() DashboardData {
	// Get statistics from database
	totalImages, totalSize, err := h.db.GetStatistics()
	if err != nil {
		log.Printf("Failed to get statistics: %v", err)
		return DashboardData{}
	}

	// Get all groups
	groupNames, err := h.db.GetGroups()
	if err != nil {
		log.Printf("Failed to get groups: %v", err)
		return DashboardData{}
	}

	// Build groups with repository count
	groups := []Group{}
	for _, groupName := range groupNames {
		repos, err := h.db.GetRepositoriesByGroup(groupName)
		if err != nil {
			log.Printf("Failed to get repositories for group %s: %v", groupName, err)
			continue
		}
		groups = append(groups, Group{
			Name:         groupName,
			Repositories: len(repos),
		})
	}

	return DashboardData{
		Groups:      groups,
		TotalImages: totalImages,
		TotalSize:   totalSize,
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
		json.NewEncoder(w).Encode(cached.data)
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
	json.NewEncoder(w).Encode(data)
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

// GetGroupsHandler returns all groups
func (h *APIHandler) GetGroupsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	groups, err := h.db.GetGroups()
	if err != nil {
		log.Printf("Failed to get groups: %v", err)
		http.Error(w, "Failed to get groups", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"groups": groups,
		"total":  len(groups),
	})
}

// GetRepositoriesByGroupHandler returns all repositories in a group
func (h *APIHandler) GetRepositoriesByGroupHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract group name from path: /api/groups/{groupName}/repositories
	path := strings.TrimPrefix(r.URL.Path, "/api/groups/")
	groupName := strings.TrimSuffix(path, "/repositories")
	
	// URL decode the group name
	decodedGroup, err := url.QueryUnescape(groupName)
	if err != nil {
		decodedGroup = groupName
	}

	repositories, err := h.db.GetRepositoriesByGroup(decodedGroup)
	if err != nil {
		log.Printf("Failed to get repositories for group %s: %v", decodedGroup, err)
		http.Error(w, "Failed to get repositories", http.StatusInternalServerError)
		return
	}

	// Get repository details with tags
	repoList := []Repository{}
	for _, repoName := range repositories {
		images, err := h.db.GetImagesByRepository(repoName)
		if err != nil {
			log.Printf("Failed to get images for repository %s: %v", repoName, err)
			continue
		}

		var tags []Tag
		var totalSize int64
		for _, img := range images {
			tags = append(tags, Tag{
				Name: img.Tag,
				Size: img.Size,
			})
			totalSize += img.Size
		}

		// Extract repository name without group (e.g., "nginx" from "ochi/nginx")
		repoNameOnly := repoName
		if strings.Contains(repoName, "/") {
			parts := strings.Split(repoName, "/")
			repoNameOnly = parts[len(parts)-1]
		}

		repoList = append(repoList, Repository{
			Name: repoNameOnly,
			Tags: tags,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"group":        decodedGroup,
		"repositories": repoList,
		"total":        len(repoList),
	})
}

// GetTagsByRepositoryHandler returns all tags for a repository
func (h *APIHandler) GetTagsByRepositoryHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract group and repository from path: /api/groups/{groupName}/repositories/{repoName}/tags
	path := strings.TrimPrefix(r.URL.Path, "/api/groups/")
	path = strings.TrimSuffix(path, "/tags")
	parts := strings.Split(path, "/repositories/")
	if len(parts) != 2 {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}

	groupName := parts[0]
	repoNameOnly := parts[1]

	// URL decode
	decodedGroup, err := url.QueryUnescape(groupName)
	if err != nil {
		decodedGroup = groupName
	}
	decodedRepo, err := url.QueryUnescape(repoNameOnly)
	if err != nil {
		decodedRepo = repoNameOnly
	}
	fullRepoName := decodedGroup + "/" + decodedRepo

	images, err := h.db.GetImagesByRepository(fullRepoName)
	if err != nil {
		log.Printf("Failed to get images for repository %s: %v", fullRepoName, err)
		http.Error(w, "Repository not found", http.StatusNotFound)
		return
	}

	var tags []Tag
	for _, img := range images {
		tags = append(tags, Tag{
			Name: img.Tag,
			Size: img.Size,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"group":      decodedGroup,
		"repository": decodedRepo,
		"tags":       tags,
		"total":      len(tags),
	})
}

