package registry

import (
	"io"
	"net/http"
	"strings"
	"fmt"
	"time"
	"log"
	"encoding/json"
	godigest "github.com/opencontainers/go-digest"
	"refity/internal/driver/sftp"
)

var sftpSemaphore = make(chan struct{}, 2) // max 2 upload paralel

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
	uploadPath := fmt.Sprintf("registry/%s/blobs/uploads/%s", name, uploadID)
	uploadPath = strings.TrimLeft(uploadPath, "/")
	// Cek folder group di SFTP
	group := groupFolder(uploadPath)
	if group != "" {
		if _, err := sftpDriver.Stat(r.Context(), group); err != nil {
			registryError(w, "INSUFFICIENT_SCOPE", "authorization failed", 403)
			return
		}
	}
	err = localDriver.PutContent(r.Context(), uploadPath, blob)
	if err != nil {
		log.Printf("uploadBlobData: failed to write blob to local: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Failed to write blob to local: "+err.Error()))
		return
	}
	w.Header().Set("Location", r.URL.Path)
	w.WriteHeader(http.StatusCreated)
	w.Write([]byte("Blob uploaded"))
}

func handleBlobDownload(w http.ResponseWriter, _ *http.Request, path string) {
	name := strings.TrimPrefix(strings.Split(path, "/blobs/")[0], "/")
	blobPath := fmt.Sprintf("registry/%s/blobs/%s", name, strings.Split(path, "/blobs/")[1])
	blobPath = strings.TrimLeft(blobPath, "/")
	blob, err := sftpDriver.GetContent(nil, blobPath)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("Blob not found on SFTP"))
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write(blob)
}

