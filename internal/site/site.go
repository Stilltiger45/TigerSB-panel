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
	// KeyPanelOpen is "1" when the panel answers on https://<domain>/<secret>/.
	KeyPanelOpen = "panel.open"
)

// AdminSocket is where the tigersb-caddy unit exposes the Caddy admin API.
const AdminSocket = "/run/tigersb-caddy/admin.sock"

// InnerTLSPort is where Caddy serves the domain on loopback. Port 443 belongs
// to the edge router, which passes the domain's connections here and the VPN
// protocols' connections to Xray.
const InnerTLSPort = 8444

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

// PanelOpen reports whether the admin wants the panel reachable through the
// domain. It is on until someone runs `tigersb close` or presses the button.
func PanelOpen(ctx context.Context, st *store.Store) (bool, error) {
	vals, err := st.Settings(ctx)
	if err != nil {
		return false, err
	}
	return vals[KeyPanelOpen] != "0", nil
}

// SetPanelOpen stores the choice.
func SetPanelOpen(ctx context.Context, st *store.Store, open bool) error {
	v := "0"
	if open {
		v = "1"
	}
	return st.SetSettings(ctx, map[string]string{KeyPanelOpen: v})
}

// Route sends the secret path through Caddy to the panel on loopback.
type Route struct {
	// Secret is the path segment without slashes.
	Secret string
	// Upstream is the panel's loopback address, host:port.
	Upstream string
}

// PanelRoute returns the route for cfg, or nil when the panel must stay hidden.
func PanelRoute(cfg *config.Config, open bool) *Route {
	if !open {
		return nil
	}
	return &Route{Secret: cfg.SecretPath, Upstream: cfg.Listen}
}

// PanelURL is the public address of the panel, or "" without a domain.
func PanelURL(s Settings, cfg *config.Config) string {
	if s.Domain == "" {
		return ""
	}
	return "https://" + s.Domain + "/" + cfg.SecretPath + "/"
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
// web-panel-proxy, MIT). Caddy itself listens on 127.0.0.1:InnerTLSPort and
// the edge router hands it the domain's connections from 443, the challenge
// included. Without a domain the page is served over plain HTTP.
//
// A non-nil route publishes the panel under its secret path, over HTTPS only;
// every other path, and the secret path while the panel is closed, gets the
// maintenance page.
func RenderCaddyfile(s Settings, p Paths, r *Route) string {
	var b strings.Builder
	b.WriteString("# Создано панелью TigerSB. Ручные правки будут перезаписаны.\n{\n")
	fmt.Fprintf(&b, "\tadmin unix/%s\n", p.Socket)
	fmt.Fprintf(&b, "\tstorage file_system %s\n", p.Dir)
	b.WriteString("\tauto_https disable_redirects\n\tgrace_period 5s\n")
	if s.Domain != "" {
		fmt.Fprintf(&b, "\thttps_port %d\n\tdefault_bind 127.0.0.1\n", InnerTLSPort)
	}
	b.WriteString("}\n\n")
	if s.Domain != "" {
		fmt.Fprintf(&b, "%s {\n\ttls {\n\t\tissuer acme {\n", s.Domain)
		if s.Email != "" {
			fmt.Fprintf(&b, "\t\t\temail %s\n", s.Email)
		}
		fmt.Fprintf(&b, "\t\t\tdisable_http_challenge\n\t\t\talt_tlsalpn_port %d\n\t\t}\n\t}\n", InnerTLSPort)
	} else {
		b.WriteString(":80 {\n")
	}
	stub := "\t"
	if r != nil && s.Domain != "" {
		fmt.Fprintf(&b, "\t@tigersb path /%s /%s/*\n", r.Secret, r.Secret)
		fmt.Fprintf(&b, "\thandle @tigersb {\n\t\treverse_proxy %s\n\t}\n", r.Upstream)
		b.WriteString("\thandle {\n")
		stub = "\t\t"
	}
	// Without this the browser keeps the stub for a tenth of its age, so
	// after a long close the secret path shows the stub even after open.
	fmt.Fprintf(&b, "%sheader Cache-Control no-cache\n", stub)
	fmt.Fprintf(&b, "%sroot * %s\n", stub, p.WWW)
	fmt.Fprintf(&b, "%stry_files {path} /index.html\n%sfile_server\n", stub, stub)
	if stub != "\t" {
		b.WriteString("\t}\n")
	}
	b.WriteString("\tencode gzip\n")
	b.WriteString("\theader {\n\t\t-Server\n\t\tReferrer-Policy no-referrer\n\t\tX-Content-Type-Options nosniff\n\t}\n}\n")
	return b.String()
}

// ErrCaddyDown means the files were written but Caddy is not running to load them.
var ErrCaddyDown = errors.New("Caddy не запущен: файлы сохранены и подхватятся при его запуске")

// Change is a rendered site: the page and the Caddyfile, not yet written.
type Change struct {
	paths Paths
	page  []byte
	conf  string
}

// Render builds the page and the Caddyfile for s.
func Render(s Settings, p Paths, r *Route, now time.Time) (*Change, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	page, err := RenderStub(s, now)
	if err != nil {
		return nil, err
	}
	return &Change{paths: p, page: page, conf: RenderCaddyfile(s, p, r)}, nil
}

// Check asks Caddy to adapt the config without running it. It returns
// ErrCaddyDown when Caddy is not running.
func (c *Change) Check(ctx context.Context) error {
	return post(ctx, c.paths.Socket, "/adapt", c.conf)
}

// Write saves the page and the Caddyfile, which Caddy reads when it starts.
func (c *Change) Write() error {
	if err := writeFile(filepath.Join(c.paths.WWW, "index.html"), c.page, 0o755); err != nil {
		return err
	}
	return writeFile(c.paths.Caddyfile(), []byte(c.conf), 0o700)
}

// Load replaces Caddy's running config. Caddy waits for requests it is
// serving to finish (grace_period), so a request that came through Caddy must
// call this only after its response is written.
func (c *Change) Load(ctx context.Context) error {
	return post(ctx, c.paths.Socket, "/load", c.conf)
}

// Apply writes the page and the Caddyfile and loads the config into Caddy.
func Apply(ctx context.Context, s Settings, p Paths, r *Route, now time.Time) error {
	c, err := Render(s, p, r, now)
	if err != nil {
		return err
	}
	if err := c.Write(); err != nil {
		return err
	}
	return c.Load(ctx)
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

func post(ctx context.Context, socket, path, conf string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://caddy"+path, strings.NewReader(conf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/caddyfile")
	res, err := adminClient(socket).Do(req)
	var dial *net.OpError
	if errors.As(err, &dial) && dial.Op == "dial" {
		// No socket, or a stale one left by a Caddy that was killed.
		return ErrCaddyDown
	}
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
