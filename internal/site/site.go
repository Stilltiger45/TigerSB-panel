// Package site runs the public face of a server: the Caddy web server with an
// automatic Let's Encrypt certificate and a harmless "maintenance" page.
//
// The panel writes the Caddyfile and the page into its data directory and
// loads the config into Caddy over a unix admin socket. Caddy is a separate
// systemd unit, so the page and certificate renewal keep working while the
// panel sleeps.
package site

import (
	"bytes"
	"context"
	"crypto/rand"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Stilltiger45/TigerSB-panel/internal/config"
	"github.com/Stilltiger45/TigerSB-panel/internal/store"
)

// Setting keys in the store.
const (
	KeyDomain   = "site.domain"
	KeyEmail    = "site.email"
	KeyTemplate = "site.template"
	KeyName     = "site.name"
)

// AdminSocket is where the tigersb-caddy unit exposes the Caddy admin API.
const AdminSocket = "/run/tigersb-caddy/admin.sock"

// Settings describe the public site of this server.
type Settings struct {
	Domain   string `json:"domain"`
	Email    string `json:"email"`
	Template string `json:"template"`
	Name     string `json:"name"`
}

// Template is one maintenance page design.
type Template struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	DefaultName string `json:"default_name"`
}

// Templates are deliberately unrelated to networking: a small hobby site that
// is down for maintenance looks like thousands of others.
var Templates = []Template{
	{ID: "bakery", Title: "Домашняя выпечка", DefaultName: "Домашняя выпечка"},
	{ID: "photo", Title: "Фотоблог", DefaultName: "Северный свет"},
	{ID: "garden", Title: "Дачный дневник", DefaultName: "Дачный дневник"},
	{ID: "books", Title: "Книжный клуб", DefaultName: "Книжный клуб"},
}

//go:embed stubs/*.html
var stubFS embed.FS

var stubs = template.Must(template.ParseFS(stubFS, "stubs/*.html"))

var (
	domainRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{0,61}[a-z0-9]$`)
	emailRe  = regexp.MustCompile(`^[^@\s{}"#]+@[^@\s{}"#]+\.[^@\s{}"#]+$`)
)

func findTemplate(id string) (Template, bool) {
	for _, t := range Templates {
		if t.ID == id {
			return t, true
		}
	}
	return Template{}, false
}

// Normalize lowercases the domain and trims spaces.
func (s *Settings) Normalize() {
	s.Domain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s.Domain)), ".")
	s.Email = strings.TrimSpace(s.Email)
	s.Name = strings.TrimSpace(s.Name)
}

// Validate checks every field; the values go into the Caddyfile and the page.
func (s *Settings) Validate() error {
	if s.Domain != "" && (len(s.Domain) > 253 || !domainRe.MatchString(s.Domain)) {
		return fmt.Errorf("некорректный домен %q: нужен домен вида vpn.example.com, IP-адрес не подойдёт", s.Domain)
	}
	if s.Email != "" && (len(s.Email) > 254 || !emailRe.MatchString(s.Email)) {
		return fmt.Errorf("некорректный email %q", s.Email)
	}
	if _, ok := findTemplate(s.Template); !ok {
		return fmt.Errorf("неизвестный шаблон заглушки %q", s.Template)
	}
	if s.Name == "" || utf8.RuneCountInString(s.Name) > 60 {
		return errors.New("название сайта должно быть от 1 до 60 символов")
	}
	for _, r := range s.Name {
		if unicode.IsControl(r) {
			return errors.New("в названии сайта есть управляющие символы")
		}
	}
	return nil
}

// Load reads the site settings, filling missing ones from the install config
// and a random template, and stores the result so it stays stable.
func Load(ctx context.Context, st *store.Store, cfg *config.Config) (Settings, error) {
	vals, err := st.Settings(ctx)
	if err != nil {
		return Settings{}, err
	}
	s := Settings{Domain: cfg.Domain, Email: cfg.Email}
	if v, ok := vals[KeyDomain]; ok {
		s.Domain = v
	}
	if v, ok := vals[KeyEmail]; ok {
		s.Email = v
	}
	s.Template = vals[KeyTemplate]
	s.Name = vals[KeyName]
	if _, ok := findTemplate(s.Template); !ok {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(Templates))))
		if err != nil {
			return Settings{}, err
		}
		s.Template = Templates[n.Int64()].ID
		s.Name = ""
	}
	if s.Name == "" {
		t, _ := findTemplate(s.Template)
		s.Name = t.DefaultName
	}
	s.Normalize()
	return s, Save(ctx, st, s)
}

