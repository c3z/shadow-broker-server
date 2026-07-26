package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tursodatabase/libsql-client-go/libsql"
	_ "modernc.org/sqlite" // pure-Go SQLite driver (no cgo)
)

// ======================= MESSAGE STORE (DB BACKENDS) =======================

// messageStore abstracts the message database so the broker can run on an
// embedded SQLite file (default, fully self-hosted) or a remote Turso DB.
type messageStore interface {
	// Insert stores a new encrypted message and returns its row id.
	Insert(queue, nonceB64, cipherB64 string) (int64, error)
	// FetchNew returns all messages with id > lastKnownID, ascending by id.
	FetchNew(lastKnownID int64) ([]DBMessage, error)
	// Pull returns up to limit messages of a queue with id > sinceID, ascending.
	Pull(queue string, sinceID int64, limit int) ([]DBMessage, error)
	// MaxID returns the highest message id (0 on an empty table).
	MaxID() (int64, error)
	// Close releases the underlying database resources.
	Close() error
}

// DBMessage is one row of the messages table.
type DBMessage struct {
	ID        int64
	Queue     string
	NonceB64  string
	CipherB64 string
	CreatedAt string
}

// schemaSQL mirrors db.sql (kept in the repo as a reference); applied at
// startup so the broker works from an empty directory with no migrations.
const schemaSQL = `
CREATE TABLE IF NOT EXISTS messages (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  queue TEXT NOT NULL,
  nonce TEXT NOT NULL,
  ciphertext TEXT NOT NULL,
  created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);`

// defaultLocalDBUrl is used when the config has no db_url at all.
const defaultLocalDBUrl = "file:./data/broker.db"

// sqlStore implements messageStore over database/sql. Both backends speak
// the same SQLite dialect, so they share the queries; only the connection
// setup and the write retry policy differ.
type sqlStore struct {
	db     *sql.DB
	remote bool // remote (Turso) backend: retry writes with backoff
}

// newMessageStore picks the backend from the db_url scheme:
//   - "" or "file:..."          -> embedded local SQLite (default)
//   - "libsql://" or "https://" -> remote Turso database
func newMessageStore(dbURL, tursoAuthToken string) (messageStore, error) {
	switch {
	case dbURL == "" || strings.HasPrefix(dbURL, "file:"):
		return newLocalStore(dbURL)
	case strings.HasPrefix(dbURL, "libsql://") || strings.HasPrefix(dbURL, "https://"):
		return newRemoteStore(dbURL, tursoAuthToken)
	default:
		return nil, fmt.Errorf("unsupported db_url scheme %q (use file:, libsql:// or https://)", dbURL)
	}
}

// newLocalStore opens (creating it if needed) an embedded SQLite database.
func newLocalStore(dbURL string) (messageStore, error) {
	if dbURL == "" {
		dbURL = defaultLocalDBUrl
	}
	// Make sure the parent directory of the database file exists.
	path := strings.TrimPrefix(strings.SplitN(dbURL, "?", 2)[0], "file:")
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create db dir: %w", err)
		}
	}
	db, err := sql.Open("sqlite", dbURL)
	if err != nil {
		return nil, err
	}
	// SQLite allows only one writer; serialize connections to avoid SQLITE_BUSY.
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA synchronous=NORMAL",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	return &sqlStore{db: db}, nil
}

// newRemoteStore connects to a remote Turso (libsql) database.
func newRemoteStore(dbURL, authToken string) (messageStore, error) {
	connector, err := libsql.NewConnector(dbURL, libsql.WithAuthToken(authToken))
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(connector)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	return &sqlStore{db: db, remote: true}, nil
}

func (s *sqlStore) Close() error { return s.db.Close() }

// Insert stores a message. Remote backends retry with backoff because
// network hiccups are expected there; local SQLite errors are not
// transient, so a single attempt is enough.
func (s *sqlStore) Insert(queue, nonceB64, cipherB64 string) (int64, error) {
	maxRetries := 0
	if s.remote {
		maxRetries = 10
	}
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		res, err := s.db.Exec(`
			INSERT INTO messages (queue, nonce, ciphertext)
			VALUES (?, ?, ?)
		`, queue, nonceB64, cipherB64)
		if err == nil {
			return res.LastInsertId()
		}
		lastErr = err
		if logger != nil {
			logger.Printf("DB insert error (attempt %d/%d): %v\n", attempt+1, maxRetries+1, err)
		}
		// Skip delay on the last attempt
		if attempt < maxRetries {
			time.Sleep(time.Duration(100*(attempt+1)) * time.Millisecond)
		}
	}
	return 0, fmt.Errorf("failed to insert after %d attempts: %w", maxRetries+1, lastErr)
}

// FetchNew - everything with id > lastKnownID, sorted ascending.
func (s *sqlStore) FetchNew(lastKnownID int64) ([]DBMessage, error) {
	rows, err := s.db.Query(`
		SELECT id, queue, nonce, ciphertext, created_at
		FROM messages
		WHERE id > ?
		ORDER BY id ASC
	`, lastKnownID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var res []DBMessage
	for rows.Next() {
		var m DBMessage
		var createdAt interface{}
		if err := rows.Scan(&m.ID, &m.Queue, &m.NonceB64, &m.CipherB64, &createdAt); err != nil {
			return nil, err
		}
		m.CreatedAt = createdAtString(createdAt)
		res = append(res, m)
	}
	return res, rows.Err()
}

func (s *sqlStore) Pull(queue string, sinceID int64, limit int) ([]DBMessage, error) {
	rows, err := s.db.Query(`
		SELECT id, queue, nonce, ciphertext, created_at
		FROM messages
		WHERE queue = ? AND id > ?
		ORDER BY id ASC
		LIMIT ?
	`, queue, sinceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var res []DBMessage
	for rows.Next() {
		var m DBMessage
		var createdAt interface{}
		if err := rows.Scan(&m.ID, &m.Queue, &m.NonceB64, &m.CipherB64, &createdAt); err != nil {
			return nil, err
		}
		m.CreatedAt = createdAtString(createdAt)
		res = append(res, m)
	}
	return res, rows.Err()
}

func (s *sqlStore) MaxID() (int64, error) {
	var maxID int64
	err := s.db.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM messages`).Scan(&maxID)
	return maxID, err
}

// createdAtString normalizes the created_at column: modernc.org/sqlite
// returns TIMESTAMP columns as time.Time, libsql returns text.
func createdAtString(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []byte:
		return string(t)
	case time.Time:
		return t.UTC().Format("2006-01-02 15:04:05")
	default:
		return fmt.Sprint(t)
	}
}
