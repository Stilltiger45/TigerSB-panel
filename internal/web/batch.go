package web

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/Stilltiger45/TigerSB-panel/internal/singbox"
)

// ApplyDelay is how long user changes wait for sing-box and mbox. Neither
// has a user API: each change rereads sing-box, which drops the TUIC and
// Hysteria 2 streams and the NaïveProxy connections of everyone, and
// restarts mbox. So changes made within this time go in together, with one
// reload. Xray takes users live and does not wait, except to end the open
// connections of removed users, which takes a restart and waits too.
const ApplyDelay = 30 * time.Second

// batch holds user changes that wait for ApplyDelay.
type batch struct {
	at    time.Time       // when they apply; zero when nothing waits
	timer *time.Timer     //
	ended map[string]bool // Xray names whose open connections end then
}

// later queues the cores for a reload, and the open Xray connections of
// names to end, at the next batch. Without a delay they apply at once, and
// the warning tells what went wrong. The caller holds xrayMu.
func (s *Server) later(ctx context.Context, names ...string) string {
	if s.applyDelay <= 0 {
		for _, n := range names {
			s.pending.ended[n] = true
		}
		return s.apply(ctx)
	}
	s.pendMu.Lock()
	defer s.pendMu.Unlock()
	for _, n := range names {
		s.pending.ended[n] = true
	}
	if s.pending.at.IsZero() {
		s.pending.at = s.now().Add(s.applyDelay)
		s.pending.timer = time.AfterFunc(s.applyDelay, func() {
			s.xrayMu.Lock()
			defer s.xrayMu.Unlock()
			s.apply(context.Background())
		})
	}
	return ""
}

// waiting returns how long until the batch applies, 0 when nothing waits.
func (s *Server) waiting() time.Duration {
	s.pendMu.Lock()
	defer s.pendMu.Unlock()
	if s.pending.at.IsZero() {
		return 0
	}
	return max(s.pending.at.Sub(s.now()), time.Second)
}

// apply puts the waiting changes into the cores: sing-box rereads its
// config and mbox restarts, if the users in them changed, and Xray restarts
// when a removed user still has open connections. The caller holds xrayMu.
func (s *Server) apply(ctx context.Context) string {
	s.pendMu.Lock()
	if s.pending.timer != nil {
		s.pending.timer.Stop()
	}
	ended := s.pending.ended
	s.pending = batch{ended: map[string]bool{}}
	s.pendMu.Unlock()

	warning := ""
	if domain, err := s.domain(ctx); err != nil {
		log.Printf("sing-box: %v", err)
	} else {
		for _, rt := range []*singbox.Runtime{s.Singbox, s.Mieru} {
			if err := rt.Sync(ctx, s.store, domain, true); err != nil {
				log.Printf("%s: %v", rt.Name, err)
				warning = "Изменение сохранено, но " + err.Error() + "."
			}
		}
	}
	if len(ended) == 0 || s.xrayState() != "active" {
		return warning
	}
	// The removed users may have gone offline meanwhile.
	for name := range ended {
		if n, err := s.Xray.Online(ctx, name); err != nil || n > 0 {
			if err := s.Xray.Restart(ctx); err != nil {
				log.Printf("xray: %v", err)
				warning = "Изменение сохранено, но открытые соединения не закрылись: " + err.Error() + "."
			}
			break
		}
	}
	return warning
}

// Flush applies the waiting changes now, for shutdown.
func (s *Server) Flush(ctx context.Context) {
	if s.waiting() == 0 {
		return
	}
	s.xrayMu.Lock()
	defer s.xrayMu.Unlock()
	if w := s.apply(ctx); w != "" {
		log.Print(w)
	}
}

// handleUsersApply applies the waiting changes without waiting.
func (s *Server) handleUsersApply(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r, true) {
		return
	}
	s.xrayMu.Lock()
	warning := ""
	if s.waiting() > 0 {
		warning = s.apply(r.Context())
	}
	s.xrayMu.Unlock()
	s.respondUsers(w, r.Context(), warning)
}
