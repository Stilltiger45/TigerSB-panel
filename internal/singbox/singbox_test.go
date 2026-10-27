package singbox

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stilltiger45/TigerSB-client/pkg/profile"

	"github.com/Stilltiger45/TigerSB-panel/internal/store"
)

func users() []store.User {
	return []store.User{
		{ID: 1, Name: "Мама", UUID: "5783a3e7-e373-41cd-8642-c83782b807c5", Enabled: true},
		{ID: 2, Name: "Папа", UUID: "33d7be1c-8923-4184-819c-140e8557fe3c", Enabled: false},
	}
}

func defaults(t *testing.T) Settings {
	t.Helper()
	s, err := Defaults()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRender(t *testing.T) {
	s := defaults(t)
	raw, err := Render(s, users(), "vpn.example.com", true)
	if err != nil {
		t.Fatal(err)
	}
	var conf struct {
		Log      map[string]any `json:"log"`
		Inbounds []struct {
			Type       string `json:"type"`
			ListenPort int    `json:"listen_port"`
			Users      []struct {
				Name, UUID, Password string
			} `json:"users"`
			TLS struct {
				ServerName string   `json:"server_name"`
				ALPN       []string `json:"alpn"`
				Cert       string   `json:"certificate_path"`
			} `json:"tls"`
			CC   string `json:"congestion_control"`
			Idle string `json:"idle_timeout"`
		} `json:"inbounds"`
		Route struct {
			Rules []map[string]any `json:"rules"`
		} `json:"route"`
	}
	if err := json.Unmarshal(raw, &conf); err != nil {
		t.Fatal(err)
	}
	if conf.Log["disabled"] != true {
		t.Error("logs must be off")
	}
	if len(conf.Inbounds) != 1 {
		t.Fatalf("inbounds: %s", raw)
	}
	in := conf.Inbounds[0]
	if in.Type != "tuic" || in.ListenPort != 8443 || in.CC != "bbr" || in.TLS.ServerName != "vpn.example.com" || in.TLS.Cert != "/etc/tigersb-singbox/tls.crt" || in.Idle != "12s" {
		t.Errorf("inbound: %s", raw)
	}
	// Disabled users are left out.
	if len(in.Users) != 1 || in.Users[0].Name != "u1" || in.Users[0].Password != s.Password(users()[0].UUID) {
		t.Errorf("users: %+v", in.Users)
	}
	if !strings.Contains(string(raw), `"ip_is_private": true`) {
		t.Errorf("private addresses are not refused: %s", raw)
	}
	// No certificate, no domain or TUIC off: sing-box runs with nothing to serve.
	for _, tc := range []struct {
		domain string
		cert   bool
		on     bool
	}{{"", true, true}, {"vpn.example.com", false, true}, {"vpn.example.com", true, false}} {
		s.TUIC.Enabled = tc.on
		raw, _ := Render(s, users(), tc.domain, tc.cert)
		if strings.Contains(string(raw), `"tuic"`) {
			t.Errorf("%+v: TUIC served: %s", tc, raw)
		}
		if Links(s, "203.0.113.7", tc.domain, tc.cert, users()[0]) != nil {
			t.Errorf("%+v: link without TUIC", tc)
		}
	}
}

// The link must be one the TigerSB client reads into a working outbound.
func TestLinkForClient(t *testing.T) {
	s := defaults(t)
	s.TUIC.ZeroRTT = true
	u := users()[0]
	links := Links(s, "203.0.113.7", "vpn.example.com", true, u)
	if len(links) != 1 || links[0].Protocol != "tuic" || links[0].Title != "Мама · TUIC" {
		t.Fatalf("links: %+v", links)
	}
	raw, name, err := profile.ParseLink(links[0].URL)
	if err != nil || name != "Мама · TUIC" {
		t.Fatalf("client rejects %s: %v", links[0].URL, err)
	}
	var out struct {
		Protocol string `json:"protocol"`
		Settings struct {
			Address, UUID, Password, CongestionControl, UDPRelayMode string
			Port                                                     int
			ZeroRTTHandshake                                         bool
		} `json:"settings"`
		Stream struct {
			TLS struct {
				ServerName    string
				AllowInsecure bool
			} `json:"tlsSettings"`
		} `json:"streamSettings"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	st := out.Settings
	if out.Protocol != "tuic" || st.Address != "203.0.113.7" || st.Port != 8443 || st.UUID != u.UUID || st.Password != s.Password(u.UUID) ||
		st.CongestionControl != "bbr" || st.UDPRelayMode != "native" || !st.ZeroRTTHandshake {
		t.Errorf("outbound: %s", raw)
	}
	if out.Stream.TLS.ServerName != "vpn.example.com" || out.Stream.TLS.AllowInsecure {
		t.Errorf("tls: %s", raw)
	}
}

func TestPasswordAndValidate(t *testing.T) {
	s := defaults(t)
	a, b := s.Password(users()[0].UUID), s.Password(users()[1].UUID)
	if a == b || len(a) != 24 || a != s.Password(users()[0].UUID) {
		t.Errorf("passwords %q %q", a, b)
	}
	other := defaults(t)
	if other.Password(users()[0].UUID) == a {
		t.Error("the secret does not change the password")
	}
	for _, mut := range []func(*Settings){
		func(s *Settings) { s.TUIC.Port = 80 },
		func(s *Settings) { s.TUIC.Port = 70000 },
		func(s *Settings) { s.TUIC.CongestionControl = "brutal" },
		func(s *Settings) { s.TUIC.UDPRelayMode = "x" },
		func(s *Settings) { s.TUIC.Heartbeat = -1 },
		func(s *Settings) { s.TUIC.AuthTimeout = 0 },
		func(s *Settings) { s.Secret = "" },
	} {
		c := defaults(t)
		mut(&c)
		if c.Validate() == nil {
			t.Errorf("accepted %+v", c.TUIC)
		}
	}
	s.TUIC.Port = 443
	if err := s.Validate(); err != nil {
		t.Error(err)
	}
}

func TestLoadKeepsSecret(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	a, err := Load(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Load(ctx, st)
	if a.Secret == "" || a.Secret != b.Secret || !a.TUIC.Enabled || a.TUIC.Port != 8443 {
		t.Fatalf("%+v %+v", a, b)
	}
}

// writeCert puts a certificate for domain into Caddy's storage layout.
func writeCert(t *testing.T, storage, issuer, domain string, notAfter time.Time) []byte {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: domain},
		DNSNames: []string{domain}, NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(storage, "certificates", issuer, domain)
	os.MkdirAll(dir, 0o700)
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	kb, _ := x509.MarshalECPrivateKey(key)
	os.WriteFile(filepath.Join(dir, domain+".crt"), cert, 0o600)
	os.WriteFile(filepath.Join(dir, domain+".key"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
	return cert
}

func TestSyncCopiesCertAndReloads(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	st.AddUser(ctx, &store.User{Name: "Мама", UUID: users()[0].UUID, Devices: 1, Enabled: true, Created: time.Now(), SubToken: strings.Repeat("a", 32)})
	var calls []string
	rt := DefaultRuntime(dir)
	rt.Exec = func(_ context.Context, _ []byte, name string, args ...string) (string, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return "", nil
	}
	const domain = "vpn.example.com"
	// No certificate yet: a config without TUIC, nothing to serve.
	if err := rt.Sync(ctx, st, domain, true); err != nil {
		t.Fatal(err)
	}
	if rt.HaveCert() || len(calls) != 1 {
		t.Fatalf("calls %v", calls)
	}
	writeCert(t, rt.Certs, "acme-v02.api.letsencrypt.org-directory", domain, time.Now().Add(24*time.Hour))
	newer := writeCert(t, rt.Certs, "acme.zerossl.com-v2-dv90", domain, time.Now().Add(80*24*time.Hour))
	writeCert(t, rt.Certs, "local", "other.example.com", time.Now().Add(80*24*time.Hour))
	if err := rt.Sync(ctx, st, domain, true); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(rt.Dir, "tls.crt"))
	if string(got) != string(newer) {
		t.Error("the newest certificate was not taken")
	}
	conf, _ := os.ReadFile(rt.ConfigPath())
	if !strings.Contains(string(conf), `"type": "tuic"`) || len(calls) != 2 || calls[1] != "systemctl try-reload-or-restart tigersb-singbox.service" {
		t.Fatalf("calls %v\n%s", calls, conf)
	}
	// Nothing changed: no reload.
	if err := rt.Sync(ctx, st, domain, true); err != nil || len(calls) != 2 {
		t.Fatalf("calls %v %v", calls, err)
	}
	// The domain is gone: the copies go too, and TUIC with them.
	if err := rt.Sync(ctx, st, "", true); err != nil || rt.HaveCert() || len(calls) != 3 {
		t.Fatalf("calls %v %v", calls, err)
	}
}
