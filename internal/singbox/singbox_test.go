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
			BBR  string `json:"bbr_profile"`
			Obfs *struct {
				Type, Password string
			} `json:"obfs"`
			Masquerade string `json:"masquerade"`
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
	if len(conf.Inbounds) != 2 {
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
	hy := conf.Inbounds[1]
	if hy.Type != "hysteria2" || hy.ListenPort != 443 || hy.BBR != "standard" || hy.Idle != "12s" || hy.TLS.ServerName != "vpn.example.com" ||
		hy.Obfs == nil || hy.Obfs.Type != "salamander" || hy.Obfs.Password != s.Hysteria2.ObfsPassword || len(hy.Obfs.Password) != 24 || hy.Masquerade != "" {
		t.Errorf("hysteria2: %s", raw)
	}
	if len(hy.Users) != 1 || hy.Users[0].Name != "u1" || hy.Users[0].Password != s.Hy2Password(users()[0].UUID) {
		t.Errorf("hysteria2 users: %+v", hy.Users)
	}
	if strings.Contains(string(raw), "up_mbps") {
		t.Errorf("speeds without settings: %s", raw)
	}
	// Without Salamander strangers who speak HTTP/3 see the site.
	plain := s
	plain.Hysteria2.Obfs = false
	plain.Hysteria2.UpMbps, plain.Hysteria2.DownMbps = 100, 50
	raw2, _ := Render(plain, users(), "vpn.example.com", true)
	if strings.Contains(string(raw2), "salamander") || !strings.Contains(string(raw2), `"masquerade": "https://vpn.example.com"`) ||
		!strings.Contains(string(raw2), `"up_mbps": 100`) || !strings.Contains(string(raw2), `"down_mbps": 50`) {
		t.Errorf("hysteria2 without salamander: %s", raw2)
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
		s.TUIC.Enabled, s.Hysteria2.Enabled = tc.on, tc.on
		raw, _ := Render(s, users(), tc.domain, tc.cert)
		if strings.Contains(string(raw), `"tuic"`) || strings.Contains(string(raw), `"hysteria2"`) {
			t.Errorf("%+v: served: %s", tc, raw)
		}
		if Links(s, "203.0.113.7", tc.domain, tc.cert, users()[0]) != nil || Hy2Links(s, "203.0.113.7", tc.domain, tc.cert, users()[0]) != nil {
			t.Errorf("%+v: link without the protocol", tc)
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

// The Hysteria 2 link must be one the TigerSB client reads into an outbound
// with Salamander.
func TestHy2LinkForClient(t *testing.T) {
	s := defaults(t)
	s.Hysteria2.ObfsPassword = "pa ss&word+1"
	u := users()[0]
	links := Hy2Links(s, "2001:db8::7", "vpn.example.com", true, u)
	if len(links) != 1 || links[0].Protocol != "hysteria2" || links[0].Title != "Мама · Hysteria 2" {
		t.Fatalf("links: %+v", links)
	}
	raw, name, err := profile.ParseLink(links[0].URL)
	if err != nil || name != "Мама · Hysteria 2" {
		t.Fatalf("client rejects %s: %v", links[0].URL, err)
	}
	var out struct {
		Protocol string `json:"protocol"`
		Settings struct {
			Address string
			Port    int
		} `json:"settings"`
		Stream struct {
			TLS struct {
				ServerName    string
				AllowInsecure bool
			} `json:"tlsSettings"`
			Hy struct {
				Auth string
			} `json:"hysteriaSettings"`
			Mask struct {
				UDP []struct {
					Type     string
					Settings struct{ Password string }
				}
			} `json:"finalmask"`
		} `json:"streamSettings"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.Protocol != "hysteria" || out.Settings.Address != "2001:db8::7" || out.Settings.Port != 443 || out.Stream.Hy.Auth != s.Hy2Password(u.UUID) {
		t.Errorf("outbound: %s", raw)
	}
	if out.Stream.TLS.ServerName != "vpn.example.com" || out.Stream.TLS.AllowInsecure {
		t.Errorf("tls: %s", raw)
	}
	if m := out.Stream.Mask.UDP; len(m) != 1 || m[0].Type != "salamander" || m[0].Settings.Password != "pa ss&word+1" {
		t.Errorf("salamander: %s", raw)
	}
	s.Hysteria2.Obfs = false
	if l := Hy2Links(s, "203.0.113.7", "vpn.example.com", true, u); strings.Contains(l[0].URL, "obfs") {
		t.Errorf("salamander off, link %s", l[0].URL)
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
		func(s *Settings) { s.Hysteria2.Port = 80 },
		func(s *Settings) { s.TUIC.Port = 443 }, // Hysteria 2 has UDP 443
		func(s *Settings) { s.Hysteria2.ObfsPassword = "short" },
		func(s *Settings) { s.Hysteria2.BBRProfile = "fast" },
		func(s *Settings) { s.Hysteria2.UpMbps = 100 },
		func(s *Settings) { s.Hysteria2.UpMbps, s.Hysteria2.DownMbps = -1, -1 },
		func(s *Settings) {
			s.Hysteria2.UpMbps, s.Hysteria2.DownMbps, s.Hysteria2.IgnoreClientBandwidth = 100, 100, true
		},
	} {
		c := defaults(t)
		mut(&c)
		if c.Validate() == nil {
			t.Errorf("accepted %+v %+v", c.TUIC, c.Hysteria2)
		}
	}
	// TUIC may take UDP 443 once Hysteria 2 moves away or is off.
	s.TUIC.Port = 443
	s.Hysteria2.Enabled = false
	if err := s.Validate(); err != nil {
		t.Error(err)
	}
	s.Hysteria2.Enabled, s.Hysteria2.Port = true, 8444
	if err := s.Validate(); err != nil {
		t.Error(err)
	}
	if s.Hy2Password(users()[0].UUID) == a {
		t.Error("TUIC and Hysteria 2 share a password")
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
	if !a.Hysteria2.Enabled || a.Hysteria2.Port != 443 || !a.Hysteria2.Obfs || a.Hysteria2.ObfsPassword == "" || a.Hysteria2.ObfsPassword != b.Hysteria2.ObfsPassword {
		t.Fatalf("hysteria2: %+v %+v", a.Hysteria2, b.Hysteria2)
	}
}

// Settings stored before Hysteria 2 existed get it switched on, with the
// same Salamander password in every process that loads them.
func TestLoadAddsHysteria2(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	old := `{"secret":"` + strings.Repeat("s", 43) + `","tuic":{"enabled":true,"port":8443,"congestion_control":"bbr","udp_relay_mode":"native","heartbeat":10,"auth_timeout":3}}`
	if err := st.SetSettings(ctx, map[string]string{Key: old}); err != nil {
		t.Fatal(err)
	}
	a, err := Load(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Load(ctx, st)
	h := a.Hysteria2
	if !h.Enabled || h.Port != 443 || !h.Obfs || h.BBRProfile != "standard" || h.ObfsPassword == "" || h.ObfsPassword != b.Hysteria2.ObfsPassword {
		t.Fatalf("%+v", h)
	}
	if err := a.Validate(); err != nil {
		t.Fatal(err)
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
