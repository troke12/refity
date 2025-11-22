package config

import (
	"os"
)

type Config struct {
	FTPHost     string
	FTPPort     string
	FTPUsername string
	FTPPassword string
}

func LoadConfig() *Config {
	return &Config{
		FTPHost:     os.Getenv("FTP_HOST"),
		FTPPort:     os.Getenv("FTP_PORT"),
		FTPUsername: os.Getenv("FTP_USERNAME"),
		FTPPassword: os.Getenv("FTP_PASSWORD"),
	}
}

