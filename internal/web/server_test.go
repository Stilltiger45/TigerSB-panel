package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/Stilltiger45/TigerSB-panel/internal/auth"
	"github.com/Stilltiger45/TigerSB-panel/internal/config"
	"github.com/Stilltiger45/TigerSB-panel/internal/site"
	"github.com/Stilltiger45/TigerSB-panel/internal/store"
)

const secret = "0123456789abcdef0123456789abcdef"

func newTestServer(t *testing.T) *Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	hash, err := auth.HashPassword("very-secret-pass")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertAdmin(context.Background(), "admin", hash); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Role: config.RoleMain, Listen: "127.0.0.1:30000", SecretPath: secret, DBPath: "unused", Domain: "example.com"}
	assets := fstest.MapFS{
		"login.html":     {Data: []byte("LOGIN PAGE")},
		"app.html":       {Data: []byte("APP PAGE")},
		"static/app.css": {Data: []byte("body{}")},
	}
	s := New(cfg, st, assets, "test")
	dir := t.TempDir()
	s.Site = site.Paths{Dir: filepath.Join(dir, "caddy"), WWW: filepath.Join(dir, "www"), Socket: filepath.Join(dir, "none.sock")}
	s.CertAddr = "127.0.0.1:1"
	s.Run = func(string, ...string) (string, error) { return "inactive", errors.New("exit 3") }
	return s
}

