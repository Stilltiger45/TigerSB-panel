// Package edge shares port 443 between the panel's site and the VPN.
//
// It reads the first TLS record of each connection, takes the SNI from it and
// passes the whole connection on untouched: the panel's domain goes to Caddy,
// the SNI of each protocol on 443 goes to its Xray inbound, and anything else
// goes to the default backend, normally XHTTP, whose REALITY then shows the
// borrowed site. TLS is never terminated here. Xray backends get a PROXY
// protocol v1 header so they see the client's real address.
package edge

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Backend is where a connection goes.
type Backend struct {
	Addr  string `json:"addr"`
	Proxy bool   `json:"proxy"` // send a PROXY v1 header first
	// Congestion is the TCP congestion control set on the client's
	// connection, such as "bbr"; empty keeps the kernel's default.
	Congestion string `json:"congestion,omitempty"`
}

// Routes map SNI to backends. Default takes unknown names and clients without SNI.
type Routes struct {
	SNI     map[string]Backend `json:"sni"`
	Default *Backend           `json:"default,omitempty"`
}

// Pick returns the backend for a server name, or nil to close the connection.
func (r Routes) Pick(sni string) *Backend {
	if b, ok := r.SNI[strings.ToLower(sni)]; ok {
		return &b
	}
	return r.Default
}

// Write saves routes atomically; the edge picks them up within seconds.
func Write(path string, r Routes) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, b)
}

func writeAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Read loads routes from path.
func Read(path string) (Routes, error) {
	var r Routes
	b, err := os.ReadFile(path)
	if err != nil {
		return r, err
	}
	return r, json.Unmarshal(b, &r)
}

// Timeouts for a connection that has not finished its ClientHello, and for
// reaching the backend.
const (
	helloTimeout = 10 * time.Second
	dialTimeout  = 5 * time.Second
)

// Server accepts connections and routes them.
type Server struct {
	// Path is the routes file, re-read when it changes.
	Path string
	// Forwards is the file of other ports the edge holds (Forward); empty
	// for none.
	Forwards string

	mu     sync.RWMutex
	routes Routes
	mtime  time.Time

	ccWarned sync.Once

	fwMu     sync.Mutex
	open     map[string]*forwarder
	fwWarned sync.Once
}

// Reload re-reads the routes file when it changed and brings the forwarded
// ports in line with their file. A broken file keeps the previous routes.
func (s *Server) Reload() {
	s.forwards()
	st, err := os.Stat(s.Path)
	if err != nil {
		return
	}
	s.mu.RLock()
	same := st.ModTime().Equal(s.mtime)
	s.mu.RUnlock()
	if same {
		return
	}
	r, err := Read(s.Path)
	if err != nil {
		log.Printf("edge: %s: %v, оставляю прежние маршруты", s.Path, err)
		return
	}
	s.mu.Lock()
	s.routes, s.mtime = r, st.ModTime()
	s.mu.Unlock()
	log.Printf("edge: маршруты загружены (%d SNI, по умолчанию %v)", len(r.SNI), r.Default != nil)
}

func (s *Server) pick(sni string) *Backend {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.routes.Pick(sni)
}

// Serve accepts connections until the listener closes.
func (s *Server) Serve(ln net.Listener) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			return err
		}
		go s.handle(c)
	}
}

// Watch reloads the routes every interval until stop is closed.
func (s *Server) Watch(interval time.Duration, stop <-chan struct{}) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			s.Reload()
		}
	}
}

func (s *Server) handle(c net.Conn) {
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(helloTimeout))
	sni, hello, err := ReadHello(c)
	if err != nil && len(hello) == 0 {
		return
	}
	c.SetReadDeadline(time.Time{})
	// Something that is not TLS goes to the default backend as well, so the
	// port answers it the way the borrowed site would.
	b := s.pick(sni)
	if b == nil {
		return
	}
	s.pass(c, *b, hello)
}

// pass hands the connection to the backend, with what was already read
// from it.
func (s *Server) pass(c net.Conn, b Backend, read []byte) {
	defer c.Close()
	if b.Congestion != "" {
		if err := setCongestion(c, b.Congestion); err != nil {
			s.ccWarned.Do(func() {
				log.Printf("edge: %s не включился, остаётся алгоритм ядра по умолчанию: %v", b.Congestion, err)
			})
		}
	}
	up, err := net.DialTimeout("tcp", b.Addr, dialTimeout)
	if err != nil {
		log.Printf("edge: %s недоступен: %v", b.Addr, err)
		return
	}
	defer up.Close()
	if b.Proxy {
		if _, err := io.WriteString(up, proxyHeader(c.RemoteAddr(), c.LocalAddr())); err != nil {
			return
		}
	}
	if len(read) > 0 {
		if _, err := up.Write(read); err != nil {
			return
		}
	}
	pipe(c, up)
}

// pipe copies both ways and passes half-closes on.
func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		io.Copy(dst, src)
		if tc, ok := dst.(*net.TCPConn); ok {
			tc.CloseWrite()
		} else {
			dst.Close()
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	<-done
}

// proxyHeader is a PROXY protocol v1 line.
func proxyHeader(src, dst net.Addr) string {
	s, sok := src.(*net.TCPAddr)
	d, dok := dst.(*net.TCPAddr)
	if !sok || !dok {
		return "PROXY UNKNOWN\r\n"
	}
	sip, dip := s.IP, d.IP
	family := "TCP4"
	if sip.To4() == nil || dip.To4() == nil {
		family = "TCP6"
		sip, dip = sip.To16(), dip.To16()
	} else {
		sip, dip = sip.To4(), dip.To4()
	}
	return fmt.Sprintf("PROXY %s %s %s %d %d\r\n", family, sip, dip, s.Port, d.Port)
}

// recorder hands reads to the TLS parser and keeps them for replay; it never
// writes, so the client sees nothing until the backend answers.
type recorder struct {
	net.Conn
	buf []byte
}

func (r *recorder) Read(p []byte) (int, error) {
	n, err := r.Conn.Read(p)
	r.buf = append(r.buf, p[:n]...)
	if len(r.buf) > 64<<10 {
		return n, errors.New("слишком длинный ClientHello")
	}
	return n, err
}

func (r *recorder) Write([]byte) (int, error) { return 0, errStop }

var errStop = errors.New("stop")

// ReadHello reads the client's TLS ClientHello and returns its SNI (may be
// empty) and every byte read so far. The standard library parses the hello;
// the handshake is abandoned before anything is sent back.
func ReadHello(c net.Conn) (string, []byte, error) {
	rec := &recorder{Conn: c}
	var sni string
	var seen bool
	err := tls.Server(rec, &tls.Config{
		GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
			sni, seen = h.ServerName, true
			return nil, errStop
		},
	}).Handshake()
	if !seen {
		if err == nil {
			err = errors.New("нет ClientHello")
		}
		return "", rec.buf, err
	}
	return sni, rec.buf, nil
}
