// Package xray configures Xray-core, which runs in a Docker container: the
// protocol settings an admin tunes on the "Протоколы" tab, the users on the
// "Пользователи" tab, the config file built from both, and share links.
//
// Every protocol is VLESS with REALITY: the server borrows the TLS handshake
// of a real site (the SNI), so a probe sees that site. Two transports exist
// for now, gRPC and XHTTP. The field names follow Xray-core (MPL-2.0); no code
// is taken from it.
package xray

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/Stilltiger45/TigerSB-panel/internal/site"
	"github.com/Stilltiger45/TigerSB-panel/internal/store"
)

// Key is where the protocol settings are stored.
const Key = "xray.protocols"

// Fixed loopback ports. Protocols on 443 listen here behind the edge router;
// Caddy serves the domain on TLSPort.
const (
	APIAddr    = "127.0.0.1:10085"
	GRPCInner  = 10441
	XHTTPInner = 10442
	TLSPort    = site.InnerTLSPort
	EdgePort   = 443
	// NaiveInner is NaïveProxy over TCP in sing-box: the edge holds its
	// public port and sets BBR there (singbox.Forwards).
	NaiveInner = 10443
)

// Protocol IDs, also the inbound tags.
const (
	GRPCTag  = "grpc"
	XHTTPTag = "xhttp"
)

// Reality is the disguise shared by both transports.
type Reality struct {
	// SNI is the site whose handshake is borrowed; clients send it.
	SNI string `json:"sni"`
	// Target is where Xray forwards anyone without a key, host:port.
	// Empty means SNI:443.
	Target      string   `json:"target"`
	Fingerprint string   `json:"fingerprint"`
	PrivateKey  string   `json:"private_key"`
	PublicKey   string   `json:"public_key"` // derived, never read from input
	ShortIDs    []string `json:"short_ids"`
	SpiderX     string   `json:"spider_x"`
}

// GRPC is the gRPC transport. Fields marked client-only go to clients.
type GRPC struct {
	Enabled     bool    `json:"enabled"`
	Port        int     `json:"port"`
	BBR         bool    `json:"bbr"` // see Congestion
	Reality     Reality `json:"reality"`
	ServiceName string  `json:"service_name"`
	Authority   string  `json:"authority"`  // client-only
	MultiMode   bool    `json:"multi_mode"` // client-only
	UserAgent   string  `json:"user_agent"` // client-only
	// Keepalive pings, seconds; 0 turns them off.
	IdleTimeout         int  `json:"idle_timeout"`
	HealthCheckTimeout  int  `json:"health_check_timeout"`
	PermitWithoutStream bool `json:"permit_without_stream"` // client-only
	InitialWindowsSize  int  `json:"initial_windows_size"`  // client-only
}

// Xmux is how XHTTP clients reuse connections (client-only).
type Xmux struct {
	MaxConcurrency   string `json:"max_concurrency"`
	MaxConnections   string `json:"max_connections"`
	CMaxReuseTimes   string `json:"c_max_reuse_times"`
	HMaxRequestTimes string `json:"h_max_request_times"`
	HMaxReusableSecs string `json:"h_max_reusable_secs"`
	HKeepAlivePeriod int    `json:"h_keep_alive_period"`
}

// XHTTP is the XHTTP transport. Number ranges are strings like "100-1000".
type XHTTP struct {
	Enabled              bool    `json:"enabled"`
	Port                 int     `json:"port"`
	BBR                  bool    `json:"bbr"` // see Congestion
	Reality              Reality `json:"reality"`
	Path                 string  `json:"path"`
	Host                 string  `json:"host"`
	Mode                 string  `json:"mode"`
	UplinkHTTPMethod     string  `json:"uplink_http_method"` // client-only
	XPaddingBytes        string  `json:"x_padding_bytes"`
	NoSSEHeader          bool    `json:"no_sse_header"`
	ScMaxEachPostBytes   string  `json:"sc_max_each_post_bytes"`
	ScMinPostsIntervalMs string  `json:"sc_min_posts_interval_ms"` // client-only
	ScMaxBufferedPosts   int     `json:"sc_max_buffered_posts"`    // server-only
	ScStreamUpServerSecs string  `json:"sc_stream_up_server_secs"` // server-only
	Xmux                 Xmux    `json:"xmux"`
}

