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

type Repository struct {
	Name string   `json:"name"`
	Tags []string `json:"tags"`
}

type DashboardData struct {
	Repositories []Repository `json:"repositories"`
	TotalImages  int          `json:"total_images"`
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
		manifestDir := fmt.Sprintf("registry/%s/manifests", repo)
		tags, err := h.sftpDriver.List(context.TODO(), manifestDir)
		if err != nil {
			// If direct access fails, try to find subdirectories
			log.Printf("Failed to list tags for %s directly, trying subdirectories: %v", repo, err)
			
			// List subdirectories in the repo folder
			subDirs, err := h.sftpDriver.List(context.TODO(), fmt.Sprintf("registry/%s", repo))
			if err != nil {
				log.Printf("Failed to list subdirectories for %s: %v", repo, err)
				continue
			}
			
			// For each subdirectory, check if it has manifests
			for _, subDir := range subDirs {
				fullRepoName := fmt.Sprintf("%s/%s", repo, subDir)
				subManifestDir := fmt.Sprintf("registry/%s/manifests", fullRepoName)
				subTags, err := h.sftpDriver.List(context.TODO(), subManifestDir)
				if err != nil {
					log.Printf("Failed to list tags for %s: %v", fullRepoName, err)
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

	data := DashboardData{
		Repositories: repositories,
		TotalImages:  totalImages,
	}

	// Parse and execute template
	tmpl, err := template.New("dashboard").Parse(dashboardTemplate)
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
		manifestDir := fmt.Sprintf("registry/%s/manifests", repo)
		tags, err := h.sftpDriver.List(context.TODO(), manifestDir)
		if err != nil {
			// If direct access fails, try to find subdirectories
			log.Printf("Failed to list tags for %s directly, trying subdirectories: %v", repo, err)
			
			// List subdirectories in the repo folder
			subDirs, err := h.sftpDriver.List(context.TODO(), fmt.Sprintf("registry/%s", repo))
			if err != nil {
				log.Printf("Failed to list subdirectories for %s: %v", repo, err)
				continue
			}
			
			// For each subdirectory, check if it has manifests
			for _, subDir := range subDirs {
				fullRepoName := fmt.Sprintf("%s/%s", repo, subDir)
				subManifestDir := fmt.Sprintf("registry/%s/manifests", fullRepoName)
				subTags, err := h.sftpDriver.List(context.TODO(), subManifestDir)
				if err != nil {
					log.Printf("Failed to list tags for %s: %v", fullRepoName, err)
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
