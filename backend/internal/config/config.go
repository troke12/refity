package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	FTPHost        string
	FTPPort        string
	FTPUsername    string
	FTPPassword    string
	HetznerToken   string
	HetznerBoxID   int
	JWTSecret      string   // Required in production; from JWT_SECRET
	CORSOrigins    []string // Allowed origins for CORS; from CORS_ORIGINS (comma-sep)
	FTPKnownHosts  string   // Optional path to known_hosts for SSH host key verification
	SFTPSyncUpload bool     // If true, upload to SFTP before responding (file on FTP when push completes). If false, upload in background (async).
	EnableFTPUsage bool     // If true, dashboard fetches Hetzner Storage Box usage (FTP Usage card). Set false if not using Hetzner to avoid API errors.

	// Async upload spool and read cache. Empty dirs are filled in by main (under the data dir).
	SpoolDir              string        // SPOOL_DIR: durable local copies of blobs/manifests waiting for upload
	SpoolMaxBytes         int64         // SPOOL_MAX_BYTES: pending bytes before commits fall back to sync (0 = unlimited)
	UploadWorkers         int           // UPLOAD_WORKERS: background uploaders (capped at the SFTP pool size)
	ReadCacheDir          string        // READ_CACHE_DIR: verified copies of pulled blobs
	ReadCacheBytes        int64         // READ_CACHE_BYTES: read cache budget (0 = disabled)
	SFTPStallTimeout      time.Duration // SFTP_STALL_TIMEOUT: abort an SFTP connection that makes no progress this long (0 = off)
	SFTPParallelThreshold int64         // SFTP_PARALLEL_THRESHOLD: blobs at least this big upload over several connections (0 = off)
	SFTPAcquireTimeout    time.Duration // SFTP_ACQUIRE_TIMEOUT: max wait for a pooled connection before failing the call
	StreamWriteTimeout    time.Duration // STREAM_WRITE_TIMEOUT: a blob download whose client accepts no data this long is aborted
	SpoolMinFreeBytes     int64         // SPOOL_MIN_FREE_BYTES: below this free space on the spool volume, commits upload synchronously
	DataDir               string        // data root holding refity.db (set by main, not from the environment)
}

const (
	defaultSpoolMaxBytes         = 20 << 30
	defaultUploadWorkers         = 4
	defaultReadCacheBytes        = 5 << 30
	defaultSFTPStallTimeout      = 60 * time.Second
	defaultSFTPParallelThreshold = 8 << 20
	defaultSFTPAcquireTimeout    = 30 * time.Second
	defaultStreamWriteTimeout    = 5 * time.Minute
	defaultSpoolMinFreeBytes     = 2 << 30
)

