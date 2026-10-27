package xray

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Stilltiger45/TigerSB-panel/internal/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestDefaults(t *testing.T) {
	s, err := Defaults()
	if err != nil {
		t.Fatal(err)
	}
	if s.GRPC.Port != 8443 || s.GRPC.Reality.SNI != "google.com" || s.XHTTP.Port != 443 || s.XHTTP.Reality.SNI != "cloudflare.com" {
		t.Fatalf("defaults %+v", s)
	}
	if s.XHTTP.UplinkHTTPMethod != "POST" || s.XHTTP.Mode != "auto" {
		t.Fatalf("xhttp defaults %+v", s.XHTTP)
	}
	if err := s.Validate("", 20000); err != nil {
		t.Fatal(err)
	}
	if s.GRPC.Reality.PrivateKey == s.XHTTP.Reality.PrivateKey || s.GRPC.Reality.PublicKey == "" {
		t.Fatal("each protocol needs its own key pair")
	}
}

func TestLoadKeepsKeys(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	a, err := Load(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Load(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if a.GRPC.Reality.PrivateKey != b.GRPC.Reality.PrivateKey || b.GRPC.Reality.PublicKey == "" {
		t.Fatal("keys changed between loads")
	}
}

func TestValidate(t *testing.T) {
	base, _ := Defaults()
	cases := []struct {
		name string
		edit func(*Settings)
		want string
	}{
		{"low port", func(s *Settings) { s.GRPC.Port = 80 }, "порт 80"},
		{"reserved", func(s *Settings) { s.GRPC.Port = TLSPort }, "Caddy"},
		{"panel port", func(s *Settings) { s.XHTTP.Port = 20000 }, "панель"},
		{"shared port", func(s *Settings) { s.GRPC.Port = 9000; s.XHTTP.Port = 9000 }, "общим может быть только 443"},
		{"shared 443 same sni", func(s *Settings) { s.GRPC.Port = 443; s.GRPC.Reality.SNI = "cloudflare.com" }, "разные SNI"},
		{"own domain", func(s *Settings) { s.GRPC.Reality.SNI = "vpn.example.com" }, "доменом панели"},
		{"bad sni", func(s *Settings) { s.GRPC.Reality.SNI = "1.2.3.4" }, "SNI"},
		{"bad key", func(s *Settings) { s.XHTTP.Reality.PrivateKey = "abc" }, "закрытый ключ"},
		{"bad sid", func(s *Settings) { s.XHTTP.Reality.ShortIDs = []string{"abc"} }, "short ID"},
		{"service", func(s *Settings) { s.GRPC.ServiceName = "a b" }, "имя сервиса"},
		{"path", func(s *Settings) { s.XHTTP.Path = "abc" }, "путь"},
		{"method", func(s *Settings) { s.XHTTP.UplinkHTTPMethod = "GET" }, "метод"},
		{"range", func(s *Settings) { s.XHTTP.XPaddingBytes = "1000-100" }, "xPaddingBytes"},
		{"xmux", func(s *Settings) { s.XHTTP.Xmux.MaxConnections = "4" }, "что-то одно"},
		{"target", func(s *Settings) { s.GRPC.Reality.Target = "google.com" }, "target"},
	}
	for _, c := range cases {
		s := base
		s.GRPC.Reality.ShortIDs = append([]string(nil), base.GRPC.Reality.ShortIDs...)
		c.edit(&s)
		s.Normalize()
		err := s.Validate("vpn.example.com", 20000)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", c.name, err, c.want)
		}
	}
	ok := base
	ok.GRPC.Port, ok.GRPC.Reality.SNI = 443, "www.microsoft.com"
	if err := ok.Validate("", 20000); err != nil {
		t.Errorf("443 shared by SNI: %v", err)
	}
}

func users() []store.User {
	return []store.User{
		{ID: 1, Name: "Мама", UUID: "8f2a3c4e-1b2d-4e5f-8a9b-0c1d2e3f4a5b", Devices: 2, Enabled: true},
		{ID: 2, Name: "off", UUID: "9f2a3c4e-1b2d-4e5f-8a9b-0c1d2e3f4a5b", Devices: 1, Enabled: false},
	}
}

func TestRenderAndRoutes(t *testing.T) {
	s, _ := Defaults()
	conf, err := Render(s, users())
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Inbounds []struct {
			Tag      string `json:"tag"`
			Listen   string `json:"listen"`
			Port     int    `json:"port"`
			Settings struct {
				Clients []struct{ Email string } `json:"clients"`
			} `json:"settings"`
			Stream map[string]any `json:"streamSettings"`
		} `json:"inbounds"`
	}
	if err := json.Unmarshal(conf, &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Inbounds) != 2 {
		t.Fatalf("inbounds %d", len(c.Inbounds))
	}
	g, x := c.Inbounds[0], c.Inbounds[1]
	if g.Tag != "grpc" || g.Port != 8443 || g.Listen != "" || g.Stream["sockopt"] != nil {
		t.Errorf("grpc inbound %+v", g)
	}
	if x.Tag != "xhttp" || x.Port != XHTTPInner || x.Listen != "127.0.0.1" || x.Stream["sockopt"] == nil {
		t.Errorf("xhttp inbound must sit behind the edge: %+v", x)
	}
	if len(g.Settings.Clients) != 1 || g.Settings.Clients[0].Email != "u1" {
		t.Errorf("only enabled users: %+v", g.Settings.Clients)
	}

	r := Routes(s, "vpn.example.com")
	if r.Pick("vpn.example.com").Addr != "127.0.0.1:8444" || !r.Pick("vpn.example.com").Proxy {
		t.Errorf("domain must go to Caddy with the PROXY header: %+v", r.Pick("vpn.example.com"))
	}
	if b := r.Pick("cloudflare.com"); b.Addr != "127.0.0.1:10442" || !b.Proxy {
		t.Errorf("xhttp sni: %+v", b)
	}
	if b := r.Pick(""); b == nil || b.Addr != "127.0.0.1:10442" {
		t.Errorf("unknown names go to xhttp: %+v", b)
	}
	s.XHTTP.Port = 2053
	if b := Routes(s, "vpn.example.com").Pick("other"); b == nil || b.Addr != "127.0.0.1:8444" {
		t.Errorf("without a protocol on 443 unknown names go to Caddy: %+v", b)
	}
	if b := Routes(s, "").Pick("other"); b != nil {
		t.Errorf("nothing on 443: %+v", b)
	}
}

