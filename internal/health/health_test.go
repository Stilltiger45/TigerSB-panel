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
	"github.com/Stilltiger45/TigerSB-panel/internal/singbox"
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
	udp     map[int]bool // bound UDP ports
	cert    bool         // sing-box has the domain's certificate
	env     Env
	cleanup func()
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{units: map[string]string{
		"tigersb.service": "active", "tigersb-caddy.service": "active",
		"tigersb-edge.service": "active", "tigersb-xray.service": "active",
		"tigersb-sub.service": "active", "tigersb-singbox.service": "active", "tigersb-mieru.service": "active",
	}, routed: true, ntp: "yes", ports: map[int]bool{8443: true, 10442: true, 10086: true, 2443: true, 10443: true, 2012: true},
		udp: map[int]bool{8443: true, 443: true}, cert: true}
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
		Lookup:       func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("203.0.113.5")}, nil },
		LocalIPs:     func() ([]net.IP, error) { return []net.IP{net.ParseIP("203.0.113.5")}, nil },
		Internet:     internet.Addr().String(),
		Listening:    func(port int) bool { return w.ports[port] },
		ListeningUDP: func(port int) bool { return w.udp[port] },
		TUICCert:     func() bool { return w.cert },
		Updater:      &update.Updater{Layout: update.DefaultLayout(filepath.Join(dir, "config.json"), filepath.Join(dir, "panel.db")), Store: st, Version: "main-1111111", Run: run},
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
		"disk": Off, "edge": OK, "xray": OK, "singbox": OK, "mieru": OK, "routers": Soon,
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
	if got := states(r)["sub"]; got != OK {
		t.Errorf("subscriptions: %s", got)
	}
	w.units["tigersb-xray.service"] = "failed"
	w.units["tigersb-edge.service"] = "inactive"
	w.ports[10086] = false
	r, _ = Run(ctx, w.env)
	if s := states(r); s["xray"] != Fail || s["edge"] != Fail || s["sub"] != Fail {
		t.Errorf("units down: %v", s)
	}
}

func TestSingbox(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	check := func(want, why string) {
		t.Helper()
		r, _ := Run(ctx, w.env)
		if got := states(r)["singbox"]; got != want {
			t.Errorf("%s: %s, want %s", why, got, want)
		}
	}
	w.cert = false
	check(Warn, "no certificate yet")
	w.cert = true
	r, _ := Run(ctx, w.env)
	for _, res := range r.Results {
		if res.ID == "singbox" && !strings.Contains(res.Detail, "TUIC :8443/udp, Hysteria 2 :443/udp, NaïveProxy :2443/tcp") {
			t.Errorf("detail: %s", res.Detail)
		}
	}
	w.udp[443] = false
	check(Fail, "UDP 443 not bound")
	w.udp[443] = true
	w.udp[8443] = false
	check(Fail, "UDP 8443 not bound")
	w.udp[8443] = true
	w.ports[2443] = false
	check(Fail, "TCP 2443 not listening")
	w.ports[2443] = true
	s, _ := singbox.Load(ctx, w.env.Store)
	s.TUIC.Enabled = false
	singbox.Save(ctx, w.env.Store, s)
	check(OK, "TUIC off, Hysteria 2 on")
	s.Hysteria2.Enabled = false
	s.Naive.Enabled = false
	singbox.Save(ctx, w.env.Store, s)
	check(Off, "all off")
	w.units["tigersb-singbox.service"] = "failed"
	check(Fail, "container down")
}

func TestBytes(t *testing.T) {
	for n, want := range map[uint64]string{512: "512 Б", 2048: "2.0 КБ", 5 << 30: "5.0 ГБ"} {
		if got := bytes(n); got != want {
			t.Errorf("%d: %q, want %q", n, got, want)
		}
	}
}

func TestListeningUDP(t *testing.T) {
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := c.LocalAddr().(*net.UDPAddr).Port
	if !listeningUDP(port) {
		t.Errorf("bound port %d not seen", port)
	}
	c.Close()
	if listeningUDP(port) {
		t.Errorf("closed port %d still seen", port)
	}
}

func TestMieru(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	check := func(want, why string) {
		t.Helper()
		r, _ := Run(ctx, w.env)
		if got := states(r)["mieru"]; got != want {
			t.Errorf("%s: %s, want %s", why, got, want)
		}
	}
	check(OK, "running")
	s, _ := singbox.Load(ctx, w.env.Store)
	s.Mieru.Transport = singbox.MieruBoth
	singbox.Save(ctx, w.env.Store, s)
	check(Fail, "UDP 2012 not listening")
	w.udp[2012] = true
	check(OK, "TCP and UDP")
	w.ports[2012] = false
	check(Fail, "TCP 2012 not listening")
	s.Mieru.Enabled = false
	singbox.Save(ctx, w.env.Store, s)
	check(Off, "off")
	w.units["tigersb-mieru.service"] = "failed"
	check(Fail, "container down")
}

func TestProtocols(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	check := func(want map[string]string, why string) {
		t.Helper()
		list, err := Protocols(ctx, w.env)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		var ids []string
		for _, r := range list {
			got[r.ID] = r.State
			ids = append(ids, r.ID)
		}
		if strings.Join(ids, ",") != "xhttp,grpc,tuic,hysteria2,naive,mieru" {
			t.Fatalf("order: %v", ids)
		}
		for id, state := range want {
			if got[id] != state {
				t.Errorf("%s: %s is %s, want %s", why, id, got[id], state)
			}
		}
	}
	all := func(state string) map[string]string {
		return map[string]string{"xhttp": state, "grpc": state, "tuic": state, "hysteria2": state, "naive": state, "mieru": state}
	}
	check(all(OK), "healthy")

	w.ports[10443] = false
	check(map[string]string{"naive": Fail, "tuic": OK}, "NaïveProxy not behind the edge")
	w.ports[10443] = true
	s, _ := singbox.Load(ctx, w.env.Store)
	s.Naive.Network = singbox.NaiveBoth
	singbox.Save(ctx, w.env.Store, s)
	check(map[string]string{"naive": Fail}, "QUIC not listening")
	w.udp[2443] = true
	check(map[string]string{"naive": OK}, "TCP and QUIC")
	s.Mieru.Transport = singbox.MieruBoth
	singbox.Save(ctx, w.env.Store, s)
	check(map[string]string{"mieru": Fail}, "Mieru UDP not listening")
	w.udp[2012] = true
	check(map[string]string{"mieru": OK}, "Mieru on TCP and UDP")

	w.cert = false
	check(map[string]string{"tuic": Warn, "hysteria2": Warn, "naive": Warn, "mieru": OK, "xhttp": OK}, "no certificate")
	w.cert = true
	w.units["tigersb-singbox.service"] = "failed"
	check(map[string]string{"tuic": Fail, "naive": Fail, "mieru": OK}, "sing-box down")
	w.units["tigersb-xray.service"] = "failed"
	check(map[string]string{"xhttp": Fail, "grpc": Fail}, "Xray down")
	s.Mieru.Enabled = false
	singbox.Save(ctx, w.env.Store, s)
	check(map[string]string{"mieru": Off}, "Mieru off")
}
