// Package store keeps panel state in SQLite.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, no cgo needed for static builds
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("не найдено")

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
}

// Admin is a panel administrator account.
type Admin struct {
	ID           int64
	Username     string
	PasswordHash string
}

var migrations = []string{
	`CREATE TABLE admins (
		id            INTEGER PRIMARY KEY,
		username      TEXT NOT NULL UNIQUE,
		password_hash TEXT NOT NULL,
		created_at    INTEGER NOT NULL
	)`,
	`CREATE TABLE sessions (
		token_hash TEXT PRIMARY KEY,
		admin_id   INTEGER NOT NULL REFERENCES admins(id) ON DELETE CASCADE,
		expires_at INTEGER NOT NULL,
		created_at INTEGER NOT NULL
	)`,
	`CREATE TABLE settings (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`,
}

// Open opens (creating if needed) the database at path and applies migrations.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	dsn := "file:" + path + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil && !os.IsNotExist(err) {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	for i := version; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("миграция %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// UpsertAdmin creates the admin or replaces its password hash.
func (s *Store) UpsertAdmin(ctx context.Context, username, passwordHash string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO admins (username, password_hash, created_at) VALUES (?, ?, ?)
		ON CONFLICT(username) DO UPDATE SET password_hash = excluded.password_hash`,
		username, passwordHash, time.Now().Unix())
	if err != nil {
		return err
	}
	// A password change must end every existing session of that admin.
	_, err = s.db.ExecContext(ctx, `
		DELETE FROM sessions WHERE admin_id = (SELECT id FROM admins WHERE username = ?)`, username)
	return err
}

// AdminByUsername looks up an admin.
func (s *Store) AdminByUsername(ctx context.Context, username string) (*Admin, error) {
	a := &Admin{}
	err := s.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash FROM admins WHERE username = ?`, username,
	).Scan(&a.ID, &a.Username, &a.PasswordHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return a, err
}

// CountAdmins returns how many admins exist.
func (s *Store) CountAdmins(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM admins`).Scan(&n)
	return n, err
}

// CreateSession stores a session by the hash of its token.
func (s *Store) CreateSession(ctx context.Context, tokenHash string, adminID int64, expires time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, admin_id, expires_at, created_at) VALUES (?, ?, ?, ?)`,
		tokenHash, adminID, expires.Unix(), time.Now().Unix())
	return err
}

// SessionAdmin returns the admin of a live session.
func (s *Store) SessionAdmin(ctx context.Context, tokenHash string, now time.Time) (*Admin, error) {
	a := &Admin{}
	err := s.db.QueryRowContext(ctx, `
		SELECT a.id, a.username, a.password_hash
		FROM sessions s JOIN admins a ON a.id = s.admin_id
		WHERE s.token_hash = ? AND s.expires_at > ?`, tokenHash, now.Unix(),
	).Scan(&a.ID, &a.Username, &a.PasswordHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return a, err
}

// DeleteSession removes one session.
func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
	return err
}

// PruneSessions deletes expired sessions.
func (s *Store) PruneSessions(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, now.Unix())
	return err
}

// Settings returns every stored setting.
func (s *Store) Settings(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// SetSettings stores the given settings in one transaction.
func (s *Store) SetSettings(ctx context.Context, values map[string]string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for k, v := range values {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO settings (key, value) VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
			return err
		}
	}
	return tx.Commit()
}
