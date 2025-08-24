package main

import (
	"log"
	"net/http"
	"os"
	"strings"

	"refity/internal/config"
	"refity/internal/database"
	"refity/internal/driver/sftp"
	"refity/internal/driver/local"
	"refity/internal/registry"
	"refity/internal/auth"
	"refity/internal/web"
)

// Refity: Docker registry proxy & storage to FTP
func main() {
	log.Println("Starting Refity Docker Registry Proxy...")

	cfg := config.LoadConfig()
	if cfg.RegistryUsername == "" || cfg.RegistryPassword == "" {
		log.Fatal("Registry username/password must be set in environment variables")
	}
	if cfg.FTPHost == "" || cfg.FTPUsername == "" || cfg.FTPPassword == "" {
		log.Fatal("FTP config must be set in environment variables")
	}

	sftpPort := cfg.FTPPort
	if sftpPort == "" {
		sftpPort = "23"
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
	webRouter := web.NewWebRouter(driver, db)

	// Create main router that handles both registry API and web UI
	mainRouter := http.NewServeMux()
	
	// Registry API routes (v2)
	mainRouter.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		regRouter.ServeHTTP(w, r)
	})
	
	// Web UI routes (root and dashboard)
	mainRouter.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Check if it's a web UI route
		if r.URL.Path == "/" || r.URL.Path == "/dashboard" || strings.HasPrefix(r.URL.Path, "/api/") {
			webRouter.ServeHTTP(w, r)
		} else {
			// Fallback to registry API
			regRouter.ServeHTTP(w, r)
		}
	})

	authRouter := auth.BasicAuthMiddleware(cfg.RegistryUsername, cfg.RegistryPassword, mainRouter)

	port := os.Getenv("PORT")
	if port == "" {
		port = "5000"
	}
	log.Printf("Listening on :%s", port)
	if err := http.ListenAndServe(":"+port, authRouter); err != nil {
		log.Fatalf("Server error: %v", err)
	}
} 