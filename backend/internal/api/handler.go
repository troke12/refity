package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"refity/backend/internal/driver/sftp"
	"refity/backend/internal/database"
	"refity/backend/internal/config"
	"log"
	"sync"
	"time"
)

type APIHandler struct {
	sftpDriver    sftp.StorageDriver
	db            *database.Database
	config        *config.Config
	cache         map[string]cachedData
	cacheMutex    sync.RWMutex
	ftpUsageCache *cachedFTPUsage
	ftpCacheMutex sync.RWMutex
	lastUpdate    time.Time
}

type cachedData struct {
	data      DashboardData
	timestamp time.Time
}

type cachedFTPUsage struct {
	data            FTPUsageResponse
	timestamp       time.Time
	rateLimitReset  time.Time // When rate limit resets
	rateLimitRemain int       // Remaining requests
}

const cacheDuration = 30 * time.Second // Cache for 30 seconds
const ftpUsageCacheDuration = 5 * time.Minute // Cache FTP usage for 5 minutes (12 requests/hour max, safe for 3600/hour limit)

func NewAPIHandler(sftpDriver sftp.StorageDriver, db *database.Database, cfg *config.Config) *APIHandler {
	return &APIHandler{
		sftpDriver: sftpDriver,
		db:         db,
		config:     cfg,
		cache:      make(map[string]cachedData),
		lastUpdate: time.Now(),
	}
}