// Settings are both protocols and the address that goes into links.
type Settings struct {
	// Address is what clients connect to; empty means the server's public IP.
	Address string `json:"address"`
	GRPC    GRPC   `json:"grpc"`
	XHTTP   XHTTP  `json:"xhttp"`
}

// Congestion is the TCP congestion control of a protocol's connections to
// clients: BBR when its switch is on, else the kernel's default. BBR keeps
// downloads fast on long and lossy paths, where CUBIC backs off too much.
// The kernel lets unprivileged sockets pick it only when it is listed in
// net.ipv4.tcp_allowed_congestion_control, which the installer sees to.
func Congestion(bbr bool) string {
	if bbr {
		return "bbr"
	}
	return ""
}

// AllowedPath lists the congestion controls any program may pick.
var AllowedPath = "/proc/sys/net/ipv4/tcp_allowed_congestion_control"

// BBRReady reports whether Xray and the edge router may switch to BBR.
func BBRReady() bool {
	b, err := os.ReadFile(AllowedPath)
	return err == nil && slices.Contains(strings.Fields(string(b)), "bbr")
}

// Choices offered by the UI.
var (
	Fingerprints = []string{"chrome", "firefox", "safari", "ios", "android", "edge", "360", "qq", "random", "randomized"}
	XHTTPModes   = []string{"auto", "packet-up", "stream-up", "stream-one"}
	HTTPMethods  = []string{"POST", "PUT", "PATCH"}
)

// Defaults returns fresh settings with new keys: gRPC on 8443 disguised as
// google.com and XHTTP on 443 disguised as cloudflare.com.
func Defaults() (Settings, error) {
	s := Settings{
		GRPC: GRPC{
			Enabled: true, Port: 8443,
			Reality: Reality{SNI: "google.com", Fingerprint: "chrome", SpiderX: "/"},
		},
		XHTTP: XHTTP{
			Enabled: true, Port: 443,
			Reality:              Reality{SNI: "cloudflare.com", Fingerprint: "chrome", SpiderX: "/"},
			Mode:                 "auto",
			UplinkHTTPMethod:     "POST",
			XPaddingBytes:        "100-1000",
			ScMaxEachPostBytes:   "1000000",
			ScMinPostsIntervalMs: "30",
			ScMaxBufferedPosts:   30,
			ScStreamUpServerSecs: "20-80",
			Xmux: Xmux{
				MaxConcurrency:   "16-32",
				HMaxRequestTimes: "600-900",
				HMaxReusableSecs: "1800-3000",
			},
		},
	}
	var err error
	if s.GRPC.ServiceName, err = randomName(10); err != nil {
		return s, err
	}
	name, err := randomName(10)
	if err != nil {
		return s, err
	}
	s.XHTTP.Path = "/" + name
	for _, r := range []*Reality{&s.GRPC.Reality, &s.XHTTP.Reality} {
		if err := r.NewKeys(); err != nil {
			return s, err
		}
	}
	return s, nil
}

// randomName is n lowercase letters and digits, starting with a letter.
func randomName(n int) (string, error) {
	const letters = "abcdefghijklmnopqrstuvwxyz"
	const alnum = letters + "0123456789"
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	out := []byte{letters[int(b[0])%len(letters)]}
	for _, c := range b[1:] {
		out = append(out, alnum[int(c)%len(alnum)])
	}
	return string(out), nil
}

// NewKeys replaces the key pair and the short ID.
func (r *Reality) NewKeys() error {
	key, err := newPrivateKey()
	if err != nil {
		return err
	}
	sid := make([]byte, 8)
	if _, err := rand.Read(sid); err != nil {
		return err
	}
	r.PrivateKey = key
	r.ShortIDs = []string{hex.EncodeToString(sid)}
	return r.derive()
}

// newPrivateKey makes an X25519 key the way `xray x25519` does.
func newPrivateKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[0] &= 248
	b[31] &= 127
	b[31] |= 64
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// derive fills PublicKey from PrivateKey.
func (r *Reality) derive() error {
	raw, err := base64.RawURLEncoding.DecodeString(r.PrivateKey)
	if err != nil || len(raw) != 32 {
		return errors.New("закрытый ключ REALITY должен быть 32 байтами в base64url, как его печатает xray x25519")
	}
	key, err := ecdh.X25519().NewPrivateKey(raw)
	if err != nil {
		return err
	}
	r.PublicKey = base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes())
	return nil
}