// Save stores validated settings.
func Save(ctx context.Context, st *store.Store, s Settings) error {
	if err := s.Validate(); err != nil {
		return err
	}
	return st.SetSettings(ctx, map[string]string{
		KeyDomain: s.Domain, KeyEmail: s.Email, KeyTemplate: s.Template, KeyName: s.Name,
	})
}

// RenderStub returns the maintenance page.
func RenderStub(s Settings, now time.Time) ([]byte, error) {
	var buf bytes.Buffer
	err := stubs.ExecuteTemplate(&buf, s.Template+".html", struct {
		Name string
		Year int
	}{s.Name, now.Year()})
	return buf.Bytes(), err
}

// Paths are the files the panel manages for Caddy.
type Paths struct {
	// Dir holds the Caddyfile and Caddy's certificate storage.
	Dir string
	// WWW is the web root with the maintenance page.
	WWW string
	// Socket is the Caddy admin socket.
	Socket string
}

// DefaultPaths returns the layout used by the installer under dataDir.
func DefaultPaths(dataDir string) Paths {
	return Paths{Dir: filepath.Join(dataDir, "caddy"), WWW: filepath.Join(dataDir, "www"), Socket: AdminSocket}
}

// Caddyfile is the path of the generated config.
func (p Paths) Caddyfile() string { return filepath.Join(p.Dir, "Caddyfile") }

// RenderCaddyfile builds the Caddy config.
//
// With a domain Caddy gets a Let's Encrypt certificate through the TLS-ALPN
// challenge on port 443 only, so port 80 stays closed (the approach of
// web-panel-proxy, MIT). Without a domain the page is served over plain HTTP.
func RenderCaddyfile(s Settings, p Paths) string {
	var b strings.Builder
	b.WriteString("# Создано панелью TigerSB. Ручные правки будут перезаписаны.\n{\n")
	fmt.Fprintf(&b, "\tadmin unix/%s\n", p.Socket)
	fmt.Fprintf(&b, "\tstorage file_system %s\n", p.Dir)
	b.WriteString("\tauto_https disable_redirects\n\tgrace_period 5s\n}\n\n")
	if s.Domain != "" {
		fmt.Fprintf(&b, "%s {\n\ttls {\n\t\tissuer acme {\n", s.Domain)
		if s.Email != "" {
			fmt.Fprintf(&b, "\t\t\temail %s\n", s.Email)
		}
		b.WriteString("\t\t\tdisable_http_challenge\n\t\t}\n\t}\n")
	} else {
		b.WriteString(":80 {\n")
	}
	fmt.Fprintf(&b, "\troot * %s\n", p.WWW)
	b.WriteString("\ttry_files {path} /index.html\n\tfile_server\n\tencode gzip\n")
	b.WriteString("\theader {\n\t\t-Server\n\t\tReferrer-Policy no-referrer\n\t\tX-Content-Type-Options nosniff\n\t}\n}\n")
	return b.String()
}

// ErrCaddyDown means the files were written but Caddy is not running to load them.
var ErrCaddyDown = errors.New("Caddy не запущен: файлы сохранены и подхватятся при его запуске")

// Apply writes the page and the Caddyfile and loads the config into Caddy.
func Apply(ctx context.Context, s Settings, p Paths, now time.Time) error {
	if err := s.Validate(); err != nil {
		return err
	}
	page, err := RenderStub(s, now)
	if err != nil {
		return err
	}
	if err := writeFile(filepath.Join(p.WWW, "index.html"), page, 0o755); err != nil {
		return err
	}
	conf := RenderCaddyfile(s, p)
	if err := writeFile(p.Caddyfile(), []byte(conf), 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(p.Socket); err != nil {
		return ErrCaddyDown
	}
	return load(ctx, p.Socket, conf)
}

func writeFile(path string, data []byte, dirMode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func adminClient(socket string) *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socket)
			},
		},
	}
}

func load(ctx context.Context, socket, conf string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://caddy/load", strings.NewReader(conf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/caddyfile")
	res, err := adminClient(socket).Do(req)
	if err != nil {
		return fmt.Errorf("Caddy не отвечает: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		return fmt.Errorf("Caddy не принял настройки: %s", strings.TrimSpace(string(body)))
	}
	return nil
}
