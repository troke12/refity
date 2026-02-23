package registry

import (
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
	"refity/backend/internal/config"
	"refity/backend/internal/driver/sftp"
	"refity/backend/internal/driver/local"
	"refity/backend/internal/database"

	"golang.org/x/crypto/bcrypt"
)

var (
	localDriver   local.StorageDriver
	sftpDriver    sftp.StorageDriver
	db            *database.Database
	cfg           *config.Config
	onImageSaved  func() // optional callback to e.g. invalidate dashboard cache

	registryAuthAttempts   = make(map[string][]time.Time)
	registryAuthAttemptsMu sync.Mutex
)

func registryRateLimit(ip string) bool {
	registryAuthAttemptsMu.Lock()
	defer registryAuthAttemptsMu.Unlock()
	now := time.Now()
	cutoff := now.Add(-5 * time.Minute)
	valid := registryAuthAttempts[ip][:0]
	for _, t := range registryAuthAttempts[ip] {
		if t.After(cutoff) {
			valid = append(valid, t)
		}
	}
	registryAuthAttempts[ip] = valid
	return len(valid) < 20
}

func registryRateRecord(ip string) {
	registryAuthAttemptsMu.Lock()
	defer registryAuthAttemptsMu.Unlock()
	registryAuthAttempts[ip] = append(registryAuthAttempts[ip], time.Now())
}

func basicAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := r.RemoteAddr
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			ip = strings.Split(xff, ",")[0]
		}
		if !registryRateLimit(ip) {
			log.Printf("Registry auth rate limited for IP: %s", ip)
			w.Header().Set("Www-Authenticate", `Basic realm="Refity Registry"`)
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"errors":[{"code":"TOOMANYREQUESTS","message":"too many failed auth attempts"}]}`))
			return
		}
		username, password, ok := r.BasicAuth()
		if !ok || db == nil {
			w.Header().Set("Www-Authenticate", `Basic realm="Refity Registry"`)
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"errors":[{"code":"UNAUTHORIZED","message":"authentication required"}]}`))
			return
		}
		user, err := db.GetUserByUsername(username)
		if err != nil {
			registryRateRecord(ip)
			w.Header().Set("Www-Authenticate", `Basic realm="Refity Registry"`)
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"errors":[{"code":"UNAUTHORIZED","message":"invalid credentials"}]}`))
			return
		}
		if bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)) != nil {
			registryRateRecord(ip)
			w.Header().Set("Www-Authenticate", `Basic realm="Refity Registry"`)
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"errors":[{"code":"UNAUTHORIZED","message":"invalid credentials"}]}`))
			return
		}
		next(w, r)
	}
}

func NewRouterWithDeps(localD local.StorageDriver, sftpD sftp.StorageDriver, c *config.Config, database *database.Database, onSaved func()) http.Handler {
	localDriver = localD
	cfg = c
	onImageSaved = onSaved
	if sftpD != nil {
		sftpDriver = sftpD
	} else {
		pool, err := sftp.NewDriverPool(c, 4)
		if err != nil {
			panic("failed to init SFTP pool: " + err.Error())
		}
		sftpDriver = &sftp.PoolStorageDriver{Pool: pool}
	}
	db = database
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", basicAuth(RegistryHandler))
	return mux
}