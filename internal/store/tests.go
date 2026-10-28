package store

import (
	"context"
	"sort"
	"strings"
)

// testPrefix starts the settings that hold test links: "test:<protocol>" is
// the UUID of the link of that protocol.
const testPrefix = "test:"

// TestUsers returns the test links the admin made on the "Протоколы" tab, as
// users that work on their one protocol only. They have no subscription,
// devices or traffic, and never show up among the users.
func (s *Store) TestUsers(ctx context.Context) ([]User, error) {
	all, err := s.Settings(ctx)
	if err != nil {
		return nil, err
	}
	var out []User
	for k, v := range all {
		if proto, ok := strings.CutPrefix(k, testPrefix); ok && v != "" {
			out = append(out, User{Name: "Тест", UUID: v, Enabled: true, Only: proto})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Only < out[j].Only })
	return out, nil
}

// SetTestUser stores the UUID of the test link of a protocol; an empty one
// deletes the link.
func (s *Store) SetTestUser(ctx context.Context, protocol, uuid string) error {
	if uuid == "" {
		_, err := s.db.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, testPrefix+protocol)
		return err
	}
	return s.SetSettings(ctx, map[string]string{testPrefix + protocol: uuid})
}

// CoreUsers is what the cores serve: the users and the test links.
func (s *Store) CoreUsers(ctx context.Context) ([]User, error) {
	users, err := s.Users(ctx)
	if err != nil {
		return nil, err
	}
	tests, err := s.TestUsers(ctx)
	return append(users, tests...), err
}
