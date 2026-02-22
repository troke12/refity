package registry

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
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

// blobUploadState matches distribution format so Docker client gets _state in Location for chunked uploads.
type blobUploadState struct {
	Name      string    `json:"name"`
	UUID      string    `json:"uuid"`
	Offset    int64     `json:"offset"`
	StartedAt time.Time `json:"startedat"`
}

func packUploadState(secret string, state blobUploadState) (string, error) {
	if secret == "" {
		secret = "refity-secret-key-change-in-production"
	}
	mac := hmac.New(sha256.New, []byte(secret))
	p, err := json.Marshal(state)
	if err != nil {
		return "", err
	}
	mac.Write(p)
	return base64.URLEncoding.EncodeToString(append(mac.Sum(nil), p...)), nil
}

func unpackUploadState(secret, token string) (blobUploadState, error) {
	var state blobUploadState
	if token == "" {
		return state, fmt.Errorf("empty _state")
	}
	if secret == "" {
		secret = "refity-secret-key-change-in-production"
	}
	tokenBytes, err := base64.URLEncoding.DecodeString(token)
	if err != nil {
		return state, err
	}
	mac := hmac.New(sha256.New, []byte(secret))
	if len(tokenBytes) < mac.Size() {
		return state, fmt.Errorf("invalid _state token")
	}
	macBytes := tokenBytes[:mac.Size()]
	messageBytes := tokenBytes[mac.Size():]
	mac.Write(messageBytes)
	if !hmac.Equal(mac.Sum(nil), macBytes) {
		return state, fmt.Errorf("invalid _state signature")
	}
	if err := json.Unmarshal(messageBytes, &state); err != nil {
		return state, err
	}
	return state, nil
}

