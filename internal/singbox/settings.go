// Package singbox configures sing-box, the second VPN core, which runs in
// its own Docker container next to Xray. It carries TUIC v5 and Hysteria 2,
// QUIC over UDP, and NaïveProxy, HTTP/2 over TCP, all with the site's own
// Let's Encrypt certificate, which Caddy gets and this package copies for
// sing-box. Further sing-box protocols go into the same Settings, config and
// container.
//
// The field names follow sing-box (GPL-3.0); no code is taken from it.
package singbox

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/Stilltiger45/TigerSB-panel/internal/store"
)

// Key is where the sing-box protocol settings are stored.
const Key = "singbox.protocols"

// Protocol IDs, also the inbound tags.
const (
	TUICTag  = "tuic"
	Hy2Tag   = "hysteria2"
	NaiveTag = "naive"
	MieruTag = "mieru"
)

// TUIC is the TUIC v5 inbound. Fields marked client-only go to clients.
type TUIC struct {
	Enabled bool `json:"enabled"`
	// Port is a UDP port, so it can be the same number as gRPC on TCP.
	Port              int    `json:"port"`
	CongestionControl string `json:"congestion_control"`
	UDPRelayMode      string `json:"udp_relay_mode"` // client-only
	ZeroRTT           bool   `json:"zero_rtt_handshake"`
	// Seconds; Heartbeat 0 turns the server's pings off.
	Heartbeat   int `json:"heartbeat"`
	AuthTimeout int `json:"auth_timeout"`
}

// Hysteria2 is the Hysteria 2 inbound.
type Hysteria2 struct {
	Enabled bool `json:"enabled"`
	// Port is a UDP port: 443 is free for it, the site and Xray use TCP.
	Port int `json:"port"`
	// Obfs turns on Salamander: every packet is scrambled with
	// ObfsPassword, so the traffic does not look like QUIC at all. Clients
	// get the password in the link.
	Obfs         bool   `json:"obfs"`
	ObfsPassword string `json:"obfs_password"`
	BBRProfile   string `json:"bbr_profile"`
	// Mbit/s of the server's channel. 0 leaves BBR in charge; set, they let
	// Hysteria send at the speed a client announces (Brutal), up to them.
	UpMbps   int `json:"up_mbps"`
	DownMbps int `json:"down_mbps"`
	// IgnoreClientBandwidth keeps BBR even for clients that announce a speed.
	IgnoreClientBandwidth bool `json:"ignore_client_bandwidth"`
}

// Naive is the NaïveProxy inbound: HTTP/2 CONNECT over TLS on a TCP port.
type Naive struct {
	Enabled bool `json:"enabled"`
	// Port is a TCP port of its own: 443 belongs to the site and Xray.
	Port int `json:"port"`
}

// Mieru is the Mieru protocol. Official sing-box has none, so mbox, the
// sing-box build of Mieru's author, serves it in a container of its own.
type Mieru struct {
	Enabled bool `json:"enabled"`
	// PortFrom-PortTo is a range: mbox listens on each port, clients spread
	// their connections over them.
	PortFrom int `json:"port_from"`
	PortTo   int `json:"port_to"`
	// Transport is TCP or UDP.
	Transport string `json:"transport"`
}

// Settings are the sing-box protocols.
type Settings struct {
	// Secret makes each user's TUIC and Hysteria 2 passwords from their
	// UUID. It never leaves the server: the API blanks it and keeps the
	// stored one.
	Secret    string    `json:"secret,omitempty"`
	TUIC      TUIC      `json:"tuic"`
	Hysteria2 Hysteria2 `json:"hysteria2"`
	Naive     Naive     `json:"naive"`
	Mieru     Mieru     `json:"mieru"`
}

// Choices offered by the UI.
var (
	CongestionControls = []string{"bbr", "cubic", "new_reno"}
	UDPRelayModes      = []string{"native", "quic"}
	BBRProfiles        = []string{"standard", "conservative", "aggressive"}
	MieruTransports    = []string{"TCP", "UDP"}
)

// maxMieruPorts caps the range: each port is an inbound of its own.
const maxMieruPorts = 100

// hy2Defaults is Hysteria 2 on UDP 443 with Salamander. The obfuscation
// password comes from the secret (see Normalize), so every process that
// loads settings stored before Hysteria 2 existed agrees on it.
func hy2Defaults() Hysteria2 {
	return Hysteria2{Enabled: true, Port: 443, Obfs: true, BBRProfile: "standard"}
}

