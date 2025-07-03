package main

import (
	"fmt"
	"os"
	"log"
	"time"
	"github.com/joho/godotenv"
	"golang.org/x/crypto/ssh"
	"github.com/pkg/sftp"
)

func main() {
	_ = godotenv.Load()

	host := os.Getenv("FTP_HOST")
	port := os.Getenv("FTP_PORT")
	user := os.Getenv("FTP_USERNAME")
	pass := os.Getenv("FTP_PASSWORD")
	if host == "" || port == "" || user == "" || pass == "" {
		log.Fatal("Set FTP_HOST, FTP_PORT, FTP_USERNAME, FTP_PASSWORD env")
	}
	addr := fmt.Sprintf("%s:%s", host, port)
	config := &ssh.ClientConfig{
		User: user,
		Auth: []ssh.AuthMethod{ssh.Password(pass)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout: 10 * time.Second,
	}
	log.Printf("Connecting to SFTP: %s@%s", user, addr)
	conn, err := ssh.Dial("tcp", addr, config)
	if err != nil {
		log.Fatalf("Failed to dial SSH: %v", err)
	}
	client, err := sftp.NewClient(conn)
	if err != nil {
		log.Fatalf("Failed to create SFTP client: %v", err)
	}
	defer client.Close()
	log.Println("SFTP connected!")

	dir := "registry/testdir"
	log.Printf("Membuat folder: %s", dir)
	err = client.MkdirAll(dir)
	if err != nil {
		log.Fatalf("Gagal membuat folder: %v", err)
	}
	log.Println("Folder berhasil dibuat")

	filePath := dir + "/testfile.txt"
	log.Printf("Upload file: %s", filePath)
	f, err := client.Create(filePath)
	if err != nil {
		log.Fatalf("Gagal membuat file: %v", err)
	}
	_, err = f.Write([]byte("hello sftp test"))
	if err != nil {
		log.Fatalf("Gagal menulis file: %v", err)
	}
	f.Close()
	log.Println("File berhasil diupload!")
} 