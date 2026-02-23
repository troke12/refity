package registry

import (
	"net/http"
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
)

func basicAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		if !ok || db == nil {
			w.Header().Set("Www-Authenticate", `Basic realm="Refity Registry"`)
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"errors":[{"code":"UNAUTHORIZED","message":"authentication required"}]}`))
			return
		}
		user, err := db.GetUserByUsername(username)
		if err != nil {
			w.Header().Set("Www-Authenticate", `Basic realm="Refity Registry"`)
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"errors":[{"code":"UNAUTHORIZED","message":"invalid credentials"}]}`))
			return
		}
		if bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)) != nil {
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