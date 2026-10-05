package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"refity/backend/internal/api"
	"refity/backend/internal/auth"
	"refity/backend/internal/config"
	"refity/backend/internal/database"
	"refity/backend/internal/driver/local"
	"refity/backend/internal/driver/sftp"
	"refity/backend/internal/registry"
)

func corsMiddleware(allowedOrigins []string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			allowed := false
			for _, o := range allowedOrigins {
				if origin == o {
					allowed = true
					break
				}
			}
			if allowed {
				w.Header().Set("Access-Control-Allow-Origin", origin)
			}
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
			w.Header().Set("Access-Control-Allow-Credentials", "true")

			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusOK)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

func main() {
	log.Println("Starting Refity Docker Registry Backend...")

	cfg := config.LoadConfig()
	if cfg.FTPHost == "" || cfg.FTPUsername == "" || cfg.FTPPassword == "" {
		log.Fatal("FTP config must be set in environment variables")
	}
	auth.InitSecret(cfg.JWTSecret)

	sftpPort := cfg.FTPPort
	if sftpPort == "" {
		sftpPort = "22"
	}
	// The pool dials cfg.FTPPort directly; an empty value would otherwise never connect now that a
	// failed connection is retried in the background instead of being fatal.
	cfg.FTPPort = sftpPort
	log.Printf("Connecting to SFTP: host=%s port=%s user=%s", cfg.FTPHost, sftpPort, cfg.FTPUsername)

	localRoot := "/tmp/refity"
	localDriver := local.NewDriver(localRoot)

	// One connection pool serves the registry and the web API. It starts even when no SFTP login works
	// (Storage Box down or unreachable, rejected credentials) and connects in the background behind
	// the auth breaker, so a restart never turns into a crash loop of failed logins and the spool and
	// cache keep being served meanwhile. The web API shares it instead of keeping its own eager
	// connection: that connection made startup fatal and would log in outside the breaker.
	regPool, err := sftp.NewDriverPool(cfg, 4)
	if err != nil {
		log.Fatalf("Invalid SFTP pool configuration: %v", err)
	}
	regDriver := &sftp.PoolStorageDriver{Pool: regPool}

	// Initialize database (use /app/data in container for consistent persistence with volume)
	dataDir := "data"
	if _, err := os.Stat("/app/data"); err == nil {
		dataDir = "/app/data"
	}
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		log.Printf("Warning: Failed to create data directory: %v", err)
	}
	// Spool and read cache live in the data dir (a volume in Docker) so pending uploads survive restarts.
	if cfg.SpoolDir == "" {
		cfg.SpoolDir = filepath.Join(dataDir, "spool")
	}
	if cfg.ReadCacheDir == "" {
		cfg.ReadCacheDir = filepath.Join(dataDir, "cache")
	}
	cfg.DataDir = dataDir
	dbPath := dataDir + "/refity.db"
	db, err := database.NewDatabase(dbPath)
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}
	defer db.Close()
	log.Println("Database initialized successfully")

	apiRouter := api.NewAPIRouter(regDriver, db, cfg)
	regRouter := registry.NewRouterWithDeps(localDriver, regDriver, cfg, db, apiRouter.InvalidateDashboardCache)

	// Create main router
	mainRouter := http.NewServeMux()

	// Registry API routes (v2) - Docker registry API
	mainRouter.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		regRouter.ServeHTTP(w, r)
	})

	// API routes (web UI API)
	mainRouter.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		apiRouter.ServeHTTP(w, r)
	})

	// Apply CORS middleware (use CORS_ORIGINS in production)
	handler := corsMiddleware(cfg.CORSOrigins)(mainRouter)

	port := os.Getenv("PORT")
	if port == "" {
		port = "5000"
	}
	// No ReadTimeout/WriteTimeout so long blob uploads (Singapore→Germany) don't get cut.
	// ReadHeaderTimeout protects against Slowloris; IdleTimeout reclaims idle keep-alive connections.
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	// Graceful shutdown: stop accepting requests, let in-flight ones finish, then stop the upload workers
	// so their jobs stay on disk cleanly (they resume on the next start).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() {
		log.Printf("Backend server listening on :%s", port)
		errCh <- srv.ListenAndServe()
	}()
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("Server error: %v", err)
		}
	case <-ctx.Done():
		log.Println("Shutting down...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("HTTP shutdown: %v", err)
		}
		cancel()
	}
	registry.Shutdown()
	regPool.Close()
	log.Println("Shutdown complete")
}
