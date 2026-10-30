package singbox

import (
	"encoding/json"
	"net/url"
	"strconv"
	"strings"

	"github.com/Stilltiger45/TigerSB-panel/internal/core"
	"github.com/Stilltiger45/TigerSB-panel/internal/edge"
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

// ServingNaive is Serving for NaïveProxy, which needs the same.
func ServingNaive(s Settings, domain string, haveCert bool) bool {
	return s.Naive.Enabled && domain != "" && haveCert
}

// tlsConf is the server side TLS of the protocols, with their ALPN.
func tlsConf(domain string, alpn ...string) obj {
	return obj{
		"enabled": true, "server_name": domain, "alpn": alpn,
		"certificate_path": mountDir + "/" + certFile,
		"key_path":         mountDir + "/" + keyFile,
	}
}

// Render builds the sing-box config. Users are named like in Xray (xray.Name).
// Logs are off, so no client address is written anywhere. Connections to
// private and loopback addresses are refused, which keeps the panel, the
// Xray API and the server's neighbours out of reach through the VPN.
func Render(s Settings, users []store.User, domain string, haveCert bool) ([]byte, error) {
	inbounds := []obj{}
	if Serving(s, domain, haveCert) {
		t := s.TUIC
		list := []obj{}
		for _, u := range users {
			if xray.Serves(u, TUICTag) {
				list = append(list, obj{"name": xray.Name(u), "uuid": u.UUID, "password": s.Password(u.UUID)})
			}
		}
		in := obj{
			"type": "tuic", "tag": TUICTag, "listen": "::", "listen_port": t.Port,
			"users":              list,
			"congestion_control": t.CongestionControl,
			"auth_timeout":       seconds(t.AuthTimeout),
			"zero_rtt_handshake": t.ZeroRTT,
			"tls":                tlsConf(domain, "h3"),
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
	if ServingNaive(s, domain, haveCert) {
		inbounds = append(inbounds, naiveInbounds(s, users, domain)...)
	}
	return marshal(inbounds)
}

// marshal is the config around inbounds, the same for sing-box and mbox.
func marshal(inbounds []obj) ([]byte, error) {
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

// RenderMieru builds the mbox config: a Mieru inbound on each port of the
// range, all with the same users. Mieru needs no domain or certificate.
func RenderMieru(s Settings, users []store.User) ([]byte, error) {
	inbounds := []obj{}
	m := s.Mieru
	if m.Enabled {
		list := []obj{}
		for _, u := range users {
			if xray.Serves(u, MieruTag) {
				list = append(list, obj{"name": xray.Name(u), "password": s.MieruPassword(u.UUID)})
			}
		}
		if len(list) == 0 {
			name, pw := s.Placeholder()
			list = append(list, obj{"name": name, "password": pw})
		}
		for _, tr := range m.Transports() {
			for port := m.PortFrom; port <= m.PortTo; port++ {
				inbounds = append(inbounds, obj{
					"type": "mieru", "tag": MieruTag + "-" + strings.ToLower(tr) + "-" + strconv.Itoa(port), "listen": "::", "listen_port": port,
					"transport": tr,
					"users":     list,
				})
			}
		}
	}
	return marshal(inbounds)
}

// hy2Inbound is the Hysteria 2 inbound. Without Salamander a stranger who
// speaks HTTP/3 to the port gets the site's stub page through the domain;
// with it, nothing but clients with the password get any answer.
func hy2Inbound(s Settings, users []store.User, domain string) obj {
	h := s.Hysteria2
	list := []obj{}
	for _, u := range users {
		if xray.Serves(u, Hy2Tag) {
			list = append(list, obj{"name": xray.Name(u), "password": s.Hy2Password(u.UUID)})
		}
	}
	in := obj{
		"type": "hysteria2", "tag": Hy2Tag, "listen": "::", "listen_port": h.Port,
		"users":                   list,
		"bbr_profile":             h.BBRProfile,
		"ignore_client_bandwidth": h.IgnoreClientBandwidth,
		"tls":                     tlsConf(domain, "h3"),
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

// naiveInbounds are the NaïveProxy inbounds: HTTP/2 over TCP, as Chrome
// speaks it, and HTTP/3 over QUIC. sing-box closes anything but a CONNECT
// with padding and the right password, so the port does not proxy for
// strangers. The TCP one listens on loopback behind the edge, which holds
// the public port and sets BBR there (Forwards): sing-box has no option
// for the TCP congestion control.
func naiveInbounds(s Settings, users []store.User, domain string) []obj {
	n := s.Naive
	list := []obj{}
	for _, u := range users {
		if xray.Serves(u, NaiveTag) {
			list = append(list, obj{"username": xray.Name(u), "password": s.NaivePassword(u.UUID)})
		}
	}
	if len(list) == 0 {
		name, pw := s.Placeholder()
		list = append(list, obj{"username": name, "password": pw})
	}
	var out []obj
	if n.TCP() {
		out = append(out, obj{
			"type": "naive", "tag": NaiveTag, "listen": "127.0.0.1", "listen_port": xray.NaiveInner,
			"network": "tcp",
			"users":   list,
			"tls":     tlsConf(domain, "h2", "http/1.1"),
		})
	}
	if n.QUIC() {
		cc := "cubic"
		if n.BBR {
			cc = "bbr"
		}
		out = append(out, obj{
			"type": "naive", "tag": NaiveTag + "-quic", "listen": "::", "listen_port": n.Port,
			"network":                 "udp",
			"users":                   list,
			"quic_congestion_control": cc,
			"tls":                     tlsConf(domain, "h3"),
		})
	}
	return out
}

// Forwards are the ports the edge holds for sing-box: NaïveProxy over TCP
// while it is served.
func Forwards(s Settings, domain string, haveCert bool) []edge.Forward {
	if !ServingNaive(s, domain, haveCert) || !s.Naive.TCP() {
		return nil
	}
	return []edge.Forward{{
		Listen:  ":" + strconv.Itoa(s.Naive.Port),
		Backend: edge.Backend{Addr: "127.0.0.1:" + strconv.Itoa(xray.NaiveInner), Congestion: xray.Congestion(s.Naive.BBR)},
	}}
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

// NaiveLinks returns the NaïveProxy links of a user when it is served, in
// the form NekoBox, v2rayN and Hiddify read: naive+https:// for TCP and
// naive+quic:// for QUIC. The user name is the one inside sing-box, padding
// is on.
func NaiveLinks(s Settings, address, domain string, haveCert bool, u store.User) []core.Link {
	if !ServingNaive(s, domain, haveCert) {
		return nil
	}
	host := address
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	link := func(scheme, title string) core.Link {
		return core.Link{Protocol: NaiveTag, Title: title, URL: scheme + "://" + url.QueryEscape(xray.Name(u)) + ":" + url.QueryEscape(s.NaivePassword(u.UUID)) +
			"@" + host + ":" + strconv.Itoa(s.Naive.Port) +
			"?sni=" + url.QueryEscape(domain) + "&padding=true#" + url.PathEscape(title)}
	}
	var out []core.Link
	if s.Naive.TCP() {
		out = append(out, link("naive+https", u.Name+" · NaïveProxy"))
	}
	if s.Naive.QUIC() {
		out = append(out, link("naive+quic", u.Name+" · NaïveProxy QUIC"))
	}
	return out
}

// IsNaiveQUIC tells a naive+quic:// link.
func IsNaiveQUIC(l core.Link) bool { return strings.HasPrefix(l.URL, "naive+quic://") }

// MieruLinks returns the mierus:// links of a user when Mieru is on, in the
// form "mieru export config simple" writes: one link, or with both
// transports one for TCP and one for UDP, so the app picks the one that
// works on its network. The address may be an IP: Mieru has no certificate.
func MieruLinks(s Settings, address string, u store.User) []core.Link {
	m := s.Mieru
	if !m.Enabled || address == "" {
		return nil
	}
	host := address
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	ports := strconv.Itoa(m.PortFrom)
	if m.PortTo > m.PortFrom {
		ports += "-" + strconv.Itoa(m.PortTo)
	}
	var out []core.Link
	for _, tr := range m.Transports() {
		title := u.Name + " · Mieru"
		if m.Transport == MieruBoth && tr == MieruUDP {
			title += " UDP"
		}
		link := "mierus://" + url.QueryEscape(xray.Name(u)) + ":" + url.QueryEscape(s.MieruPassword(u.UUID)) + "@" + host +
			"?profile=TigerSB&port=" + ports + "&protocol=" + tr + "#" + url.PathEscape(title)
		out = append(out, core.Link{Protocol: MieruTag, Title: title, URL: link})
	}
	return out
}

// IsMieruUDP tells a mierus:// link over UDP.
func IsMieruUDP(l core.Link) bool {
	return strings.HasPrefix(l.URL, "mierus://") && strings.Contains(l.URL, "&protocol="+MieruUDP)
}
