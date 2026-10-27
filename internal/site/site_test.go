package site

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stilltiger45/TigerSB-panel/internal/config"
	"github.com/Stilltiger45/TigerSB-panel/internal/store"
)

func valid() Settings {
	return Settings{Domain: "vpn.example.com", Email: "me@example.com", Template: "bakery", Name: "Пекарня"}
}

func TestValidate(t *testing.T) {
	bad := []func(*Settings){
		func(s *Settings) { s.Domain = "1.2.3.4" },
		func(s *Settings) { s.Domain = "example.com {\n respond hi" },
		func(s *Settings) { s.Domain = "localhost" },
		func(s *Settings) { s.Email = "me@example.com\n}" },
		func(s *Settings) { s.Email = "nope" },
		func(s *Settings) { s.Template = "vpn" },
		func(s *Settings) { s.Name = "" },
		func(s *Settings) { s.Name = strings.Repeat("я", 61) },
		func(s *Settings) { s.Name = "a\nb" },
	}
	for i, mut := range bad {
		s := valid()
		mut(&s)
		if s.Validate() == nil {
			t.Errorf("case %d: %+v accepted", i, s)
		}
	}
	s := valid()
	s.Domain, s.Email = "", ""
	if err := s.Validate(); err != nil {
		t.Fatalf("empty domain and email must be allowed: %v", err)
	}
}

func TestNormalize(t *testing.T) {
	s := Settings{Domain: "  VPN.Example.COM. ", Email: " a@b.c ", Name: " x "}
	s.Normalize()
	if s.Domain != "vpn.example.com" || s.Email != "a@b.c" || s.Name != "x" {
		t.Fatalf("got %+v", s)
	}
}

func TestCaddyfile(t *testing.T) {
	p := Paths{Dir: "/d/caddy", WWW: "/d/www", Socket: "/run/x.sock"}
	got := RenderCaddyfile(valid(), p, nil)
	for _, want := range []string{
		"admin unix//run/x.sock",
		"storage file_system /d/caddy",
		"auto_https disable_redirects",
		"vpn.example.com {",
		"email me@example.com",
		"disable_http_challenge",
		"alt_tlsalpn_port 8444",
		"https_port 8444",
		"default_bind 127.0.0.1",
		"header Cache-Control no-cache",
		"root * /d/www",
		"try_files {path} /index.html",
		"-Server",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Caddyfile lacks %q:\n%s", want, got)
		}
	}
	s := valid()
	s.Domain, s.Email = "", ""
	got = RenderCaddyfile(s, p, nil)
	if !strings.Contains(got, ":80 {") || strings.Contains(got, "tls") || strings.Contains(got, "default_bind") {
		t.Errorf("no-domain Caddyfile must serve plain HTTP:\n%s", got)
	}
}

func TestStubsRenderAndEscape(t *testing.T) {
	now := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	for _, tpl := range Templates {
		s := Settings{Template: tpl.ID, Name: `<script>alert(1)</script>`}
		page, err := RenderStub(s, now)
		if err != nil {
			t.Fatalf("%s: %v", tpl.ID, err)
		}
		html := string(page)
		if strings.Contains(html, "<script>") {
			t.Errorf("%s: name is not escaped", tpl.ID)
		}
		if !strings.Contains(html, "2026") {
			t.Errorf("%s: no year", tpl.ID)
		}
		for _, word := range []string{"VPN", "впн", "Xray", "прокси", "TigerSB"} {
			if strings.Contains(strings.ToLower(html), strings.ToLower(word)) {
				t.Errorf("%s: page mentions %q", tpl.ID, word)
			}
		}
		if len(page) > 8<<10 {
			t.Errorf("%s: page is %d bytes, keep it light", tpl.ID, len(page))
		}
	}
}

func TestLoadSeedsFromConfigAndStaysStable(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	cfg := &config.Config{Domain: "Vpn.Example.com", Email: "a@b.cd"}
	first, err := Load(ctx, st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if first.Domain != "vpn.example.com" || first.Email != "a@b.cd" || first.Name == "" {
		t.Fatalf("seed: %+v", first)
	}
	cfg.Domain = "other.example.com"
	second, err := Load(ctx, st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("settings changed between loads: %+v vs %+v", first, second)
	}
}

func TestApplyWithoutCaddy(t *testing.T) {
	dir := t.TempDir()
	p := Paths{Dir: filepath.Join(dir, "caddy"), WWW: filepath.Join(dir, "www"), Socket: filepath.Join(dir, "none.sock")}
	err := Apply(context.Background(), valid(), p, nil, time.Now())
	if !errors.Is(err, ErrCaddyDown) {
		t.Fatalf("want ErrCaddyDown, got %v", err)
	}
	for _, f := range []string{p.Caddyfile(), filepath.Join(p.WWW, "index.html")} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("%s not written: %v", f, err)
		}
	}
}

func TestApplyLoadsIntoCaddy(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "admin.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("unix sockets unavailable: %v", err)
	}
	var gotType, gotBody string
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/load" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		b, _ := io.ReadAll(r.Body)
		gotType, gotBody = r.Header.Get("Content-Type"), string(b)
		if strings.Contains(gotBody, "broken") {
			http.Error(w, `{"error":"adapting config: broken"}`, http.StatusBadRequest)
		}
	})}
	go srv.Serve(l)
	defer srv.Close()

	p := Paths{Dir: filepath.Join(dir, "caddy"), WWW: filepath.Join(dir, "www"), Socket: sock}
	if err := Apply(context.Background(), valid(), p, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if gotType != "text/caddyfile" || !strings.Contains(gotBody, "vpn.example.com {") {
		t.Fatalf("Caddy got %q:\n%s", gotType, gotBody)
	}
	s := valid()
	s.Name = "broken"
	// The name is not in the Caddyfile, so use the domain to trigger the fake error.
	s.Domain = "broken.example.com"
	if err := Apply(context.Background(), s, p, nil, time.Now()); err == nil || !strings.Contains(err.Error(), "не принял") {
		t.Fatalf("want rejection, got %v", err)
	}
}