// TargetAddr is where unknown visitors are forwarded.
func (r Reality) TargetAddr() string {
	if r.Target != "" {
		return r.Target
	}
	return net.JoinHostPort(r.SNI, "443")
}

var (
	domainRe  = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{0,61}[a-z0-9]$`)
	serviceRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	pathRe    = regexp.MustCompile(`^/[A-Za-z0-9._~/-]{0,100}$`)
	shortIDRe = regexp.MustCompile(`^([0-9a-f]{2}){0,8}$`)
	rangeRe   = regexp.MustCompile(`^([0-9]{1,9})(-([0-9]{1,9}))?$`)
)

func lower(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// Normalize trims input and fills derived fields.
func (s *Settings) Normalize() {
	s.Address = lower(s.Address)
	for _, r := range []*Reality{&s.GRPC.Reality, &s.XHTTP.Reality} {
		r.SNI = strings.TrimSuffix(lower(r.SNI), ".")
		r.Target = lower(r.Target)
		r.PrivateKey = strings.TrimSpace(r.PrivateKey)
		r.PublicKey = ""
		var ids []string
		for _, id := range r.ShortIDs {
			if id = lower(id); !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
		r.ShortIDs = ids
		r.SpiderX = strings.TrimSpace(r.SpiderX)
		if r.SpiderX == "" {
			r.SpiderX = "/"
		}
	}
	g := &s.GRPC
	g.ServiceName = strings.TrimSpace(g.ServiceName)
	g.Authority = lower(g.Authority)
	g.UserAgent = strings.TrimSpace(g.UserAgent)
	x := &s.XHTTP
	x.Path = strings.TrimSpace(x.Path)
	x.Host = lower(x.Host)
	x.UplinkHTTPMethod = strings.ToUpper(strings.TrimSpace(x.UplinkHTTPMethod))
	for _, p := range []*string{&x.XPaddingBytes, &x.ScMaxEachPostBytes, &x.ScMinPostsIntervalMs, &x.ScStreamUpServerSecs,
		&x.Xmux.MaxConcurrency, &x.Xmux.MaxConnections, &x.Xmux.CMaxReuseTimes, &x.Xmux.HMaxRequestTimes, &x.Xmux.HMaxReusableSecs} {
		*p = strings.ReplaceAll(strings.TrimSpace(*p), " ", "")
	}
}

// Reserved lists ports protocols may not take: Caddy, the API, the inner
// ports behind the edge router, the panel and plain HTTP.
func Reserved(panelPort int) map[int]string {
	return map[int]string{
		80:           "HTTP (заглушка)",
		TLSPort:      "Caddy",
		10085:        "API Xray",
		site.SubPort: "подписки",
		10087:        "статистика sing-box",
		10088:        "статистика Mieru",
		GRPCInner:    "внутренний порт gRPC",
		XHTTPInner:   "внутренний порт XHTTP",
		NaiveInner:   "внутренний порт NaïveProxy",
		panelPort:    "панель",
	}
}

// Validate checks everything that goes into the Xray config and the links.
// ownDomain is the panel's domain: it cannot be a disguise.
func (s *Settings) Validate(ownDomain string, panelPort int) error {
	if s.Address != "" {
		if ip := net.ParseIP(s.Address); ip == nil && !domainRe.MatchString(s.Address) {
			return fmt.Errorf("адрес для ссылок %q: нужен IP или домен", s.Address)
		}
	}
	reserved := Reserved(panelPort)
	type proto struct {
		name    string
		enabled bool
		port    int
		r       *Reality
	}
	list := []proto{{"gRPC", s.GRPC.Enabled, s.GRPC.Port, &s.GRPC.Reality}, {"XHTTP", s.XHTTP.Enabled, s.XHTTP.Port, &s.XHTTP.Reality}}
	for _, p := range list {
		if p.port != EdgePort && (p.port < 1024 || p.port > 65535) {
			return fmt.Errorf("%s: порт %d. Можно 443 или от 1024 до 65535", p.name, p.port)
		}
		if why, ok := reserved[p.port]; ok {
			return fmt.Errorf("%s: порт %d занят (%s)", p.name, p.port, why)
		}
		if err := p.r.validate(p.name, ownDomain); err != nil {
			return err
		}
	}
	if s.GRPC.Enabled && s.XHTTP.Enabled && s.GRPC.Port == s.XHTTP.Port {
		if s.GRPC.Port != EdgePort {
			return fmt.Errorf("gRPC и XHTTP не могут делить порт %d: общим может быть только 443", s.GRPC.Port)
		}
		if s.GRPC.Reality.SNI == s.XHTTP.Reality.SNI {
			return errors.New("на общем порту 443 у gRPC и XHTTP должны быть разные SNI: по SNI сервер их и различает")
		}
	}
	if err := s.GRPC.validate(); err != nil {
		return err
	}
	return s.XHTTP.validate()
}

func (r *Reality) validate(name, ownDomain string) error {
	if !domainRe.MatchString(r.SNI) {
		return fmt.Errorf("%s: SNI %q должен быть доменом сайта, например google.com", name, r.SNI)
	}
	if ownDomain != "" && r.SNI == ownDomain {
		return fmt.Errorf("%s: SNI не может совпадать с доменом панели", name)
	}
	if r.Target != "" {
		host, port, err := net.SplitHostPort(r.Target)
		n, perr := strconv.Atoi(port)
		if err != nil || perr != nil || n < 1 || n > 65535 || (net.ParseIP(host) == nil && !domainRe.MatchString(host)) {
			return fmt.Errorf("%s: куда пересылать чужих (target) %q: нужен адрес вида сайт:443", name, r.Target)
		}
	}
	if !slices.Contains(Fingerprints, r.Fingerprint) {
		return fmt.Errorf("%s: неизвестный отпечаток браузера %q", name, r.Fingerprint)
	}
	if len(r.ShortIDs) == 0 || len(r.ShortIDs) > 8 {
		return fmt.Errorf("%s: нужно от 1 до 8 short ID", name)
	}
	for _, id := range r.ShortIDs {
		if !shortIDRe.MatchString(id) {
			return fmt.Errorf("%s: short ID %q должен быть hex-строкой чётной длины до 16 символов", name, id)
		}
	}
	if !strings.HasPrefix(r.SpiderX, "/") || len(r.SpiderX) > 200 || strings.ContainsAny(r.SpiderX, " \"\\") {
		return fmt.Errorf("%s: spiderX должен начинаться с /", name)
	}
	if err := r.derive(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

func printable(s string, max int) bool {
	if len(s) > max {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || r == '"' || r == '\\' {
			return false
		}
	}
	return true
}

func (g *GRPC) validate() error {
	if !serviceRe.MatchString(g.ServiceName) {
		return fmt.Errorf("gRPC: имя сервиса %q: латиница, цифры, точка, дефис, подчёркивание, до 64 символов", g.ServiceName)
	}
	if g.Authority != "" && !domainRe.MatchString(g.Authority) {
		return fmt.Errorf("gRPC: authority %q должен быть доменом", g.Authority)
	}
	if !printable(g.UserAgent, 200) {
		return errors.New("gRPC: в User-Agent недопустимые символы")
	}
	switch {
	case g.IdleTimeout < 0 || g.IdleTimeout > 3600:
		return errors.New("gRPC: idle_timeout от 0 до 3600 секунд")
	case g.HealthCheckTimeout < 0 || g.HealthCheckTimeout > 600:
		return errors.New("gRPC: health_check_timeout от 0 до 600 секунд")
	case g.InitialWindowsSize < 0 || g.InitialWindowsSize > 1<<30:
		return errors.New("gRPC: initial_windows_size от 0 до 1073741824")
	}
	return nil
}

// checkRange accepts "", "N" or "A-B" with lo <= A <= B <= hi.
func checkRange(field, v string, lo, hi int, required bool) error {
	if v == "" {
		if required {
			return fmt.Errorf("XHTTP: %s не задан", field)
		}
		return nil
	}
	m := rangeRe.FindStringSubmatch(v)
	if m == nil {
		return fmt.Errorf("XHTTP: %s %q: нужно число или диапазон вида 100-1000", field, v)
	}
	a, _ := strconv.Atoi(m[1])
	b := a
	if m[3] != "" {
		b, _ = strconv.Atoi(m[3])
	}
	if a > b || a < lo || b > hi {
		return fmt.Errorf("XHTTP: %s %q: допустимо от %d до %d", field, v, lo, hi)
	}
	return nil
}

func (x *XHTTP) validate() error {
	if !pathRe.MatchString(x.Path) {
		return fmt.Errorf("XHTTP: путь %q должен начинаться с / и состоять из латиницы, цифр и ._~-/", x.Path)
	}
	if x.Host != "" && !domainRe.MatchString(x.Host) {
		return fmt.Errorf("XHTTP: host %q должен быть доменом", x.Host)
	}
	if !slices.Contains(XHTTPModes, x.Mode) {
		return fmt.Errorf("XHTTP: неизвестный режим %q", x.Mode)
	}
	if !slices.Contains(HTTPMethods, x.UplinkHTTPMethod) {
		return fmt.Errorf("XHTTP: метод отправки %q: можно POST, PUT или PATCH", x.UplinkHTTPMethod)
	}
	checks := []struct {
		field, v string
		lo, hi   int
		required bool
	}{
		{"xPaddingBytes", x.XPaddingBytes, 1, 65536, true},
		{"scMaxEachPostBytes", x.ScMaxEachPostBytes, 1024, 100 << 20, true},
		{"scMinPostsIntervalMs", x.ScMinPostsIntervalMs, 0, 10000, false},
		{"scStreamUpServerSecs", x.ScStreamUpServerSecs, 0, 3600, false},
		{"xmux.maxConcurrency", x.Xmux.MaxConcurrency, 0, 1024, false},
		{"xmux.maxConnections", x.Xmux.MaxConnections, 0, 1024, false},
		{"xmux.cMaxReuseTimes", x.Xmux.CMaxReuseTimes, 0, 1 << 20, false},
		{"xmux.hMaxRequestTimes", x.Xmux.HMaxRequestTimes, 0, 1 << 20, false},
		{"xmux.hMaxReusableSecs", x.Xmux.HMaxReusableSecs, 0, 86400, false},
	}
	for _, c := range checks {
		if err := checkRange(c.field, c.v, c.lo, c.hi, c.required); err != nil {
			return err
		}
	}
	if x.Xmux.MaxConcurrency != "" && x.Xmux.MaxConnections != "" {
		return errors.New("XHTTP: в xmux задайте что-то одно: maxConcurrency или maxConnections")
	}
	if x.ScMaxBufferedPosts < 1 || x.ScMaxBufferedPosts > 10000 {
		return errors.New("XHTTP: scMaxBufferedPosts от 1 до 10000")
	}
	if x.Xmux.HKeepAlivePeriod < 0 || x.Xmux.HKeepAlivePeriod > 3600 {
		return errors.New("XHTTP: xmux.hKeepAlivePeriod от 0 до 3600 секунд")
	}
	return nil
}

// Load returns the stored settings. The first call creates the defaults with
// new keys and stores them, so keys never change by themselves.
func Load(ctx context.Context, st *store.Store) (Settings, error) {
	vals, err := st.Settings(ctx)
	if err != nil {
		return Settings{}, err
	}
	if raw := vals[Key]; raw != "" {
		var s Settings
		if err := json.Unmarshal([]byte(raw), &s); err != nil {
			return Settings{}, fmt.Errorf("настройки протоколов повреждены: %w", err)
		}
		for _, r := range []*Reality{&s.GRPC.Reality, &s.XHTTP.Reality} {
			if err := r.derive(); err != nil {
				return Settings{}, err
			}
		}
		return s, nil
	}
	s, err := Defaults()
	if err != nil {
		return Settings{}, err
	}
	return s, Save(ctx, st, s)
}

// Save stores settings that already passed Validate.
func Save(ctx context.Context, st *store.Store, s Settings) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return st.SetSettings(ctx, map[string]string{Key: string(b)})
}

// CheckDomainFree refuses a panel domain that a protocol already uses as SNI:
// port 443 tells them apart by that name.
func CheckDomainFree(ctx context.Context, st *store.Store, domain string) error {
	if domain == "" {
		return nil
	}
	s, err := Load(ctx, st)
	if err != nil {
		return err
	}
	if s.GRPC.Reality.SNI == domain || s.XHTTP.Reality.SNI == domain {
		return errors.New("этот домен уже указан как SNI протокола; выберите другой SNI на вкладке «Протоколы»")
	}
	return nil
}