type Tag struct {
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"created_at"`
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

// CreateGroupHandler creates a new group
func (h *APIHandler) CreateGroupHandler(w http.ResponseWriter, r *http.Request) {
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

	// Validate group name
	if req.Name == "" {
		http.Error(w, "Group name is required", http.StatusBadRequest)
		return
	}

	// Validate group name format (no slashes allowed)
	if strings.Contains(req.Name, "/") {
		http.Error(w, "Group name cannot contain forward slashes", http.StatusBadRequest)
		return
	}

	// Check if group already exists by checking in database and existing groups
	existingGroups, err := h.db.GetGroups()
	if err != nil {
		log.Printf("Failed to get groups: %v", err)
		http.Error(w, "Failed to check existing groups", http.StatusInternalServerError)
		return
	}

	for _, group := range existingGroups {
		if group == req.Name {
			http.Error(w, "Group already exists", http.StatusConflict)
			return
		}
	}

	// Create group in database first
	err = h.db.CreateGroup(req.Name)
	if err != nil {
		// Check if it's a duplicate key error
		if strings.Contains(err.Error(), "UNIQUE constraint") || strings.Contains(err.Error(), "duplicate") {
			http.Error(w, "Group already exists", http.StatusConflict)
			return
		}
		log.Printf("Failed to create group in database: %v", err)
		http.Error(w, "Failed to create group", http.StatusInternalServerError)
		return
	}

	// Create group folder structure in SFTP
	err = h.sftpDriver.CreateGroupFolder(context.TODO(), req.Name)
	if err != nil {
		log.Printf("Failed to create group folder in SFTP: %v", err)
		// Don't fail the request, just log the error
		// The folder will be created automatically when needed during upload
	} else {
		log.Printf("Successfully created group folder structure for: %s", req.Name)
	}

	// Invalidate cache
	h.cacheMutex.Lock()
	delete(h.cache, "dashboard")
	h.cacheMutex.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"group":   req.Name,
		"message": fmt.Sprintf("Group %s created successfully", req.Name),
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
				Name:      img.Tag,
				Size:      img.Size,
				CreatedAt: img.CreatedAt,
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
			Name:      img.Tag,
			Size:      img.Size,
			CreatedAt: img.CreatedAt,
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

type FTPUsageResponse struct {
	UsedSize     int64   `json:"used_size"`     // size_data in bytes
	TotalSize    int64   `json:"total_size"`    // size in bytes
	UsedSizeTB   float64 `json:"used_size_tb"`  // size_data in TB
	TotalSizeTB  float64 `json:"total_size_tb"` // size in TB
	UsagePercent float64 `json:"usage_percent"` // percentage used
}

func (h *APIHandler) FTPUsageHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Check if Hetzner config is available
	if h.config.HetznerToken == "" || h.config.HetznerBoxID == 0 {
		log.Printf("Hetzner config not available - Token: %v, BoxID: %d", h.config.HetznerToken != "", h.config.HetznerBoxID)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": "Hetzner configuration not available",
		})
		return
	}

	// Check cache first
	h.ftpCacheMutex.RLock()
	if h.ftpUsageCache != nil && time.Since(h.ftpUsageCache.timestamp) < ftpUsageCacheDuration {
		cached := h.ftpUsageCache
		h.ftpCacheMutex.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Cache", "HIT")
		w.Header().Set("X-RateLimit-Remaining", fmt.Sprintf("%d", cached.rateLimitRemain))
		if !cached.rateLimitReset.IsZero() {
			w.Header().Set("X-RateLimit-Reset", fmt.Sprintf("%d", cached.rateLimitReset.Unix()))
		}
		json.NewEncoder(w).Encode(cached.data)
		return
	}
	h.ftpCacheMutex.RUnlock()

	// Cache miss or expired, fetch from API

	// Storage Boxes use a separate API endpoint, not the hcloud-go SDK
	apiURL := fmt.Sprintf("https://api.hetzner.com/v1/storage_boxes/%d", h.config.HetznerBoxID)
	log.Printf("Fetching storage box from: %s", apiURL)
	
	req, err := http.NewRequestWithContext(context.TODO(), "GET", apiURL, nil)
	if err != nil {
		log.Printf("Failed to create request: %v", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": fmt.Sprintf("Failed to create request: %v", err),
		})
		return
	}
	req.Header.Set("Authorization", "Bearer "+h.config.HetznerToken)
	req.Header.Set("Accept", "application/json")

	httpClient := &http.Client{
		Timeout: 10 * time.Second,
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		log.Printf("Failed to get storage box from Hetzner API: %v", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": fmt.Sprintf("Failed to fetch storage box: %v", err),
		})
		return
	}
	defer resp.Body.Close()

	// Read the full response body first
	var bodyBuf bytes.Buffer
	_, err = bodyBuf.ReadFrom(resp.Body)
	if err != nil {
		log.Printf("Failed to read response body: %v", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": fmt.Sprintf("Failed to read response: %v", err),
		})
		return
	}
	
	bodyStr := bodyBuf.String()
	log.Printf("Hetzner API response status: %d", resp.StatusCode)

	// Parse rate limit headers before checking status
	rateLimitRemain := 0
	rateLimitReset := time.Time{}
	if remainStr := resp.Header.Get("RateLimit-Remaining"); remainStr != "" {
		fmt.Sscanf(remainStr, "%d", &rateLimitRemain)
	}
	if resetStr := resp.Header.Get("RateLimit-Reset"); resetStr != "" {
		var resetUnix int64
		if _, err := fmt.Sscanf(resetStr, "%d", &resetUnix); err == nil {
			rateLimitReset = time.Unix(resetUnix, 0)
		}
	}
	log.Printf("Rate limit status - Remaining: %d, Reset: %v", rateLimitRemain, rateLimitReset)

	if resp.StatusCode == http.StatusTooManyRequests {
		// Rate limit exceeded - return cached data if available, or error
		log.Printf("Hetzner API rate limit exceeded (429)")
		h.ftpCacheMutex.RLock()
		if h.ftpUsageCache != nil {
			cached := h.ftpUsageCache
			h.ftpCacheMutex.RUnlock()
			// Return cached data even if expired
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Cache", "STALE")
			w.Header().Set("X-RateLimit-Exceeded", "true")
			w.Header().Set("X-RateLimit-Remaining", fmt.Sprintf("%d", rateLimitRemain))
			if !rateLimitReset.IsZero() {
				w.Header().Set("X-RateLimit-Reset", fmt.Sprintf("%d", rateLimitReset.Unix()))
			}
			json.NewEncoder(w).Encode(cached.data)
			return
		}
		h.ftpCacheMutex.RUnlock()
		
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": "Rate limit exceeded. Please try again later.",
		})
		return
	}

	if resp.StatusCode != http.StatusOK {
		log.Printf("Hetzner API returned non-200 status: %d, response: %s", resp.StatusCode, bodyStr)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": fmt.Sprintf("Hetzner API error: status %d, response: %s", resp.StatusCode, bodyStr),
		})
		return
	}


	// Try to decode the response - handle different possible structures
	var apiResponse map[string]interface{}
	if err := json.Unmarshal(bodyBuf.Bytes(), &apiResponse); err != nil {
		log.Printf("Failed to decode Hetzner API response as JSON: %v, body: %s", err, bodyStr)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": fmt.Sprintf("Failed to decode response: %v", err),
		})
		return
	}

	log.Printf("Decoded API response: %+v", apiResponse)

	// Extract storage_box data - handle nested structure
	var storageBox map[string]interface{}
	if sb, ok := apiResponse["storage_box"].(map[string]interface{}); ok {
		storageBox = sb
	} else {
		// Maybe the response is directly the storage_box?
		storageBox = apiResponse
	}

	// Extract used_size from stats.size_data
	var usedSize int64 = 0
	var totalSize int64 = 0
	
	// Get used_size from stats.size_data
	if stats, ok := storageBox["stats"].(map[string]interface{}); ok {
		if sizeData, ok := stats["size_data"].(float64); ok {
			usedSize = int64(sizeData)
		} else if sizeData, ok := stats["size_data"].(int64); ok {
			usedSize = sizeData
		}
		log.Printf("Storage box used size (stats.size_data): %d bytes", usedSize)
	} else {
		log.Printf("Warning: Storage box stats not found for box ID %d", h.config.HetznerBoxID)
	}
	
	// Get total_size from storage_box.storage_box_type.size
	if storageBoxType, ok := storageBox["storage_box_type"].(map[string]interface{}); ok {
		if size, ok := storageBoxType["size"].(float64); ok {
			totalSize = int64(size)
		} else if size, ok := storageBoxType["size"].(int64); ok {
			totalSize = size
		}
		log.Printf("Storage box total size (storage_box_type.size): %d bytes", totalSize)
	} else {
		// Log available keys for debugging
		keys := make([]string, 0, len(storageBox))
		for k := range storageBox {
			keys = append(keys, k)
		}
		log.Printf("Warning: Storage box type not found for box ID %d. Available keys: %v", h.config.HetznerBoxID, keys)
		// Return error response instead of empty data
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": fmt.Sprintf("Storage box type not found in response for box ID %d", h.config.HetznerBoxID),
		})
		return
	}
	
	// Validate that we got some data
	if totalSize == 0 {
		log.Printf("Warning: Total size is 0, which might indicate an issue")
	}
	
	log.Printf("Storage box - Used: %d bytes, Total: %d bytes", usedSize, totalSize)

	// Convert to TB (1 TB = 1024^4 bytes)
	const bytesPerTB = 1024 * 1024 * 1024 * 1024
	usedSizeTB := float64(usedSize) / float64(bytesPerTB)
	totalSizeTB := float64(totalSize) / float64(bytesPerTB)

	// Calculate usage percentage
	usagePercent := 0.0
	if totalSize > 0 {
		usagePercent = (float64(usedSize) / float64(totalSize)) * 100.0
	}

	response := FTPUsageResponse{
		UsedSize:     usedSize,
		TotalSize:    totalSize,
		UsedSizeTB:   usedSizeTB,
		TotalSizeTB:  totalSizeTB,
		UsagePercent: usagePercent,
	}

	// Cache the response with rate limit info
	h.ftpCacheMutex.Lock()
	h.ftpUsageCache = &cachedFTPUsage{
		data:            response,
		timestamp:       time.Now(),
		rateLimitRemain: rateLimitRemain,
		rateLimitReset:  rateLimitReset,
	}
	h.ftpCacheMutex.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Cache", "MISS")
	w.Header().Set("X-RateLimit-Remaining", fmt.Sprintf("%d", rateLimitRemain))
	if !rateLimitReset.IsZero() {
		w.Header().Set("X-RateLimit-Reset", fmt.Sprintf("%d", rateLimitReset.Unix()))
	}
	json.NewEncoder(w).Encode(response)
}
