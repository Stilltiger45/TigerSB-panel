// Package web serves the panel UI and API on the loopback interface.
//
// The panel is never published by Caddy: an admin reaches it only through an
// SSH tunnel to 127.0.0.1, and even then every path outside the secret prefix
// answers 404.
package web

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Stilltiger45/TigerSB-panel/internal/auth"
	"github.com/Stilltiger45/TigerSB-panel/internal/config"
	"github.com/Stilltiger45/TigerSB-panel/internal/health"
	"github.com/Stilltiger45/TigerSB-panel/internal/service"
	"github.com/Stilltiger45/TigerSB-panel/internal/singbox"
	"github.com/Stilltiger45/TigerSB-panel/internal/site"
	"github.com/Stilltiger45/TigerSB-panel/internal/store"
	"github.com/Stilltiger45/TigerSB-panel/internal/update"
	"github.com/Stilltiger45/TigerSB-panel/internal/xray"
)

const (
	cookieName = "tsb_session"
	sessionTTL = 12 * time.Hour
	maxBody    = 4 << 10
)

// Server is the panel HTTP handler.
type Server struct {
	cfg     *config.Config
	store   *store.Store
	assets  fs.FS
	prefix  string
	byUser  *auth.Limiter
	global  *auth.Limiter
	version string
	now     func() time.Time

	// Site is where the Caddyfile and maintenance page live.
	Site site.Paths
	// CertAddr is where Caddy serves TLS; the certificate is checked there.
	CertAddr string
	// Run executes systemctl; tests replace it.
	Run service.Runner

	// Updater runs updates and keeps restore points; nil turns them off.
	Updater *update.Updater
	// GitHub is the client for update checks.
	GitHub *http.Client
	// HealthBase holds probe targets for the checks; tests replace them.
	HealthBase health.Env

	// Xray is the VPN core in Docker and the edge router on 443.
	Xray *xray.Runtime
	// Singbox is the second core, with TUIC and Hysteria 2.
	Singbox *singbox.Runtime
	// LocalIPs lists the server's addresses, for links.
	LocalIPs func() ([]net.IP, error)
	// xrayMu keeps user and protocol changes in order.
	xrayMu sync.Mutex

	// loaded receives the result of each background Caddy load in tests.
	loaded chan error

	dummyOnce sync.Once
	dummyHash string
}

// New builds the handler. assets holds login.html, app.html and static files.
func New(cfg *config.Config, st *store.Store, assets fs.FS, version string) *Server {
	return &Server{
		cfg:     cfg,
		store:   st,
		assets:  assets,
		prefix:  "/" + cfg.SecretPath,
		byUser:  auth.NewLimiter(5, 15*time.Minute, 15*time.Minute),
		global:  auth.NewLimiter(30, 15*time.Minute, 15*time.Minute),
		version: version,
		now:     time.Now,

		Site:     site.DefaultPaths(filepath.Dir(cfg.DBPath)),
		CertAddr: site.LocalTLS,
		Run:      service.ExecRunner,
		GitHub:   &http.Client{Timeout: 20 * time.Second},
		Xray:     xray.DefaultRuntime(filepath.Dir(cfg.DBPath)),
		Singbox:  singbox.DefaultRuntime(filepath.Dir(cfg.DBPath)),
		LocalIPs: xray.LocalIPs,
	}
}

// ServeHTTP routes requests; anything outside the secret prefix is 404.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	path := r.URL.Path
	if path == s.prefix {
		http.Redirect(w, r, s.prefix+"/", http.StatusFound)
		return
	}
	if !strings.HasPrefix(path, s.prefix+"/") {
		http.NotFound(w, r)
		return
	}
	rest := strings.TrimPrefix(path, s.prefix)
	switch {
	case rest == "/":
		s.handleIndex(w, r)
	case rest == "/api/login":
		s.handleLogin(w, r)
	case rest == "/api/logout":
		s.handleLogout(w, r)
	case rest == "/api/me":
		s.handleMe(w, r)
	case rest == "/api/site":
		s.handleSite(w, r)
	case rest == "/api/panel/close":
		s.handleClose(w, r)
	case rest == "/api/control":
		s.handleControl(w, r)
	case rest == "/api/control/health":
		s.handleHealth(w, r)
	case rest == "/api/control/core":
		s.handleCoreRestart(w, r)
	case rest == "/api/control/job":
		s.handleJob(w, r)
	case rest == "/api/control/check":
		s.handleCheck(w, r)
	case rest == "/api/control/update":
		s.handleUpdate(w, r)
	case rest == "/api/control/restore":
		s.handleRestore(w, r)
	case rest == "/api/users":
		s.handleUsers(w, r)
	case rest == "/api/users/add":
		s.handleUserAdd(w, r)
	case rest == "/api/users/update":
		s.handleUserUpdate(w, r)
	case rest == "/api/users/delete":
		s.handleUserDelete(w, r)
	case rest == "/api/users/device/delete":
		s.handleDeviceDelete(w, r)
	case rest == "/api/users/qr":
		s.handleUserQR(w, r)
	case rest == "/api/protocols":
		s.handleProtocols(w, r)
	case rest == "/api/protocols/keys":
		s.handleKeys(w, r)
	case rest == "/api/protocols/check":
		s.handleSNICheck(w, r)
	case rest == "/api/protocols/scan":
		s.handleScan(w, r)
	case rest == "/api/protocols/test":
		s.handleTestLink(w, r)
	case rest == "/api/protocols/test/qr":
		s.handleTestQR(w, r)
	case rest == "/stub/preview":
		s.handleStubPreview(w, r)
	case strings.HasPrefix(rest, "/static/"):
		s.handleStatic(w, r, strings.TrimPrefix(rest, "/"))
	default:
		http.NotFound(w, r)
	}
}

func setSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; form-action 'self'; base-uri 'none'")
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	page := "login.html"
	if _, err := s.currentAdmin(r); err == nil {
		page = "app.html"
	}
	data, err := fs.ReadFile(s.assets, page)
	if err != nil {
		http.Error(w, "внутренняя ошибка", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	http.ServeFileFS(w, r, s.assets, name)
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.checkPost(w, r) {
		return
	}
	var req loginRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "некорректный запрос"})
		return
	}
	userKey := "u:" + strings.ToLower(req.Username)
	for _, check := range []struct {
		l   *auth.Limiter
		key string
	}{{s.byUser, userKey}, {s.global, "all"}} {
		if ok, wait := check.l.Allowed(check.key); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "слишком много попыток, попробуйте позже"})
			return
		}
	}

	ctx := r.Context()
	admin, err := s.store.AdminByUsername(ctx, req.Username)
	hash := s.dummy()
	if err == nil {
		hash = admin.PasswordHash
	} else if !errors.Is(err, store.ErrNotFound) {
		log.Printf("login lookup: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "внутренняя ошибка"})
		return
	}
	// Verify even for unknown users so timing does not reveal which logins exist.
	ok, verr := auth.VerifyPassword(hash, req.Password)
	if err != nil || verr != nil || !ok {
		s.byUser.Fail(userKey)
		s.global.Fail("all")
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "неверный логин или пароль"})
		return
	}
	s.byUser.Reset(userKey)

	token, tokenHash, err := auth.NewToken()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "внутренняя ошибка"})
		return
	}
	expires := s.now().Add(sessionTTL)
	if err := s.store.CreateSession(ctx, tokenHash, admin.ID, expires); err != nil {
		log.Printf("create session: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "внутренняя ошибка"})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    token,
		Path:     s.prefix + "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   viaHTTPS(r),
		SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, http.StatusOK, map[string]string{"username": admin.Username})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if !s.checkPost(w, r) {
		return
	}
	if c, err := r.Cookie(cookieName); err == nil {
		if err := s.store.DeleteSession(r.Context(), auth.HashToken(c.Value)); err != nil {
			log.Printf("delete session: %v", err)
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: "", Path: s.prefix + "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	admin, err := s.currentAdmin(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "требуется вход"})
		return
	}
	set, err := site.Load(r.Context(), s.store, s.cfg)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "внутренняя ошибка"})
		return
	}
	open, err := site.PanelOpen(r.Context(), s.store)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "внутренняя ошибка"})
		return
	}
	url := site.PanelURL(set, s.cfg)
	writeJSON(w, http.StatusOK, map[string]any{
		"username": admin.Username,
		"role":     string(s.cfg.Role),
		"domain":   set.Domain,
		"version":  s.version,
		"open":     open && url != "",
		"url":      url,
	})
}

// viaHTTPS reports a request that came through Caddy on https://<domain>/.
// The header is trusted because the panel listens on loopback only.
func viaHTTPS(r *http.Request) bool {
	return r.Header.Get("X-Forwarded-Proto") == "https"
}

// checkPost enforces POST + JSON + same-origin, which together with the
// SameSite=Strict cookie blocks cross-site request forgery.
func (s *Server) checkPost(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return false
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "ожидается JSON"})
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.Host != r.Host {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "чужой источник запроса"})
			return false
		}
	}
	return true
}

func (s *Server) currentAdmin(r *http.Request) (*store.Admin, error) {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return nil, err
	}
	return s.store.SessionAdmin(r.Context(), auth.HashToken(c.Value), s.now())
}

func (s *Server) dummy() string {
	s.dummyOnce.Do(func() {
		h, err := auth.HashPassword("tigersb-dummy-password")
		if err != nil {
			log.Fatalf("dummy hash: %v", err)
		}
		s.dummyHash = h
	})
	return s.dummyHash
}

