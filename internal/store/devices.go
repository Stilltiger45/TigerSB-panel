package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ErrDeviceLimit means the user already has as many devices as allowed.
var ErrDeviceLimit = errors.New("лимит устройств исчерпан")

// Device is a phone or computer that fetched a user's subscription.
type Device struct {
	ID        int64
	UserID    int64
	HWID      string
	OS        string
	OSVersion string
	Model     string
	App       string
	IP        string
	FirstSeen time.Time
	LastSeen  time.Time
	// Slot is the place the device holds, 1 to the limit; 0 when it has
	// none (it came before the places, over the limit).
	Slot int
}

const deviceColumns = `id, user_id, hwid, os, os_version, model, app, ip, first_seen, last_seen, slot`

// Devices returns the devices of every user, oldest first.
func (s *Store) Devices(ctx context.Context) ([]Device, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+deviceColumns+` FROM devices ORDER BY user_id, first_seen, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Device
	for rows.Next() {
		var d Device
		var first, last int64
		if err := rows.Scan(&d.ID, &d.UserID, &d.HWID, &d.OS, &d.OSVersion, &d.Model, &d.App, &d.IP, &first, &last, &d.Slot); err != nil {
			return nil, err
		}
		d.FirstSeen, d.LastSeen = time.Unix(first, 0).UTC(), time.Unix(last, 0).UTC()
		out = append(out, d)
	}
	return out, rows.Err()
}

// TouchDevice records a subscription fetch by d and returns the place it
// holds. A new device, or one without a place, takes the first free place
// of 1..limit; with none free, ErrDeviceLimit is returned.
func (s *Store) TouchDevice(ctx context.Context, d Device, limit int, now time.Time) (slot int, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx, `SELECT slot FROM devices WHERE user_id = ? AND hwid = ?`, d.UserID, d.HWID).Scan(&slot)
	known := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if slot < 1 || slot > limit {
		if slot, err = freeSlot(ctx, tx, d.UserID, limit); err != nil {
			return 0, err
		}
	}
	if known {
		_, err = tx.ExecContext(ctx, `
			UPDATE devices SET os = ?, os_version = ?, model = ?, app = ?, ip = ?, last_seen = ?, slot = ?
			WHERE user_id = ? AND hwid = ?`,
			d.OS, d.OSVersion, d.Model, d.App, d.IP, now.Unix(), slot, d.UserID, d.HWID)
	} else {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO devices (user_id, hwid, os, os_version, model, app, ip, first_seen, last_seen, slot)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			d.UserID, d.HWID, d.OS, d.OSVersion, d.Model, d.App, d.IP, now.Unix(), now.Unix(), slot)
	}
	if err != nil {
		return 0, err
	}
	return slot, tx.Commit()
}

// freeSlot finds the first place of 1..limit no device holds.
func freeSlot(ctx context.Context, tx *sql.Tx, userID int64, limit int) (int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT slot FROM devices WHERE user_id = ?`, userID)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	taken := map[int]bool{}
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			return 0, err
		}
		taken[n] = true
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for n := 1; n <= limit; n++ {
		if !taken[n] {
			return n, nil
		}
	}
	return 0, ErrDeviceLimit
}

// newKey gives a place a new key, so the device that had it cannot connect.
func newKey(ctx context.Context, tx *sql.Tx, userID int64, slot int) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO slot_keys (user_id, slot, gen) VALUES (?, ?, 1)
		ON CONFLICT (user_id, slot) DO UPDATE SET gen = gen + 1`, userID, slot)
	return err
}

// DeleteDevice forgets one device of a user and gives its place a new key,
// so the device is cut off and the place is free for another.
func (s *Store) DeleteDevice(ctx context.Context, userID, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var slot int
	err = tx.QueryRowContext(ctx, `DELETE FROM devices WHERE user_id = ? AND id = ? RETURNING slot`, userID, id).Scan(&slot)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if slot > 0 {
		if err := newKey(ctx, tx, userID, slot); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// TrimSlots takes away the places above a lowered limit: their devices are
// forgotten and the keys of the places change, so a raised limit later
// does not let those devices back.
func (s *Store) TrimSlots(ctx context.Context, userID int64, from, to int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM devices WHERE user_id = ? AND slot > ?`, userID, to); err != nil {
		return err
	}
	for slot := to + 1; slot <= from; slot++ {
		if err := newKey(ctx, tx, userID, slot); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ClearDevices forgets every device of a user. The caller gives the user
// a new UUID, which changes the keys of all places.
func (s *Store) ClearDevices(ctx context.Context, userID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM devices WHERE user_id = ?`, userID)
	return err
}
