package registry

import (
	"net/http"
	"refity/backend/internal/config"
	"refity/backend/internal/driver/sftp"
	"refity/backend/internal/driver/local"
	"refity/backend/internal/database"
)

var (
	localDriver local.StorageDriver
	sftpDriver sftp.StorageDriver
	db         *database.Database
)

func NewRouterWithDeps(localD local.StorageDriver, sftpD sftp.StorageDriver, c *config.Config, database *database.Database) http.Handler {
	localDriver = localD
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
	mux.HandleFunc("/v2/", RegistryHandler)
	return mux
} 