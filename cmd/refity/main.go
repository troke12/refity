package main

import (
	"log"
	"net/http"
	"os"

	"refity/internal/config"
	"refity/internal/ftp"
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
	sftpClient, err := ftp.NewSFTPClient(cfg.FTPHost, sftpPort, cfg.FTPUsername, cfg.FTPPassword)
	if err != nil {
		log.Fatalf("Failed to connect to SFTP: %v", err)
	}
	defer sftpClient.Close()

	// Inject SFTP client & config ke registry handler (pakai closure/global sementara)
	regRouter := registry.NewRouterWithDeps(sftpClient, cfg)

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