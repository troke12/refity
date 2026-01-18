package main

import (
	"log"
	"net/http"
	"os"

	"refity/backend/internal/config"
	"refity/backend/internal/database"
	"refity/backend/internal/driver/sftp"
	"refity/backend/internal/driver/local"
	"refity/backend/internal/registry"
	"refity/backend/internal/api"
)

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		// Allow frontend origins
		allowedOrigins := []string{"http://localhost:8080", "http://127.0.0.1:8080"}
		allowed := false
		for _, allowedOrigin := range allowedOrigins {
			if origin == allowedOrigin {
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

		// Handle preflight requests
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func main() {
	log.Println("Starting Refity Docker Registry Backend...")

	cfg := config.LoadConfig()
	if cfg.FTPHost == "" || cfg.FTPUsername == "" || cfg.FTPPassword == "" {
		log.Fatal("FTP config must be set in environment variables")
	}

	sftpPort := cfg.FTPPort
	if sftpPort == "" {
		sftpPort = "22"
	}
	log.Printf("Connecting to SFTP: host=%s port=%s user=%s", cfg.FTPHost, sftpPort, cfg.FTPUsername)

	localRoot := "/tmp/refity"
	localDriver := local.NewDriver(localRoot)
	driver, err := sftp.NewDriverWithConfig(cfg)
	if err != nil {
		log.Fatalf("Failed to connect to SFTP: %v", err)
	}
	log.Println("SFTP connection established successfully")

	// Initialize database
	dbPath := "data/refity.db"
	// Ensure data directory exists
	if err := os.MkdirAll("data", 0755); err != nil {
		log.Printf("Warning: Failed to create data directory: %v", err)
	}
	db, err := database.NewDatabase(dbPath)
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}
	defer db.Close()
	log.Println("Database initialized successfully")

	regRouter := registry.NewRouterWithDeps(localDriver, driver, cfg, db)
	apiRouter := api.NewAPIRouter(driver, db, cfg)

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

	// Apply CORS middleware
	handler := corsMiddleware(mainRouter)

	port := os.Getenv("PORT")
	if port == "" {
		port = "5000"
	}
	log.Printf("Backend server listening on :%s", port)
	if err := http.ListenAndServe(":"+port, handler); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}