// rewriteManifestToOCI rewrites Docker v2 manifest media types to OCI so pull works with daemons that require OCI.
func rewriteManifestToOCI(manifest []byte) []byte {
	s := string(manifest)
	s = strings.ReplaceAll(s, "application/vnd.docker.distribution.manifest.v2+json", "application/vnd.oci.image.manifest.v1+json")
	s = strings.ReplaceAll(s, "application/vnd.docker.distribution.manifest.list.v2+json", "application/vnd.oci.image.index.v1+json")
	s = strings.ReplaceAll(s, "application/vnd.docker.container.image.v1+json", "application/vnd.oci.image.config.v1+json")
	s = strings.ReplaceAll(s, "application/vnd.docker.image.rootfs.diff.tar.gzip", "application/vnd.oci.image.layer.v1.tar+gzip")
	return []byte(s)
}

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
	// /<name>/blobs/uploads/<upload_id> — GET/HEAD for upload status (resume); avoid falling through to blob download which rejects "uploads/..." as invalid path.
	if strings.Contains(path, "/blobs/uploads/") && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		handleBlobUploadStatus(w, r, path)
		return
	}
	// /<name>/blobs/<digest> — HEAD so Docker can skip re-upload; existence = SFTP only (delete from SFTP = gone).
	if strings.Contains(path, "/blobs/") && r.Method == http.MethodHead {
		handleBlobHead(w, r, path)
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

func initiateBlobUpload(w http.ResponseWriter, r *http.Request, path string) {
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

	// Distribution spec: Initiate Monolithic Blob Upload — POST with ?digest= and body completes upload in one request (no PATCH).
	// Use when SFTPSyncUpload so we can stream body directly to SFTP; avoids PATCH behind proxies that drop or buffer it.
	digest := r.URL.Query().Get("digest")
	if digest != "" && cfg != nil && cfg.SFTPSyncUpload && sftpDriver != nil && r.Body != nil {
		parsedDigest, parseErr := godigest.Parse(digest)
		if parseErr != nil {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte("invalid checksum digest format (parse)"))
			return
		}
		blobPath := fmt.Sprintf("registry/%s/blobs/%s", name, digest)
		blobPath = strings.TrimLeft(blobPath, "/")
		ctx := context.TODO()
		sftpWriter, err := sftpDriver.Writer(ctx, blobPath, false)
		if err != nil {
			if err == sftp.ErrRepoNotFound {
				registryError(w, "NAME_INVALID", fmt.Sprintf("repository name %s not found", name), 404)
				return
			}
			log.Printf("initiateBlobUpload (monolithic): SFTP Writer failed: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("Failed to open storage writer: " + err.Error()))
			return
		}
		digester := godigest.Canonical.Digester()
		multiWriter := io.MultiWriter(sftpWriter, digester.Hash())
		buf := make([]byte, 1024*1024)
		n, copyErr := io.CopyBuffer(multiWriter, r.Body, buf)
		_ = r.Body.Close()
		closeErr := sftpWriter.Close()
		if closeErr != nil {
			log.Printf("initiateBlobUpload (monolithic): Writer close: %v", closeErr)
		}
		if copyErr != nil {
			log.Printf("initiateBlobUpload (monolithic): copy failed: %v", copyErr)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("Failed to stream blob: " + copyErr.Error()))
			return
		}
		calculated := digester.Digest()
		if calculated != parsedDigest {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte("invalid checksum digest format (mismatch)"))
			return
		}
		uploadID := strconv.FormatInt(time.Now().UnixNano(), 10)
		w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/%s", name, calculated.String()))
		w.Header().Set("Docker-Content-Digest", calculated.String())
		w.Header().Set("Docker-Upload-UUID", uploadID)
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(""))
		log.Printf("initiateBlobUpload: monolithic upload completed %s (%d bytes)", name, n)
		return
	}

	// Chunked upload: drain body so connection can be reused for PATCH (Docker reuses same connection)
	io.Copy(io.Discard, r.Body)
	r.Body.Close()
	uploadID := strconv.FormatInt(time.Now().UnixNano(), 10)
	scheme := "http"
	if r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	location := scheme + "://" + r.Host + "/v2/" + name + "/blobs/uploads/" + uploadID
	// Per distribution: Location includes _state so client can send next PATCH (chunked upload).
	secret := ""
	if cfg != nil {
		secret = cfg.JWTSecret
	}
	state := blobUploadState{Name: name, UUID: uploadID, Offset: 0, StartedAt: time.Now()}
	if token, err := packUploadState(secret, state); err == nil {
		location += "?_state=" + token
	}
	w.Header().Set("Location", location)
	w.Header().Set("Range", "0-0")
	w.WriteHeader(http.StatusAccepted)
	w.Write([]byte(""))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func uploadBlobData(w http.ResponseWriter, r *http.Request, path string) {
	log.Printf("uploadBlobData: PATCH received for %s", path)
	// Tell client we accept the body immediately (avoids client closing when waiting for 100 Continue).
	if r.Header.Get("Expect") == "100-continue" {
		w.WriteHeader(http.StatusContinue)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
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
	uploadID := strings.TrimSuffix(parts[1], "/")
	if idx := strings.Index(uploadID, "?"); idx >= 0 {
		uploadID = uploadID[:idx]
	}
	uploadPath := fmt.Sprintf("registry/%s/blobs/uploads/%s", name, uploadID)
	uploadPath = strings.TrimLeft(uploadPath, "/")

	ctx := context.TODO()
	// Size before this PATCH (for append; 0 if first chunk).
	sizeBefore, _ := localDriver.Size(ctx, uploadPath)
	// Append so multiple PATCHes (chunked upload) accumulate; first PATCH creates the file.
	dest, err := localDriver.WriterAppend(ctx, uploadPath)
	if err != nil {
		log.Printf("uploadBlobData: failed to open writer: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Failed to open upload target"))
		return
	}
	// Large buffer so we pull from client quickly and avoid back-pressure / timeouts (e.g. "short copy").
	buf := make([]byte, 1024*1024)
	n, err := io.CopyBuffer(dest, r.Body, buf)
	if err != nil {
		dest.Close()
		log.Printf("uploadBlobData: failed to stream blob data: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Failed to read blob data"))
		return
	}
	// Total after this chunk (no need to stat; send 202 before Close so client gets response fast and can send next PATCH).
	totalSize := sizeBefore + n
	endRange := totalSize - 1
	if totalSize == 0 {
		endRange = 0
	}
	scheme := "http"
	if r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	location := scheme + "://" + r.Host + "/v2/" + name + "/blobs/uploads/" + uploadID
	// Per distribution: Location must include _state so Docker client sends next PATCH.
	secret := ""
	if cfg != nil {
		secret = cfg.JWTSecret
	}
	startedAt := time.Time{}
	if prevState, err := unpackUploadState(secret, r.URL.Query().Get("_state")); err == nil && prevState.UUID == uploadID {
		startedAt = prevState.StartedAt
	}
	state := blobUploadState{Name: name, UUID: uploadID, Offset: totalSize, StartedAt: startedAt}
	if token, err := packUploadState(secret, state); err == nil {
		location += "?_state=" + token
	}
	w.Header().Set("Location", location)
	w.Header().Set("Range", fmt.Sprintf("0-%d", endRange))
	w.Header().Set("Content-Length", "0")
	w.Header().Set("Docker-Upload-UUID", uploadID)
	w.WriteHeader(http.StatusAccepted)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	log.Printf("uploadBlobData: 202 sent for %s (Range 0-%d, +%d this chunk)", path, endRange, n)
	// Close after response so client isn't waiting on disk sync; may help high-latency clients send next PATCH.
	if err := dest.Close(); err != nil {
		log.Printf("uploadBlobData: writer close after 202: %v", err)
	}
}

