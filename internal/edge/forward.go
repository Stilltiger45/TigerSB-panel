package edge

import (
	"encoding/json"
	"errors"
	"log"
	"net"
	"os"
	"slices"
	"sync"
	"time"
)

// Forward is a TCP port the edge holds for a protocol whose core cannot set
// the congestion control itself: NaïveProxy in sing-box. Connections go to
// Backend whole, without reading anything; Backend.Congestion is set on
// the client's side, like on 443.
type Forward struct {
	Listen string `json:"listen"` // ":2443"
	Backend
	// Gen names what the backend serves now. When it changes, the open
	// connections of the port are closed: sing-box keeps NaïveProxy's
	// connections over a reload, but they open no more streams, and the
	// clients go on using them instead of connecting again.
	Gen string `json:"gen,omitempty"`
}

// WriteForwards saves the forwarded ports atomically; the edge opens and
// closes them within seconds.
func WriteForwards(path string, f []Forward) error {
	if f == nil {
		f = []Forward{}
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, b)
}

// ReadForwards loads the forwarded ports from path.
func ReadForwards(path string) ([]Forward, error) {
	var f []Forward
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return f, json.Unmarshal(b, &f)
}

// forwarder is one open forwarded port.
type forwarder struct {
	ln    net.Listener
	mu    sync.RWMutex
	b     Backend
	gen   string
	conns map[net.Conn]struct{}
}

// track adds or removes an open connection.
func (f *forwarder) track(c net.Conn, open bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if open {
		f.conns[c] = struct{}{}
	} else {
		delete(f.conns, c)
	}
}

// update moves the port to w. A new generation closes the connections to
// the old one; each closes its backend side as it ends.
func (f *forwarder) update(w Forward) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.b = w.Backend
	if w.Gen == f.gen {
		return
	}
	f.gen = w.Gen
	for c := range f.conns {
		c.Close()
	}
	if n := len(f.conns); n > 0 {
		log.Printf("edge: %s: закрыто соединений к прежнему серверу: %d", w.Listen, n)
	}
}

func (f *forwarder) backend() Backend {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.b
}

// forwards opens the ports of the forwards file that are not open yet,
// moves open ones to their new backend and closes the ones gone from it.
// A port that is busy, say while sing-box still holds it after an update,
// is tried again on the next call.
func (s *Server) forwards() {
	var want []Forward
	if s.Forwards != "" {
		if f, err := ReadForwards(s.Forwards); err == nil {
			want = f
		} else if !errors.Is(err, os.ErrNotExist) {
			// A broken file keeps the ports as they are.
			return
		}
	}
	s.fwMu.Lock()
	defer s.fwMu.Unlock()
	if s.open == nil {
		s.open = map[string]*forwarder{}
	}
	for addr, f := range s.open {
		if !slices.ContainsFunc(want, func(w Forward) bool { return w.Listen == addr }) {
			f.ln.Close()
			delete(s.open, addr)
			log.Printf("edge: порт %s закрыт", addr)
		}
	}
	for _, w := range want {
		if f, ok := s.open[w.Listen]; ok {
			f.update(w)
			continue
		}
		ln, err := net.Listen("tcp", w.Listen)
		if err != nil {
			s.fwWarned.Do(func() {
				log.Printf("edge: порт %s пока занят, пробую снова: %v", w.Listen, err)
			})
			continue
		}
		f := &forwarder{ln: ln, b: w.Backend, gen: w.Gen, conns: map[net.Conn]struct{}{}}
		s.open[w.Listen] = f
		log.Printf("edge: слушаю %s → %s", w.Listen, w.Addr)
		go s.serveForward(f)
	}
}

func (s *Server) serveForward(f *forwarder) {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		go func() {
			f.track(c, true)
			defer f.track(c, false)
			s.pass(c, f.backend(), nil)
		}()
	}
}

// CloseForwards closes every forwarded port.
func (s *Server) CloseForwards() {
	s.fwMu.Lock()
	defer s.fwMu.Unlock()
	for addr, f := range s.open {
		f.ln.Close()
		delete(s.open, addr)
	}
}
