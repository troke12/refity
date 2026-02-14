package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	godigest "github.com/opencontainers/go-digest"
	"refity/backend/internal/database"
	"refity/backend/internal/driver/sftp"
)

// validRepoName restricts repo name to avoid path traversal and invalid chars (Docker: alphanumeric, separators, one optional /)
var validRepoName = regexp.MustCompile(`^[a-zA-Z0-9._-]+(/[a-zA-Z0-9._-]+)?$`)

func validateRepoName(name string) bool {
	if name == "" || strings.Contains(name, "..") {
		return false
	}
	return validRepoName.MatchString(name)
}

func validateManifestRef(ref string) bool {
	if ref == "" || strings.Contains(ref, "..") || strings.Contains(ref, "/") || strings.Contains(ref, "\\") {
		return false
	}
	return true
}

var sftpSemaphore = make(chan struct{}, 2) // max 2 upload paralel
var sftpPathLocks sync.Map // map[string]*sync.Mutex

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
		handleBlobDownload(w, path)
		return
	}
	// /<name>/manifests/<reference>
	if strings.Contains(path, "/manifests/") {
		handleManifest(w, r, path)
		return
	}
	// /<name>/signatures/<digest>
	if strings.Contains(path, "/signatures/") {
		log.Printf("Handling signatures request: %s", path)
		handleSignatures(w, r, path)
		return
	}
	// /_catalog
	if path == "_catalog" && r.Method == http.MethodGet {
		handleCatalog(w)
		return
	}
	// /<name>/tags/list
	if strings.HasSuffix(path, "/tags/list") && r.Method == http.MethodGet {
		handleTagsList(w, path)
		return
	}

	w.WriteHeader(http.StatusNotFound)
	w.Write([]byte("Not found"))
}

func initiateBlobUpload(w http.ResponseWriter, _ *http.Request, path string) {
	uploadID := strconv.FormatInt(time.Now().UnixNano(), 10)
	name := strings.TrimSuffix(strings.TrimPrefix(strings.Split(path, "/blobs/")[0], "/"), "/")
	if !validateRepoName(name) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("invalid repository name"))
		return
	}

	// Auto-create repository if it doesn't exist (Docker registry standard behavior)
	if db != nil {
		if _, err := db.GetRepository(name); err != nil {
			// Repository doesn't exist, create it automatically
			_, createErr := db.CreateRepository(name)
			if createErr != nil {
				log.Printf("initiateBlobUpload: failed to auto-create repository %s: %v", name, createErr)
				// Continue anyway, the upload might still work
			} else {
				log.Printf("initiateBlobUpload: auto-created repository %s", name)
				// Also create SFTP folder structure
				if sftpDriver != nil {
					if err := sftpDriver.CreateRepositoryFolder(context.TODO(), name); err != nil {
						log.Printf("initiateBlobUpload: failed to create SFTP folder for %s: %v", name, err)
						// Continue anyway, folder will be created when needed
					}
				}
			}
		}
	}
	
	location := "/v2/" + name + "/blobs/uploads/" + uploadID
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
	if !validateRepoName(name) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("invalid repository name"))
		return
	}
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
	// Skip group folder check for now - it's causing 403 errors
	err = localDriver.PutContent(context.TODO(), uploadPath, blob, nil)
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

func handleBlobDownload(w http.ResponseWriter, path string) {
	name := strings.TrimPrefix(strings.TrimSuffix(strings.Split(path, "/blobs/")[0], "/"), "/")
	if !validateRepoName(name) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("invalid repository name"))
		return
	}
	blobPart := strings.Split(path, "/blobs/")[1]
	if strings.Contains(blobPart, "..") || strings.Contains(blobPart, "/") {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("invalid blob path"))
		return
	}
	blobPath := fmt.Sprintf("registry/%s/blobs/%s", name, blobPart)
	blobPath = strings.TrimLeft(blobPath, "/")
	blob, err := sftpDriver.GetContent(context.TODO(), blobPath)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("Blob not found on SFTP"))
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write(blob)
}

