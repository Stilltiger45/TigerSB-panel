// Package sub serves user subscriptions: a signed TigerSB profile for the
// TigerSB client and a plain list of vless:// links for every other client.
//
// Every address that is not a live subscription answers 404, which Caddy
// turns into the maintenance page, so a wrong or revoked address looks like
// any other page of the site.
package sub

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Stilltiger45/TigerSB-client/pkg/profile"

	"github.com/Stilltiger45/TigerSB-panel/internal/core"
	"github.com/Stilltiger45/TigerSB-panel/internal/singbox"
	"github.com/Stilltiger45/TigerSB-panel/internal/site"
	"github.com/Stilltiger45/TigerSB-panel/internal/store"
	"github.com/Stilltiger45/TigerSB-panel/internal/xray"
)

// Unit runs this handler (`tigersb sub`) on site.SubPort.
const Unit = "tigersb-sub.service"

// KeySigning is the setting that holds the Ed25519 seed the profiles are
// signed with. Clients pin its public key, so it never changes by itself.
const KeySigning = "sub.key"

// Name is the brand in the subscription titles.
const Name = "TigerSB"

// Title is what other clients show for a user's subscription. The TigerSB
// client already carries the brand, so its profile is named by the user.
func Title(user string) string { return Name + " · " + user }

// UpdateHours is how often clients are asked to refresh the subscription.
const UpdateHours = 6

var b64 = base64.RawURLEncoding

// SigningKey returns the panel's signing key, creating it on first use.
func SigningKey(ctx context.Context, st *store.Store) (ed25519.PrivateKey, error) {
	_, priv, err := profile.GenerateKey()
	if err != nil {
		return nil, err
	}
	seed, err := st.SettingOnce(ctx, KeySigning, profile.EncodeKey(priv.Seed()))
	if err != nil {
		return nil, err
	}
	return profile.DecodePrivateKey(seed)
}

// PublicKey is the key that goes into tigersb:// links.
func PublicKey(ctx context.Context, st *store.Store) (string, error) {
	priv, err := SigningKey(ctx, st)
	if err != nil {
		return "", err
	}
	return profile.EncodeKey(priv.Public().(ed25519.PublicKey)), nil
}

// NewToken returns a random subscription token: 192 bits, 32 characters.
func NewToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return b64.EncodeToString(b), nil
}

// Tokens made by NewToken, and the hex ones the database migration made.
var tokenRe = regexp.MustCompile(`^[A-Za-z0-9_-]{32}$`)

// URL is the https:// subscription address.
func URL(domain, token string) string {
	return "https://" + domain + site.SubPrefix + token
}

// Links are the two ways to hand a subscription to a user.
type Links struct {
	// TigerSB is the tigersb:// key: the client checks every profile
	// against the panel's key in it.
	TigerSB string `json:"tigersb"`
	// HTTPS is the plain address for v2rayNG, Hiddify and other clients. The
	// TigerSB client accepts it too and pins the key of the first profile.
	HTTPS string `json:"https"`
}

// LinksFor returns the subscription links of a user, or nothing without a
// domain: a subscription needs a certificate.
func LinksFor(domain, pk string, u store.User) *Links {
	token := u.SubToken
	if domain == "" || token == "" {
		return nil
	}
	k := url.Values{}
	k.Set("url", URL(domain, token))
	k.Set("pk", pk)
	k.Set("name", u.Name)
	return &Links{TigerSB: profile.Scheme + "://add?" + k.Encode(), HTTPS: URL(domain, token)}
}

// prio orders the transports for the client: XHTTP first, then gRPC, then
// TUIC, whose UDP some networks block.
var prio = map[string]int{xray.XHTTPTag: 10, xray.GRPCTag: 20, singbox.TUICTag: 30}

// Profile builds the signed profile of a user: one exit (this server) with
// every enabled protocol of every core.
func Profile(priv ed25519.PrivateKey, links []core.Link, domain string, u store.User, now time.Time) ([]byte, error) {
	exit := profile.Exit{ID: "main", Role: profile.RoleForeign}
	for _, l := range links {
		out, _, err := profile.ParseLink(l.URL)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", l.Protocol, err)
		}
		exit.Transports = append(exit.Transports, profile.Transport{ID: l.Protocol, Prio: prio[l.Protocol], Name: l.Title, Xray: out})
	}
	if len(exit.Transports) == 0 {
		return nil, errors.New("нет включённых протоколов")
	}
	p := profile.Profile{
		V:       profile.Version,
		ID:      "tigersb",
		Name:    u.Name,
		Updated: now.UTC().Truncate(time.Second),
		Update:  &profile.Update{URL: URL(domain, u.SubToken), Every: strconv.Itoa(UpdateHours) + "h"},
		Exits:   []profile.Exit{exit},
		Routing: profile.Routing{Blocked: profile.Matchers{
			Domains: []string{"geosite:ru-blocked"},
			IPs:     []string{"geoip:ru-blocked"},
		}},
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	return profile.Sign(priv, raw)
}

// Plain is the subscription for other clients: the links, base64 encoded.
func Plain(links []core.Link) []byte {
	var lines []string
	for _, l := range links {
		lines = append(lines, l.URL)
	}
	return []byte(base64.StdEncoding.EncodeToString([]byte(strings.Join(lines, "\n"))))
}