// naiveDefaults is NaïveProxy on TCP 2443.
func naiveDefaults() Naive { return Naive{Enabled: true, Port: 2443} }

// mieruDefaults is Mieru on TCP 2012-2022, the range of Mieru's own example.
func mieruDefaults() Mieru {
	return Mieru{Enabled: true, PortFrom: 2012, PortTo: 2022, Transport: "TCP"}
}

// Defaults returns TUIC on UDP 8443 with BBR, Hysteria 2 on UDP 443 with
// Salamander, NaïveProxy on TCP 2443, Mieru on TCP 2012-2022 and a new
// secret.
func Defaults() (Settings, error) {
	s := Settings{TUIC: TUIC{
		Enabled: true, Port: 8443,
		CongestionControl: "bbr", UDPRelayMode: "native",
		Heartbeat: 10, AuthTimeout: 3,
	}, Hysteria2: hy2Defaults(), Naive: naiveDefaults(), Mieru: mieruDefaults()}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return s, err
	}
	s.Secret = base64.RawURLEncoding.EncodeToString(b)
	s.Normalize()
	return s, nil
}

// derive is an HMAC of label under the secret, 24 URL-safe characters.
func (s Settings) derive(label string) string {
	m := hmac.New(sha256.New, []byte(s.Secret))
	m.Write([]byte(label))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))[:24]
}

// Password is a user's TUIC password: an HMAC of the UUID, so it changes
// together with the UUID when the admin issues new links.
func (s Settings) Password(uuid string) string { return s.derive("tuic:" + uuid) }

// Hy2Password is a user's Hysteria 2 password, made like Password.
func (s Settings) Hy2Password(uuid string) string { return s.derive("hysteria2:" + uuid) }

// NaivePassword is a user's NaïveProxy password, made like Password.
func (s Settings) NaivePassword(uuid string) string { return s.derive("naive:" + uuid) }

// MieruPassword is a user's Mieru password, made like Password.
func (s Settings) MieruPassword(uuid string) string { return s.derive("mieru:" + uuid) }

// MieruHas reports whether port is in Mieru's range.
func (s Settings) MieruHas(port int) bool { return port >= s.Mieru.PortFrom && port <= s.Mieru.PortTo }

// Normalize trims input. An empty Salamander password becomes the one
// derived from the secret.
func (s *Settings) Normalize() {
	t := &s.TUIC
	t.CongestionControl = strings.ToLower(strings.TrimSpace(t.CongestionControl))
	t.UDPRelayMode = strings.ToLower(strings.TrimSpace(t.UDPRelayMode))
	h := &s.Hysteria2
	h.BBRProfile = strings.ToLower(strings.TrimSpace(h.BBRProfile))
	h.ObfsPassword = strings.TrimSpace(h.ObfsPassword)
	s.Mieru.Transport = strings.ToUpper(strings.TrimSpace(s.Mieru.Transport))
	if h.ObfsPassword == "" && s.Secret != "" {
		h.ObfsPassword = s.derive("salamander")
	}
}