func handleManifest(w http.ResponseWriter, r *http.Request, path string) {
	name := strings.TrimPrefix(strings.Split(path, "/manifests/")[0], "/")
	ref := strings.Split(path, "/manifests/")[1]
	manifestPath := fmt.Sprintf("registry/%s/manifests/%s", name, ref)
	manifestPath = strings.TrimLeft(manifestPath, "/")
	switch r.Method {
	case http.MethodPut:
		// Cek folder group di SFTP
		group := groupFolder(manifestPath)
		if group != "" {
			if _, err := sftpDriver.Stat(r.Context(), group); err != nil {
				registryError(w, "INSUFFICIENT_SCOPE", "authorization failed", 403)
				return
			}
		}
		manifest, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("Failed to read manifest"))
			return
		}
		contentType := r.Header.Get("Content-Type")
		if strings.Contains(contentType, "manifest.list.v2+json") || strings.Contains(contentType, "oci.image.index.v1+json") {
			// Validasi semua referensi manifest ada di SFTP
			type ManifestList struct {
				Manifests []struct {
					Digest string `json:"digest"`
				} `json:"manifests"`
			}
			var ml ManifestList
			if err := json.Unmarshal(manifest, &ml); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte("Invalid manifest list JSON"))
				return
			}
			missing := []string{}
			for _, m := range ml.Manifests {
				manifestPath := fmt.Sprintf("registry/%s/manifests/%s", name, m.Digest)
				manifestPath = strings.TrimLeft(manifestPath, "/")
				_, err := sftpDriver.GetContent(r.Context(), manifestPath)
				if err != nil {
					missing = append(missing, m.Digest)
				}
			}
			if len(missing) > 0 {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte("Missing referenced manifests: " + strings.Join(missing, ", ")))
				return
			}
		}
		// Hitung digest manifest
		manifestDigest := godigest.FromBytes(manifest)
		err = localDriver.PutContent(r.Context(), manifestPath, manifest)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("Failed to write manifest to local"))
			return
		}
		w.Header().Set("Docker-Content-Digest", manifestDigest.String())
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte("Manifest uploaded"))
	case http.MethodGet:
		manifest, err := sftpDriver.GetContent(nil, manifestPath)
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte("Manifest not found on SFTP"))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write(manifest)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func handleCatalog(w http.ResponseWriter, _ *http.Request) {
	entries, err := sftpDriver.List(nil, "registry")
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Failed to list repositories"))
		return
	}
	repos := []string{}
	for _, entry := range entries {
		repos = append(repos, entry)
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
	blobPath := fmt.Sprintf("registry/%s/blobs/%s", name, digest)
	blobPath = strings.TrimLeft(blobPath, "/")
	uploadPath := fmt.Sprintf("registry/%s/blobs/uploads/%s", name, uploadID)
	uploadPath = strings.TrimLeft(uploadPath, "/")
	// Cek folder group di SFTP
	group := groupFolder(blobPath)
	if group != "" {
		if _, err := sftpDriver.Stat(r.Context(), group); err != nil {
			registryError(w, "INSUFFICIENT_SCOPE", "authorization failed", 403)
			return
		}
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("commitBlobUpload: failed to read blob data: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Failed to read blob data: "+err.Error()))
		return
	}
	if len(body) == 0 {
		err = localDriver.Move(r.Context(), uploadPath, blobPath)
		if err != nil {
			if err == sftp.ErrRepoNotFound {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte("repository not found (create group folder first)"))
				return
			}
			log.Printf("commitBlobUpload: failed to move blob on local: %v (from: %s, to: %s)", err, uploadPath, blobPath)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("Failed to move blob on local: "+err.Error()))
			return
		}
	} else {
		err = localDriver.PutContent(r.Context(), blobPath, body)
		if err != nil {
			if err == sftp.ErrRepoNotFound {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte("repository not found (create group folder first)"))
				return
			}
			log.Printf("commitBlobUpload: failed to write blob to local: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("Failed to write blob to local: "+err.Error()))
			return
		}
	}
	// Validasi digest: baca file, hitung digest, bandingkan
	localData, err := localDriver.GetContent(r.Context(), blobPath)
	if err != nil {
		log.Printf("commitBlobUpload: failed to read blob from local: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Failed to read blob from local: "+err.Error()))
		return
	}
	calculated := godigest.FromBytes(localData)
	parsedDigest, err := godigest.Parse(digest)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("invalid checksum digest format (parse)"))
		return
	}
	if calculated != parsedDigest {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("invalid checksum digest format (mismatch)"))
		return
	}
	// Setelah valid, upload ke SFTP secara async
	ctx := r.Context()
	go func(localPath, sftpPath string, data []byte) {
		sftpSemaphore <- struct{}{} // ambil slot
		defer func() { <-sftpSemaphore }() // lepas slot setelah selesai
		log.Printf("[async SFTP] Start upload: %s -> %s", localPath, sftpPath)
		maxRetry := 5
		var err error
		for i := 0; i < maxRetry; i++ {
			err = sftpDriver.PutContent(ctx, sftpPath, data)
			if err == nil {
				log.Printf("[async SFTP] Success upload: %s -> %s (try %d)", localPath, sftpPath, i+1)
				break
			}
			log.Printf("[async SFTP] Retry %d: failed to upload to SFTP: %v", i+1, err)
			time.Sleep(2 * time.Second)
		}
		if err != nil {
			log.Printf("[async SFTP] FINAL FAIL: %v", err)
			return
		}
		err = localDriver.Delete(ctx, localPath)
		if err != nil {
			log.Printf("[async SFTP] Failed to delete local: %v", err)
		} else {
			log.Printf("[async SFTP] Deleted local: %s", localPath)
		}
	}(blobPath, blobPath, localData)
	w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/%s", name, calculated.String()))
	w.Header().Set("Docker-Content-Digest", calculated.String())
	w.WriteHeader(http.StatusCreated)
	w.Write([]byte("Blob committed (async SFTP, digest validated)"))
}

// Helper groupFolder
func groupFolder(path string) string {
	parts := strings.Split(path, "/")
	if len(parts) >= 3 && parts[0] == "registry" {
		return parts[0] + "/" + parts[1]
	}
	return ""
}

func registryError(w http.ResponseWriter, code, message string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write([]byte(fmt.Sprintf(`{"errors":[{"code":"%s","message":"%s","detail":null}]}`, code, message)))
} 