// TestXrayAcceptsConfig runs the real Xray binary when XRAY_BIN is set.
func TestXrayAcceptsConfig(t *testing.T) {
	bin := os.Getenv("XRAY_BIN")
	if bin == "" {
		t.Skip("XRAY_BIN not set")
	}
	s, _ := Defaults()
	for _, edit := range []func(*Settings){
		func(*Settings) {},
		func(s *Settings) {
			s.GRPC.Port, s.GRPC.IdleTimeout, s.GRPC.HealthCheckTimeout = 443, 60, 20
			s.GRPC.Reality.SNI = "www.microsoft.com"
			s.XHTTP.Mode, s.XHTTP.NoSSEHeader, s.XHTTP.Host = "packet-up", true, "cdn.example.com"
		},
	} {
		c := s
		edit(&c)
		conf, err := Render(c, users())
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "config.json")
		os.WriteFile(path, conf, 0o600)
		out, err := exec.Command(bin, "run", "-test", "-c", path).CombinedOutput()
		if err != nil || !strings.Contains(string(out), "Configuration OK") {
			t.Fatalf("xray -test: %v\n%s\n%s", err, out, conf)
		}
	}
}

func TestLinks(t *testing.T) {
	s, _ := Defaults()
	u := users()[0]
	links := Links(s, "203.0.113.7", u)
	if len(links) != 2 {
		t.Fatalf("links %d", len(links))
	}
	g, err := url.Parse(links[0].URL)
	if err != nil {
		t.Fatal(err)
	}
	q := g.Query()
	if g.Scheme != "vless" || g.User.Username() != u.UUID || g.Host != "203.0.113.7:8443" {
		t.Errorf("grpc link %s", links[0].URL)
	}
	if q.Get("type") != "grpc" || q.Get("security") != "reality" || q.Get("sni") != "google.com" ||
		q.Get("pbk") != s.GRPC.Reality.PublicKey || q.Get("sid") != s.GRPC.Reality.ShortIDs[0] ||
		q.Get("serviceName") != s.GRPC.ServiceName || q.Get("mode") != "gun" || q.Get("fp") != "chrome" {
		t.Errorf("grpc query %v", q)
	}
	if g.Fragment != "Мама · gRPC" {
		t.Errorf("fragment %q", g.Fragment)
	}
	x, _ := url.Parse(links[1].URL)
	xq := x.Query()
	if x.Port() != "443" || xq.Get("type") != "xhttp" || xq.Get("path") != s.XHTTP.Path || xq.Get("mode") != "auto" {
		t.Errorf("xhttp link %s", links[1].URL)
	}
	var extra map[string]any
	if err := json.Unmarshal([]byte(xq.Get("extra")), &extra); err != nil {
		t.Fatalf("extra: %v", err)
	}
	if extra["uplinkHTTPMethod"] != "POST" || extra["xPaddingBytes"] != "100-1000" || extra["xmux"] == nil {
		t.Errorf("extra %v", extra)
	}
	v6 := Links(s, "2001:db8::1", u)[0].URL
	if !strings.Contains(v6, "@[2001:db8::1]:8443?") {
		t.Errorf("ipv6 link %s", v6)
	}
}

