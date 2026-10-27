// Package singbox configures sing-box, the second VPN core, which runs in
// its own Docker container next to Xray. It carries TUIC v5: QUIC over UDP
// with the site's own Let's Encrypt certificate, which Caddy gets and this
// package copies for sing-box. Further sing-box protocols (Hysteria 2 and the
// like) go into the same Settings, config and container.
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

// TUICTag is the protocol ID and the inbound tag.
const TUICTag = "tuic"

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

// Settings are the sing-box protocols.
type Settings struct {
	// Secret makes each user's TUIC password from their UUID. It never
	// leaves the server: the API blanks it and keeps the stored one.
	Secret string `json:"secret,omitempty"`
	TUIC   TUIC   `json:"tuic"`
}

// Choices offered by the UI.
var (
	CongestionControls = []string{"bbr", "cubic", "new_reno"}
	UDPRelayModes      = []string{"native", "quic"}
)

// Defaults returns TUIC on UDP 8443 with BBR and a new secret.
func Defaults() (Settings, error) {
	s := Settings{TUIC: TUIC{
		Enabled: true, Port: 8443,
		CongestionControl: "bbr", UDPRelayMode: "native",
		Heartbeat: 10, AuthTimeout: 3,
	}}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return s, err
	}
	s.Secret = base64.RawURLEncoding.EncodeToString(b)
	return s, nil
}

// Password is a user's TUIC password: an HMAC of the UUID, so it changes
// together with the UUID when the admin issues new links.
func (s Settings) Password(uuid string) string {
	m := hmac.New(sha256.New, []byte(s.Secret))
	m.Write([]byte("tuic:" + uuid))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))[:24]
}

// Normalize trims input.
func (s *Settings) Normalize() {
	t := &s.TUIC
	t.CongestionControl = strings.ToLower(strings.TrimSpace(t.CongestionControl))
	t.UDPRelayMode = strings.ToLower(strings.TrimSpace(t.UDPRelayMode))
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
	if len(s.Secret) < 32 {
		return errors.New("TUIC: нет секрета для паролей")
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
	var s Settings
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return Settings{}, fmt.Errorf("настройки sing-box повреждены: %w", err)
	}
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
