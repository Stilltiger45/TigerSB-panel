package xray

import (
	"encoding/json"
	"net"
	"strconv"

	"github.com/Stilltiger45/TigerSB-panel/internal/edge"
	"github.com/Stilltiger45/TigerSB-panel/internal/store"
)

// Email is the name a user has inside Xray; stats are keyed by it.
func Email(id int64) string { return "u" + strconv.FormatInt(id, 10) }

// UserID parses an Email back; ok is false for anything else.
func UserID(email string) (int64, bool) {
	if len(email) < 2 || email[0] != 'u' {
		return 0, false
	}
	id, err := strconv.ParseInt(email[1:], 10, 64)
	return id, err == nil && id > 0
}

type obj = map[string]any

// behindEdge reports a protocol that shares 443 through the edge router.
func behindEdge(port int) bool { return port == EdgePort }

func clients(users []store.User) []obj {
	out := []obj{}
	for _, u := range users {
		if u.Enabled {
			out = append(out, obj{"id": u.UUID, "email": Email(u.ID), "level": 0})
		}
	}
	return out
}

func realitySettings(r Reality) obj {
	return obj{
		"show":        false,
		"target":      r.TargetAddr(),
		"xver":        0,
		"serverNames": []string{r.SNI},
		"privateKey":  r.PrivateKey,
		"shortIds":    r.ShortIDs,
	}
}

func inbound(tag string, port, inner int, users []store.User, stream obj) obj {
	in := obj{
		"tag":      tag,
		"port":     port,
		"protocol": "vless",
		"settings": obj{"clients": clients(users), "decryption": "none"},
		"sniffing": obj{"enabled": true, "destOverride": []string{"http", "tls", "quic"}, "routeOnly": true},
	}
	if behindEdge(port) {
		in["listen"] = "127.0.0.1"
		in["port"] = inner
		stream["sockopt"] = obj{"acceptProxyProtocol": true}
	}
	in["streamSettings"] = stream
	return in
}

func putRange(m obj, key, v string) {
	if v != "" {
		m[key] = v
	}
}

func grpcInbound(g GRPC, users []store.User) obj {
	gs := obj{"serviceName": g.ServiceName}
	if g.IdleTimeout > 0 {
		gs["idle_timeout"] = g.IdleTimeout
	}
	if g.HealthCheckTimeout > 0 {
		gs["health_check_timeout"] = g.HealthCheckTimeout
	}
	return inbound(GRPCTag, g.Port, GRPCInner, users, obj{
		"network":         "grpc",
		"security":        "reality",
		"realitySettings": realitySettings(g.Reality),
		"grpcSettings":    gs,
	})
}

func xhttpInbound(x XHTTP, users []store.User) obj {
	xs := obj{"path": x.Path, "mode": x.Mode, "scMaxBufferedPosts": x.ScMaxBufferedPosts}
	if x.Host != "" {
		xs["host"] = x.Host
	}
	if x.NoSSEHeader {
		xs["noSSEHeader"] = true
	}
	putRange(xs, "xPaddingBytes", x.XPaddingBytes)
	putRange(xs, "scMaxEachPostBytes", x.ScMaxEachPostBytes)
	putRange(xs, "scStreamUpServerSecs", x.ScStreamUpServerSecs)
	return inbound(XHTTPTag, x.Port, XHTTPInner, users, obj{
		"network":         "xhttp",
		"security":        "reality",
		"realitySettings": realitySettings(x.Reality),
		"xhttpSettings":   xs,
	})
}

// Render builds the Xray config: the enabled protocols with every enabled
// user, the API on loopback for live user changes and stats, and a direct
// exit. Xray itself refuses to connect VLESS users to private and loopback
// addresses, so the panel and the API stay out of reach through the VPN.
func Render(s Settings, users []store.User) ([]byte, error) {
	inbounds := []obj{}
	if s.GRPC.Enabled {
		inbounds = append(inbounds, grpcInbound(s.GRPC, users))
	}
	if s.XHTTP.Enabled {
		inbounds = append(inbounds, xhttpInbound(s.XHTTP, users))
	}
	conf := obj{
		"log":   obj{"loglevel": "warning", "access": "none"},
		"api":   obj{"tag": "api", "listen": APIAddr, "services": []string{"HandlerService", "StatsService"}},
		"stats": obj{},
		"policy": obj{
			"levels": obj{"0": obj{"statsUserUplink": true, "statsUserDownlink": true, "statsUserOnline": true}},
		},
		"inbounds": inbounds,
		"outbounds": []obj{
			{"tag": "direct", "protocol": "freedom"},
			{"tag": "block", "protocol": "blackhole"},
		},
	}
	return json.MarshalIndent(conf, "", "  ")
}

// Routes tells the edge router where each name on 443 goes: the panel's
// domain to Caddy, each protocol's SNI on 443 to its inbound. Unknown names
// go to XHTTP (or gRPC) on 443, so REALITY shows the borrowed site; without
// a protocol on 443 they go to Caddy.
func Routes(s Settings, domain string) edge.Routes {
	r := edge.Routes{SNI: map[string]edge.Backend{}}
	loop := func(port int) string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) }
	if domain != "" {
		// Caddy reads the PROXY header, so subscriptions see the device's address.
		caddy := edge.Backend{Addr: loop(TLSPort), Proxy: true}
		r.SNI[domain] = caddy
		r.Default = &caddy
	}
	if s.GRPC.Enabled && behindEdge(s.GRPC.Port) {
		b := edge.Backend{Addr: loop(GRPCInner), Proxy: true}
		r.SNI[s.GRPC.Reality.SNI] = b
		r.Default = &b
	}
	if s.XHTTP.Enabled && behindEdge(s.XHTTP.Port) {
		b := edge.Backend{Addr: loop(XHTTPInner), Proxy: true}
		r.SNI[s.XHTTP.Reality.SNI] = b
		r.Default = &b
	}
	return r
}
