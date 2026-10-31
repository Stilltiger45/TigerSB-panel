package xray

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"
)

// A good SNI for REALITY is a real site that speaks TLS 1.3 with HTTP/2 and
// X25519 key exchange, answers fast from this server and, best of all, lives
// in the same network: then the traffic looks like a visit to a neighbour.

// Probe is what one TLS handshake showed.
type Probe struct {
	Addr    string `json:"addr"`
	Domain  string `json:"domain"`
	TLS13   bool   `json:"tls13"`
	H2      bool   `json:"h2"`
	X25519  bool   `json:"x25519"`
	Group   string `json:"group,omitempty"`
	Latency int64  `json:"latency_ms"`
	Issuer  string `json:"issuer,omitempty"`
	CertOK  bool   `json:"cert_ok"`
	Good    bool   `json:"good"` // fits REALITY
	Error   string `json:"error,omitempty"`
}

func groupOK(id tls.CurveID) bool {
	return id == tls.X25519 || id == tls.X25519MLKEM768
}

func handshake(ctx context.Context, addr, sni string, timeout time.Duration) (*tls.ConnectionState, time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, 0, err
	}
	defer conn.Close()
	rtt := time.Since(start)
	c := tls.Client(conn, &tls.Config{
		ServerName:         sni,
		InsecureSkipVerify: true, // the chain is checked below, by name when there is one
		NextProtos:         []string{"h2", "http/1.1"},
		MinVersion:         tls.VersionTLS12,
	})
	if err := c.HandshakeContext(ctx); err != nil {
		return nil, rtt, err
	}
	st := c.ConnectionState()
	return &st, rtt, nil
}

func describe(p *Probe, st *tls.ConnectionState, rtt time.Duration) {
	p.Latency = rtt.Milliseconds()
	p.TLS13 = st.Version == tls.VersionTLS13
	p.H2 = st.NegotiatedProtocol == "h2"
	p.X25519 = groupOK(st.CurveID)
	if st.CurveID != 0 {
		p.Group = st.CurveID.String()
	}
	if len(st.PeerCertificates) > 0 {
		leaf := st.PeerCertificates[0]
		p.Issuer = leaf.Issuer.CommonName
		if len(leaf.Issuer.Organization) > 0 {
			p.Issuer = leaf.Issuer.Organization[0]
		}
		if p.Domain == "" {
			p.Domain = certDomain(leaf)
		}
		pool := x509.NewCertPool()
		for _, c := range st.PeerCertificates[1:] {
			pool.AddCert(c)
		}
		_, err := leaf.Verify(x509.VerifyOptions{DNSName: p.Domain, Intermediates: pool})
		p.CertOK = err == nil
	}
	p.Good = p.TLS13 && p.H2 && p.X25519 && p.CertOK
}

// certDomain picks a name a client could send as SNI: the first SAN without
// a wildcard, or the common name.
func certDomain(c *x509.Certificate) string {
	for _, n := range c.DNSNames {
		if !strings.Contains(n, "*") {
			return strings.ToLower(n)
		}
	}
	if !strings.Contains(c.Subject.CommonName, "*") && domainRe.MatchString(strings.ToLower(c.Subject.CommonName)) {
		return strings.ToLower(c.Subject.CommonName)
	}
	return ""
}

// CheckSNI connects from this server with domain as SNI, as REALITY will:
// to target when it is set (host:port), to domain:443 when it is not.
func CheckSNI(ctx context.Context, domain, target string) Probe {
	domain = strings.TrimSuffix(lower(domain), ".")
	p := Probe{Domain: domain, Addr: net.JoinHostPort(domain, "443")}
	if !domainRe.MatchString(domain) {
		p.Error = "нужен домен сайта, например www.microsoft.com"
		return p
	}
	if target = strings.TrimSpace(target); target != "" {
		if _, port, err := net.SplitHostPort(target); err != nil || port == "" {
			p.Error = "target должен быть адресом с портом, например 1.2.3.4:443"
			return p
		}
		p.Addr = target
	}
	st, rtt, err := handshake(ctx, p.Addr, domain, 6*time.Second)
	if err != nil {
		p.Error = "не удалось подключиться: " + err.Error()
		return p
	}
	describe(&p, st, rtt)
	return p
}

// MaxScan caps how many addresses one scan touches.
const MaxScan = 512

