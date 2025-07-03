package registry

import (
	"io"
	"net/http"
	"strings"
	"fmt"
	"time"
)

// Handler untuk endpoint Docker Registry API v2
func RegistryHandler(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/v2/")
	if path == "" {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("{}"))
		return
	}

	// /<name>/blobs/uploads/
	if strings.HasSuffix(path, "/blobs/uploads/") && r.Method == http.MethodPost {
		initiateBlobUpload(w, r, path)
		return
	}
	// /<name>/blobs/uploads/<upload_id>
	if strings.Contains(path, "/blobs/uploads/") && (r.Method == http.MethodPatch || r.Method == http.MethodPut) {
		uploadBlobData(w, r, path)
		return
	}
	// /<name>/blobs/<digest>
	if strings.Contains(path, "/blobs/") && r.Method == http.MethodGet {
		handleBlobDownload(w, r, path)
		return
	}
	// /<name>/manifests/<reference>
	if strings.Contains(path, "/manifests/") {
		handleManifest(w, r, path)
		return
	}
	// /_catalog
	if path == "_catalog" && r.Method == http.MethodGet {
		handleCatalog(w, r)
		return
	}

	w.WriteHeader(http.StatusNotFound)
	w.Write([]byte("Not found"))
}

func initiateBlobUpload(w http.ResponseWriter, _ *http.Request, path string) {
	uploadID := fmt.Sprintf("%d", time.Now().UnixNano())
	location := fmt.Sprintf("/v2/%sblobs/uploads/%s", strings.Split(path, "/blobs/")[0], uploadID)
	w.Header().Set("Location", location)
	w.Header().Set("Range", "0-0")
	w.WriteHeader(http.StatusAccepted)
	w.Write([]byte(""))
}

func uploadBlobData(w http.ResponseWriter, r *http.Request, path string) {
	name := strings.Split(path, "/blobs/")[0]
	parts := strings.Split(path, "/blobs/uploads/")
	uploadID := ""
	if len(parts) > 1 {
		uploadID = parts[1]
	}
	blob, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Failed to read blob data"))
		return
	}
	blobPath := fmt.Sprintf("%s/blobs/%s", name, uploadID)
	err = ftpClient.Upload(blobPath, blob)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Failed to upload blob to FTP"))
		return
	}
	w.Header().Set("Location", r.URL.Path)
	w.WriteHeader(http.StatusCreated)
	w.Write([]byte("Blob uploaded"))
}

func handleBlobDownload(w http.ResponseWriter, _ *http.Request, path string) {
	name := strings.Split(path, "/blobs/")[0]
	blobPath := fmt.Sprintf("%s/blobs/%s", name, strings.Split(path, "/blobs/")[1])
	blob, err := ftpClient.Download(blobPath)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("Blob not found on FTP"))
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write(blob)
}

func handleManifest(w http.ResponseWriter, r *http.Request, path string) {
	name := strings.Split(path, "/manifests/")[0]
	ref := strings.Split(path, "/manifests/")[1]
	manifestPath := fmt.Sprintf("%s/manifests/%s", name, ref)
	switch r.Method {
	case http.MethodPut:
		manifest, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("Failed to read manifest"))
			return
		}
		err = ftpClient.Upload(manifestPath, manifest)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("Failed to upload manifest to FTP"))
			return
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte("Manifest uploaded"))
	case http.MethodGet:
		manifest, err := ftpClient.Download(manifestPath)
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte("Manifest not found on FTP"))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write(manifest)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func handleCatalog(w http.ResponseWriter, _ *http.Request) {
	entries, err := ftpClient.List("")
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Failed to list repositories"))
		return
	}
	repos := []string{}
	for _, entry := range entries {
		if entry.Type == 1 { // Directory
			repos = append(repos, entry.Name)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(fmt.Sprintf(`{"repositories":%q}`, repos)))
} 