func handleManifest(w http.ResponseWriter, r *http.Request, path string) {
	name := strings.TrimPrefix(strings.TrimSuffix(strings.Split(path, "/manifests/")[0], "/"), "/")
	if !validateRepoName(name) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("invalid repository name"))
		return
	}
	ref := strings.Split(path, "/manifests/")[1]
	if !validateManifestRef(ref) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("invalid manifest reference"))
		return
	}
	manifestPath := fmt.Sprintf("registry/%s/manifests/%s", name, ref)
	manifestPath = strings.TrimLeft(manifestPath, "/")
	switch r.Method {
	case http.MethodPut:
		// Auto-create repository if it doesn't exist (Docker registry standard behavior)
		if db != nil {
			if _, err := db.GetRepository(name); err != nil {
				// Repository doesn't exist, create it automatically
				_, createErr := db.CreateRepository(name)
				if createErr != nil {
					log.Printf("handleManifest: failed to auto-create repository %s: %v", name, createErr)
					// Continue anyway, the upload might still work
				} else {
					log.Printf("handleManifest: auto-created repository %s", name)
					// Also create SFTP folder structure
					if sftpDriver != nil {
						if err := sftpDriver.CreateRepositoryFolder(context.TODO(), name); err != nil {
							log.Printf("handleManifest: failed to create SFTP folder for %s: %v", name, err)
							// Continue anyway, folder will be created when needed
						}
					}
				}
			}
		}
		
		// Skip group folder check for now - it's causing 403 errors
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
				_, err := sftpDriver.GetContent(context.TODO(), manifestPath)
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
		digestStr := manifestDigest.String()
		
		// Simpan manifest dengan nama tag (ref)
		err = localDriver.PutContent(context.TODO(), manifestPath, manifest, nil)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("Failed to write manifest to local"))
			return
		}
		
		// Simpan juga manifest dengan nama digest untuk akses via digest
		manifestDigestPath := fmt.Sprintf("registry/%s/manifests/%s", name, digestStr)
		manifestDigestPath = strings.TrimLeft(manifestDigestPath, "/")
		err = localDriver.PutContent(context.TODO(), manifestDigestPath, manifest, nil)
		if err != nil {
			log.Printf("Warning: failed to write manifest with digest name: %v", err)
			// Continue anyway, tag-based access should still work
		}
		
		// Setelah ditulis ke local, upload ke SFTP secara async (keduanya: tag dan digest)
		ctx := context.TODO()
		go func(tagPath, digestPath string, data []byte) {
			sftpSemaphore <- struct{}{}
			defer func() { <-sftpSemaphore }()
			
			// Upload dengan nama tag
			lockIface, _ := sftpPathLocks.LoadOrStore(tagPath, &sync.Mutex{})
			pathLock := lockIface.(*sync.Mutex)
			pathLock.Lock()
			log.Printf("[async SFTP] Start upload manifest (tag): %s -> %s", tagPath, tagPath)
			maxRetry := 5
			var err error
			for i := 0; i < maxRetry; i++ {
				err = sftpDriver.PutContent(ctx, tagPath, data, nil)
				if err == nil {
					log.Printf("[async SFTP] Success upload manifest (tag): %s -> %s (try %d)", tagPath, tagPath, i+1)
					break
				}
				log.Printf("[async SFTP] Retry %d: failed to upload manifest (tag) to SFTP: %v", i+1, err)
				time.Sleep(2 * time.Second)
			}
			if err != nil {
				log.Printf("[async SFTP] FINAL FAIL manifest (tag): %v", err)
			}
			pathLock.Unlock()
			
			// Upload dengan nama digest
			lockIface2, _ := sftpPathLocks.LoadOrStore(digestPath, &sync.Mutex{})
			pathLock2 := lockIface2.(*sync.Mutex)
			pathLock2.Lock()
			log.Printf("[async SFTP] Start upload manifest (digest): %s -> %s", digestPath, digestPath)
			for i := 0; i < maxRetry; i++ {
				err = sftpDriver.PutContent(ctx, digestPath, data, nil)
				if err == nil {
					log.Printf("[async SFTP] Success upload manifest (digest): %s -> %s (try %d)", digestPath, digestPath, i+1)
					break
				}
				log.Printf("[async SFTP] Retry %d: failed to upload manifest (digest) to SFTP: %v", i+1, err)
				time.Sleep(2 * time.Second)
			}
			if err != nil {
				log.Printf("[async SFTP] FINAL FAIL manifest (digest): %v", err)
			}
			pathLock2.Unlock()
		}(manifestPath, manifestDigestPath, manifest)
		
		// Save image metadata to database
		if db != nil {
			go func() {
				if err := saveImageToDatabase(name, ref, manifestDigest.String(), manifest); err != nil {
					log.Printf("Failed to save image to database: %v", err)
				}
			}()
		}
		
		w.Header().Set("Docker-Content-Digest", manifestDigest.String())
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte("Manifest uploaded"))
	case http.MethodGet:
		// Coba ambil manifest dengan ref yang diberikan (bisa tag atau digest)
		manifest, err := sftpDriver.GetContent(context.TODO(), manifestPath)
		if err != nil {
			// Fallback: coba cari via database
			if db != nil {
				var img *database.Image
				var dbErr error
				
				if strings.HasPrefix(ref, "sha256:") {
					// Ref adalah digest, cari image dengan digest ini
					img, dbErr = db.GetImageByDigest(ref)
				} else {
					// Ref adalah tag, cari image dengan tag ini
					img, dbErr = db.GetImage(name, ref)
				}
				
				if dbErr == nil && img != nil {
					// Coba ambil manifest dengan nama tag (untuk backward compatibility)
					tagPath := fmt.Sprintf("registry/%s/manifests/%s", name, img.Tag)
					tagPath = strings.TrimLeft(tagPath, "/")
					manifest, err = sftpDriver.GetContent(context.TODO(), tagPath)
					if err == nil {
						manifestPath = tagPath
					} else {
						// Jika tidak ditemukan dengan tag, coba dengan digest
						digestPath := fmt.Sprintf("registry/%s/manifests/%s", name, img.Digest)
						digestPath = strings.TrimLeft(digestPath, "/")
						manifest, err = sftpDriver.GetContent(context.TODO(), digestPath)
						if err == nil {
							manifestPath = digestPath
						}
					}
				}
			}
			
			if err != nil {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte("Manifest not found on SFTP"))
				return
			}
		}
		
		// Set proper Content-Type header for manifest
		// Cek apakah ini manifest list atau single manifest
		var manifestData map[string]interface{}
		if err := json.Unmarshal(manifest, &manifestData); err == nil {
			if _, isList := manifestData["manifests"]; isList {
				w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.list.v2+json")
			} else {
				w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			}
		} else {
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		}
		
		// Set Docker-Content-Digest header
		manifestDigest := godigest.FromBytes(manifest)
		w.Header().Set("Docker-Content-Digest", manifestDigest.String())
		w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
		w.WriteHeader(http.StatusOK)
		w.Write(manifest)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func handleCatalog(w http.ResponseWriter) {
	entries, err := sftpDriver.List(context.TODO(), "registry")
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Failed to list repositories"))
		return
	}
	repos := []string{}
	repos = append(repos, entries...)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"repositories":` + toJSONString(repos) + `}`))
}

// Helper untuk konversi slice ke JSON string
func toJSONString(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
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
	if !validateRepoName(name) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("invalid repository name"))
		return
	}
	uploadID := parts[1]
	digest := r.URL.Query().Get("digest")
	if digest == "" {
		log.Printf("commitBlobUpload: missing digest query param")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("Missing digest query param"))
		return
	}

	// Auto-create repository if it doesn't exist (Docker registry standard behavior)
	if db != nil {
		if _, err := db.GetRepository(name); err != nil {
			// Repository doesn't exist, create it automatically
			_, createErr := db.CreateRepository(name)
			if createErr != nil {
				log.Printf("commitBlobUpload: failed to auto-create repository %s: %v", name, createErr)
				// Continue anyway, the upload might still work
			} else {
				log.Printf("commitBlobUpload: auto-created repository %s", name)
				// Also create SFTP folder structure
				if sftpDriver != nil {
					if err := sftpDriver.CreateRepositoryFolder(context.TODO(), name); err != nil {
						log.Printf("commitBlobUpload: failed to create SFTP folder for %s: %v", name, err)
						// Continue anyway, folder will be created when needed
					}
				}
			}
		}
	}

	blobPath := fmt.Sprintf("registry/%s/blobs/%s", name, digest)
	blobPath = strings.TrimLeft(blobPath, "/")
	uploadPath := fmt.Sprintf("registry/%s/blobs/uploads/%s", name, uploadID)
	uploadPath = strings.TrimLeft(uploadPath, "/")
	// Skip group folder check for now - it's causing 403 errors
	// The SFTP driver will handle directory creation automatically
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("commitBlobUpload: failed to read blob data: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Failed to read blob data: "+err.Error()))
		return
	}
	if len(body) == 0 {
		err = localDriver.Move(context.TODO(), uploadPath, blobPath)
		if err != nil {
			if err == sftp.ErrRepoNotFound {
				registryError(w, "NAME_INVALID", fmt.Sprintf("repository name %s not found", name), 404)
				return
			}
			log.Printf("commitBlobUpload: failed to move blob on local: %v (from: %s, to: %s)", err, uploadPath, blobPath)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("Failed to move blob on local: "+err.Error()))
			return
		}
	} else {
		err = localDriver.PutContent(context.TODO(), blobPath, body, nil)
		if err != nil {
			if err == sftp.ErrRepoNotFound {
				registryError(w, "NAME_INVALID", fmt.Sprintf("repository name %s not found", name), 404)
				return
			}
			log.Printf("commitBlobUpload: failed to write blob to local: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("Failed to write blob to local: "+err.Error()))
			return
		}
	}
	// Validasi digest: baca file, hitung digest, bandingkan
	localData, err := localDriver.GetContent(context.TODO(), blobPath)
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
	ctx := context.TODO()
	go func(localPath, sftpPath string, data []byte) {
		sftpSemaphore <- struct{}{} // ambil slot
		defer func() { <-sftpSemaphore }() // lepas slot setelah selesai

		// Lock per path
		lockIface, _ := sftpPathLocks.LoadOrStore(sftpPath, &sync.Mutex{})
		pathLock := lockIface.(*sync.Mutex)
		pathLock.Lock()
		defer pathLock.Unlock()

		// Cek apakah file sudah ada di SFTP
		if _, err := sftpDriver.Stat(ctx, sftpPath); err == nil {
			log.Printf("[async SFTP] SKIP: blob already exists on SFTP: %s", sftpPath)
			_ = localDriver.Delete(ctx, localPath) // tetap hapus local
			return
		}

		log.Printf("[async SFTP] Start upload: %s -> %s", localPath, sftpPath)
		maxRetry := 5
		var err error
		for i := 0; i < maxRetry; i++ {
			err = sftpDriver.PutContent(ctx, sftpPath, data, func(written, total int64) {
				percent := written * 100 / total
				log.Printf("[async SFTP] Progress: %s -> %s: %d%% (%d/%d bytes)", localPath, sftpPath, percent, written, total)
			})
			if err == nil {
				log.Printf("[async SFTP] Success upload: %s -> %s (try %d)", localPath, sftpPath, i+1)
				break
			}
			backoff := 1 << i // 2^i detik
			if backoff > 16 { backoff = 16 }
			log.Printf("[async SFTP] Retry %d: failed to upload to SFTP: %v, will retry in %ds", i+1, err, backoff)
			time.Sleep(time.Duration(backoff) * time.Second)
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

// Handler untuk endpoint signatures
func handleSignatures(w http.ResponseWriter, r *http.Request, path string) {
	// Parse path but don't use variables for now since we're just returning empty responses
	_ = strings.TrimPrefix(strings.Split(path, "/signatures/")[0], "/")
	_ = strings.Split(path, "/signatures/")[1]
	
	switch r.Method {
	case http.MethodGet:
		// Return empty signatures list - Docker expects this endpoint to exist
		// even if no signatures are available
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"signatures":[]}`))
	case http.MethodPost:
		// Accept signature uploads but don't store them for now
		// This prevents Docker from failing when trying to push signatures
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte("Signature uploaded"))
	case http.MethodDelete:
		// Accept signature deletions
		w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}