// scanTargets turns an IP, a CIDR or a domain into addresses. A single IP
// or a domain means its /24 (IPv6: /120), where neighbours live.
func scanTargets(ctx context.Context, target string) ([]netip.Addr, error) {
	target = strings.TrimSpace(target)
	var prefix netip.Prefix
	switch {
	case strings.Contains(target, "/"):
		p, err := netip.ParsePrefix(target)
		if err != nil {
			return nil, fmt.Errorf("неверная подсеть %q", target)
		}
		prefix = p.Masked()
	default:
		addr, err := netip.ParseAddr(target)
		if err != nil {
			if !domainRe.MatchString(lower(target)) {
				return nil, fmt.Errorf("укажите IP, подсеть или домен, а не %q", target)
			}
			ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", lower(target))
			if err != nil || len(ips) == 0 {
				return nil, fmt.Errorf("домен %s не найден в DNS", target)
			}
			addr = ips[0]
		}
		bits := 24
		if addr.Is6() && !addr.Is4In6() {
			bits = 120
		}
		prefix, _ = addr.Unmap().Prefix(bits)
	}
	size := prefix.Addr().BitLen() - prefix.Bits()
	if size > 9 {
		return nil, fmt.Errorf("подсеть %s слишком большая: не больше %d адресов за раз", prefix, MaxScan)
	}
	var out []netip.Addr
	for a := prefix.Addr(); prefix.Contains(a) && len(out) < MaxScan; a = a.Next() {
		out = append(out, a)
	}
	// Skip the network and broadcast addresses of an IPv4 /24-like range.
	if prefix.Addr().Is4() && len(out) > 2 {
		out = out[1 : len(out)-1]
	}
	return out, nil
}

// sniNames lists the names a client could send as SNI for this certificate:
// the SANs without a wildcard, then the common name, at most three.
func sniNames(c *x509.Certificate) []string {
	var out []string
	add := func(n string) {
		n = strings.ToLower(n)
		if strings.Contains(n, "*") || !domainRe.MatchString(n) || slices.Contains(out, n) || len(out) >= 3 {
			return
		}
		out = append(out, n)
	}
	for _, n := range c.DNSNames {
		add(n)
	}
	add(c.Subject.CommonName)
	return out
}

// pickSNI shakes hands with addr again, now with each name from its
// certificate as SNI, the way REALITY will forward a client, and keeps the
// first name the site fits for. Without one, the first name's result stays.
func pickSNI(ctx context.Context, addr string, names []string) (Probe, bool) {
	var first Probe
	ok := false
	for _, n := range names {
		st, rtt, err := handshake(ctx, addr, n, 3*time.Second)
		if err != nil {
			continue
		}
		p := Probe{Addr: addr, Domain: n}
		describe(&p, st, rtt)
		if p.Good {
			return p, true
		}
		if !ok {
			first, ok = p, true
		}
	}
	return first, ok
}

// Scan looks for REALITY-friendly sites among the addresses of target (the
// idea of RealiTLScanner, MPL-2.0) on each of ports: it shakes hands without
// SNI, reads which site the certificate belongs to, then picks the name the
// site answers best to. Addresses in skip (this server itself) and those
// that did not answer are left out. Results per port come back best first.
func Scan(ctx context.Context, target string, ports []uint16, skip []netip.Addr) (map[uint16][]Probe, error) {
	addrs, err := scanTargets(ctx, target)
	if err != nil {
		return nil, err
	}
	addrs = slices.DeleteFunc(addrs, func(a netip.Addr) bool { return slices.Contains(skip, a) })
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	var mu sync.Mutex
	out := map[uint16][]Probe{}
	for _, port := range ports {
		out[port] = []Probe{}
	}
	jobs := make(chan netip.AddrPort)
	var wg sync.WaitGroup
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ap := range jobs {
				addr := ap.String()
				st, _, err := handshake(ctx, addr, "", 3*time.Second)
				if err != nil || len(st.PeerCertificates) == 0 {
					continue
				}
				names := sniNames(st.PeerCertificates[0])
				if len(names) == 0 {
					continue
				}
				p, ok := pickSNI(ctx, addr, names)
				if !ok {
					continue
				}
				mu.Lock()
				out[ap.Port()] = append(out[ap.Port()], p)
				mu.Unlock()
			}
		}()
	}
	found := false
send:
	for _, port := range ports {
		for _, a := range addrs {
			select {
			case jobs <- netip.AddrPortFrom(a, port):
			case <-ctx.Done():
				break send
			}
		}
	}
	close(jobs)
	wg.Wait()
	for port, list := range out {
		slices.SortFunc(list, func(a, b Probe) int {
			if a.Good != b.Good {
				if a.Good {
					return -1
				}
				return 1
			}
			return int(a.Latency - b.Latency)
		})
		out[port] = list
		found = found || len(list) > 0
	}
	if !found && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, errors.New("сканирование не уложилось во время")
	}
	return out, nil
}
