package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"strings"
)

type Config struct {
	FTPHost         string
	FTPPort         string
	FTPUsername     string
	FTPPassword     string
	HetznerToken    string
	HetznerBoxID    int
	JWTSecret       string   // Required in production; from JWT_SECRET
	CORSOrigins     []string // Allowed origins for CORS; from CORS_ORIGINS (comma-sep)
	FTPKnownHosts   string   // Optional path to known_hosts for SSH host key verification
	SFTPSyncUpload  bool     // If true, upload to SFTP before responding (file on FTP when push completes). If false, upload in background (async).
	EnableFTPUsage  bool     // If true, dashboard fetches Hetzner Storage Box usage (FTP Usage card). Set false if not using Hetzner to avoid API errors.
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
		FTPKnownHosts:   os.Getenv("FTP_KNOWN_HOSTS"),
		SFTPSyncUpload:  syncUpload,
		EnableFTPUsage:  enableFTPUsage,
	}
}

