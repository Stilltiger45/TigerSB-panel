package store

import (
	"context"
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
}

const deviceColumns = `id, user_id, hwid, os, os_version, model, app, ip, first_seen, last_seen`

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
		if err := rows.Scan(&d.ID, &d.UserID, &d.HWID, &d.OS, &d.OSVersion, &d.Model, &d.App, &d.IP, &first, &last); err != nil {
			return nil, err
		}
		d.FirstSeen, d.LastSeen = time.Unix(first, 0).UTC(), time.Unix(last, 0).UTC()
		out = append(out, d)
	}
	return out, rows.Err()
}

// TouchDevice records a subscription fetch by d. A known device is updated;
// a new one is added only while the user has fewer than limit devices,
// otherwise ErrDeviceLimit is returned.
func (s *Store) TouchDevice(ctx context.Context, d Device, limit int, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `
		UPDATE devices SET os = ?, os_version = ?, model = ?, app = ?, ip = ?, last_seen = ?
		WHERE user_id = ? AND hwid = ?`,
		d.OS, d.OSVersion, d.Model, d.App, d.IP, now.Unix(), d.UserID, d.HWID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM devices WHERE user_id = ?`, d.UserID).Scan(&count); err != nil {
			return err
		}
		if count >= limit {
			return ErrDeviceLimit
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO devices (user_id, hwid, os, os_version, model, app, ip, first_seen, last_seen)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			d.UserID, d.HWID, d.OS, d.OSVersion, d.Model, d.App, d.IP, now.Unix(), now.Unix()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteDevice forgets one device of a user, freeing its place.
func (s *Store) DeleteDevice(ctx context.Context, userID, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM devices WHERE user_id = ? AND id = ?`, userID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ClearDevices forgets every device of a user.
func (s *Store) ClearDevices(ctx context.Context, userID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM devices WHERE user_id = ?`, userID)
	return err
}
