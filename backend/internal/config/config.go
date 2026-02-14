package config

import (
	"fmt"
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
}

func LoadConfig() *Config {
	boxID := 0
	if boxIDStr := os.Getenv("HETZNER_BOX_ID"); boxIDStr != "" {
		fmt.Sscanf(boxIDStr, "%d", &boxID)
	}
	jwtSecret := os.Getenv("JWT_SECRET")
	if jwtSecret == "" {
		jwtSecret = "refity-secret-key-change-in-production" // dev only; production must set JWT_SECRET
	}
	corsOrigins := []string{"http://localhost:8080", "http://127.0.0.1:8080"}
	if s := os.Getenv("CORS_ORIGINS"); s != "" {
		corsOrigins = strings.Split(strings.TrimSpace(s), ",")
		for i := range corsOrigins {
			corsOrigins[i] = strings.TrimSpace(corsOrigins[i])
		}
	}
	return &Config{
		FTPHost:       os.Getenv("FTP_HOST"),
		FTPPort:       os.Getenv("FTP_PORT"),
		FTPUsername:   os.Getenv("FTP_USERNAME"),
		FTPPassword:   os.Getenv("FTP_PASSWORD"),
		HetznerToken:  os.Getenv("HCLOUD_TOKEN"),
		HetznerBoxID:  boxID,
		JWTSecret:     jwtSecret,
		CORSOrigins:   corsOrigins,
		FTPKnownHosts: os.Getenv("FTP_KNOWN_HOSTS"),
	}
}

