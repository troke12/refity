package database

import (
	"database/sql"
	"time"

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
	ID       int64  `json:"id"`
	ImageID  int64  `json:"image_id"`
	Digest   string `json:"digest"`
	Size     int64  `json:"size"`
	MediaType string `json:"media_type"`
}

type Manifest struct {
	ID       int64  `json:"id"`
	ImageID  int64  `json:"image_id"`
	Digest   string `json:"digest"`
	Content  string `json:"content"`
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

	return database, nil
}

func (d *Database) createTables() error {
	// Create images table
	_, err := d.db.Exec(`
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

	return nil
}

func (d *Database) Close() error {
	return d.db.Close()
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
	_, err := d.db.Exec(`DELETE FROM images WHERE name = ?`, name)
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
