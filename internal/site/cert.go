package site

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"time"
)

// Cert states shown in the panel.
const (
	CertOK        = "ok"        // trusted and not close to expiry
	CertExpiring  = "expiring"  // trusted, less than 14 days left (Caddy renews at ~30)
	CertUntrusted = "untrusted" // a certificate is served but browsers would reject it
	CertMissing   = "missing"   // nothing served yet: Caddy is still obtaining it or failed
	CertNoDomain  = "no_domain" // no domain, the page is served over plain HTTP
)

// CertStatus describes the certificate Caddy serves for the domain.
type CertStatus struct {
	State    string    `json:"state"`
	Issuer   string    `json:"issuer,omitempty"`
	NotAfter time.Time `json:"not_after,omitzero"`
	DaysLeft int       `json:"days_left"`
	Error    string    `json:"error,omitempty"`
}

// CheckCert connects to addr (normally 127.0.0.1:443) with the domain as SNI
// and inspects the served certificate. roots nil means the system roots.
func CheckCert(ctx context.Context, domain, addr string, roots *x509.CertPool, now time.Time) CertStatus {
	if domain == "" {
		return CertStatus{State: CertNoDomain}
	}
	d := tls.Dialer{Config: &tls.Config{ServerName: domain, InsecureSkipVerify: true}}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return CertStatus{State: CertMissing, Error: err.Error()}
	}
	defer conn.Close()
	certs := conn.(*tls.Conn).ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return CertStatus{State: CertMissing, Error: "сервер не прислал сертификат"}
	}
	leaf := certs[0]
	st := CertStatus{
		Issuer:   issuerName(leaf),
		NotAfter: leaf.NotAfter,
		DaysLeft: int(leaf.NotAfter.Sub(now).Hours() / 24),
	}
	inter := x509.NewCertPool()
	for _, c := range certs[1:] {
		inter.AddCert(c)
	}
	_, err = leaf.Verify(x509.VerifyOptions{DNSName: domain, Roots: roots, Intermediates: inter, CurrentTime: now})
	switch {
	case err != nil:
		st.State, st.Error = CertUntrusted, err.Error()
	case st.DaysLeft < 14:
		st.State = CertExpiring
	default:
		st.State = CertOK
	}
	return st
}

func issuerName(c *x509.Certificate) string {
	name := c.Issuer.CommonName
	if len(c.Issuer.Organization) > 0 {
		if name != "" {
			return c.Issuer.Organization[0] + " (" + name + ")"
		}
		return c.Issuer.Organization[0]
	}
	return name
}

// LocalTLS is where Caddy serves TLS on this machine.
var LocalTLS = net.JoinHostPort("127.0.0.1", "443")
