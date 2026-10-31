package singbox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/Stilltiger45/TigerSB-panel/internal/store"
)

func TestRenderUserOutbounds(t *testing.T) {
	s := defaults(t)
	type conf struct {
		Outbounds []struct{ Type, Tag string }
		Route     struct {
			Rules []struct {
				AuthUser []string `json:"auth_user"`
				Outbound string
				Action   string
			}
			Final string
		}
		Experimental struct {
			ClashAPI struct {
				Controller string `json:"external_controller"`
				Secret     string
			} `json:"clash_api"`
		}
	}
	for _, c := range []struct {
		name string
		addr string
		raw  func() ([]byte, error)
	}{
		{"sing-box", "127.0.0.1:10087", func() ([]byte, error) { return Render(s, users(), "vpn.example.com", true) }},
		{"mbox", "127.0.0.1:10088", func() ([]byte, error) { return RenderMieru(s, users()) }},
	} {
		raw, err := c.raw()
		if err != nil {
			t.Fatal(err)
		}
		var got conf
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		api := got.Experimental.ClashAPI
		if api.Controller != c.addr || api.Secret != s.ClashSecret() || len(api.Secret) < 20 {
			t.Fatalf("%s: clash api %+v", c.name, api)
		}
		// Only the enabled user gets an outbound; the rest leave by direct.
		if len(got.Outbounds) != 2 || got.Outbounds[1].Tag != "user-u1" || got.Outbounds[1].Type != "direct" || got.Route.Final != "direct" {
			t.Fatalf("%s: outbounds %+v", c.name, got.Outbounds)
		}
		r := got.Route.Rules
		// Private addresses are refused before any user rule.
		if len(r) != 3 || r[1].Action != "reject" || len(r[2].AuthUser) != 1 || r[2].AuthUser[0] != "u1" || r[2].Outbound != "user-u1" {
			t.Fatalf("%s: rules %+v", c.name, r)
		}
	}
}

// snapshot is a Clash API answer as sing-box 1.14 gives it.
func snapshot(conns ...string) string {
	out := `{"downloadTotal":1,"uploadTotal":1,"memory":1,"connections":[`
	for i, c := range conns {
		if i > 0 {
			out += ","
		}
		out += c
	}
	return out + "]}"
}

func conn(id, user, ip string, up, down int64) string {
	b, _ := json.Marshal(map[string]any{
		"id": id, "upload": up, "download": down, "chains": []string{user}, "rule": "auth_user=x => route(" + user + ")",
		"metadata": map[string]any{"type": "tuic/tuic", "sourceIP": ip, "sourcePort": "4000", "destinationIP": "45.45.45.45", "host": ""},
	})
	return string(b)
}

func TestCountUsers(t *testing.T) {
	ctx := context.Background()
	var answer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Write([]byte(answer))
	}))
	defer srv.Close()
	if _, err := connections(ctx, srv.Client(), srv.URL, "wrong"); err == nil {
		t.Fatal("wrong secret accepted")
	}
	look := func(c *counter, now time.Time, conns ...string) {
		t.Helper()
		answer = snapshot(conns...)
		list, err := connections(ctx, srv.Client(), srv.URL, "secret")
		if err != nil {
			t.Fatal(err)
		}
		c.add(list, now)
	}

	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	u := store.User{Name: "Мама", UUID: "5783a3e7-e373-41cd-8642-c83782b807c5", Devices: 2, Enabled: true}
	if err := st.AddUser(ctx, &u); err != nil {
		t.Fatal(err)
	}
	name := "user-u" + strconv.FormatInt(u.ID, 10)
	rt := &Runtime{Dir: dir}
	t0 := time.Unix(1_800_000_000, 0)

	c := newCounter()
	look(c, t0,
		conn("a", name, "203.0.113.5", 100, 1000),
		conn("b", name, "::ffff:203.0.113.5", 10, 10), // the same phone over IPv4-mapped
		conn("n", name, "127.0.0.1", 1, 5),            // NaïveProxy through the edge
		conn("x", "direct", "198.51.100.1", 7, 7),     // the placeholder or a test link
		conn("t", "user-test-tuic", "198.51.100.2", 7, 7))
	look(c, t0.Add(time.Second),
		conn("a", name, "203.0.113.5", 150, 3000), // +50 +2000
		conn("c", name, "2001:db8::1", 5, 5))      // "b" and "n" closed, a new one opened
	now := c.users[u.ID]
	if now == nil || now.Up != 100+10+1+50+5 || now.Down != 1000+10+5+2000+5 {
		t.Fatalf("counted %+v", now)
	}
	if len(c.users) != 1 || len(now.Online) != 2 || now.Online[0].IP != "203.0.113.5" || now.Online[1].IP != "2001:db8::1" {
		t.Fatalf("online %+v of %d users", now.Online, len(c.users))
	}

	// The panel sees what is not stored yet, while the file is fresh.
	if err := c.write(rt.OnlinePath(), t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	got := rt.ReadOnline(t0.Add(3 * time.Second))
	if g := got[u.ID]; g.Up != now.Up || len(g.Online) != 2 {
		t.Fatalf("file: %+v", got)
	}
	if rt.ReadOnline(t0.Add(time.Minute)) != nil {
		t.Fatal("a stale file counts")
	}

	// Stored, the counts start again; a gone user is forgotten.
	if err := c.store(ctx, st, t0.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	look(c, t0.Add(3*time.Second))
	if err := c.store(ctx, st, t0.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if len(c.users) != 0 {
		t.Fatalf("left %+v", c.users)
	}
	list, _ := st.Users(ctx)
	if list[0].Up != 166 || list[0].Down != 3020 || list[0].SeenAt != t0.Add(2*time.Second).Unix() || list[0].Online != "[]" {
		t.Fatalf("stored %+v", list[0])
	}
}
