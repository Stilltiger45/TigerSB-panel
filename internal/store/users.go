package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// ErrExists is returned when a unique field is already taken.
var ErrExists = errors.New("уже существует")

// User is a VPN user. Online is JSON written by the stats collector.
type User struct {
	ID      int64
	Name    string
	UUID    string
	Devices int
	Enabled bool
	Created time.Time
	Up      int64
	Down    int64
	Online  string
	SeenAt  int64
	// SubToken is the secret part of the subscription address.
	SubToken string
	// Only, when set, is the one protocol a test link works on (see
	// TestUsers); such a user is not in the users table.
	Only string
	// Slot, when set, makes this one key of the user: the key of the
	// device in that place, with its own UUID (see Keys).
	Slot int
}

const userColumns = `id, name, uuid, devices, enabled, created_at, up, down, online, seen_at, sub_token`

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	u := &User{}
	var created int64
	err := row.Scan(&u.ID, &u.Name, &u.UUID, &u.Devices, &u.Enabled, &created, &u.Up, &u.Down, &u.Online, &u.SeenAt, &u.SubToken)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	u.Created = time.Unix(created, 0).UTC()
	return u, err
}

func uniqueErr(err error) error {
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return ErrExists
	}
	return err
}

// AddUser creates a user and fills its ID. IDs are never given out twice.
func (s *Store) AddUser(ctx context.Context, u *User) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id int64
	if err := tx.QueryRowContext(ctx, `
		UPDATE user_ids SET last = max(last, (SELECT coalesce(max(id), 0) FROM users)) + 1 RETURNING last`).Scan(&id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO users (id, name, uuid, devices, enabled, created_at, sub_token) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, u.Name, u.UUID, u.Devices, u.Enabled, u.Created.Unix(), u.SubToken); err != nil {
		return uniqueErr(err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	u.ID = id
	return nil
}

// Users returns every user, oldest first.
func (s *Store) Users(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+userColumns+` FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

// UserByID returns one user.
func (s *Store) UserByID(ctx context.Context, id int64) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id = ?`, id))
}

// UserByToken returns the user a subscription address belongs to.
func (s *Store) UserByToken(ctx context.Context, token string) (*User, error) {
	if token == "" {
		return nil, ErrNotFound
	}
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE sub_token = ?`, token))
}

// UpdateUser saves the fields an admin can change.
func (s *Store) UpdateUser(ctx context.Context, u *User) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE users SET name = ?, uuid = ?, devices = ?, enabled = ?, sub_token = ? WHERE id = ?`,
		u.Name, u.UUID, u.Devices, u.Enabled, u.SubToken, u.ID)
	if err != nil {
		return uniqueErr(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteUser removes a user.
func (s *Store) DeleteUser(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// UserActivity is what one stats collection learned about a user.
type UserActivity struct {
	ID     int64
	Up     int64 // bytes since the previous collection
	Down   int64
	Online string // JSON list of addresses
	Seen   bool   // the user had connections
}

// RecordActivity adds traffic and stores who is online, in one transaction.
// Users missing from the list are marked offline.
func (s *Store) RecordActivity(ctx context.Context, list []UserActivity, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE users SET online = '[]'`); err != nil {
		return err
	}
	for _, a := range list {
		seen := 0
		if a.Seen {
			seen = 1
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE users SET up = up + ?, down = down + ?, online = ?,
				seen_at = CASE WHEN ? = 1 THEN ? ELSE seen_at END
			WHERE id = ?`, a.Up, a.Down, a.Online, seen, now.Unix(), a.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AddTraffic adds traffic and marks the users seen, leaving who is online
// to RecordActivity: sing-box and mbox count their own users.
func (s *Store) AddTraffic(ctx context.Context, list []UserActivity, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, a := range list {
		seen := 0
		if a.Seen {
			seen = 1
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE users SET up = up + ?, down = down + ?,
				seen_at = CASE WHEN ? = 1 THEN ? ELSE seen_at END
			WHERE id = ?`, a.Up, a.Down, seen, now.Unix(), a.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ResetTraffic zeroes a user's traffic counters.
func (s *Store) ResetTraffic(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET up = 0, down = 0 WHERE id = ?`, id)
	return err
}
