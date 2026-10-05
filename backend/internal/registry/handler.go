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
	"os"
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
		return "", fmt.Errorf("JWT secret is not configured")
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
		return state, fmt.Errorf("JWT secret is not configured")
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
var validRepoName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*(/[a-zA-Z0-9][a-zA-Z0-9._-]*)?$`)

// validDigest validates Docker content digest format: algorithm:hex
var validDigest = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

func validateRepoName(name string) bool {
	if name == "" || len(name) > 256 || strings.Contains(name, "..") {
		return false
	}
	return validRepoName.MatchString(name)
}

// validateRepoNameForPush requires group/repo (at least one '/') so images are not pushed at registry root.
func validateRepoNameForPush(name string) bool {
	return validateRepoName(name) && strings.Contains(name, "/")
}

// autoEnsureGroupForPush records the path prefix as a group (DB + SFTP) so docker push group/repo works without pre-creating the group in the UI.
func autoEnsureGroupForPush(repoName string) {
	idx := strings.IndexByte(repoName, '/')
	if idx <= 0 {
		return
	}
	group := repoName[:idx]
	ctx := context.TODO()
	if db != nil {
		if err := db.EnsureGroup(group); err != nil {
			log.Printf("autoEnsureGroupForPush: EnsureGroup %q: %v", group, err)
		}
	}
	if sftpDriver != nil {
		if err := sftpDriver.CreateGroupFolder(ctx, group); err != nil {
			log.Printf("autoEnsureGroupForPush: CreateGroupFolder %q: %v", group, err)
		}
	}
}

func validateManifestRef(ref string) bool {
	if ref == "" || len(ref) > 128 || strings.ContainsAny(ref, "../\\") || strings.Contains(ref, "\x00") {
		return false
	}
	return true
}

func validateBlobDigest(digest string) bool {
	return validDigest.MatchString(digest)
}

var sftpSemaphore = make(chan struct{}, 2) // max 2 upload paralel
var sftpPathLocks sync.Map                 // map[string]*sync.Mutex

// sftpRetry wraps an SFTP operation with retry on transient connection failures.
func sftpRetry(desc string, maxRetries int, fn func() error) error {
	var lastErr error
	for i := 0; i < maxRetries; i++ {
		err := fn()
		if err == nil {
			return nil
		}
		lastErr = err
		if !sftp.IsConnectionLost(err) {
			return err
		}
		backoff := time.Duration(1<<i) * time.Second
		if backoff > 10*time.Second {
			backoff = 10 * time.Second
		}
		log.Printf("[SFTP] Retry %d/%d %s failed: %v, retrying in %v", i+1, maxRetries, desc, err, backoff)
		time.Sleep(backoff)
	}
	return fmt.Errorf("%s: all %d retries failed: %w", desc, maxRetries, lastErr)
}

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
	// /<name>/blobs/<digest> — HEAD so Docker can skip re-upload; existence = spool, read cache or SFTP.
	if strings.Contains(path, "/blobs/") && r.Method == http.MethodHead {
		handleBlobHead(w, r, path)
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
	if !validateRepoNameForPush(name) {
		registryError(w, "NAME_INVALID", "repository name must include a group prefix, e.g. mygroup/myimage", http.StatusBadRequest)
		return
	}
	autoEnsureGroupForPush(name)

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

	// Distribution spec: Initiate Monolithic Blob Upload — POST with ?digest= and body
	// completes upload in one request (no PATCH). Docker uses this for small blobs
	// (config, etc). Must work in both sync and async modes.
	digest := r.URL.Query().Get("digest")
	if digest != "" && sftpDriver != nil && r.Body != nil {
		parsedDigest, parseErr := godigest.Parse(digest)
		if parseErr != nil {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte("invalid checksum digest format (parse)"))
			return
		}
		blobPath := fmt.Sprintf("registry/%s/blobs/%s", name, digest)
		blobPath = strings.TrimLeft(blobPath, "/")
		// Stage the body locally while it arrives (fast for the client), verify, then persist: spooled in
		// async mode, uploaded before the response in sync mode. Staging first, instead of streaming the
		// body straight to SFTP, is what makes the remote write retryable on another connection.
		uploadID := strconv.FormatInt(time.Now().UnixNano(), 10)
		stagingPath := strings.TrimLeft(fmt.Sprintf("registry/%s/blobs/uploads/%s", name, uploadID), "/")
		local, err := stageBody(stagingPath, r.Body)
		_ = r.Body.Close()
		if err != nil {
			log.Printf("initiateBlobUpload (monolithic): staging failed: %v", err)
			_ = localDriver.Delete(context.TODO(), stagingPath)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("Failed to stream blob: " + err.Error()))
			return
		}
		calculated, n, err := hashFile(local)
		if err != nil {
			log.Printf("initiateBlobUpload (monolithic): hashing failed: %v", err)
			_ = localDriver.Delete(context.TODO(), stagingPath)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("Failed to read blob from local"))
			return
		}
		if calculated != parsedDigest {
			_ = localDriver.Delete(context.TODO(), stagingPath)
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte("invalid checksum digest format (mismatch)"))
			return
		}
		blobPath = strings.TrimLeft(fmt.Sprintf("registry/%s/blobs/%s", name, calculated.String()), "/")
		mode, err := persistBlob(local, blobPath, n, calculated.String())
		if err != nil {
			log.Printf("initiateBlobUpload (monolithic): persisting %s failed: %v", blobPath, err)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("Failed to upload blob to storage: " + err.Error()))
			return
		}
		w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/%s", name, calculated.String()))
		w.Header().Set("Docker-Content-Digest", calculated.String())
		w.Header().Set("Docker-Upload-UUID", uploadID)
		w.WriteHeader(http.StatusCreated)
		log.Printf("initiateBlobUpload: monolithic %s upload completed %s (%d bytes)", mode, name, n)
		return
	}

	// Chunked upload: drain body so connection can be reused for PATCH (Docker reuses same connection)
	io.Copy(io.Discard, r.Body)
	r.Body.Close()
	uploadID := strconv.FormatInt(time.Now().UnixNano(), 10)
	// Use relative path for Location (no scheme/host) so it works behind any reverse proxy.
	location := "/v2/" + name + "/blobs/uploads/" + uploadID
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
	w.Header().Set("Content-Length", "0")
	w.Header().Set("Docker-Upload-UUID", uploadID)
	w.WriteHeader(http.StatusAccepted)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func uploadBlobData(w http.ResponseWriter, r *http.Request, path string) {
	log.Printf("uploadBlobData: PATCH received for %s", path)
	// NOTE: Do NOT manually send 100 Continue — Go's net/http handles Expect: 100-continue
	// automatically when r.Body is read. Manually sending it interferes with reverse proxies
	// (Nginx/NPM) that handle the 100-continue protocol themselves.
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
	if !validateRepoNameForPush(name) {
		registryError(w, "NAME_INVALID", "repository name must include a group prefix, e.g. mygroup/myimage", http.StatusBadRequest)
		return
	}
	autoEnsureGroupForPush(name)
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
	// Large buffer so we pull from client quickly and avoid back-pressure / timeouts.
	buf := make([]byte, 1024*1024)
	n, err := io.CopyBuffer(dest, r.Body, buf)
	if err != nil {
		dest.Close()
		log.Printf("uploadBlobData: failed to stream blob data: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Failed to read blob data"))
		return
	}
	// Close writer before responding so data is fully flushed to disk.
	// This prevents a race where Docker sends PUT on a different connection
	// before the file is fully written.
	if err := dest.Close(); err != nil {
		log.Printf("uploadBlobData: writer close failed: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Failed to finalize chunk write"))
		return
	}
	totalSize := sizeBefore + n
	endRange := totalSize - 1
	if totalSize == 0 {
		endRange = 0
	}
	// Use relative path for Location (no scheme/host) so it works behind any reverse proxy.
	location := "/v2/" + name + "/blobs/uploads/" + uploadID
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
	// Use relative path for Location (no scheme/host) so it works behind any reverse proxy.
	location := "/v2/" + name + "/blobs/uploads/" + uploadID
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

// handleBlobHead returns 200 + Docker-Content-Digest + Content-Length if the blob exists in the upload spool, the read cache or on SFTP, else 404.
func handleBlobHead(w http.ResponseWriter, r *http.Request, path string) {
	name := strings.TrimPrefix(strings.TrimSuffix(strings.Split(path, "/blobs/")[0], "/"), "/")
	if !validateRepoName(name) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	blobPart := strings.Split(path, "/blobs/")[1]
	if !validateBlobDigest(blobPart) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	blobPath := fmt.Sprintf("registry/%s/blobs/%s", name, blobPart)
	blobPath = strings.TrimLeft(blobPath, "/")
	serveBlob(w, r, blobPath, blobPart)
}

func handleBlobDownload(w http.ResponseWriter, r *http.Request, path string) {
	name := strings.TrimPrefix(strings.TrimSuffix(strings.Split(path, "/blobs/")[0], "/"), "/")
	if !validateRepoName(name) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("invalid repository name"))
		return
	}
	blobPart := strings.Split(path, "/blobs/")[1]
	if !validateBlobDigest(blobPart) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("invalid blob digest format"))
		return
	}
	blobPath := fmt.Sprintf("registry/%s/blobs/%s", name, blobPart)
	blobPath = strings.TrimLeft(blobPath, "/")
	serveBlob(w, r, blobPath, blobPart)
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
		if !validateRepoNameForPush(name) {
			registryError(w, "NAME_INVALID", "repository name must include a group prefix, e.g. mygroup/myimage", http.StatusBadRequest)
			return
		}
		autoEnsureGroupForPush(name)
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
				exists, err := objectExistsErr(context.TODO(), manifestPath)
				if err != nil {
					// Cannot tell whether the child exists: retryable, not a client error.
					storageUnavailable(w, r, err)
					return
				}
				if !exists {
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

		manifestDigestPath := fmt.Sprintf("registry/%s/manifests/%s", name, digestStr)
		manifestDigestPath = strings.TrimLeft(manifestDigestPath, "/")

		ctx := context.TODO()
		if cfg != nil && cfg.SFTPSyncUpload {
			if err := uploadManifestToSFTP(ctx, manifestPath, manifestDigestPath, manifest); err != nil {
				log.Printf("handleManifest (sync): SFTP upload failed: %v", err)
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte("Failed to upload manifest to storage: " + err.Error()))
				return
			}
		} else {
			// Spooled durably before answering, so a pull right after the push finds the tag locally.
			for _, p := range []string{manifestPath, manifestDigestPath} {
				if err := persistManifest(manifest, p); err != nil {
					log.Printf("handleManifest: persisting %s failed: %v", p, err)
					w.WriteHeader(http.StatusInternalServerError)
					w.Write([]byte("Failed to upload manifest to storage: " + err.Error()))
					return
				}
			}
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
	case http.MethodGet, http.MethodHead:
		// Coba ambil manifest dengan ref yang diberikan (bisa tag atau digest)
		manifest, err := readManifest(context.TODO(), manifestPath)
		// A storage failure is not "manifest unknown": Docker treats 404 as permanent, 503 as retryable.
		storageErr := err != nil && !isNotFound(err)
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
					manifest, err = readManifest(context.TODO(), tagPath)
					if err == nil {
						manifestPath = tagPath
					} else {
						// Jika tidak ditemukan dengan tag, coba dengan digest
						digestPath := fmt.Sprintf("registry/%s/manifests/%s", name, img.Digest)
						digestPath = strings.TrimLeft(digestPath, "/")
						manifest, err = readManifest(context.TODO(), digestPath)
						if err == nil {
							manifestPath = digestPath
						}
					}
				}
			}

			if err != nil {
				if storageErr || !isNotFound(err) {
					storageUnavailable(w, r, err)
					return
				}
				registryError(w, "MANIFEST_UNKNOWN", "manifest not found", http.StatusNotFound)
				return
			}
		}

		storedDigest := godigest.FromBytes(manifest)
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
		w.Header().Set("Content-Length", strconv.Itoa(len(manifest)))
		w.Header().Set("Docker-Content-Digest", manifestDigest.String())
		w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
		// Save OCI manifest by digest so pull-by-digest conforms to distribution spec (avoids "falling back to pull by tag" warning).
		ociDigestPath := fmt.Sprintf("registry/%s/manifests/%s", name, manifestDigest.String())
		ociDigestPath = strings.TrimLeft(ociDigestPath, "/")
		// When the rewrite changed nothing, the digest copy was already stored at PUT time. Otherwise it is
		// written in the background: checking the remote can take up to the acquire timeout during an
		// outage, and a pull must not wait for a cosmetic copy.
		if manifestDigest != storedDigest {
			goOCICopy(ociDigestPath, manifest)
		}
		w.WriteHeader(http.StatusOK)
		w.Write(manifest)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func handleCatalog(w http.ResponseWriter) {
	entries, err := sftpDriver.List(context.TODO(), "registry")
	// Names that so far exist only in the upload spool (pushed, not yet on SFTP).
	var pending []string
	if sp := blobSpool(); sp != nil {
		for _, job := range sp.Pending("registry/") {
			if top, _, ok := strings.Cut(strings.TrimPrefix(job.Remote, "registry/"), "/"); ok && top != "" {
				pending = append(pending, top)
			}
		}
	}
	if err != nil && len(pending) == 0 {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Failed to list repositories"))
		return
	}
	repos := []string{}
	seen := map[string]bool{}
	for _, e := range append(entries, pending...) {
		if !seen[e] {
			seen[e] = true
			repos = append(repos, e)
		}
	}
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
	if !validateRepoNameForPush(name) {
		registryError(w, "NAME_INVALID", "repository name must include a group prefix, e.g. mygroup/myimage", http.StatusBadRequest)
		return
	}
	autoEnsureGroupForPush(name)
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

	parsedDigest, parseErr := godigest.Parse(digest)
	if parseErr != nil {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("invalid checksum digest format (parse)"))
		return
	}
	blobPath := strings.TrimLeft(fmt.Sprintf("registry/%s/blobs/%s", name, parsedDigest.String()), "/")
	uploadPath := fmt.Sprintf("registry/%s/blobs/uploads/%s", name, uploadID)
	uploadPath = strings.TrimLeft(uploadPath, "/")
	ctx := context.TODO()

	// A PUT body is the final chunk of the upload (all of it for a monolithic PUT), so it is appended to
	// whatever earlier PATCHes staged. The blob is then verified by streaming the staged file through
	// sha256 and persisted from disk; it is never held in memory.
	local, err := stageBody(uploadPath, r.Body)
	if err != nil {
		log.Printf("commitBlobUpload: failed to stage blob data: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Failed to read blob data: " + err.Error()))
		return
	}
	st, statErr := os.Stat(local)
	if statErr != nil || st.Size() == 0 {
		_ = localDriver.Delete(ctx, uploadPath)
		emptyDigest := godigest.FromBytes(nil)
		if parsedDigest == emptyDigest {
			// GET/PUT with digest but no data: only valid for the empty blob.
			if err := localDriver.PutContent(ctx, uploadPath, []byte{}, nil); err != nil {
				log.Printf("commitBlobUpload: failed to write empty blob: %v", err)
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte("Failed to write empty blob"))
				return
			}
			if _, err := persistBlob(local, blobPath, 0, emptyDigest.String()); err != nil {
				log.Printf("commitBlobUpload: persisting empty blob failed: %v", err)
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte("Failed to upload blob to storage"))
				return
			}
			w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/%s", name, emptyDigest))
			w.Header().Set("Docker-Content-Digest", emptyDigest.String())
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte("Blob committed (empty)"))
			return
		}
		// No data was sent: the client may be mounting a blob that already exists (spool, cache or remote).
		if sftpDriver != nil && objectExists(ctx, blobPath) {
			w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/%s", name, parsedDigest.String()))
			w.Header().Set("Docker-Content-Digest", parsedDigest.String())
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte("Blob committed (mount from existing)"))
			return
		}
		log.Printf("commitBlobUpload: no staged data for %s", uploadPath)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Failed to read blob from local: no data uploaded"))
		return
	}
	calculated, size, err := hashFile(local)
	if err != nil {
		log.Printf("commitBlobUpload: failed to hash staged blob: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Failed to read blob from local: " + err.Error()))
		return
	}
	if calculated != parsedDigest {
		log.Printf("commitBlobUpload: DIGEST_INVALID upload size %d, expected %s, got %s (incomplete chunked upload?)", size, parsedDigest, calculated)
		_ = localDriver.Delete(ctx, uploadPath)
		registryError(w, "DIGEST_INVALID", fmt.Sprintf("blob upload incomplete or digest mismatch (upload size %d)", size), 400)
		return
	}

	mode, err := persistBlob(local, blobPath, size, calculated.String())
	if err != nil {
		log.Printf("commitBlobUpload (%s): SFTP upload failed: %v", mode, err)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Failed to upload blob to storage: " + err.Error()))
		return
	}

	w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/%s", name, calculated.String()))
	w.Header().Set("Docker-Content-Digest", calculated.String())
	w.WriteHeader(http.StatusCreated)
	if mode == "sync" {
		w.Write([]byte("Blob committed (sync SFTP, digest validated)"))
	} else {
		w.Write([]byte("Blob committed (async SFTP, digest validated)"))
	}
}

// uploadManifestToSFTP writes a manifest to SFTP now (tag + digest paths), each atomically (local temp,
// remote temp, verify, rename) so a failed write never truncates the version already there.
func uploadManifestToSFTP(ctx context.Context, tagPath, digestPath string, data []byte) error {
	sftpSemaphore <- struct{}{}
	defer func() { <-sftpSemaphore }()
	for _, p := range []string{tagPath, digestPath} {
		if err := putManifestDirect(data, p); err != nil {
			log.Printf("[SFTP] FINAL FAIL manifest %s: %v", p, err)
			return err
		}
		log.Printf("[SFTP] Success manifest: %s", p)
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
	// Include tags pushed moments ago whose manifests are still in the upload spool.
	var pending []string
	if sp := blobSpool(); sp != nil {
		for _, job := range sp.Pending(manifestDir + "/") {
			pending = append(pending, remoteBase(job.Remote))
		}
	}
	if err != nil && !isNotFound(err) && len(pending) == 0 {
		// Nothing to show locally either: report the outage. When tags are spooled, they are listed
		// (a freshly pushed tag must stay visible while the Storage Box is unreachable).
		log.Printf("[REGISTRY] tags/list %s: storage unavailable: %v", repo, err)
		registryError(w, "UNAVAILABLE", "storage backend temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	if err != nil && len(pending) == 0 {
		w.WriteHeader(http.StatusNotFound)
		resp := map[string]interface{}{
			"errors": []map[string]interface{}{
				{"code": "NOT_FOUND", "message": "repo or tags not found"},
			},
		}
		json.NewEncoder(w).Encode(resp)
		return
	}
	// Only return actual tag names; exclude digest-named manifest files (sha256:...) and in-progress
	// upload temp files (<name>.uploading-<token>).
	tags := make([]string, 0, len(allEntries)+len(pending))
	seen := map[string]bool{}
	for _, e := range append(allEntries, pending...) {
		if strings.HasPrefix(e, "sha256:") || strings.Contains(e, ".uploading-") || seen[e] {
			continue
		}
		seen[e] = true
		tags = append(tags, e)
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
				subManifestBytes, err := readManifest(context.TODO(), manifestPathBase+digestStr)
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
