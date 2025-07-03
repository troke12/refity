package registry

import (
	"io"
	"net/http"
	"strings"
	"fmt"
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
		handleBlobUpload(w, r, path)
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

	w.WriteHeader(http.StatusNotFound)
	w.Write([]byte("Not found"))
}

func handleBlobUpload(w http.ResponseWriter, r *http.Request, path string) {
	name := strings.Split(path, "/blobs/")[0]
	blob, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Failed to read blob"))
		return
	}
	blobPath := fmt.Sprintf("%s/blobs/%d", name, len(blob))
	err = ftpClient.Upload(blobPath, blob)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Failed to upload blob to FTP"))
		return
	}
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
	if r.Method == http.MethodPut {
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
		return
	} else if r.Method == http.MethodGet {
		manifest, err := ftpClient.Download(manifestPath)
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte("Manifest not found on FTP"))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write(manifest)
		return
	}
	w.WriteHeader(http.StatusMethodNotAllowed)
} 