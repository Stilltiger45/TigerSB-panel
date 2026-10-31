package store

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// sharedKeysSetting holds, in unix seconds, until when a user's own UUID
// works next to the keys of its places: the devices that fetched the
// subscription before there were places have links with it.
const sharedKeysSetting = "keys.shared_until"

// SlotUUID is the key of a place: an HMAC of the place and its generation
// under the user's UUID, as a UUID. A new UUID for the user changes all of
// them; a new generation, one.
func SlotUUID(base string, slot, gen int) string {
	m := hmac.New(sha256.New, []byte(base))
	fmt.Fprintf(m, "slot:%d:%d", slot, gen)
	b := m.Sum(nil)[:16]
	b[6] = b[6]&0x0f | 0x40 // version 4
	b[8] = b[8]&0x3f | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// SharedKeysUntil returns until when the users' own keys still work; zero
// when they do not.
func (s *Store) SharedKeysUntil(ctx context.Context) (time.Time, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, sharedKeysSetting).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	} else if err != nil {
		return time.Time{}, err
	}
	n, _ := strconv.ParseInt(v, 10, 64)
	return time.Unix(n, 0), nil
}

// EndSharedKeys forgets the users' own keys for good.
func (s *Store) EndSharedKeys(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, sharedKeysSetting)
	return err
}

// gens returns the generations of the places of every user.
func (s *Store) gens(ctx context.Context) (map[int64]map[int]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT user_id, slot, gen FROM slot_keys`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]map[int]int{}
	for rows.Next() {
		var id int64
		var slot, gen int
		if err := rows.Scan(&id, &slot, &gen); err != nil {
			return nil, err
		}
		if out[id] == nil {
			out[id] = map[int]int{}
		}
		out[id][slot] = gen
	}
	return out, rows.Err()
}

// keys expands a user into the keys the cores know: one per place, and
// the user's own while shared is set. A disabled user has none.
func keys(u User, gens map[int]int, shared bool) []User {
	if !u.Enabled {
		return nil
	}
	var out []User
	if shared {
		out = append(out, u)
	}
	for slot := 1; slot <= u.Devices; slot++ {
		k := u
		k.Slot = slot
		k.UUID = SlotUUID(u.UUID, slot, gens[slot])
		out = append(out, k)
	}
	return out
}

func (s *Store) sharedNow(ctx context.Context, now time.Time) (bool, error) {
	until, err := s.SharedKeysUntil(ctx)
	return now.Before(until), err
}

// Keys returns the keys of a user (see keys).
func (s *Store) Keys(ctx context.Context, u User, now time.Time) ([]User, error) {
	gens, err := s.gens(ctx)
	if err != nil {
		return nil, err
	}
	shared, err := s.sharedNow(ctx, now)
	if err != nil {
		return nil, err
	}
	return keys(u, gens[u.ID], shared), nil
}

// Key returns the key of one place of a user.
func (s *Store) Key(ctx context.Context, u User, slot int) (User, error) {
	var gen int
	err := s.db.QueryRowContext(ctx, `SELECT coalesce(max(gen), 0) FROM slot_keys WHERE user_id = ? AND slot = ?`, u.ID, slot).Scan(&gen)
	k := u
	k.Slot = slot
	k.UUID = SlotUUID(u.UUID, slot, gen)
	return k, err
}
