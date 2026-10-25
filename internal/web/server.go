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
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Stilltiger45/TigerSB-panel/internal/auth"
	"github.com/Stilltiger45/TigerSB-panel/internal/config"
	"github.com/Stilltiger45/TigerSB-panel/internal/store"
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
	writeJSON(w, http.StatusOK, map[string]string{
		"username": admin.Username,
		"role":     string(s.cfg.Role),
		"domain":   s.cfg.Domain,
		"version":  s.version,
	})
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
