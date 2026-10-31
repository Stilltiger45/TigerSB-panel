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
	`CREATE TABLE metrics (
		time INTEGER PRIMARY KEY,
		data TEXT NOT NULL
	)`,
	`CREATE TABLE users (
		id         INTEGER PRIMARY KEY,
		name       TEXT NOT NULL UNIQUE,
		uuid       TEXT NOT NULL UNIQUE,
		devices    INTEGER NOT NULL,
		enabled    INTEGER NOT NULL DEFAULT 1,
		created_at INTEGER NOT NULL,
		up         INTEGER NOT NULL DEFAULT 0,
		down       INTEGER NOT NULL DEFAULT 0,
		online     TEXT NOT NULL DEFAULT '[]',
		seen_at    INTEGER NOT NULL DEFAULT 0
	)`,
	// Subscription address of each user: /sub/<sub_token>.
	`ALTER TABLE users ADD COLUMN sub_token TEXT NOT NULL DEFAULT ''`,
	`UPDATE users SET sub_token = lower(hex(randomblob(16)))`,
	`CREATE UNIQUE INDEX users_sub_token ON users(sub_token)`,
	// Devices that fetched a user's subscription, told apart by x-hwid.
	`CREATE TABLE devices (
		id         INTEGER PRIMARY KEY,
		user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		hwid       TEXT NOT NULL,
		os         TEXT NOT NULL DEFAULT '',
		os_version TEXT NOT NULL DEFAULT '',
		model      TEXT NOT NULL DEFAULT '',
		app        TEXT NOT NULL DEFAULT '',
		ip         TEXT NOT NULL DEFAULT '',
		first_seen INTEGER NOT NULL,
		last_seen  INTEGER NOT NULL,
		UNIQUE (user_id, hwid)
	)`,
	// The last user ID given out. SQLite gives a deleted last user's ID to
	// the next one, and with it the name the cores know the user by
	// (u<id>): Xray's counters and open connections of the deleted user
	// would show up as the new one's.
	`CREATE TABLE user_ids (last INTEGER NOT NULL)`,
	`INSERT INTO user_ids SELECT coalesce(max(id), 0) FROM users`,
	// Each device holds a place of its user, 1 to the device limit, and
	// connects with the key of that place (see Keys). Devices from before
	// take places in the order they came.
	`ALTER TABLE devices ADD COLUMN slot INTEGER NOT NULL DEFAULT 0`,
	`UPDATE devices SET slot = (SELECT count(*) FROM devices d WHERE d.user_id = devices.user_id
		AND (d.first_seen < devices.first_seen OR (d.first_seen = devices.first_seen AND d.id <= devices.id)))`,
	// The generation of each place's key, raised when its device goes.
	`CREATE TABLE slot_keys (
		user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		slot    INTEGER NOT NULL,
		gen     INTEGER NOT NULL,
		PRIMARY KEY (user_id, slot)
	)`,
	// Devices that have links from before still have the user's own key:
	// it works for a day more, until they update the subscription.
	`INSERT INTO settings (key, value) SELECT '` + sharedKeysSetting + `', CAST(strftime('%s', 'now') + 86400 AS TEXT)
		WHERE EXISTS (SELECT 1 FROM users)`,
}

// Open opens (creating if needed) the database at path and applies migrations.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// busy_timeout goes first, so that switching a new database to WAL
	// waits for another process doing the same.
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)"
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
	// The services open the database at the same time after an update, so
	// each migration takes the write lock first and checks the version
	// under it: another process may have applied it meanwhile.
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	for {
		if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
			return err
		}
		var version int
		err := conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version)
		if err == nil && version >= len(migrations) {
			_, err = conn.ExecContext(ctx, "COMMIT")
			return err
		}
		if err == nil {
			if _, err = conn.ExecContext(ctx, migrations[version]); err != nil {
				err = fmt.Errorf("миграция %d: %w", version+1, err)
			}
		}
		if err == nil {
			_, err = conn.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", version+1))
		}
		if err != nil {
			conn.ExecContext(ctx, "ROLLBACK")
			return err
		}
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return err
		}
	}
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

// SettingOnce stores value under key unless the key is already set, and
// returns what is stored. Two processes calling it at once agree on one value.
func (s *Store) SettingOnce(ctx context.Context, key, value string) (string, error) {
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO NOTHING`, key, value); err != nil {
		return "", err
	}
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	return v, err
}

// AddMetric stores one resource sample, encoded by the caller.
func (s *Store) AddMetric(ctx context.Context, at time.Time, data []byte) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO metrics (time, data) VALUES (?, ?)
		ON CONFLICT(time) DO UPDATE SET data = excluded.data`, at.Unix(), string(data))
	return err
}

// Metrics returns samples taken at or after since, oldest first.
func (s *Store) Metrics(ctx context.Context, since time.Time) ([][]byte, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM metrics WHERE time >= ? ORDER BY time`, since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][]byte
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, []byte(d))
	}
	return out, rows.Err()
}

// LastMetric returns the newest sample, or nil when there is none.
func (s *Store) LastMetric(ctx context.Context) ([]byte, error) {
	var d string
	err := s.db.QueryRowContext(ctx, `SELECT data FROM metrics ORDER BY time DESC LIMIT 1`).Scan(&d)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return []byte(d), err
}

// PruneMetrics deletes samples older than before.
func (s *Store) PruneMetrics(ctx context.Context, before time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM metrics WHERE time < ?`, before.Unix())
	return err
}

// Snapshot writes a consistent copy of the database to path, which must not exist.
func (s *Store) Snapshot(ctx context.Context, path string) error {
	_, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, path)
	return err
}

// QuickCheck runs SQLite's integrity check and returns "ok" when the file is sound.
func (s *Store) QuickCheck(ctx context.Context) (string, error) {
	var res string
	err := s.db.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&res)
	return res, err
}
