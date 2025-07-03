package registry

import (
	"io"
	"net/http"
	"strings"
	"fmt"
	"time"
	"log"
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
	// /<name>/blobs/uploads/<upload_id>?digest=sha256:...
	if strings.Contains(path, "/blobs/uploads/") && r.Method == http.MethodPut && r.URL.Query().Get("digest") != "" {
		commitBlobUpload(w, r, path)
		return
	}
	// /<name>/blobs/uploads/<upload_id> (PATCH)
	if strings.Contains(path, "/blobs/uploads/") && r.Method == http.MethodPatch {
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
	name := strings.TrimSuffix(strings.Split(path, "/blobs/")[0], "/")
	location := fmt.Sprintf("/v2/%s/blobs/uploads/%s", name, uploadID)
	w.Header().Set("Location", location)
	w.Header().Set("Range", "0-0")
	w.WriteHeader(http.StatusAccepted)
	w.Write([]byte(""))
}

func uploadBlobData(w http.ResponseWriter, r *http.Request, path string) {
	parts := strings.SplitN(path, "/blobs/uploads/", 2)
	if len(parts) != 2 {
		log.Printf("uploadBlobData: invalid upload path: %s", path)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("Invalid upload path"))
		return
	}
	name := strings.TrimPrefix(strings.TrimSuffix(parts[0], "/"), "/")
	uploadID := parts[1]
	blob, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("uploadBlobData: failed to read blob data: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Failed to read blob data"))
		return
	}
	uploadPath := fmt.Sprintf("%s/blobs/uploads/%s", name, uploadID)
	uploadPath = strings.TrimPrefix(uploadPath, "/")
	err = ftpClient.Upload(uploadPath, blob)
	if err != nil {
		log.Printf("uploadBlobData: failed to upload blob to FTP: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Failed to upload blob to FTP: "+err.Error()))
		return
	}
	w.Header().Set("Location", r.URL.Path)
	w.WriteHeader(http.StatusCreated)
	w.Write([]byte("Blob uploaded"))
}

func handleBlobDownload(w http.ResponseWriter, _ *http.Request, path string) {
	name := strings.TrimPrefix(strings.Split(path, "/blobs/")[0], "/")
	blobPath := fmt.Sprintf("%s/blobs/%s", name, strings.Split(path, "/blobs/")[1])
	blobPath = strings.TrimPrefix(blobPath, "/")
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

func commitBlobUpload(w http.ResponseWriter, r *http.Request, path string) {
	parts := strings.SplitN(path, "/blobs/uploads/", 2)
	if len(parts) != 2 {
		log.Printf("commitBlobUpload: invalid commit path: %s", path)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("Invalid commit path"))
		return
	}
	name := strings.TrimPrefix(strings.TrimSuffix(parts[0], "/"), "/")
	uploadID := parts[1]
	digest := r.URL.Query().Get("digest")
	if digest == "" {
		log.Printf("commitBlobUpload: missing digest query param")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("Missing digest query param"))
		return
	}
	blobPath := fmt.Sprintf("%s/blobs/%s", name, strings.ReplaceAll(digest, ":", "_"))
	blobPath = strings.TrimPrefix(blobPath, "/")
	uploadPath := fmt.Sprintf("%s/blobs/uploads/%s", name, uploadID)
	uploadPath = strings.TrimPrefix(uploadPath, "/")
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("commitBlobUpload: failed to read blob data: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Failed to read blob data: "+err.Error()))
		return
	}
	if len(body) == 0 {
		// Move/rename file dari uploads ke blobs
		err = ftpClient.Rename(uploadPath, blobPath)
		if err != nil {
			log.Printf("commitBlobUpload: failed to move blob on FTP: %v (from: %s, to: %s)", err, uploadPath, blobPath)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("Failed to move blob on FTP: "+err.Error()))
			return
		}
	} else {
		// Jika body ada, upload ulang ke blobs
		err = ftpClient.Upload(blobPath, body)
		if err != nil {
			log.Printf("commitBlobUpload: failed to upload blob to FTP: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("Failed to upload blob to FTP: "+err.Error()))
			return
		}
	}
	w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/%s", name, digest))
	w.Header().Set("Docker-Content-Digest", digest)
	w.WriteHeader(http.StatusCreated)
	w.Write([]byte("Blob committed"))
} 