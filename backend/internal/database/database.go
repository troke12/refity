package database

import (
	"database/sql"
	"time"
	"golang.org/x/crypto/bcrypt"
	"log"

	_ "github.com/mattn/go-sqlite3"
)

type Database struct {
	db *sql.DB
}

type Image struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Tag       string    `json:"tag"`
	Digest    string    `json:"digest"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"created_at"`
}

type Layer struct {
	ID        int64  `json:"id"`
	ImageID   int64  `json:"image_id"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	MediaType string `json:"media_type"`
}

type Manifest struct {
	ID      int64  `json:"id"`
	ImageID int64  `json:"image_id"`
	Digest  string `json:"digest"`
	Content string `json:"content"`
}

type Repository struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

type User struct {
	ID        int64     `json:"id"`
	Username  string    `json:"username"`
	PasswordHash string `json:"-"` // Don't expose password hash in JSON
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
}

func NewDatabase(dbPath string) (*Database, error) {
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, err
	}

	if err := db.Ping(); err != nil {
		return nil, err
	}

	database := &Database{db: db}
	if err := database.createTables(); err != nil {
		return nil, err
	}

	// Create default admin user if it doesn't exist
	if err := database.createDefaultAdmin(); err != nil {
		log.Printf("Warning: Failed to create default admin user: %v", err)
	}

	return database, nil
}

func (d *Database) createTables() error {
	// Create users table
	_, err := d.db.Exec(`
		CREATE TABLE IF NOT EXISTS users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			username TEXT NOT NULL UNIQUE,
			password_hash TEXT NOT NULL,
			role TEXT DEFAULT 'user',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)
	`)
	if err != nil {
		return err
	}

	// Create images table
	_, err = d.db.Exec(`
		CREATE TABLE IF NOT EXISTS images (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			tag TEXT NOT NULL,
			digest TEXT NOT NULL UNIQUE,
			size INTEGER NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(name, tag)
		)
	`)
	if err != nil {
		return err
	}

	// Create repositories table
	_, err = d.db.Exec(`
		CREATE TABLE IF NOT EXISTS repositories (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL UNIQUE,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)
	`)
	if err != nil {
		return err
	}

	// Create layers table
	_, err = d.db.Exec(`
		CREATE TABLE IF NOT EXISTS layers (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			image_id INTEGER NOT NULL,
			digest TEXT NOT NULL,
			size INTEGER NOT NULL,
			media_type TEXT NOT NULL,
			FOREIGN KEY (image_id) REFERENCES images (id) ON DELETE CASCADE
		)
	`)
	if err != nil {
		return err
	}

	// Create manifests table
	_, err = d.db.Exec(`
		CREATE TABLE IF NOT EXISTS manifests (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			image_id INTEGER NOT NULL,
			digest TEXT NOT NULL,
			content TEXT NOT NULL,
			FOREIGN KEY (image_id) REFERENCES images (id) ON DELETE CASCADE
		)
	`)
	if err != nil {
		return err
	}

	// Create groups table
	_, err = d.db.Exec(`
		CREATE TABLE IF NOT EXISTS groups (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL UNIQUE,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)
	`)
	if err != nil {
		return err
	}

	return nil
}

func (d *Database) createDefaultAdmin() error {
	// Check if admin user already exists
	var count int
	err := d.db.QueryRow(`SELECT COUNT(*) FROM users WHERE username = 'admin'`).Scan(&count)
	if err != nil {
		return err
	}

	if count > 0 {
		return nil // Admin already exists
	}

	// Hash default password "admin"
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte("admin"), bcrypt.DefaultCost)
	if err != nil {
		return err
	}

	// Create admin user
	_, err = d.db.Exec(`
		INSERT INTO users (username, password_hash, role, created_at)
		VALUES (?, ?, 'admin', CURRENT_TIMESTAMP)
	`, "admin", string(hashedPassword))
	
	return err
}

func (d *Database) Close() error {
	return d.db.Close()
}

// User operations
func (d *Database) GetUserByUsername(username string) (*User, error) {
	var user User
	err := d.db.QueryRow(`
		SELECT id, username, password_hash, role, created_at
		FROM users WHERE username = ?
	`, username).Scan(&user.ID, &user.Username, &user.PasswordHash, &user.Role, &user.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &user, nil
}

func (d *Database) CreateUser(username, password string, role string) (*User, error) {
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}

	result, err := d.db.Exec(`
		INSERT INTO users (username, password_hash, role, created_at)
		VALUES (?, ?, ?, CURRENT_TIMESTAMP)
	`, username, string(hashedPassword), role)
	if err != nil {
		return nil, err
	}

	id, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}

	return &User{
		ID:        id,
		Username:  username,
		Role:      role,
		CreatedAt: time.Now(),
	}, nil
}

func (d *Database) GetAllUsers() ([]*User, error) {
	rows, err := d.db.Query(`
		SELECT id, username, role, created_at
		FROM users ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []*User
	for rows.Next() {
		var user User
		err := rows.Scan(&user.ID, &user.Username, &user.Role, &user.CreatedAt)
		if err != nil {
			return nil, err
		}
		users = append(users, &user)
	}
	return users, nil
}