// limitLink tells clients without the TigerSB format why there are no
// servers: it shows up as the only server and connects nowhere.
const limitLink = "vless://00000000-0000-0000-0000-000000000000@0.0.0.0:1?encryption=none&type=tcp&security=none#"

// Handler answers /sub/<token>.
type Handler struct {
	Store *store.Store
	// Domain returns the domain of the site; subscriptions need one.
	Domain func(ctx context.Context) (string, error)
	// LocalIPs lists this server's addresses for the links (xray.LocalIPs).
	LocalIPs func() ([]net.IP, error)
	// TUICCert reports whether sing-box has the certificate TUIC needs
	// (singbox.Runtime.HaveCert); nil means it has not.
	TUICCert func() bool
	Now      func() time.Time
}

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// IsTigerSB tells the TigerSB client (TigerSB-android/…, TigerSB-client/…)
// by its user agent.
func IsTigerSB(userAgent string) bool { return strings.HasPrefix(userAgent, "TigerSB-") }

// clip keeps a header value printable and short.
func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	for len(s) > n {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s
}

// clientIP is the address Caddy saw. Only Caddy on loopback is trusted to
// say it; it replaces whatever X-Forwarded-For the client sent.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return ""
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return host
	}
	parts := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	if ip := net.ParseIP(strings.TrimSpace(parts[len(parts)-1])); ip != nil {
		return ip.String()
	}
	return host
}

// device describes the requesting device. Clients that send no x-hwid are
// counted per app, so each of them still takes one place.
func device(r *http.Request, userID int64) store.Device {
	ua := clip(r.UserAgent(), 120)
	d := store.Device{
		UserID:    userID,
		HWID:      clip(r.Header.Get("X-Hwid"), 128),
		OS:        clip(r.Header.Get("X-Device-Os"), 40),
		OSVersion: clip(r.Header.Get("X-Ver-Os"), 40),
		Model:     clip(r.Header.Get("X-Device-Model"), 80),
		App:       ua,
		IP:        clientIP(r),
	}
	if d.HWID == "" {
		app, _, _ := strings.Cut(ua, "/")
		if app = strings.TrimSpace(app); app == "" {
			app = "неизвестное приложение"
		}
		d.HWID = "app:" + app
	}
	return d
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token, ok := strings.CutPrefix(r.URL.Path, site.SubPrefix)
	if (r.Method != http.MethodGet && r.Method != http.MethodHead) || !ok || !tokenRe.MatchString(token) {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	u, err := h.Store.UserByToken(ctx, token)
	if errors.Is(err, store.ErrNotFound) || (err == nil && !u.Enabled) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		log.Printf("sub: %v", err)
		http.Error(w, "внутренняя ошибка", http.StatusInternalServerError)
		return
	}
	domain, err := h.Domain(ctx)
	if err != nil || domain == "" {
		http.NotFound(w, r)
		return
	}
	ua := r.UserAgent()
	hdr := w.Header()
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("Profile-Title", "base64:"+base64.StdEncoding.EncodeToString([]byte(Title(u.Name))))
	hdr.Set("Profile-Update-Interval", strconv.Itoa(UpdateHours))
	hdr.Set("Subscription-Userinfo", fmt.Sprintf("upload=%d; download=%d; total=0; expire=0", u.Up, u.Down))

	if err := h.Store.TouchDevice(ctx, device(r, u.ID), u.Devices, h.now()); errors.Is(err, store.ErrDeviceLimit) {
		msg := fmt.Sprintf("Лимит устройств (%d) исчерпан: попросите администратора удалить старое устройство", u.Devices)
		if IsTigerSB(ua) {
			hdr.Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprintln(w, msg)
			return
		}
		hdr.Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte(base64.StdEncoding.EncodeToString([]byte(limitLink + url.PathEscape(msg)))))
		return
	} else if err != nil {
		log.Printf("sub: device: %v", err)
		http.Error(w, "внутренняя ошибка", http.StatusInternalServerError)
		return
	}

	set, err := xray.Load(ctx, h.Store)
	if err != nil {
		log.Printf("sub: %v", err)
		http.Error(w, "внутренняя ошибка", http.StatusInternalServerError)
		return
	}
	var ips []net.IP
	if h.LocalIPs != nil {
		ips, _ = h.LocalIPs()
	}
	addr := xray.PublicAddress(set, ips, domain)
	links := xray.Links(set, addr, *u)
	if sb, err := singbox.Load(ctx, h.Store); err != nil {
		log.Printf("sub: %v", err)
	} else {
		links = append(links, singbox.Links(sb, addr, domain, h.TUICCert != nil && h.TUICCert(), *u)...)
	}
	if !IsTigerSB(ua) {
		hdr.Set("Content-Type", "text/plain; charset=utf-8")
		w.Write(Plain(links))
		return
	}
	priv, err := SigningKey(ctx, h.Store)
	if err == nil {
		var body []byte
		if body, err = Profile(priv, links, domain, *u, h.now()); err == nil {
			hdr.Set("Content-Type", "application/json")
			w.Write(body)
			return
		}
	}
	log.Printf("sub: profile: %v", err)
	http.Error(w, "профиль не собрался: "+err.Error(), http.StatusServiceUnavailable)
}