// handleBlobUploadStatus handles GET/HEAD on blob upload URL (resume/status). If GET has ?digest=, client is committing the upload (no PATCH); delegate to commitBlobUpload so blob is created and we don't return "unknown blob".
func handleBlobUploadStatus(w http.ResponseWriter, r *http.Request, path string) {
	if r.Method == http.MethodGet && r.URL.Query().Get("digest") != "" {
		commitBlobUpload(w, r, path)
		return
	}
	parts := strings.SplitN(path, "/blobs/uploads/", 2)
	if len(parts) != 2 {
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
	uploadID := strings.TrimSuffix(parts[1], "/")
	if idx := strings.Index(uploadID, "?"); idx >= 0 {
		uploadID = uploadID[:idx]
	}
	uploadPath := fmt.Sprintf("registry/%s/blobs/uploads/%s", name, uploadID)
	uploadPath = strings.TrimLeft(uploadPath, "/")
	ctx := context.TODO()
	size, _ := localDriver.Size(ctx, uploadPath)
	endRange := size - 1
	if size <= 0 {
		endRange = 0
	}
	scheme := "http"
	if r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	location := scheme + "://" + r.Host + "/v2/" + name + "/blobs/uploads/" + uploadID
	secret := ""
	if cfg != nil {
		secret = cfg.JWTSecret
	}
	startedAt := time.Time{}
	if prevState, err := unpackUploadState(secret, r.URL.Query().Get("_state")); err == nil && prevState.UUID == uploadID {
		startedAt = prevState.StartedAt
	}
	state := blobUploadState{Name: name, UUID: uploadID, Offset: size, StartedAt: startedAt}
	if token, err := packUploadState(secret, state); err == nil {
		location += "?_state=" + token
	}
	w.Header().Set("Location", location)
	w.Header().Set("Range", fmt.Sprintf("0-%d", endRange))
	w.Header().Set("Docker-Upload-UUID", uploadID)
	w.Header().Set("Content-Length", "0")
	if size <= 0 {
		w.WriteHeader(http.StatusNoContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}
}

// handleBlobHead returns 200 + Docker-Content-Digest + Content-Length if blob exists on SFTP, else 404. Source of truth = SFTP only.
func handleBlobHead(w http.ResponseWriter, r *http.Request, path string) {
	name := strings.TrimPrefix(strings.TrimSuffix(strings.Split(path, "/blobs/")[0], "/"), "/")
	if !validateRepoName(name) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	blobPart := strings.Split(path, "/blobs/")[1]
	if strings.Contains(blobPart, "..") || strings.Contains(blobPart, "/") {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	blobPath := fmt.Sprintf("registry/%s/blobs/%s", name, blobPart)
	blobPath = strings.TrimLeft(blobPath, "/")
	ctx := context.TODO()
	fi, err := sftpDriver.Stat(ctx, blobPath)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	type sizeable interface{ Size() int64 }
	if s, ok := fi.(sizeable); ok {
		w.Header().Set("Content-Length", strconv.FormatInt(s.Size(), 10))
	}
	w.Header().Set("Docker-Content-Digest", blobPart)
	w.WriteHeader(http.StatusOK)
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
		registryError(w, "BLOB_UNKNOWN", "blob not found", http.StatusNotFound)
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
		
		ctx := context.TODO()
		doManifestUpload := func() error {
			return uploadManifestToSFTP(ctx, manifestPath, manifestDigestPath, manifest)
		}

		if cfg != nil && cfg.SFTPSyncUpload {
			if err := doManifestUpload(); err != nil {
				log.Printf("handleManifest (sync): SFTP upload failed: %v", err)
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte("Failed to upload manifest to storage: " + err.Error()))
				return
			}
		} else {
			go doManifestUpload()
		}

		// Save image metadata to database only for real tags (not digest refs like sha256:...)
		// Docker pushes manifest by digest first, then by tag; we only want one row per tag.
		if db != nil && !strings.HasPrefix(ref, "sha256:") {
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
				registryError(w, "MANIFEST_UNKNOWN", "manifest not found", http.StatusNotFound)
				return
			}
		}
		
		// Rewrite Docker v2 media types to OCI so daemon accepts (pull expects OCI when configured).
		manifest = rewriteManifestToOCI(manifest)

		// Set Content-Type and digest for the bytes we're sending (OCI format).
		var manifestData map[string]interface{}
		if err := json.Unmarshal(manifest, &manifestData); err == nil {
			if _, isList := manifestData["manifests"]; isList {
				w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
			} else {
				w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			}
		} else {
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		}
		manifestDigest := godigest.FromBytes(manifest)
		w.Header().Set("Docker-Content-Digest", manifestDigest.String())
		w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
		// Save OCI manifest by digest so pull-by-digest conforms to distribution spec (avoids "falling back to pull by tag" warning).
		ociDigestPath := fmt.Sprintf("registry/%s/manifests/%s", name, manifestDigest.String())
		ociDigestPath = strings.TrimLeft(ociDigestPath, "/")
		if _, err := sftpDriver.Stat(context.TODO(), ociDigestPath); err != nil {
			_ = sftpDriver.PutContent(context.TODO(), ociDigestPath, manifest, nil)
		}
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
	if idx := strings.Index(uploadID, "?"); idx >= 0 {
		uploadID = uploadID[:idx]
	}
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
	ctx := context.TODO()

	// Sync mode + monolithic upload: stream r.Body directly to SFTP while hashing.
	// Client progress bar then moves in sync with our SFTP write (we read body only as fast as we write to SFTP).
	if cfg != nil && cfg.SFTPSyncUpload && r.Body != nil {
		sftpWriter, err := sftpDriver.Writer(ctx, blobPath, false)
		if err != nil {
			if err == sftp.ErrRepoNotFound {
				registryError(w, "NAME_INVALID", fmt.Sprintf("repository name %s not found", name), 404)
				return
			}
			log.Printf("commitBlobUpload (sync stream): SFTP Writer failed: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("Failed to open storage writer: " + err.Error()))
			return
		}
		digester := godigest.Canonical.Digester()
		multiWriter := io.MultiWriter(sftpWriter, digester.Hash())
		buf := make([]byte, 256*1024)
		n, copyErr := io.CopyBuffer(multiWriter, r.Body, buf)
		closeErr := sftpWriter.Close()
		if closeErr != nil {
			log.Printf("commitBlobUpload (sync stream): Writer close: %v", closeErr)
		}
		if copyErr != nil {
			log.Printf("commitBlobUpload (sync stream): copy failed: %v", copyErr)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("Failed to stream blob: " + copyErr.Error()))
			return
		}
		if n > 0 {
			calculated := digester.Digest()
			parsedDigest, parseErr := godigest.Parse(digest)
			if parseErr != nil {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte("invalid checksum digest format (parse)"))
				return
			}
			if calculated != parsedDigest {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte("invalid checksum digest format (mismatch)"))
				return
			}
			w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/%s", name, calculated.String()))
			w.Header().Set("Docker-Content-Digest", calculated.String())
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte("Blob committed (sync stream to SFTP, digest validated)"))
			return
		}
		// n == 0: empty body (chunked upload), blob was sent via PATCHes — read from local, validate, upload to SFTP. Or GET with digest and no PATCH (empty blob only).
		localData, err := localDriver.GetContent(ctx, uploadPath)
		if err != nil {
			emptyDigest := godigest.FromBytes(nil).String()
			parsedDigest, parseErr := godigest.Parse(digest)
			if parseErr == nil && parsedDigest.String() == emptyDigest {
				localData = []byte{}
				if putErr := localDriver.PutContent(ctx, blobPath, localData, nil); putErr != nil && putErr != sftp.ErrRepoNotFound {
					log.Printf("commitBlobUpload (sync empty): failed to write empty blob: %v", putErr)
					w.WriteHeader(http.StatusInternalServerError)
					w.Write([]byte("Failed to write empty blob"))
					return
				}
				if err := uploadBlobToSFTP(ctx, blobPath, blobPath, localData); err != nil {
					log.Printf("commitBlobUpload (sync empty): SFTP upload failed: %v", err)
					w.WriteHeader(http.StatusInternalServerError)
					w.Write([]byte("Failed to upload blob to storage"))
					return
				}
				w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/%s", name, emptyDigest))
				w.Header().Set("Docker-Content-Digest", emptyDigest)
				w.WriteHeader(http.StatusCreated)
				w.Write([]byte("Blob committed (empty)"))
				return
			}
			// No local upload file and not empty blob: client may be mounting from existing blob (no PATCH sent). If blob exists on SFTP, accept.
			if sftpDriver != nil {
				if _, statErr := sftpDriver.Stat(ctx, blobPath); statErr == nil {
					parsedDigest, parseErr := godigest.Parse(digest)
					if parseErr == nil {
						w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/%s", name, parsedDigest.String()))
						w.Header().Set("Docker-Content-Digest", parsedDigest.String())
						w.WriteHeader(http.StatusCreated)
						w.Write([]byte("Blob committed (mount from existing)"))
						return
					}
				}
			}
			log.Printf("commitBlobUpload (sync chunked): failed to read local: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("Failed to read blob from local: " + err.Error()))
			return
		}
		calculated := godigest.FromBytes(localData)
		parsedDigest, parseErr := godigest.Parse(digest)
		if parseErr != nil {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte("invalid checksum digest format (parse)"))
			return
		}
		if calculated != parsedDigest {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte("invalid checksum digest format (mismatch)"))
			return
		}
		if err := uploadBlobToSFTP(ctx, uploadPath, blobPath, localData); err != nil {
			log.Printf("commitBlobUpload (sync chunked): SFTP upload failed: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("Failed to upload blob to storage: " + err.Error()))
			return
		}
		w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/%s", name, calculated.String()))
		w.Header().Set("Docker-Content-Digest", calculated.String())
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte("Blob committed (sync chunked to SFTP, digest validated)"))
		return
	}

	// Non-streaming path: read body (or use existing local from PATCHes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("commitBlobUpload: failed to read blob data: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Failed to read blob data: " + err.Error()))
		return
	}
	var localData []byte
	if len(body) == 0 {
		// Validate digest before move so we don't leave a bad blob at blobPath on mismatch.
		localData, err = localDriver.GetContent(ctx, uploadPath)
		if err != nil {
			// GET with digest but no PATCH: client commits without sending data (e.g. empty layer or mount). Only valid for empty blob.
			emptyDigest := godigest.FromBytes(nil).String()
			parsedDigest, parseErr := godigest.Parse(digest)
			if parseErr == nil && parsedDigest.String() == emptyDigest {
				localData = []byte{}
				if putErr := localDriver.PutContent(ctx, blobPath, localData, nil); putErr != nil && putErr != sftp.ErrRepoNotFound {
					log.Printf("commitBlobUpload: failed to write empty blob: %v", putErr)
					w.WriteHeader(http.StatusInternalServerError)
					w.Write([]byte("Failed to write empty blob"))
					return
				}
				if err := uploadBlobToSFTP(ctx, blobPath, blobPath, localData); err != nil {
					log.Printf("commitBlobUpload: SFTP upload empty blob failed: %v", err)
					w.WriteHeader(http.StatusInternalServerError)
					w.Write([]byte("Failed to upload blob to storage"))
					return
				}
				w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/%s", name, emptyDigest))
				w.Header().Set("Docker-Content-Digest", emptyDigest)
				w.WriteHeader(http.StatusCreated)
				w.Write([]byte("Blob committed (empty)"))
				return
			}
			// Mount from existing blob on SFTP (no PATCH sent).
			if sftpDriver != nil {
				if _, statErr := sftpDriver.Stat(ctx, blobPath); statErr == nil {
					parsedDigest, parseErr := godigest.Parse(digest)
					if parseErr == nil {
						w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/%s", name, parsedDigest.String()))
						w.Header().Set("Docker-Content-Digest", parsedDigest.String())
						w.WriteHeader(http.StatusCreated)
						w.Write([]byte("Blob committed (mount from existing)"))
						return
					}
				}
			}
			log.Printf("commitBlobUpload: failed to read upload from local: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("Failed to read blob from local: " + err.Error()))
			return
		}
		calculated := godigest.FromBytes(localData)
		parsedDigest, parseErr := godigest.Parse(digest)
		if parseErr != nil {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte("invalid checksum digest format (parse)"))
			return
		}
		if calculated != parsedDigest {
			log.Printf("commitBlobUpload: DIGEST_INVALID upload size %d, expected %s, got %s (incomplete chunked upload?)", len(localData), parsedDigest, calculated)
			registryError(w, "DIGEST_INVALID", fmt.Sprintf("blob upload incomplete or digest mismatch (upload size %d)", len(localData)), 400)
			return
		}
		err = localDriver.Move(ctx, uploadPath, blobPath)
		if err != nil {
			if err == sftp.ErrRepoNotFound {
				registryError(w, "NAME_INVALID", fmt.Sprintf("repository name %s not found", name), 404)
				return
			}
			log.Printf("commitBlobUpload: failed to move blob on local: %v (from: %s, to: %s)", err, uploadPath, blobPath)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("Failed to move blob on local: " + err.Error()))
			return
		}
	} else {
		err = localDriver.PutContent(ctx, blobPath, body, nil)
		if err != nil {
			if err == sftp.ErrRepoNotFound {
				registryError(w, "NAME_INVALID", fmt.Sprintf("repository name %s not found", name), 404)
				return
			}
			log.Printf("commitBlobUpload: failed to write blob to local: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("Failed to write blob to local: " + err.Error()))
			return
		}
		localData, err = localDriver.GetContent(ctx, blobPath)
		if err != nil {
			log.Printf("commitBlobUpload: failed to read blob from local: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("Failed to read blob from local: " + err.Error()))
			return
		}
		calculated := godigest.FromBytes(localData)
		parsedDigest, parseErr := godigest.Parse(digest)
		if parseErr != nil {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte("invalid checksum digest format (parse)"))
			return
		}
		if calculated != parsedDigest {
			log.Printf("commitBlobUpload: DIGEST_INVALID (PUT with body) size %d", len(body))
			registryError(w, "DIGEST_INVALID", "digest mismatch", 400)
			return
		}
	}

	calculated := godigest.FromBytes(localData)
	doBlobUpload := func() error {
		return uploadBlobToSFTP(ctx, blobPath, blobPath, localData)
	}

	if cfg != nil && cfg.SFTPSyncUpload {
		if err := doBlobUpload(); err != nil {
			log.Printf("commitBlobUpload (sync): SFTP upload failed: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("Failed to upload blob to storage: " + err.Error()))
			return
		}
	} else {
		go doBlobUpload()
	}

	w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/%s", name, calculated.String()))
	w.Header().Set("Docker-Content-Digest", calculated.String())
	w.WriteHeader(http.StatusCreated)
	if cfg != nil && cfg.SFTPSyncUpload {
		w.Write([]byte("Blob committed (sync SFTP, digest validated)"))
	} else {
		w.Write([]byte("Blob committed (async SFTP, digest validated)"))
	}
}

// uploadBlobToSFTP uploads blob data to SFTP (with semaphore, lock, retry). Call in goroutine for async or inline for sync.
func uploadBlobToSFTP(ctx context.Context, localPath, sftpPath string, data []byte) error {
	sftpSemaphore <- struct{}{}
	defer func() { <-sftpSemaphore }()

	lockIface, _ := sftpPathLocks.LoadOrStore(sftpPath, &sync.Mutex{})
	pathLock := lockIface.(*sync.Mutex)
	pathLock.Lock()
	defer pathLock.Unlock()

	wantSize := int64(len(data))
	if fi, err := sftpDriver.Stat(ctx, sftpPath); err == nil {
		type sizeable interface{ Size() int64 }
		var existing int64
		if s, ok := fi.(sizeable); ok {
			existing = s.Size()
			if existing == wantSize {
				log.Printf("[SFTP] SKIP: blob already exists (same size): %s", sftpPath)
				_ = localDriver.Delete(ctx, localPath)
				return nil
			}
		}
		_ = sftpDriver.Delete(ctx, sftpPath)
		log.Printf("[SFTP] Overwrite: replacing blob (existing %d vs %d): %s", existing, wantSize, sftpPath)
	}

	log.Printf("[SFTP] Start upload: %s -> %s", localPath, sftpPath)
	maxRetry := 5
	var err error
	for i := 0; i < maxRetry; i++ {
		err = sftpDriver.PutContent(ctx, sftpPath, data, func(written, total int64) {
			percent := int64(0)
			if total > 0 {
				percent = written * 100 / total
			}
			log.Printf("[SFTP] Progress: %s -> %s: %d%% (%d/%d bytes)", localPath, sftpPath, percent, written, total)
		})
		if err == nil {
			log.Printf("[SFTP] Success upload: %s -> %s (try %d)", localPath, sftpPath, i+1)
			break
		}
		backoff := 1 << i
		if backoff > 16 {
			backoff = 16
		}
		log.Printf("[SFTP] Retry %d: failed to upload: %v, retry in %ds", i+1, err, backoff)
		time.Sleep(time.Duration(backoff) * time.Second)
	}
	if err != nil {
		log.Printf("[SFTP] FINAL FAIL: %v", err)
		return err
	}
	_ = localDriver.Delete(ctx, localPath)
	return nil
}

// uploadManifestToSFTP uploads manifest to SFTP (tag + digest paths). Call in goroutine for async or inline for sync.
func uploadManifestToSFTP(ctx context.Context, tagPath, digestPath string, data []byte) error {
	sftpSemaphore <- struct{}{}
	defer func() { <-sftpSemaphore }()

	maxRetry := 5

	// Upload by tag path
	lockIface, _ := sftpPathLocks.LoadOrStore(tagPath, &sync.Mutex{})
	pathLock := lockIface.(*sync.Mutex)
	pathLock.Lock()
	log.Printf("[SFTP] Start upload manifest (tag): %s", tagPath)
	var err error
	for i := 0; i < maxRetry; i++ {
		err = sftpDriver.PutContent(ctx, tagPath, data, nil)
		if err == nil {
			log.Printf("[SFTP] Success manifest (tag): %s (try %d)", tagPath, i+1)
			break
		}
		log.Printf("[SFTP] Retry %d manifest (tag): %v", i+1, err)
		time.Sleep(2 * time.Second)
	}
	pathLock.Unlock()
	if err != nil {
		log.Printf("[SFTP] FINAL FAIL manifest (tag): %v", err)
		return err
	}

	// Upload by digest path
	lockIface2, _ := sftpPathLocks.LoadOrStore(digestPath, &sync.Mutex{})
	pathLock2 := lockIface2.(*sync.Mutex)
	pathLock2.Lock()
	log.Printf("[SFTP] Start upload manifest (digest): %s", digestPath)
	for i := 0; i < maxRetry; i++ {
		err = sftpDriver.PutContent(ctx, digestPath, data, nil)
		if err == nil {
			log.Printf("[SFTP] Success manifest (digest): %s (try %d)", digestPath, i+1)
			break
		}
		log.Printf("[SFTP] Retry %d manifest (digest): %v", i+1, err)
		time.Sleep(2 * time.Second)
	}
	pathLock2.Unlock()
	if err != nil {
		log.Printf("[SFTP] FINAL FAIL manifest (digest): %v", err)
		return err
	}
	return nil
}

// Handler untuk endpoint signatures
func handleSignatures(w http.ResponseWriter, r *http.Request, path string) {
	// Parse path but don't use variables for now since we're just returning empty responses
	_ = strings.TrimPrefix(strings.Split(path, "/signatures/")[0], "/")
	_ = strings.Split(path, "/signatures/")[1]

	// Drain body when we don't use it so connection can be reused (same as initiateBlobUpload)
	if r.Body != nil && (r.Method == http.MethodPost || r.Method == http.MethodDelete) {
		io.Copy(io.Discard, r.Body)
		r.Body.Close()
	}

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
	allEntries, err := sftpDriver.List(context.TODO(), manifestDir)
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
	// Only return actual tag names; exclude digest-named manifest files (sha256:...)
	tags := make([]string, 0, len(allEntries))
	for _, e := range allEntries {
		if !strings.HasPrefix(e, "sha256:") {
			tags = append(tags, e)
		}
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

	// Calculate total size: from layers (single image) or for manifest list fetch each sub-manifest and sum layer sizes
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
	if totalSize == 0 {
		// Manifest list (multi-arch): resolve each referenced manifest and sum its layer sizes
		if manifests, ok := manifest["manifests"].([]interface{}); ok && sftpDriver != nil {
			manifestPathBase := "registry/" + name + "/manifests/"
			for _, m := range manifests {
				mMap, ok := m.(map[string]interface{})
				if !ok {
					continue
				}
				digestStr, _ := mMap["digest"].(string)
				if digestStr == "" {
					continue
				}
				subManifestBytes, err := sftpDriver.GetContent(context.TODO(), manifestPathBase+digestStr)
				if err != nil {
					continue
				}
				var subManifest map[string]interface{}
				if json.Unmarshal(subManifestBytes, &subManifest) != nil {
					continue
				}
				if layers, ok := subManifest["layers"].([]interface{}); ok {
					for _, layer := range layers {
						if layerMap, ok := layer.(map[string]interface{}); ok {
							if size, ok := layerMap["size"].(float64); ok {
								totalSize += int64(size)
							}
						}
					}
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
		return err
	}

	if onImageSaved != nil {
		onImageSaved()
	}
	return nil
} 