// PruneLoop deletes expired sessions until ctx is done.
func (s *Server) PruneLoop(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if err := s.store.PruneSessions(ctx, s.now()); err != nil && ctx.Err() == nil {
			log.Printf("prune sessions: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

type siteResponse struct {
	Settings  site.Settings   `json:"settings"`
	Templates []site.Template `json:"templates"`
	Cert      site.CertStatus `json:"cert"`
	Caddy     string          `json:"caddy"`
	Warning   string          `json:"warning,omitempty"`
}

func (s *Server) siteState(r *http.Request, set site.Settings) siteResponse {
	return siteResponse{
		Settings:  set,
		Templates: site.Templates,
		Cert:      site.CheckCert(r.Context(), set.Domain, s.CertAddr, nil, s.now()),
		Caddy:     service.UnitState(s.Run, service.CaddyUnit),
	}
}

func (s *Server) handleSite(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if _, err := s.currentAdmin(r); err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "требуется вход"})
			return
		}
		set, err := site.Load(r.Context(), s.store, s.cfg)
		if err != nil {
			log.Printf("load site: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "внутренняя ошибка"})
			return
		}
		writeJSON(w, http.StatusOK, s.siteState(r, set))
	case http.MethodPost:
		if !s.checkPost(w, r) {
			return
		}
		if _, err := s.currentAdmin(r); err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "требуется вход"})
			return
		}
		var set site.Settings
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&set); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "некорректный запрос"})
			return
		}
		set.Normalize()
		if err := set.Validate(); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if err := xray.CheckDomainFree(r.Context(), s.store, set.Domain); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if err := site.Save(r.Context(), s.store, set); err != nil {
			log.Printf("save site: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "внутренняя ошибка"})
			return
		}
		// Port 443 must send the new domain to Caddy.
		s.xrayMu.Lock()
		if err := s.Xray.Sync(r.Context(), s.store, set.Domain, false); err != nil {
			log.Printf("edge routes: %v", err)
		}
		// TUIC and Hysteria 2 go by the domain and its certificate; the monitor timer
		// brings in a certificate Caddy gets later.
		if err := s.Singbox.Sync(r.Context(), s.store, set.Domain, true); err != nil {
			log.Printf("sing-box: %v", err)
		}
		s.xrayMu.Unlock()
		open, err := site.PanelOpen(r.Context(), s.store)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "внутренняя ошибка"})
			return
		}
		change, err := s.prepare(r, set, site.PanelRoute(s.cfg, open))
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		resp := s.siteState(r, set)
		if change == nil {
			resp.Warning = site.ErrCaddyDown.Error()
		} else {
			resp.Warning = "Caddy применяет настройки. Сертификат для нового домена появится через минуту, нажмите «Проверить сертификат»."
		}
		writeJSON(w, http.StatusOK, resp)
		s.loadLater(change)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// handleStubPreview renders a maintenance page with unsaved settings.
func (s *Server) handleStubPreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if _, err := s.currentAdmin(r); err != nil {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	set := site.Settings{Template: q.Get("t"), Name: q.Get("name")}
	set.Normalize()
	if set.Name == "" {
		for _, t := range site.Templates {
			if t.ID == set.Template {
				set.Name = t.DefaultName
			}
		}
	}
	if err := set.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	page, err := site.RenderStub(set, s.now())
	if err != nil {
		http.Error(w, "внутренняя ошибка", http.StatusInternalServerError)
		return
	}
	// The stub carries its own inline styles; nothing else may load.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'self'")
	w.Header().Set("X-Frame-Options", "SAMEORIGIN")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(page)
}

// handleClose hides the panel from the domain. Only `tigersb open` over SSH
// brings it back, so a stolen session cannot reopen it later.
func (s *Server) handleClose(w http.ResponseWriter, r *http.Request) {
	if !s.checkPost(w, r) {
		return
	}
	if _, err := s.currentAdmin(r); err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "требуется вход"})
		return
	}
	set, err := site.Load(r.Context(), s.store, s.cfg)
	if err == nil {
		err = site.SetPanelOpen(r.Context(), s.store, false)
	}
	if err != nil {
		log.Printf("close panel: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "внутренняя ошибка"})
		return
	}
	change, err := s.prepare(r, set, nil)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
	s.loadLater(change)
}

// prepare renders and writes the site. It returns nil when Caddy is not
// running: the files are on disk and Caddy reads them when it starts.
func (s *Server) prepare(r *http.Request, set site.Settings, route *site.Route) (*site.Change, error) {
	change, err := site.Render(set, s.Site, route, s.now())
	if err != nil {
		return nil, err
	}
	if err := change.Check(r.Context()); errors.Is(err, site.ErrCaddyDown) {
		return nil, change.Write()
	} else if err != nil {
		return nil, err
	}
	return change, change.Write()
}

// loadLater hands the config to Caddy once the current response is sent:
// Caddy waits for in-flight requests before it switches, and this request
// may itself have come through Caddy.
func (s *Server) loadLater(change *site.Change) {
	if change == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		err := change.Load(ctx)
		if err != nil {
			log.Printf("caddy load: %v", err)
		}
		if s.loaded != nil {
			s.loaded <- err
		}
	}()
}
