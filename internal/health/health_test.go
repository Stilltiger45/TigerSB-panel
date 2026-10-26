package health

import (
	"context"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stilltiger45/TigerSB-panel/internal/config"
	"github.com/Stilltiger45/TigerSB-panel/internal/site"
	"github.com/Stilltiger45/TigerSB-panel/internal/store"
	"github.com/Stilltiger45/TigerSB-panel/internal/update"
)

const secret = "0123456789abcdef0123456789abcdef"

type world struct {
	units   map[string]string
	routed  bool // Caddy proxies the secret path to the panel
	ntp     string
	ports   map[int]bool // loopback ports that accept connections
	env     Env
	cleanup func()
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{units: map[string]string{
		"tigersb.service": "active", "tigersb-caddy.service": "active",
		"tigersb-edge.service": "active", "tigersb-xray.service": "active",
	}, routed: true, ntp: "yes", ports: map[int]bool{8443: true, 10442: true}}
	caddy := httptest.NewTLSServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if w.routed && strings.HasPrefix(r.URL.Path, "/"+secret+"/") {
			rw.Header().Set("Content-Security-Policy", "default-src 'self'; form-action 'self'")
		}
		rw.Write([]byte("page"))
	}))
	internet, _ := net.Listen("tcp", "127.0.0.1:0")
	t.Cleanup(func() { caddy.Close(); internet.Close() })

	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{Role: config.RoleMain, Listen: "127.0.0.1:30000", SecretPath: secret, Domain: "vpn.example.com"}
	roots := x509.NewCertPool()
	roots.AddCert(caddy.Certificate())
	run := func(name string, args ...string) (string, error) {
		switch {
		case name == "systemctl" && args[0] == "is-active":
			return w.units[args[1]], nil
		case name == "systemctl" && args[0] == "is-enabled":
			if w.units[args[1]] == "inactive" {
				return "disabled", nil
			}
			return "enabled", nil
		case name == "timedatectl":
			return w.ntp, nil
		}
		return "", nil
	}
	w.env = Env{
		Cfg: cfg, Store: st, Run: run, Roots: roots,
		HTTPSAddr: caddy.Listener.Addr().String(), HTTPAddr: caddy.Listener.Addr().String(),
		Lookup:    func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("203.0.113.5")}, nil },
		LocalIPs:  func() ([]net.IP, error) { return []net.IP{net.ParseIP("203.0.113.5")}, nil },
		Internet:  internet.Addr().String(),
		Listening: func(port int) bool { return w.ports[port] },
		Updater:   &update.Updater{Layout: update.DefaultLayout(filepath.Join(dir, "config.json"), filepath.Join(dir, "panel.db")), Store: st, Version: "main-1111111", Run: run},
	}
	return w
}

func states(r *Report) map[string]string {
	out := map[string]string{}
	for _, res := range r.Results {
		out[res.ID] = res.State
	}
	return out
}

func TestHealthyServer(t *testing.T) {
	w := newWorld(t)
	r, err := Run(context.Background(), w.env)
	if err != nil {
		t.Fatal(err)
	}
	got := states(r)
	// httptest's certificate is for example.com, not vpn.example.com, and no
	// samples or restore points exist yet.
	for id, want := range map[string]string{
		"panel": OK, "caddy": OK, "stub": OK, "dns": OK, "route": OK, "clock": OK,
		"internet": OK, "db": OK, "update": OK, "monitor": Warn, "restore": Warn,
		"disk": Off, "edge": OK, "xray": OK, "singbox": Soon, "routers": Soon,
	} {
		if got[id] != want {
			t.Errorf("%s: %s, want %s", id, got[id], want)
		}
	}
	last, err := Last(context.Background(), w.env.Store)
	if err != nil || last == nil || len(last.Results) != len(r.Results) {
		t.Fatalf("stored report: %+v %v", last, err)
	}
}

func TestRouteMismatchAndSleep(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	// The panel is open, but Caddy serves the stub on the secret path.
	w.routed = false
	r, _ := Run(ctx, w.env)
	if states(r)["route"] != Fail || r.State != Fail {
		t.Fatalf("broken route: %+v", r.Results)
	}

	// Closed and not routed: fine.
	site.SetPanelOpen(ctx, w.env.Store, false)
	r, _ = Run(ctx, w.env)
	if states(r)["route"] != OK {
		t.Fatalf("closed: %v", states(r))
	}

	// Closed but still routed: a leak.
	w.routed = true
	r, _ = Run(ctx, w.env)
	if states(r)["route"] != Fail {
		t.Fatalf("leak: %v", states(r))
	}

	// Asleep is not a failure.
	w.routed = false
	site.SetPanelOpen(ctx, w.env.Store, true)
	w.units["tigersb.service"] = "inactive"
	r, _ = Run(ctx, w.env)
	if s := states(r); s["panel"] != Off || s["route"] != Off {
		t.Fatalf("asleep: %v", s)
	}
}

func TestCaddyDownAndOtherFailures(t *testing.T) {
	w := newWorld(t)
	w.units["tigersb-caddy.service"] = "failed"
	w.ntp = "no"
	w.env.Lookup = func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("198.51.100.1")}, nil }
	w.env.Internet = "127.0.0.1:1"
	w.env.Now = time.Now()
	r, _ := Run(context.Background(), w.env)
	got := states(r)
	for id, want := range map[string]string{"caddy": Fail, "stub": Off, "clock": Warn, "dns": Warn, "internet": Warn} {
		if got[id] != want {
			t.Errorf("%s: %s, want %s", id, got[id], want)
		}
	}
}

func TestXrayAndEdge(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	r, _ := Run(ctx, w.env)
	for _, res := range r.Results {
		if res.ID == "xray" && !strings.Contains(res.Detail, "gRPC :8443, XHTTP :443") {
			t.Errorf("detail: %s", res.Detail)
		}
	}
	w.ports[10442] = false
	r, _ = Run(ctx, w.env)
	if got := states(r)["xray"]; got != Fail {
		t.Errorf("XHTTP not listening: %s", got)
	}
	w.units["tigersb-xray.service"] = "failed"
	w.units["tigersb-edge.service"] = "inactive"
	r, _ = Run(ctx, w.env)
	if s := states(r); s["xray"] != Fail || s["edge"] != Fail {
		t.Errorf("units down: %v", s)
	}
}

func TestBytes(t *testing.T) {
	for n, want := range map[uint64]string{512: "512 Б", 2048: "2.0 КБ", 5 << 30: "5.0 ГБ"} {
		if got := bytes(n); got != want {
			t.Errorf("%d: %q, want %q", n, got, want)
		}
	}
}
