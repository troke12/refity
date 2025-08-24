package web

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"refity/internal/driver/sftp"
	"context"
	"log"
)

type WebHandler struct {
	sftpDriver sftp.StorageDriver
}

func NewWebHandler(sftpDriver sftp.StorageDriver) *WebHandler {
	return &WebHandler{
		sftpDriver: sftpDriver,
	}
}

// calculateImageSize calculates the total size of an image by summing up all blob sizes
func (h *WebHandler) calculateImageSize(repoName, tagName string) int64 {
	var totalSize int64
	
	// Get manifest to find blob references
	manifestPath := fmt.Sprintf("registry/%s/manifests/%s", repoName, tagName)
	manifestData, err := h.sftpDriver.GetContent(context.TODO(), manifestPath)
	if err != nil {
		log.Printf("Failed to get manifest for %s:%s: %v", repoName, tagName, err)
		return 0
	}
	
	// Parse manifest to get blob references
	var manifest map[string]interface{}
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		log.Printf("Failed to parse manifest for %s:%s: %v", repoName, tagName, err)
		return 0
	}
	
	// Get layers from manifest
	layers, ok := manifest["layers"].([]interface{})
	if !ok {
		log.Printf("No layers found in manifest for %s:%s", repoName, tagName)
		return 0
	}
	
	// Calculate size from each layer
	for _, layer := range layers {
		if layerMap, ok := layer.(map[string]interface{}); ok {
			if digest, ok := layerMap["digest"].(string); ok {
				// Get blob size
				blobPath := fmt.Sprintf("registry/%s/blobs/%s", repoName, digest)
				if stat, err := h.sftpDriver.Stat(context.TODO(), blobPath); err == nil {
					if sftpStat, ok := stat.(interface{ Size() int64 }); ok {
						totalSize += sftpStat.Size()
					}
				}
			}
		}
	}
	
	// Also include config blob size if present
	if config, ok := manifest["config"].(map[string]interface{}); ok {
		if digest, ok := config["digest"].(string); ok {
			blobPath := fmt.Sprintf("registry/%s/blobs/%s", repoName, digest)
			if stat, err := h.sftpDriver.Stat(context.TODO(), blobPath); err == nil {
				if sftpStat, ok := stat.(interface{ Size() int64 }); ok {
					totalSize += sftpStat.Size()
				}
			}
		}
	}
	
	return totalSize
}

// getTagsWithSize gets tags with their sizes for a repository
func (h *WebHandler) getTagsWithSize(repoName string) []Tag {
	manifestDir := fmt.Sprintf("registry/%s/manifests", repoName)
	tagNames, err := h.sftpDriver.List(context.TODO(), manifestDir)
	if err != nil {
		log.Printf("Failed to list tags for %s: %v", repoName, err)
		return []Tag{}
	}
	
	var tags []Tag
	for _, tagName := range tagNames {
		size := h.calculateImageSize(repoName, tagName)
		tags = append(tags, Tag{
			Name: tagName,
			Size: size,
		})
	}
	
	return tags
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

	// Get repositories from SFTP - use same logic as registry handler
	entries, err := h.sftpDriver.List(context.TODO(), "registry")
	if err != nil {
		log.Printf("Failed to list repositories: %v", err)
		http.Error(w, "Failed to load repositories", http.StatusInternalServerError)
		return
	}

	repositories := []Repository{}
	totalImages := 0

	for _, repo := range entries {
		// Check if this is a group folder (like "ochi") or actual repository
		// First try to get manifests directly
		tags := h.getTagsWithSize(repo)
		if len(tags) == 0 {
			// If direct access fails, try to find subdirectories
			log.Printf("Failed to list tags for %s directly, trying subdirectories", repo)
			
			// List subdirectories in the repo folder
			subDirs, err := h.sftpDriver.List(context.TODO(), fmt.Sprintf("registry/%s", repo))
			if err != nil {
				log.Printf("Failed to list subdirectories for %s: %v", repo, err)
				continue
			}
			
			// For each subdirectory, check if it has manifests
			for _, subDir := range subDirs {
				fullRepoName := fmt.Sprintf("%s/%s", repo, subDir)
				subTags := h.getTagsWithSize(fullRepoName)
				if len(subTags) == 0 {
					log.Printf("Failed to list tags for %s", fullRepoName)
					continue
				}
				
				repository := Repository{
					Name: fullRepoName,
					Tags: subTags,
				}
				repositories = append(repositories, repository)
				totalImages += len(subTags)
			}
		} else {
			// Direct repository access worked
			repository := Repository{
				Name: repo,
				Tags: tags,
			}
			repositories = append(repositories, repository)
			totalImages += len(tags)
		}
	}

	// Calculate total size
	var totalSize int64
	for _, repo := range repositories {
		for _, tag := range repo.Tags {
			totalSize += tag.Size
		}
	}

	data := DashboardData{
		Repositories: repositories,
		TotalImages:  totalImages,
		TotalSize:    totalSize,
	}

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

func (h *WebHandler) APIRepositoriesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Get repositories from SFTP - use same logic as registry handler
	entries, err := h.sftpDriver.List(context.TODO(), "registry")
	if err != nil {
		log.Printf("Failed to list repositories: %v", err)
		http.Error(w, "Failed to load repositories", http.StatusInternalServerError)
		return
	}

	repositories := []Repository{}
	for _, repo := range entries {
		// Check if this is a group folder (like "ochi") or actual repository
		// First try to get manifests directly
		tags := h.getTagsWithSize(repo)
		if len(tags) == 0 {
			// If direct access fails, try to find subdirectories
			log.Printf("Failed to list tags for %s directly, trying subdirectories", repo)
			
			// List subdirectories in the repo folder
			subDirs, err := h.sftpDriver.List(context.TODO(), fmt.Sprintf("registry/%s", repo))
			if err != nil {
				log.Printf("Failed to list subdirectories for %s: %v", repo, err)
				continue
			}
			
			// For each subdirectory, check if it has manifests
			for _, subDir := range subDirs {
				fullRepoName := fmt.Sprintf("%s/%s", repo, subDir)
				subTags := h.getTagsWithSize(fullRepoName)
				if len(subTags) == 0 {
					log.Printf("Failed to list tags for %s", fullRepoName)
					continue
				}
				
				repository := Repository{
					Name: fullRepoName,
					Tags: subTags,
				}
				repositories = append(repositories, repository)
			}
		} else {
			// Direct repository access worked
			repository := Repository{
				Name: repo,
				Tags: tags,
			}
			repositories = append(repositories, repository)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"repositories": repositories,
		"total":        len(repositories),
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

	// Delete manifest file
	manifestPath := fmt.Sprintf("registry/%s/manifests/%s", repo, tag)
	err := h.sftpDriver.Delete(context.TODO(), manifestPath)
	if err != nil {
		log.Printf("Failed to delete tag %s for repo %s: %v", tag, repo, err)
		http.Error(w, "Failed to delete tag", http.StatusInternalServerError)
		return
	}

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

	// Delete entire repository directory
	repoPath := fmt.Sprintf("registry/%s", repo)
	err := h.sftpDriver.Delete(context.TODO(), repoPath)
	if err != nil {
		log.Printf("Failed to delete repository %s: %v", repo, err)
		http.Error(w, "Failed to delete repository", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": fmt.Sprintf("Repository %s deleted", repo),
	})
}
