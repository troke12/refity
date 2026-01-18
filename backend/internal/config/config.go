package config

import (
	"fmt"
	"os"
)

type Config struct {
	FTPHost         string
	FTPPort         string
	FTPUsername     string
	FTPPassword     string
	HetznerToken    string
	HetznerBoxID    int
}

func LoadConfig() *Config {
	boxID := 0
	if boxIDStr := os.Getenv("HETZNER_BOX_ID"); boxIDStr != "" {
		fmt.Sscanf(boxIDStr, "%d", &boxID)
	}
	return &Config{
		FTPHost:      os.Getenv("FTP_HOST"),
		FTPPort:      os.Getenv("FTP_PORT"),
		FTPUsername:  os.Getenv("FTP_USERNAME"),
		FTPPassword:  os.Getenv("FTP_PASSWORD"),
		HetznerToken: os.Getenv("HCLOUD_TOKEN"),
		HetznerBoxID: boxID,
	}
}