// Validate checks everything that goes into the sing-box config and links.
func (s *Settings) Validate() error {
	t := s.TUIC
	if t.Port != 443 && (t.Port < 1024 || t.Port > 65535) {
		return fmt.Errorf("TUIC: порт %d. Можно 443 или от 1024 до 65535", t.Port)
	}
	if !slices.Contains(CongestionControls, t.CongestionControl) {
		return fmt.Errorf("TUIC: неизвестный алгоритм перегрузки %q", t.CongestionControl)
	}
	if !slices.Contains(UDPRelayModes, t.UDPRelayMode) {
		return fmt.Errorf("TUIC: режим UDP %q: можно native или quic", t.UDPRelayMode)
	}
	if t.Heartbeat < 0 || t.Heartbeat > 300 {
		return errors.New("TUIC: пинг (heartbeat) от 0 до 300 секунд")
	}
	if t.AuthTimeout < 1 || t.AuthTimeout > 60 {
		return errors.New("TUIC: ожидание входа (auth_timeout) от 1 до 60 секунд")
	}
	h := s.Hysteria2
	if h.Port != 443 && (h.Port < 1024 || h.Port > 65535) {
		return fmt.Errorf("Hysteria 2: порт %d. Можно 443 или от 1024 до 65535", h.Port)
	}
	if t.Enabled && h.Enabled && t.Port == h.Port {
		return fmt.Errorf("TUIC и Hysteria 2 оба работают по UDP, им нужны разные порты, а не %d", h.Port)
	}
	if h.Obfs && len(h.ObfsPassword) < 8 {
		return errors.New("Hysteria 2: пароль Salamander не короче 8 символов")
	}
	if !slices.Contains(BBRProfiles, h.BBRProfile) {
		return fmt.Errorf("Hysteria 2: неизвестный профиль BBR %q", h.BBRProfile)
	}
	if h.UpMbps < 0 || h.DownMbps < 0 || h.UpMbps > 100000 || h.DownMbps > 100000 {
		return errors.New("Hysteria 2: скорость от 0 до 100000 Мбит/с")
	}
	if (h.UpMbps == 0) != (h.DownMbps == 0) {
		return errors.New("Hysteria 2: укажите обе скорости сервера или ни одной")
	}
	if h.IgnoreClientBandwidth && h.UpMbps > 0 {
		return errors.New("Hysteria 2: «Всегда BBR» и скорость сервера вместе не работают, оставьте что-то одно")
	}
	if n := s.Naive.Port; n < 1024 || n > 65535 {
		return fmt.Errorf("NaïveProxy: порт %d. Можно от 1024 до 65535: 443 по TCP занят сайтом и Xray", n)
	}
	m := s.Mieru
	if m.PortFrom < 1024 || m.PortTo > 65535 || m.PortFrom > m.PortTo {
		return fmt.Errorf("Mieru: порты %d-%d. Можно диапазон от 1024 до 65535, первый не больше последнего", m.PortFrom, m.PortTo)
	}
	if m.PortTo-m.PortFrom >= maxMieruPorts {
		return fmt.Errorf("Mieru: не больше %d портов в диапазоне", maxMieruPorts)
	}
	if !slices.Contains(MieruTransports, m.Transport) {
		return fmt.Errorf("Mieru: транспорт %q, можно TCP или UDP", m.Transport)
	}
	if m.Enabled && m.Transport == "UDP" {
		if t.Enabled && s.MieruHas(t.Port) {
			return fmt.Errorf("Mieru по UDP и TUIC: порт %d в обоих, им нужны разные порты", t.Port)
		}
		if h.Enabled && s.MieruHas(h.Port) {
			return fmt.Errorf("Mieru по UDP и Hysteria 2: порт %d в обоих, им нужны разные порты", h.Port)
		}
	}
	if m.Enabled && m.Transport == "TCP" && s.Naive.Enabled && s.MieruHas(s.Naive.Port) {
		return fmt.Errorf("Mieru и NaïveProxy оба работают по TCP: порт %d в обоих, им нужны разные порты", s.Naive.Port)
	}
	if len(s.Secret) < 32 {
		return errors.New("sing-box: нет секрета для паролей")
	}
	return nil
}

// Load returns the stored settings. The first call stores the defaults, so
// the secret and with it every password stays the same.
func Load(ctx context.Context, st *store.Store) (Settings, error) {
	vals, err := st.Settings(ctx)
	if err != nil {
		return Settings{}, err
	}
	raw := vals[Key]
	if raw == "" {
		// The panel, the subscriptions and the container may all start at
		// once; whichever stores its defaults first wins for all of them.
		s, err := Defaults()
		if err != nil {
			return Settings{}, err
		}
		b, err := json.Marshal(s)
		if err != nil {
			return Settings{}, err
		}
		if raw, err = st.SettingOnce(ctx, Key, string(b)); err != nil {
			return Settings{}, err
		}
	}
	// Settings stored before Hysteria 2, NaïveProxy or Mieru existed get
	// their defaults.
	s := Settings{Hysteria2: hy2Defaults(), Naive: naiveDefaults(), Mieru: mieruDefaults()}
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return Settings{}, fmt.Errorf("настройки sing-box повреждены: %w", err)
	}
	s.Normalize()
	return s, nil
}

// Save stores settings that already passed Validate.
func Save(ctx context.Context, st *store.Store, s Settings) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return st.SetSettings(ctx, map[string]string{Key: string(b)})
}
