package edge

import (
	"bufio"
	"crypto/tls"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// backend accepts one connection at a time and reports its first line and
// whether a TLS record followed.
func backend(t *testing.T) (string, chan [2]string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	got := make(chan [2]string, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				c.SetReadDeadline(time.Now().Add(3 * time.Second))
				r := bufio.NewReader(c)
				b, _ := r.Peek(5)
				first := ""
				if strings.HasPrefix(string(b), "PROXY") {
					first, _ = r.ReadString('\n')
				}
				rest, _ := r.Peek(1)
				got <- [2]string{first, string(rest)}
			}()
		}
	}()
	return ln.Addr().String(), got
}

func TestRoutesBySNI(t *testing.T) {
	caddy, toCaddy := backend(t)
	xray, toXray := backend(t)
	path := filepath.Join(t.TempDir(), "routes.json")
	if err := Write(path, Routes{
		SNI:     map[string]Backend{"vpn.example.com": {Addr: caddy}, "cloudflare.com": {Addr: xray, Proxy: true}},
		Default: &Backend{Addr: xray, Proxy: true},
	}); err != nil {
		t.Fatal(err)
	}
	srv := &Server{Path: path}
	srv.Reload()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go srv.Serve(ln)

	hello := func(sni string) {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Error(err)
			return
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(time.Second))
		tls.Client(c, &tls.Config{ServerName: sni, InsecureSkipVerify: true}).Handshake()
	}
	wait := func(ch chan [2]string, what string) [2]string {
		select {
		case v := <-ch:
			return v
		case <-time.After(3 * time.Second):
			t.Fatalf("%s: nothing arrived", what)
		}
		return [2]string{}
	}

	go hello("vpn.example.com")
	if v := wait(toCaddy, "domain"); v[0] != "" || v[1] != "\x16" {
		t.Errorf("Caddy must get the raw ClientHello without PROXY: %q", v)
	}
	go hello("cloudflare.com")
	if v := wait(toXray, "sni"); !strings.HasPrefix(v[0], "PROXY TCP4 127.0.0.1 127.0.0.1 ") || v[1] != "\x16" {
		t.Errorf("Xray must get PROXY then the ClientHello: %q", v)
	}
	go hello("unknown.example")
	if v := wait(toXray, "default"); v[1] != "\x16" {
		t.Errorf("default: %q", v)
	}

	// Plain HTTP goes to the default too, as the borrowed site would see it.
	c, _ := net.Dial("tcp", ln.Addr().String())
	io.WriteString(c, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")
	if v := wait(toXray, "http"); v[1] != "G" {
		t.Errorf("http: %q", v)
	}
	c.Close()

	// New routes are picked up from the file.
	time.Sleep(10 * time.Millisecond)
	if err := Write(path, Routes{SNI: map[string]Backend{"vpn.example.com": {Addr: caddy}}, Default: &Backend{Addr: caddy}}); err != nil {
		t.Fatal(err)
	}
	srv.Reload()
	go hello("cloudflare.com")
	if v := wait(toCaddy, "reloaded"); v[0] != "" {
		t.Errorf("after reload: %q", v)
	}
}

func TestProxyHeader(t *testing.T) {
	v6 := proxyHeader(&net.TCPAddr{IP: net.ParseIP("2001:db8::1"), Port: 5000}, &net.TCPAddr{IP: net.ParseIP("2001:db8::2"), Port: 443})
	if v6 != "PROXY TCP6 2001:db8::1 2001:db8::2 5000 443\r\n" {
		t.Errorf("%q", v6)
	}
	mapped := proxyHeader(&net.TCPAddr{IP: net.ParseIP("::ffff:198.51.100.1"), Port: 5000}, &net.TCPAddr{IP: net.ParseIP("::ffff:203.0.113.1"), Port: 443})
	if mapped != "PROXY TCP4 198.51.100.1 203.0.113.1 5000 443\r\n" {
		t.Errorf("%q", mapped)
	}
}