func TestPublicAddress(t *testing.T) {
	ips := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("10.0.0.5"), net.ParseIP("2001:db8::1"), net.ParseIP("203.0.113.7")}
	if a := PublicAddress(Settings{}, ips, "vpn.example.com"); a != "203.0.113.7" {
		t.Errorf("got %s", a)
	}
	if a := PublicAddress(Settings{}, ips[:2], "vpn.example.com"); a != "vpn.example.com" {
		t.Errorf("got %s", a)
	}
	if a := PublicAddress(Settings{Address: "1.2.3.4"}, ips, ""); a != "1.2.3.4" {
		t.Errorf("got %s", a)
	}
}

func TestParseStats(t *testing.T) {
	online := `{
  "users": [
    {"email": "u1", "ips": [{"ip": "198.51.100.4", "lastSeen": "1761500000"}]},
    {"email": "admin@x", "ips": [{"ip": "198.51.100.9"}]}
  ]
}`
	traffic := `{"stat": [
  {"name": "user>>>u1>>>traffic>>>uplink", "value": 1000},
  {"name": "user>>>u1>>>traffic>>>downlink", "value": "5000"},
  {"name": "user>>>u2>>>traffic>>>downlink", "value": 7},
  {"name": "inbound>>>xhttp>>>traffic>>>uplink", "value": 9}
]}`
	list, err := ParseStats(online, traffic)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != 1 || list[0].Up != 1000 || list[0].Down != 5000 ||
		len(list[0].Online) != 1 || list[0].Online[0].LastSeen != 1761500000 ||
		list[1].ID != 2 || list[1].Down != 7 || len(list[1].Online) != 0 {
		t.Fatalf("%+v", list)
	}
	if l, err := ParseStats("", "\n"); err != nil || len(l) != 0 {
		t.Fatalf("empty: %v %v", l, err)
	}
}

func TestScanTargets(t *testing.T) {
	ctx := context.Background()
	a, err := scanTargets(ctx, "203.0.113.77")
	if err != nil || len(a) != 254 || a[0].String() != "203.0.113.1" || a[253].String() != "203.0.113.254" {
		t.Fatalf("single ip: %d %v", len(a), err)
	}
	if a, err := scanTargets(ctx, "198.51.100.0/23"); err != nil || len(a) != 510 {
		t.Fatalf("/23: %d %v", len(a), err)
	}
	if _, err := scanTargets(ctx, "10.0.0.0/16"); err == nil {
		t.Fatal("too big a range must be refused")
	}
	if _, err := scanTargets(ctx, "not a target"); err == nil {
		t.Fatal("garbage must be refused")
	}
}

func TestScanFindsSite(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	n, _ := strconv.Atoi(port)
	old := scanPort
	scanPort = uint16(n)
	defer func() { scanPort = old }()
	list, err := Scan(context.Background(), "127.0.0.1/32")
	if err != nil || len(list) != 1 {
		t.Fatalf("%v %+v", err, list)
	}
	p := list[0]
	if p.Domain != "example.com" || !p.TLS13 || !p.H2 || !p.X25519 || p.CertOK || p.Good {
		t.Fatalf("%+v", p)
	}
}
