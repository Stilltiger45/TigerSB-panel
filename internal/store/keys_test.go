package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestSlotUUID(t *testing.T) {
	base := "5783a3e7-e373-41cd-8642-c83782b807c5"
	seen := map[string]bool{base: true}
	for _, k := range []string{SlotUUID(base, 1, 0), SlotUUID(base, 2, 0), SlotUUID(base, 1, 1), SlotUUID("33d7be1c-8923-4184-819c-140e8557fe3c", 1, 0)} {
		if !uuidRe.MatchString(k) || seen[k] {
			t.Fatalf("key %q (seen %v)", k, seen[k])
		}
		seen[k] = true
	}
	if SlotUUID(base, 1, 0) != SlotUUID(base, 1, 0) {
		t.Fatal("not stable")
	}
}

// An install from before the places: its devices take places in the order
// they came, and the users' own keys work for a day more.
func TestMigrateToPlaces(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "panel.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	old := 0
	for i, m := range migrations {
		if strings.Contains(m, "ADD COLUMN slot") {
			old = i
			break
		}
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`INSERT INTO users (id, name, uuid, devices, created_at, sub_token) VALUES (1, 'Мама', 'aaaaaaaa-0000-4000-8000-000000000001', 2, 0, 't1'), (2, 'Папа', 'aaaaaaaa-0000-4000-8000-000000000002', 1, 0, 't2')`,
		`INSERT INTO devices (user_id, hwid, first_seen, last_seen) VALUES (1, 'new', 300, 300), (1, 'old', 100, 100), (2, 'one', 200, 200), (1, 'extra', 400, 400)`,
		fmt.Sprintf("PRAGMA user_version = %d", old),
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	devs, _ := st.Devices(ctx)
	got := map[string]int{}
	for _, d := range devs {
		got[d.HWID] = d.Slot
	}
	if got["old"] != 1 || got["new"] != 2 || got["extra"] != 3 || got["one"] != 1 {
		t.Fatalf("places: %v", got)
	}
	until, err := st.SharedKeysUntil(ctx)
	if d := time.Until(until); err != nil || d < 23*time.Hour || d > 25*time.Hour {
		t.Fatalf("shared keys until %v (%v)", until, err)
	}
	users, _ := st.CoreUsers(ctx)
	var names []string
	for _, u := range users {
		names = append(names, fmt.Sprintf("%d/%d", u.ID, u.Slot))
	}
	if strings.Join(names, " ") != "1/0 1/1 1/2 2/0 2/1" {
		t.Fatalf("core users: %v", names)
	}

	// The device above the limit gets a place once one is free.
	mama, _ := st.UserByID(ctx, 1)
	if _, err := st.TouchDevice(ctx, Device{UserID: 1, HWID: "extra"}, mama.Devices, time.Now()); err != ErrDeviceLimit {
		t.Fatalf("extra device: %v", err)
	}
	if err := st.DeleteDevice(ctx, 1, devIDOf(devs, "new")); err != nil {
		t.Fatal(err)
	}
	if slot, err := st.TouchDevice(ctx, Device{UserID: 1, HWID: "extra"}, mama.Devices, time.Now()); err != nil || slot != 2 {
		t.Fatalf("extra device: %d %v", slot, err)
	}
	if k, _ := st.Key(ctx, *mama, 2); k.UUID != SlotUUID(mama.UUID, 2, 1) {
		t.Fatalf("freed place kept its key: %+v", k)
	}

	// After the day only the places remain.
	if err := st.EndSharedKeys(ctx); err != nil {
		t.Fatal(err)
	}
	users, _ = st.CoreUsers(ctx)
	if len(users) != 3 || users[0].Slot != 1 {
		t.Fatalf("core users after the day: %+v", users)
	}
}

func devIDOf(list []Device, hwid string) int64 {
	for _, d := range list {
		if d.HWID == hwid {
			return d.ID
		}
	}
	return 0
}

// A new install has no shared keys.
func TestFreshInstallHasPlacesOnly(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	u := &User{Name: "Мама", UUID: "aaaaaaaa-0000-4000-8000-000000000001", Devices: 2, Enabled: true, Created: time.Now(), SubToken: "t"}
	if err := st.AddUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	keys, _ := st.Keys(ctx, *u, time.Now())
	if until, _ := st.SharedKeysUntil(ctx); !until.IsZero() || len(keys) != 2 || keys[0].Slot != 1 || keys[1].UUID != SlotUUID(u.UUID, 2, 0) {
		t.Fatalf("keys %+v, shared until %v", keys, until)
	}
	u.Enabled = false
	if keys, _ := st.Keys(ctx, *u, time.Now()); len(keys) != 0 {
		t.Fatalf("disabled user has keys: %+v", keys)
	}
}

// The services open the database together after an update: each must see
// the migrations applied once.
func TestMigrateConcurrently(t *testing.T) {
	path := filepath.Join(t.TempDir(), "panel.db")
	errs := make(chan error, 6)
	for range 6 {
		go func() {
			st, err := Open(path)
			if err == nil {
				st.Close()
			}
			errs <- err
		}()
	}
	for range 6 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var version int
	st.db.QueryRow("PRAGMA user_version").Scan(&version)
	if version != len(migrations) {
		t.Fatalf("version %d of %d", version, len(migrations))
	}
}
