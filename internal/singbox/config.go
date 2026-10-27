package singbox

import (
	"encoding/json"
	"net/url"
	"strconv"
	"strings"

	"github.com/Stilltiger45/TigerSB-panel/internal/core"
	"github.com/Stilltiger45/TigerSB-panel/internal/store"
	"github.com/Stilltiger45/TigerSB-panel/internal/xray"
)

type obj = map[string]any

// mountDir is where the config directory appears inside the container; the
// certificate copies lie next to the config.
const mountDir = "/etc/tigersb-singbox"

// Names of the certificate copies in the config directory.
const (
	certFile = "tls.crt"
	keyFile  = "tls.key"
)

// idleTimeout is the QUIC idle timeout of the inbounds, in seconds; see
// Render. Pings of both sides (TUIC heartbeat, Hysteria keep-alive, 10 s)
// keep a quiet connection alive.
const idleTimeout = 12

func seconds(n int) string { return strconv.Itoa(n) + "s" }

// Serving reports whether TUIC goes into the config: it needs the domain
// and Caddy's certificate for it, since clients check the certificate.
func Serving(s Settings, domain string, haveCert bool) bool {
	return s.TUIC.Enabled && domain != "" && haveCert
}

// ServingHy2 is Serving for Hysteria 2, which needs the same.
func ServingHy2(s Settings, domain string, haveCert bool) bool {
	return s.Hysteria2.Enabled && domain != "" && haveCert
}

// tlsConf is the server side TLS of both protocols.
func tlsConf(domain string) obj {
	return obj{
		"enabled": true, "server_name": domain, "alpn": []string{"h3"},
		"certificate_path": mountDir + "/" + certFile,
		"key_path":         mountDir + "/" + keyFile,
	}
}

// Render builds the sing-box config. Users are named like in Xray (u<id>).
// Logs are off, so no client address is written anywhere. Connections to
// private and loopback addresses are refused, which keeps the panel, the
// Xray API and the server's neighbours out of reach through the VPN.
func Render(s Settings, users []store.User, domain string, haveCert bool) ([]byte, error) {
	inbounds := []obj{}
	if Serving(s, domain, haveCert) {
		t := s.TUIC
		list := []obj{}
		for _, u := range users {
			if u.Enabled {
				list = append(list, obj{"name": xray.Email(u.ID), "uuid": u.UUID, "password": s.Password(u.UUID)})
			}
		}
		in := obj{
			"type": "tuic", "tag": TUICTag, "listen": "::", "listen_port": t.Port,
			"users":              list,
			"congestion_control": t.CongestionControl,
			"auth_timeout":       seconds(t.AuthTimeout),
			"zero_rtt_handshake": t.ZeroRTT,
			"tls":                tlsConf(domain),
			// A reload starts a new listener that knows nothing of open QUIC
			// connections and sends no reset, so clients notice only when the
			// idle timeout runs out: 30 s by default, 12 s here.
			"idle_timeout": seconds(idleTimeout),
		}
		if t.Heartbeat > 0 {
			in["heartbeat"] = seconds(t.Heartbeat)
		}
		inbounds = append(inbounds, in)
	}
	if ServingHy2(s, domain, haveCert) {
		inbounds = append(inbounds, hy2Inbound(s, users, domain))
	}
	conf := obj{
		"log":       obj{"disabled": true},
		"inbounds":  inbounds,
		"outbounds": []obj{{"type": "direct", "tag": "direct"}},
		"route": obj{
			"rules": []obj{
				// Resolve names first, so a name of a private address is
				// refused like the address itself.
				{"action": "resolve"},
				{"ip_is_private": true, "action": "reject"},
			},
			"final": "direct",
		},
	}
	return json.MarshalIndent(conf, "", "  ")
}

// hy2Inbound is the Hysteria 2 inbound. Without Salamander a stranger who
// speaks HTTP/3 to the port gets the site's stub page through the domain;
// with it, nothing but clients with the password get any answer.
func hy2Inbound(s Settings, users []store.User, domain string) obj {
	h := s.Hysteria2
	list := []obj{}
	for _, u := range users {
		if u.Enabled {
			list = append(list, obj{"name": xray.Email(u.ID), "password": s.Hy2Password(u.UUID)})
		}
	}
	in := obj{
		"type": "hysteria2", "tag": Hy2Tag, "listen": "::", "listen_port": h.Port,
		"users":                   list,
		"bbr_profile":             h.BBRProfile,
		"ignore_client_bandwidth": h.IgnoreClientBandwidth,
		"tls":                     tlsConf(domain),
		"idle_timeout":            seconds(idleTimeout), // see the TUIC inbound
	}
	if h.UpMbps > 0 {
		in["up_mbps"], in["down_mbps"] = h.UpMbps, h.DownMbps
	}
	if h.Obfs {
		in["obfs"] = obj{"type": "salamander", "password": h.ObfsPassword}
	} else {
		in["masquerade"] = "https://" + domain
	}
	return in
}

// Links returns the tuic:// link of a user when TUIC is served. The address
// may be an IP: sni carries the domain the certificate is for.
func Links(s Settings, address, domain string, haveCert bool, u store.User) []core.Link {
	if !Serving(s, domain, haveCert) {
		return nil
	}
	t := s.TUIC
	host := address
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	q := []string{
		"sni=" + url.QueryEscape(domain),
		"alpn=h3",
		"congestion_control=" + t.CongestionControl,
		"udp_relay_mode=" + t.UDPRelayMode,
	}
	if t.ZeroRTT {
		q = append(q, "reduce_rtt=1")
	}
	title := u.Name + " · TUIC"
	link := "tuic://" + u.UUID + ":" + url.QueryEscape(s.Password(u.UUID)) + "@" + host + ":" + strconv.Itoa(t.Port) +
		"?" + strings.Join(q, "&") + "#" + url.PathEscape(title)
	return []core.Link{{Protocol: TUICTag, Title: title, URL: link}}
}

// Hy2Links returns the hysteria2:// link of a user when Hysteria 2 is
// served, in the form of the official client.
func Hy2Links(s Settings, address, domain string, haveCert bool, u store.User) []core.Link {
	if !ServingHy2(s, domain, haveCert) {
		return nil
	}
	h := s.Hysteria2
	host := address
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	q := []string{"sni=" + url.QueryEscape(domain)}
	if h.Obfs {
		q = append(q, "obfs=salamander", "obfs-password="+url.QueryEscape(h.ObfsPassword))
	}
	title := u.Name + " · Hysteria 2"
	link := "hysteria2://" + url.QueryEscape(s.Hy2Password(u.UUID)) + "@" + host + ":" + strconv.Itoa(h.Port) +
		"/?" + strings.Join(q, "&") + "#" + url.PathEscape(title)
	return []core.Link{{Protocol: Hy2Tag, Title: title, URL: link}}
}
