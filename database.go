package main

import (
	"database/sql"
	"fmt"
	"os"
	"time"
	_ "github.com/jackc/pgx/v5/stdlib"
)

var db *sql.DB

type FileHashRecord struct {
	Hash      string
	CatboxURL sql.NullString
	PomfURL   sql.NullString
}

func InitDB() error {
	dbURI := os.Getenv("DATABASE_URI")
	if dbURI == "" {
		return fmt.Errorf("DATABASE_URI environment variable not set")
	}

	var err error
	db, err = sql.Open("pgx", dbURI)
	if err != nil {
		return fmt.Errorf("could not open db connection: %w", err)
	}

	db.SetConnMaxLifetime(time.Minute * 3)
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(10)

	if err := db.Ping(); err != nil {
		return fmt.Errorf("could not ping database: %w", err)
	}

	return createTableIfNotExists()
}

func createTableIfNotExists() error {
	query := `
    CREATE TABLE IF NOT EXISTS hash (
        hash CHAR(64) PRIMARY KEY,
        catbox TEXT,
        pomf TEXT,
        created_at TIMESTAMPTZ DEFAULT NOW()
    );`
	_, err := db.Exec(query)
	return err
}

func GetURLsByHash(hash string) (FileHashRecord, error) {
	var record FileHashRecord
	record.Hash = hash

	query := "SELECT catbox, pomf FROM hash WHERE hash = $1 LIMIT 1;"
	err := db.QueryRow(query, hash).Scan(&record.CatboxURL, &record.PomfURL)
	if err != nil {
		if err == sql.ErrNoRows {
			return record, nil
		}
		return record, fmt.Errorf("database query failed: %w", err)
	}
	return record, nil
}

func storeUrl(hash, destination, url string) error {
	var query string
	switch destination {
	case "catbox":
		query = `
        INSERT INTO hash (hash, catbox)
        VALUES ($1, $2)
        ON CONFLICT (hash) DO UPDATE SET catbox = EXCLUDED.catbox;`
	case "pomf":
		query = `
        INSERT INTO hash (hash, pomf)
        VALUES ($1, $2)
        ON CONFLICT (hash) DO UPDATE SET pomf = EXCLUDED.pomf;`
	default:
		return fmt.Errorf("cannot store URL for unsupported destination: %s", destination)
	}

	_, err := db.Exec(query, hash, url)
	if err != nil {
		return fmt.Errorf("database insert/update failed: %w", err)
	}
	return nil
}

func CloseDB() {
	if db != nil {
		db.Close()
	}
}