func TestCheckCert(t *testing.T) {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	defer srv.Close()
	addr := srv.Listener.Addr().String()
	now := time.Now()

	// httptest's certificate is valid for example.com and signed by its own CA.
	if st := CheckCert(context.Background(), "example.com", addr, x509.NewCertPool(), now); st.State != CertUntrusted {
		t.Fatalf("unknown CA: %+v", st)
	}
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	if st := CheckCert(context.Background(), "example.com", addr, roots, now); st.State != CertOK || st.DaysLeft < 14 {
		t.Fatalf("trusted: %+v", st)
	}
	if st := CheckCert(context.Background(), "example.com", addr, roots, srv.Certificate().NotAfter.Add(-72*time.Hour)); st.State != CertExpiring {
		t.Fatalf("expiring: %+v", st)
	}
	if st := CheckCert(context.Background(), "example.com", "127.0.0.1:1", roots, now); st.State != CertMissing {
		t.Fatalf("closed port: %+v", st)
	}
	if st := CheckCert(context.Background(), "", addr, roots, now); st.State != CertNoDomain {
		t.Fatalf("no domain: %+v", st)
	}
}

func TestCaddyfilePanelRoute(t *testing.T) {
	p := Paths{Dir: "/d/caddy", WWW: "/d/www", Socket: "/run/x.sock"}
	cfg := &config.Config{SecretPath: "0123456789abcdef0123456789abcdef", Listen: "127.0.0.1:34567"}
	if PanelRoute(cfg, false) != nil {
		t.Fatal("closed panel must have no route")
	}
	r := PanelRoute(cfg, true)
	got := RenderCaddyfile(valid(), p, r)
	for _, want := range []string{
		"@tigersb path /0123456789abcdef0123456789abcdef /0123456789abcdef0123456789abcdef/*",
		"handle @tigersb {\n\t\treverse_proxy 127.0.0.1:34567\n\t}",
		// The stub must not stay in the browser cache, or the secret path
		// shows it after open; the panel sets its own no-store.
		"\thandle {\n\t\theader Cache-Control no-cache\n\t\troot * /d/www",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Caddyfile lacks %q:\n%s", want, got)
		}
	}
	closed := RenderCaddyfile(valid(), p, nil)
	if strings.Contains(closed, "reverse_proxy 127.0.0.1:34567") || strings.Contains(closed, "@tigersb") {
		t.Errorf("closed panel is still proxied:\n%s", closed)
	}
	// Subscriptions work whether the panel is open or closed, and a miss
	// (404 from tigersb-sub) or a service that is down shows the stub.
	for _, conf := range []string{got, closed} {
		for _, want := range []string{
			"handle /sub/* {\n\t\treverse_proxy 127.0.0.1:10086 {\n\t\t\t@miss status 404\n\t\t\thandle_response @miss {",
			"rewrite * /index.html",
			"\thandle_errors {",
			"status 200",
			"proxy_protocol {\n\t\t\t\tallow 127.0.0.1/32",
			"protocols h1 h2",
			"-Via",
		} {
			if !strings.Contains(conf, want) {
				t.Errorf("Caddyfile lacks %q:\n%s", want, conf)
			}
		}
	}
	// Without a domain the panel would be served over plain HTTP, so never publish it.
	s := valid()
	s.Domain = ""
	if plain := RenderCaddyfile(s, p, r); strings.Contains(plain, "reverse_proxy") || strings.Contains(plain, "proxy_protocol") {
		t.Errorf("panel published without TLS:\n%s", plain)
	}
	if u := PanelURL(valid(), cfg); u != "https://vpn.example.com/0123456789abcdef0123456789abcdef/" {
		t.Errorf("url: %q", u)
	}
	if u := PanelURL(s, cfg); u != "" {
		t.Errorf("url without domain: %q", u)
	}
}

func TestPanelOpenDefaultsToOpen(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	for _, step := range []struct {
		set  *bool
		want bool
	}{{nil, true}, {ptr(false), false}, {ptr(true), true}} {
		if step.set != nil {
			if err := SetPanelOpen(ctx, st, *step.set); err != nil {
				t.Fatal(err)
			}
		}
		if open, err := PanelOpen(ctx, st); err != nil || open != step.want {
			t.Fatalf("after %v: open=%v err=%v", step.set, open, err)
		}
	}
}

func ptr(b bool) *bool { return &b }

func TestApplyWithStaleSocket(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "admin.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("unix sockets unavailable: %v", err)
	}
	// A killed Caddy leaves its socket file behind.
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	p := Paths{Dir: filepath.Join(dir, "caddy"), WWW: filepath.Join(dir, "www"), Socket: sock}
	if err := Apply(context.Background(), valid(), p, nil, time.Now()); !errors.Is(err, ErrCaddyDown) {
		t.Fatalf("want ErrCaddyDown, got %v", err)
	}
	if _, err := os.Stat(p.Caddyfile()); err != nil {
		t.Fatalf("Caddyfile not written: %v", err)
	}
}
