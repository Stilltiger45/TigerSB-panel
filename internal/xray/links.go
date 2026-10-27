package xray

import (
	"encoding/json"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/Stilltiger45/TigerSB-panel/internal/core"
	"github.com/Stilltiger45/TigerSB-panel/internal/store"
)

// Link is one ready-to-import connection of a user.
type Link = core.Link

// PublicAddress picks the address for links: the admin's choice, else the
// first public IP of the server, else the panel's domain.
func PublicAddress(s Settings, ips []net.IP, domain string) string {
	if s.Address != "" {
		return s.Address
	}
	var v6 string
	for _, ip := range ips {
		if !ip.IsGlobalUnicast() || ip.IsPrivate() {
			continue
		}
		if ip.To4() != nil {
			return ip.String()
		}
		if v6 == "" {
			v6 = ip.String()
		}
	}
	if v6 != "" {
		return v6
	}
	return domain
}

// query keeps parameters in a fixed order, which makes links stable.
type query []string

func (q *query) add(k, v string) {
	if v != "" {
		*q = append(*q, k+"="+url.QueryEscape(v))
	}
}

func realityQuery(q *query, r Reality) {
	q.add("security", "reality")
	q.add("sni", r.SNI)
	q.add("fp", r.Fingerprint)
	q.add("pbk", r.PublicKey)
	if len(r.ShortIDs) > 0 {
		q.add("sid", r.ShortIDs[0])
	}
	q.add("spx", r.SpiderX)
}

func vless(u store.User, address string, port int, q query, title string) string {
	host := address
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return "vless://" + u.UUID + "@" + host + ":" + strconv.Itoa(port) +
		"?" + strings.Join(q, "&") + "#" + url.PathEscape(title)
}

// xhttpExtra holds the XHTTP client settings that have no link parameter of
// their own; clients read them from the "extra" JSON.
func xhttpExtra(x XHTTP) string {
	e := obj{"uplinkHTTPMethod": x.UplinkHTTPMethod}
	putRange(e, "xPaddingBytes", x.XPaddingBytes)
	putRange(e, "scMaxEachPostBytes", x.ScMaxEachPostBytes)
	putRange(e, "scMinPostsIntervalMs", x.ScMinPostsIntervalMs)
	if x.NoSSEHeader {
		e["noSSEHeader"] = true
	}
	m := obj{}
	putRange(m, "maxConcurrency", x.Xmux.MaxConcurrency)
	putRange(m, "maxConnections", x.Xmux.MaxConnections)
	putRange(m, "cMaxReuseTimes", x.Xmux.CMaxReuseTimes)
	putRange(m, "hMaxRequestTimes", x.Xmux.HMaxRequestTimes)
	putRange(m, "hMaxReusableSecs", x.Xmux.HMaxReusableSecs)
	if x.Xmux.HKeepAlivePeriod > 0 {
		m["hKeepAlivePeriod"] = x.Xmux.HKeepAlivePeriod
	}
	if len(m) > 0 {
		e["xmux"] = m
	}
	b, _ := json.Marshal(e)
	return string(b)
}

// Links returns a vless:// link for every enabled protocol. Disabled users
// get links too, so they work again once the user is turned back on.
func Links(s Settings, address string, u store.User) []Link {
	var out []Link
	if s.GRPC.Enabled {
		g := s.GRPC
		q := query{"encryption=none"}
		realityQuery(&q, g.Reality)
		q.add("type", "grpc")
		q.add("serviceName", g.ServiceName)
		q.add("authority", g.Authority)
		mode := "gun"
		if g.MultiMode {
			mode = "multi"
		}
		q.add("mode", mode)
		title := u.Name + " · gRPC"
		out = append(out, Link{Protocol: GRPCTag, Title: title, URL: vless(u, address, g.Port, q, title)})
	}
	if s.XHTTP.Enabled {
		x := s.XHTTP
		q := query{"encryption=none"}
		realityQuery(&q, x.Reality)
		q.add("type", "xhttp")
		q.add("path", x.Path)
		q.add("host", x.Host)
		q.add("mode", x.Mode)
		q.add("extra", xhttpExtra(x))
		title := u.Name + " · XHTTP"
		out = append(out, Link{Protocol: XHTTPTag, Title: title, URL: vless(u, address, x.Port, q, title)})
	}
	return out
}

// LocalIPs lists the addresses of this server's interfaces.
func LocalIPs() ([]net.IP, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	var out []net.IP
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			out = append(out, n.IP)
		}
	}
	return out, nil
}
