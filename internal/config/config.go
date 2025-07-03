package config

import (
	"os"
)

type Config struct {
	RegistryUsername string
	RegistryPassword string
	FTPHost          string
	FTPPort          string
	FTPUsername      string
	FTPPassword      string
}

func LoadConfig() *Config {
	return &Config{
		RegistryUsername: os.Getenv("REGISTRY_USERNAME"),
		RegistryPassword: os.Getenv("REGISTRY_PASSWORD"),
		FTPHost:          os.Getenv("FTP_HOST"),
		FTPPort:          os.Getenv("FTP_PORT"),
		FTPUsername:      os.Getenv("FTP_USERNAME"),
		FTPPassword:      os.Getenv("FTP_PASSWORD"),
	}
} 