func do(s *Server, method, path, body string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func login(t *testing.T, s *Server, user, pass string) *httptest.ResponseRecorder {
	t.Helper()
	return do(s, http.MethodPost, "/"+secret+"/api/login", `{"username":"`+user+`","password":"`+pass+`"}`)
}

func TestOutsideSecretPathIs404(t *testing.T) {
	s := newTestServer(t)
	for _, p := range []string{"/", "/login", "/api/login", "/0123456789abcdef0123456789abcdee/", "/" + secret + "x/"} {
		if w := do(s, http.MethodGet, p, ""); w.Code != http.StatusNotFound {
			t.Errorf("%s: want 404, got %d", p, w.Code)
		}
	}
	if w := do(s, http.MethodGet, "/"+secret+"/unknown", ""); w.Code != http.StatusNotFound {
		t.Errorf("unknown path inside prefix: want 404, got %d", w.Code)
	}
}

func TestLoginFlow(t *testing.T) {
	s := newTestServer(t)
	w := do(s, http.MethodGet, "/"+secret+"/", "")
	if w.Code != http.StatusOK || w.Body.String() != "LOGIN PAGE" {
		t.Fatalf("anonymous index: %d %q", w.Code, w.Body.String())
	}
	if w := do(s, http.MethodGet, "/"+secret+"/api/me", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("me without session: %d", w.Code)
	}

	w = login(t, s, "admin", "very-secret-pass")
	if w.Code != http.StatusOK {
		t.Fatalf("login: %d %s", w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("want 1 cookie, got %d", len(cookies))
	}
	c := cookies[0]
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/"+secret+"/" {
		t.Fatalf("weak cookie: %+v", c)
	}

	if w := do(s, http.MethodGet, "/"+secret+"/", "", c); w.Body.String() != "APP PAGE" {
		t.Fatalf("logged-in index: %q", w.Body.String())
	}
	w = do(s, http.MethodGet, "/"+secret+"/api/me", "", c)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"role":"main"`) {
		t.Fatalf("me: %d %s", w.Code, w.Body.String())
	}

	if w := do(s, http.MethodPost, "/"+secret+"/api/logout", "{}", c); w.Code != http.StatusNoContent {
		t.Fatalf("logout: %d", w.Code)
	}
	if w := do(s, http.MethodGet, "/"+secret+"/api/me", "", c); w.Code != http.StatusUnauthorized {
		t.Fatalf("session must be gone after logout: %d", w.Code)
	}
}

func TestWrongPasswordAndLockout(t *testing.T) {
	s := newTestServer(t)
	for i := 0; i < 5; i++ {
		if w := login(t, s, "admin", "nope"); w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: want 401, got %d", i, w.Code)
		}
	}
	w := login(t, s, "admin", "very-secret-pass")
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Fatalf("want 429 with Retry-After after 5 failures, got %d", w.Code)
	}
	if w := login(t, s, "nobody", "x"); w.Code != http.StatusUnauthorized {
		t.Fatalf("unknown user: want 401, got %d", w.Code)
	}
}

func TestPasswordChangeEndsSessions(t *testing.T) {
	s := newTestServer(t)
	c := login(t, s, "admin", "very-secret-pass").Result().Cookies()[0]
	hash, _ := auth.HashPassword("another-secret-pass")
	if err := s.store.UpsertAdmin(context.Background(), "admin", hash); err != nil {
		t.Fatal(err)
	}
	if w := do(s, http.MethodGet, "/"+secret+"/api/me", "", c); w.Code != http.StatusUnauthorized {
		t.Fatalf("old session must die after password change: %d", w.Code)
	}
}

func TestPostGuards(t *testing.T) {
	s := newTestServer(t)
	r := httptest.NewRequest(http.MethodPost, "/"+secret+"/api/login", strings.NewReader("username=a"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("form post: want 415, got %d", w.Code)
	}

	r = httptest.NewRequest(http.MethodPost, "/"+secret+"/api/login", strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "https://evil.example")
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin post: want 403, got %d", w.Code)
	}

	if w := do(s, http.MethodGet, "/"+secret+"/api/login", ""); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET login: want 405, got %d", w.Code)
	}
}

func TestSecurityHeadersAndStatic(t *testing.T) {
	s := newTestServer(t)
	w := do(s, http.MethodGet, "/"+secret+"/static/app.css", "")
	if w.Code != http.StatusOK || w.Body.String() != "body{}" {
		t.Fatalf("static: %d %q", w.Code, w.Body.String())
	}
	for _, h := range []string{"Content-Security-Policy", "X-Frame-Options", "X-Content-Type-Options"} {
		if w.Header().Get(h) == "" {
			t.Errorf("missing %s", h)
		}
	}
	if w := do(s, http.MethodGet, "/"+secret+"/static/../app.html", ""); w.Code == http.StatusOK && strings.Contains(w.Body.String(), "APP") {
		t.Fatal("path traversal served app.html")
	}
}

func TestSiteSettings(t *testing.T) {
	s := newTestServer(t)
	api := "/" + secret + "/api/site"
	if w := do(s, http.MethodGet, api, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", w.Code)
	}
	c := login(t, s, "admin", "very-secret-pass").Result().Cookies()[0]

	w := do(s, http.MethodGet, api, "", c)
	var got struct {
		Settings  site.Settings
		Templates []site.Template
		Cert      site.CertStatus
		Caddy     string
		Warning   string
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusOK {
		t.Fatalf("get: %d %s", w.Code, w.Body.String())
	}
	if got.Settings.Domain != "example.com" || got.Settings.Template == "" || len(got.Templates) == 0 {
		t.Fatalf("seeded settings: %+v", got)
	}
	if got.Cert.State != site.CertMissing || got.Caddy != "inactive" {
		t.Fatalf("status: %+v %q", got.Cert, got.Caddy)
	}

	if w := do(s, http.MethodPost, api, `{"domain":"1.2.3.4","template":"books","name":"x"}`, c); w.Code != http.StatusBadRequest {
		t.Fatalf("IP as domain: %d %s", w.Code, w.Body.String())
	}
	w = do(s, http.MethodPost, api, `{"domain":"Site.Example.org","email":"a@b.cd","template":"books","name":"Клуб"}`, c)
	if w.Code != http.StatusOK {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	json.Unmarshal(w.Body.Bytes(), &got)
	if got.Settings.Domain != "site.example.org" || got.Warning == "" {
		t.Fatalf("saved: %+v", got)
	}
	conf, err := os.ReadFile(s.Site.Caddyfile())
	if err != nil || !strings.Contains(string(conf), "site.example.org {") {
		t.Fatalf("Caddyfile: %v\n%s", err, conf)
	}
	page, err := os.ReadFile(filepath.Join(s.Site.WWW, "index.html"))
	if err != nil || !strings.Contains(string(page), "Клуб") {
		t.Fatalf("page: %v", err)
	}
}

func TestStubPreview(t *testing.T) {
	s := newTestServer(t)
	path := "/" + secret + "/stub/preview?t=garden&name=%3Cb%3Ehi"
	if w := do(s, http.MethodGet, path, ""); w.Code != http.StatusNotFound {
		t.Fatalf("anonymous preview: %d", w.Code)
	}
	c := login(t, s, "admin", "very-secret-pass").Result().Cookies()[0]
	w := do(s, http.MethodGet, path, "", c)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "<b>hi") {
		t.Fatalf("preview: %d", w.Code)
	}
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Fatalf("preview CSP: %q", csp)
	}
	if w := do(s, http.MethodGet, "/"+secret+"/stub/preview?t=nope", "", c); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown template: %d", w.Code)
	}
}

func TestSecureCookieBehindCaddy(t *testing.T) {
	s := newTestServer(t)
	r := httptest.NewRequest(http.MethodPost, "/"+secret+"/api/login", strings.NewReader(`{"username":"admin","password":"very-secret-pass"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Forwarded-Proto", "https")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if c := w.Result().Cookies(); w.Code != http.StatusOK || len(c) != 1 || !c[0].Secure {
		t.Fatalf("login via https: %d %+v", w.Code, c)
	}
	if c := login(t, s, "admin", "very-secret-pass").Result().Cookies(); c[0].Secure {
		t.Fatal("an SSH-tunnel login over plain http must still work")
	}
}

func TestClosePanel(t *testing.T) {
	s := newTestServer(t)
	c := login(t, s, "admin", "very-secret-pass").Result().Cookies()[0]
	me := "/" + secret + "/api/me"
	if w := do(s, http.MethodGet, me, "", c); !strings.Contains(w.Body.String(), `"url":"https://example.com/`+secret+`/"`) || !strings.Contains(w.Body.String(), `"open":true`) {
		t.Fatalf("me before close: %s", w.Body.String())
	}
	// Publish the panel first, so the close has a route to take away.
	if w := do(s, http.MethodPost, "/"+secret+"/api/site", `{"domain":"example.com","template":"books","name":"Клуб"}`, c); w.Code != http.StatusOK {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	if conf, _ := os.ReadFile(s.Site.Caddyfile()); !strings.Contains(string(conf), "reverse_proxy 127.0.0.1:30000") {
		t.Fatalf("open panel is not routed:\n%s", conf)
	}

	if w := do(s, http.MethodPost, "/"+secret+"/api/panel/close", "", c); w.Code != http.StatusBadRequest && w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("close without JSON must be refused: %d", w.Code)
	}
	if w := do(s, http.MethodPost, "/"+secret+"/api/panel/close", "{}", c); w.Code != http.StatusNoContent {
		t.Fatalf("close: %d %s", w.Code, w.Body.String())
	}
	if conf, _ := os.ReadFile(s.Site.Caddyfile()); strings.Contains(string(conf), "reverse_proxy") {
		t.Fatalf("closed panel is still routed:\n%s", conf)
	}
	if open, err := site.PanelOpen(context.Background(), s.store); err != nil || open {
		t.Fatalf("panel.open after close: %v %v", open, err)
	}
	if w := do(s, http.MethodGet, me, "", c); !strings.Contains(w.Body.String(), `"open":false`) || !strings.Contains(w.Body.String(), `"url":"https://`) {
		t.Fatalf("me after close: %s", w.Body.String())
	}
}