func (d *Database) DeleteUser(id int64) error {
	_, err := d.db.Exec(`DELETE FROM users WHERE id = ?`, id)
	return err
}

// Image operations
func (d *Database) CreateImage(name, tag, digest string, size int64) (*Image, error) {
	result, err := d.db.Exec(`
		INSERT OR REPLACE INTO images (name, tag, digest, size, created_at)
		VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)
	`, name, tag, digest, size)
	if err != nil {
		return nil, err
	}

	id, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}

	return &Image{
		ID:        id,
		Name:      name,
		Tag:       tag,
		Digest:    digest,
		Size:      size,
		CreatedAt: time.Now(),
	}, nil
}

func (d *Database) GetImage(name, tag string) (*Image, error) {
	var img Image
	err := d.db.QueryRow(`
		SELECT id, name, tag, digest, size, created_at
		FROM images WHERE name = ? AND tag = ?
	`, name, tag).Scan(&img.ID, &img.Name, &img.Tag, &img.Digest, &img.Size, &img.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &img, nil
}

func (d *Database) GetImageByDigest(digest string) (*Image, error) {
	var img Image
	err := d.db.QueryRow(`
		SELECT id, name, tag, digest, size, created_at
		FROM images WHERE digest = ?
	`, digest).Scan(&img.ID, &img.Name, &img.Tag, &img.Digest, &img.Size, &img.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &img, nil
}

func (d *Database) GetAllImages() ([]*Image, error) {
	rows, err := d.db.Query(`
		SELECT id, name, tag, digest, size, created_at
		FROM images ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var images []*Image
	for rows.Next() {
		var img Image
		err := rows.Scan(&img.ID, &img.Name, &img.Tag, &img.Digest, &img.Size, &img.CreatedAt)
		if err != nil {
			return nil, err
		}
		images = append(images, &img)
	}
	return images, nil
}

func (d *Database) DeleteImage(name, tag string) error {
	_, err := d.db.Exec(`DELETE FROM images WHERE name = ? AND tag = ?`, name, tag)
	return err
}

func (d *Database) DeleteRepository(name string) error {
	// Delete from repositories table
	_, err := d.db.Exec(`DELETE FROM repositories WHERE name = ?`, name)
	if err != nil {
		return err
	}

	// Delete all images for this repository
	_, err = d.db.Exec(`DELETE FROM images WHERE name = ?`, name)
	return err
}

// Layer operations
func (d *Database) CreateLayer(imageID int64, digest, mediaType string, size int64) error {
	_, err := d.db.Exec(`
		INSERT INTO layers (image_id, digest, size, media_type)
		VALUES (?, ?, ?, ?)
	`, imageID, digest, size, mediaType)
	return err
}

func (d *Database) GetLayersByImageID(imageID int64) ([]*Layer, error) {
	rows, err := d.db.Query(`
		SELECT id, image_id, digest, size, media_type
		FROM layers WHERE image_id = ?
	`, imageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var layers []*Layer
	for rows.Next() {
		var layer Layer
		err := rows.Scan(&layer.ID, &layer.ImageID, &layer.Digest, &layer.Size, &layer.MediaType)
		if err != nil {
			return nil, err
		}
		layers = append(layers, &layer)
	}
	return layers, nil
}

// Manifest operations
func (d *Database) CreateManifest(imageID int64, digest, content string) error {
	_, err := d.db.Exec(`
		INSERT INTO manifests (image_id, digest, content)
		VALUES (?, ?, ?)
	`, imageID, digest, content)
	return err
}

func (d *Database) GetManifestByImageID(imageID int64) (*Manifest, error) {
	var manifest Manifest
	err := d.db.QueryRow(`
		SELECT id, image_id, digest, content
		FROM manifests WHERE image_id = ?
	`, imageID).Scan(&manifest.ID, &manifest.ImageID, &manifest.Digest, &manifest.Content)
	if err != nil {
		return nil, err
	}
	return &manifest, nil
}

// Statistics
func (d *Database) GetStatistics() (int, int64, error) {
	var totalImages int
	var totalSize int64

	err := d.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(size), 0) FROM images`).Scan(&totalImages, &totalSize)
	if err != nil {
		return 0, 0, err
	}

	return totalImages, totalSize, nil
}

// Repository operations
func (d *Database) CreateRepository(name string) (*Repository, error) {
	result, err := d.db.Exec(`
		INSERT INTO repositories (name, created_at)
		VALUES (?, CURRENT_TIMESTAMP)
	`, name)
	if err != nil {
		return nil, err
	}

	id, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}

	return &Repository{
		ID:        id,
		Name:      name,
		CreatedAt: time.Now(),
	}, nil
}

func (d *Database) GetRepository(name string) (*Repository, error) {
	var repo Repository
	err := d.db.QueryRow(`
		SELECT id, name, created_at
		FROM repositories WHERE name = ?
	`, name).Scan(&repo.ID, &repo.Name, &repo.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &repo, nil
}

func (d *Database) GetAllRepositories() ([]*Repository, error) {
	rows, err := d.db.Query(`
		SELECT id, name, created_at
		FROM repositories ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var repositories []*Repository
	for rows.Next() {
		var repo Repository
		err := rows.Scan(&repo.ID, &repo.Name, &repo.CreatedAt)
		if err != nil {
			return nil, err
		}
		repositories = append(repositories, &repo)
	}
	return repositories, nil
}

func (d *Database) GetRepositories() ([]string, error) {
	rows, err := d.db.Query(`SELECT DISTINCT name FROM images ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var repositories []string
	for rows.Next() {
		var name string
		err := rows.Scan(&name)
		if err != nil {
			return nil, err
		}
		repositories = append(repositories, name)
	}
	return repositories, nil
}

func (d *Database) GetImagesByRepository(name string) ([]*Image, error) {
	rows, err := d.db.Query(`
		SELECT id, name, tag, digest, size, created_at
		FROM images WHERE name = ? ORDER BY created_at DESC
	`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var images []*Image
	for rows.Next() {
		var img Image
		err := rows.Scan(&img.ID, &img.Name, &img.Tag, &img.Digest, &img.Size, &img.CreatedAt)
		if err != nil {
			return nil, err
		}
		images = append(images, &img)
	}
	return images, nil
}

// CreateGroup creates a new group in the database
func (d *Database) CreateGroup(name string) error {
	_, err := d.db.Exec(`
		INSERT INTO groups (name, created_at)
		VALUES (?, CURRENT_TIMESTAMP)
	`, name)
	return err
}

// GetGroups returns all unique groups from both the groups table and repository names
func (d *Database) GetGroups() ([]string, error) {
	// Get groups from groups table
	rows, err := d.db.Query(`
		SELECT name FROM groups ORDER BY name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	groupMap := make(map[string]bool)
	var groups []string
	
	// Add groups from groups table
	for rows.Next() {
		var groupName string
		err := rows.Scan(&groupName)
		if err != nil {
			return nil, err
		}
		if !groupMap[groupName] {
			groups = append(groups, groupName)
			groupMap[groupName] = true
		}
	}
	rows.Close()

	// Get groups from images table (extracted from repository names)
	rows, err = d.db.Query(`
		SELECT DISTINCT 
			CASE 
				WHEN name LIKE '%/%' THEN substr(name, 1, instr(name, '/') - 1)
				ELSE name
			END as group_name
		FROM images
		ORDER BY group_name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Add groups from images table (avoid duplicates)
	for rows.Next() {
		var groupName string
		err := rows.Scan(&groupName)
		if err != nil {
			return nil, err
		}
		if !groupMap[groupName] {
			groups = append(groups, groupName)
			groupMap[groupName] = true
		}
	}

	return groups, nil
}

// GetRepositoriesByGroup returns all repositories (images) that belong to a specific group
func (d *Database) GetRepositoriesByGroup(groupName string) ([]string, error) {
	rows, err := d.db.Query(`
		SELECT DISTINCT name
		FROM images
		WHERE name LIKE ? || '/%'
		ORDER BY name
	`, groupName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var repositories []string
	for rows.Next() {
		var repoName string
		err := rows.Scan(&repoName)
		if err != nil {
			return nil, err
		}
		repositories = append(repositories, repoName)
	}
	return repositories, nil
}