// parseBytes accepts a plain byte count or a binary-suffixed size ("512MiB", "20G", "5GB").
// Decimal-looking suffixes are treated as binary on purpose: these are disk budgets, not marketing sizes.
func parseBytes(s string) (int64, error) {
	s = strings.TrimSpace(s)
	upper := strings.ToUpper(s)
	mult := int64(1)
	for _, u := range []struct {
		suffix string
		mult   int64
	}{
		{"KIB", 1 << 10}, {"MIB", 1 << 20}, {"GIB", 1 << 30}, {"TIB", 1 << 40},
		{"KB", 1 << 10}, {"MB", 1 << 20}, {"GB", 1 << 30}, {"TB", 1 << 40},
		{"K", 1 << 10}, {"M", 1 << 20}, {"G", 1 << 30}, {"T", 1 << 40},
		{"B", 1},
	} {
		if strings.HasSuffix(upper, u.suffix) {
			mult = u.mult
			s = strings.TrimSpace(s[:len(s)-len(u.suffix)])
			break
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	// Reject instead of wrapping: an overflowed product can turn negative, and a negative or zero budget
	// would silently mean "unlimited".
	if n > math.MaxInt64/mult {
		return 0, fmt.Errorf("size %q overflows", s)
	}
	return n * mult, nil
}

func envBytes(name string, def int64) int64 {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := parseBytes(v)
	if err != nil {
		log.Printf("WARNING: %s=%q is not a valid size, using default %d", name, v, def)
		return def
	}
	return n
}

// envDuration accepts Go durations ("90s", "2m") or a bare number of seconds.
func envDuration(name string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil && secs >= 0 {
		if secs > int64(math.MaxInt64/time.Second) {
			log.Printf("WARNING: %s=%q overflows, using default %v", name, v, def)
			return def
		}
		return time.Duration(secs) * time.Second
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		log.Printf("WARNING: %s=%q is not a valid duration, using default %v", name, v, def)
		return def
	}
	return d
}

func envInt(name string, def int) int {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		log.Printf("WARNING: %s=%q is not a positive integer, using default %d", name, v, def)
		return def
	}
	return n
}

func LoadConfig() *Config {
	boxID := 0
	if boxIDStr := os.Getenv("HETZNER_BOX_ID"); boxIDStr != "" {
		fmt.Sscanf(boxIDStr, "%d", &boxID)
	}
	jwtSecret := os.Getenv("JWT_SECRET")
	if jwtSecret == "" {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			panic("failed to generate random JWT secret: " + err.Error())
		}
		jwtSecret = hex.EncodeToString(b)
		log.Println("WARNING: JWT_SECRET not set. Generated random secret for this session. Tokens will be invalidated on restart. Set JWT_SECRET in production.")
	}
	corsOrigins := []string{"http://localhost:8080", "http://127.0.0.1:8080"}
	if s := os.Getenv("CORS_ORIGINS"); s != "" {
		corsOrigins = strings.Split(strings.TrimSpace(s), ",")
		for i := range corsOrigins {
			corsOrigins[i] = strings.TrimSpace(corsOrigins[i])
		}
	}
	syncUpload := strings.ToLower(os.Getenv("SFTP_SYNC_UPLOAD")) == "true" || os.Getenv("SFTP_SYNC_UPLOAD") == "1"
	enableFTPUsage := false
	if s := os.Getenv("FTP_USAGE_ENABLED"); s != "" {
		enableFTPUsage = strings.ToLower(s) == "true" || s == "1" || strings.ToLower(s) == "yes"
	}
	return &Config{
		FTPHost:        os.Getenv("FTP_HOST"),
		FTPPort:        os.Getenv("FTP_PORT"),
		FTPUsername:    os.Getenv("FTP_USERNAME"),
		FTPPassword:    os.Getenv("FTP_PASSWORD"),
		HetznerToken:   os.Getenv("HCLOUD_TOKEN"),
		HetznerBoxID:   boxID,
		JWTSecret:      jwtSecret,
		CORSOrigins:    corsOrigins,
		FTPKnownHosts:  os.Getenv("FTP_KNOWN_HOSTS"),
		SFTPSyncUpload: syncUpload,
		EnableFTPUsage: enableFTPUsage,

		SpoolDir:              os.Getenv("SPOOL_DIR"),
		SpoolMaxBytes:         envBytes("SPOOL_MAX_BYTES", defaultSpoolMaxBytes),
		UploadWorkers:         envInt("UPLOAD_WORKERS", defaultUploadWorkers),
		ReadCacheDir:          os.Getenv("READ_CACHE_DIR"),
		ReadCacheBytes:        envBytes("READ_CACHE_BYTES", defaultReadCacheBytes),
		SFTPStallTimeout:      envDuration("SFTP_STALL_TIMEOUT", defaultSFTPStallTimeout),
		SFTPParallelThreshold: envBytes("SFTP_PARALLEL_THRESHOLD", defaultSFTPParallelThreshold),
		SFTPAcquireTimeout:    envDuration("SFTP_ACQUIRE_TIMEOUT", defaultSFTPAcquireTimeout),
		StreamWriteTimeout:    envDuration("STREAM_WRITE_TIMEOUT", defaultStreamWriteTimeout),
		SpoolMinFreeBytes:     envBytes("SPOOL_MIN_FREE_BYTES", defaultSpoolMinFreeBytes),
	}
}