func registryError(w http.ResponseWriter, code, message string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	resp := map[string]interface{}{
		"errors": []map[string]interface{}{
			{"code": code, "message": message, "detail": nil},
		},
	}
	json.NewEncoder(w).Encode(resp)
}

func handleTagsList(w http.ResponseWriter, path string) {
	parts := strings.SplitN(path, "/tags/list", 2)
	repo := strings.TrimPrefix(strings.TrimSuffix(parts[0], "/"), "/")
	if !validateRepoName(repo) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("invalid repository name"))
		return
	}
	manifestDir := "registry/" + repo + "/manifests"
	manifestDir = strings.TrimLeft(manifestDir, "/")
	tags, err := sftpDriver.List(context.TODO(), manifestDir)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		resp := map[string]interface{}{
			"errors": []map[string]interface{}{
				{"code": "NOT_FOUND", "message": "repo or tags not found"},
			},
		}
		json.NewEncoder(w).Encode(resp)
		return
	}
	resp := map[string]interface{}{
		"name": repo,
		"tags": tags,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// saveImageToDatabase saves image metadata to database
func saveImageToDatabase(name, tag, digest string, manifestData []byte) error {
	if db == nil {
		return nil
	}

	// Parse manifest to get size information
	var manifest map[string]interface{}
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return err
	}

	// Calculate total size from layers
	var totalSize int64
	if layers, ok := manifest["layers"].([]interface{}); ok {
		for _, layer := range layers {
			if layerMap, ok := layer.(map[string]interface{}); ok {
				if size, ok := layerMap["size"].(float64); ok {
					totalSize += int64(size)
				}
			}
		}
	}

	// Create image in database
	image, err := db.CreateImage(name, tag, digest, totalSize)
	if err != nil {
		return err
	}

	// Save layers
	if layers, ok := manifest["layers"].([]interface{}); ok {
		for _, layer := range layers {
			if layerMap, ok := layer.(map[string]interface{}); ok {
				if digest, ok := layerMap["digest"].(string); ok {
					if mediaType, ok := layerMap["mediaType"].(string); ok {
						if size, ok := layerMap["size"].(float64); ok {
							err := db.CreateLayer(image.ID, digest, mediaType, int64(size))
							if err != nil {
								log.Printf("Failed to save layer %s: %v", digest, err)
							}
						}
					}
				}
			}
		}
	}

	// Save manifest
	err = db.CreateManifest(image.ID, digest, string(manifestData))
	if err != nil {
		log.Printf("Failed to save manifest: %v", err)
	}

	return nil
} 