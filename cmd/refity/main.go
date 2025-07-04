package main

import (
	"log"
	"net/http"
	"os"

	"refity/internal/config"
	"refity/internal/driver/sftp"
	"refity/internal/registry"
	"refity/internal/auth"
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
	driver, err := sftp.NewDriver()
	if err != nil {
		log.Fatalf("Failed to connect to SFTP: %v", err)
	}
	log.Println("SFTP connection established successfully")

	// Inject driver ke registry
	regRouter := registry.NewRouterWithDeps(driver, cfg)

	authRouter := auth.BasicAuthMiddleware(cfg.RegistryUsername, cfg.RegistryPassword, regRouter)

	port := os.Getenv("PORT")
	if port == "" {
		port = "5000"
	}
	log.Printf("Listening on :%s", port)
	if err := http.ListenAndServe(":"+port, authRouter); err != nil {
		log.Fatalf("Server error: %v", err)
	}
} 