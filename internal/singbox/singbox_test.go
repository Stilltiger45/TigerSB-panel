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
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stilltiger45/TigerSB-client/pkg/profile"

	"github.com/Stilltiger45/TigerSB-panel/internal/store"
	"github.com/Stilltiger45/TigerSB-panel/internal/xray"
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
	if len(conf.Inbounds) != 3 {
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

// A test link works on its own protocol only.
func TestRenderTestLink(t *testing.T) {
	s := defaults(t)
	us := append(users(), store.User{UUID: "33333333-3333-4333-8333-333333333333", Enabled: true, Only: TUICTag})
	raw, err := Render(s, us, "vpn.example.com", true)
	if err != nil {
		t.Fatal(err)
	}
	var conf struct {
		Inbounds []struct {
			Tag   string
			Users []struct{ Name string }
		}
	}
	if err := json.Unmarshal(raw, &conf); err != nil {
		t.Fatal(err)
	}
	for _, in := range conf.Inbounds {
		has := strings.Contains(fmt.Sprint(in.Users), "test-tuic")
		if has != (in.Tag == TUICTag) {
			t.Fatalf("%s: %v", in.Tag, in.Users)
		}
	}
}

func TestRenderNaive(t *testing.T) {
	s := defaults(t)
	type inbound struct {
		Type, Network, Listen string
		Tag                   string
		ListenPort            int `json:"listen_port"`
		Users                 []struct{ Username, Password string }
		TLS                   struct{ ALPN []string }
		CC                    string `json:"quic_congestion_control"`
	}
	render := func() []inbound {
		t.Helper()
		raw, err := Render(s, users(), "vpn.example.com", true)
		if err != nil {
			t.Fatal(err)
		}
		var conf struct{ Inbounds []inbound }
		if err := json.Unmarshal(raw, &conf); err != nil {
			t.Fatal(err)
		}
		return conf.Inbounds[2:]
	}
	// TCP listens behind the edge, which holds the public port.
	in := render()
	if len(in) != 1 || in[0].Type != "naive" || in[0].Network != "tcp" || in[0].Listen != "127.0.0.1" ||
		in[0].ListenPort != xray.NaiveInner || fmt.Sprint(in[0].TLS.ALPN) != "[h2 http/1.1]" {
		t.Fatalf("inbound: %+v", in)
	}
	if len(in[0].Users) != 1 || in[0].Users[0].Username != "u1" || in[0].Users[0].Password != s.NaivePassword(users()[0].UUID) {
		t.Fatalf("users: %+v", in[0].Users)
	}
	f := Forwards(s, "vpn.example.com", true)
	if len(f) != 1 || f[0].Listen != ":2443" || f[0].Addr != "127.0.0.1:10443" || f[0].Congestion != "" || f[0].Proxy {
		t.Fatalf("forwards: %+v", f)
	}
	if Forwards(s, "", false) != nil {
		t.Fatal("no forward without the certificate")
	}

	// Both: QUIC listens on the public UDP port itself, BBR goes to both.
	s.Naive.Network, s.Naive.BBR = NaiveBoth, true
	in = render()
	if len(in) != 2 || in[1].Network != "udp" || in[1].Listen != "::" || in[1].ListenPort != 2443 ||
		in[1].Tag != "naive-quic" || in[1].CC != "bbr" || fmt.Sprint(in[1].TLS.ALPN) != "[h3]" || len(in[1].Users) != 1 {
		t.Fatalf("inbounds: %+v", in)
	}
	if f := Forwards(s, "vpn.example.com", true); len(f) != 1 || f[0].Congestion != "bbr" {
		t.Fatalf("forwards: %+v", f)
	}
	links := NaiveLinks(s, "203.0.113.5", "vpn.example.com", true, users()[0])
	if len(links) != 2 || !strings.HasPrefix(links[0].URL, "naive+https://u1:") || !strings.HasPrefix(links[1].URL, "naive+quic://u1:") ||
		!strings.Contains(links[1].URL, "@203.0.113.5:2443?sni=vpn.example.com&padding=true#") || !IsNaiveQUIC(links[1]) || IsNaiveQUIC(links[0]) {
		t.Fatalf("links: %+v", links)
	}

	// QUIC only: no TCP, nothing for the edge, CUBIC without BBR.
	s.Naive.Network, s.Naive.BBR = NaiveQUIC, false
	if in = render(); len(in) != 1 || in[0].Network != "udp" || in[0].CC != "cubic" {
		t.Fatalf("inbounds: %+v", in)
	}
	if Forwards(s, "vpn.example.com", true) != nil {
		t.Fatal("QUIC needs no forward")
	}
	s.Naive.Port = s.TUIC.Port
	if err := s.Validate(); err == nil {
		t.Fatal("NaïveProxy QUIC on the TUIC port must be refused")
	}
	s.Naive.Port, s.Naive.Network = 2443, "http3"
	if err := s.Validate(); err == nil {
		t.Fatal("unknown network must be refused")
	}
	s.Naive.Network = NaiveTCP
	s.Naive.Port = 443
	if err := s.Validate(); err == nil {
		t.Fatal("NaïveProxy on TCP 443 must be refused")
	}
}

func TestRenderMieru(t *testing.T) {
	s := defaults(t)
	type inbound struct {
		Tag, Transport string
		ListenPort     int `json:"listen_port"`
		Users          []struct{ Name, Password string }
	}
	render := func() []inbound {
		t.Helper()
		raw, err := RenderMieru(s, users())
		if err != nil {
			t.Fatal(err)
		}
		var conf struct{ Inbounds []inbound }
		if err := json.Unmarshal(raw, &conf); err != nil {
			t.Fatal(err)
		}
		return conf.Inbounds
	}
	// TCP: an inbound on each port of 2012-2022 with the enabled user only.
	in := render()
	if len(in) != 11 || in[0].Tag != "mieru-tcp-2012" || in[0].Transport != "TCP" || in[10].ListenPort != 2022 {
		t.Fatalf("inbounds: %+v", in)
	}
	if len(in[0].Users) != 1 || in[0].Users[0].Name != "u1" || in[0].Users[0].Password != s.MieruPassword(users()[0].UUID) {
		t.Fatalf("users: %+v", in[0].Users)
	}
	if l := MieruLinks(s, "203.0.113.5", users()[0]); len(l) != 1 || !strings.HasSuffix(l[0].Title, "· Mieru") || IsMieruUDP(l[0]) ||
		!strings.Contains(l[0].URL, "@203.0.113.5?profile=TigerSB&port=2012-2022&protocol=TCP#") {
		t.Fatalf("links: %+v", l)
	}

	// Both: TCP and UDP on every port, and a link for each.
	s.Mieru.Transport = MieruBoth
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	in = render()
	if len(in) != 22 || in[11].Tag != "mieru-udp-2012" || in[11].Transport != "UDP" || in[11].ListenPort != 2012 || len(in[11].Users) != 1 {
		t.Fatalf("inbounds: %+v", in)
	}
	links := MieruLinks(s, "203.0.113.5", users()[0])
	if len(links) != 2 || IsMieruUDP(links[0]) || !IsMieruUDP(links[1]) || !strings.HasSuffix(links[1].Title, "· Mieru UDP") {
		t.Fatalf("links: %+v", links)
	}
	for i, l := range links {
		out, _, err := profile.ParseLink(l.URL)
		if err != nil || !strings.Contains(string(out), `"protocol":"`+[]string{"TCP", "UDP"}[i]+`"`) {
			t.Fatalf("the app does not read %s: %v %s", l.URL, err, out)
		}
	}
	if s.Mieru.Label() != "tcp и udp" {
		t.Fatalf("label %q", s.Mieru.Label())
	}

	// Each side of both must not share ports: UDP with TUIC, TCP with NaïveProxy.
	s.TUIC.Port = 2015
	if err := s.Validate(); err == nil {
		t.Fatal("Mieru UDP and TUIC on one port must be refused")
	}
	s.TUIC.Port = 443
	s.Naive.Port = 2016
	if err := s.Validate(); err == nil {
		t.Fatal("Mieru TCP and NaïveProxy on one port must be refused")
	}
	s.Naive.Port, s.Mieru.Transport = 2443, "SCTP"
	if err := s.Validate(); err == nil {
		t.Fatal("unknown transport must be refused")
	}
}
