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
}

const userColumns = `id, name, uuid, devices, enabled, created_at, up, down, online, seen_at`

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	u := &User{}
	var created int64
	err := row.Scan(&u.ID, &u.Name, &u.UUID, &u.Devices, &u.Enabled, &created, &u.Up, &u.Down, &u.Online, &u.SeenAt)
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

// AddUser creates a user and fills its ID.
func (s *Store) AddUser(ctx context.Context, u *User) error {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO users (name, uuid, devices, enabled, created_at) VALUES (?, ?, ?, ?, ?)`,
		u.Name, u.UUID, u.Devices, u.Enabled, u.Created.Unix())
	if err != nil {
		return uniqueErr(err)
	}
	u.ID, err = res.LastInsertId()
	return err
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

// UpdateUser saves the fields an admin can change.
func (s *Store) UpdateUser(ctx context.Context, u *User) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE users SET name = ?, uuid = ?, devices = ?, enabled = ? WHERE id = ?`,
		u.Name, u.UUID, u.Devices, u.Enabled, u.ID)
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

// ResetTraffic zeroes a user's traffic counters.
func (s *Store) ResetTraffic(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET up = 0, down = 0 WHERE id = ?`, id)
	return err
}
