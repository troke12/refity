package registry

import (
	"net/http"
	"refity/internal/config"
	"refity/internal/driver/sftp"
	"refity/internal/driver/local"
	"refity/internal/database"
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