// Package singbox configures sing-box, the second VPN core, which runs in
// its own Docker container next to Xray. It carries TUIC v5 and Hysteria 2:
// QUIC over UDP with the site's own Let's Encrypt certificate, which Caddy
// gets and this package copies for sing-box. Further sing-box protocols go
// into the same Settings, config and container.
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
	TUICTag = "tuic"
	Hy2Tag  = "hysteria2"
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

// Settings are the sing-box protocols.
type Settings struct {
	// Secret makes each user's TUIC and Hysteria 2 passwords from their
	// UUID. It never leaves the server: the API blanks it and keeps the
	// stored one.
	Secret    string    `json:"secret,omitempty"`
	TUIC      TUIC      `json:"tuic"`
	Hysteria2 Hysteria2 `json:"hysteria2"`
}

// Choices offered by the UI.
var (
	CongestionControls = []string{"bbr", "cubic", "new_reno"}
	UDPRelayModes      = []string{"native", "quic"}
	BBRProfiles        = []string{"standard", "conservative", "aggressive"}
)

// hy2Defaults is Hysteria 2 on UDP 443 with Salamander. The obfuscation
// password comes from the secret (see Normalize), so every process that
// loads settings stored before Hysteria 2 existed agrees on it.
func hy2Defaults() Hysteria2 {
	return Hysteria2{Enabled: true, Port: 443, Obfs: true, BBRProfile: "standard"}
}

// Defaults returns TUIC on UDP 8443 with BBR, Hysteria 2 on UDP 443 with
// Salamander and a new secret.
func Defaults() (Settings, error) {
	s := Settings{TUIC: TUIC{
		Enabled: true, Port: 8443,
		CongestionControl: "bbr", UDPRelayMode: "native",
		Heartbeat: 10, AuthTimeout: 3,
	}, Hysteria2: hy2Defaults()}
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

// Normalize trims input. An empty Salamander password becomes the one
// derived from the secret.
func (s *Settings) Normalize() {
	t := &s.TUIC
	t.CongestionControl = strings.ToLower(strings.TrimSpace(t.CongestionControl))
	t.UDPRelayMode = strings.ToLower(strings.TrimSpace(t.UDPRelayMode))
	h := &s.Hysteria2
	h.BBRProfile = strings.ToLower(strings.TrimSpace(h.BBRProfile))
	h.ObfsPassword = strings.TrimSpace(h.ObfsPassword)
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
	// Settings stored before Hysteria 2 existed get its defaults.
	s := Settings{Hysteria2: hy2Defaults()}
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
