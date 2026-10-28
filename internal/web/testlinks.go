package web

import (
	"context"
	"log"
	"net/http"
	"slices"

	"github.com/Stilltiger45/TigerSB-panel/internal/singbox"
	"github.com/Stilltiger45/TigerSB-panel/internal/store"
	"github.com/Stilltiger45/TigerSB-panel/internal/xray"
)

// testProtocols are the protocols that can have a test link.
var testProtocols = []string{xray.GRPCTag, xray.XHTTPTag, singbox.TUICTag, singbox.Hy2Tag, singbox.NaiveTag, singbox.MieruTag}

// testLinks returns the test link of every protocol that has one, or "" for
// a protocol that is off now: its link works again once it is on.
func (s *Server) testLinks(ctx context.Context, set xray.Settings, sb singbox.Settings, domain string) (map[string]string, error) {
	tests, err := s.store.TestUsers(ctx)
	if err != nil {
		return nil, err
	}
	addr := s.address(ctx, set)
	out := map[string]string{}
	for _, u := range tests {
		links := xray.Links(set, addr, u)
		links = append(links, singbox.Links(sb, addr, domain, s.Singbox.HaveCert(), u)...)
		links = append(links, singbox.Hy2Links(sb, addr, domain, s.Singbox.HaveCert(), u)...)
		links = append(links, singbox.NaiveLinks(sb, addr, domain, s.Singbox.HaveCert(), u)...)
		links = append(links, singbox.MieruLinks(sb, addr, u)...)
		out[u.Only] = ""
		for _, l := range links {
			if l.Protocol == u.Only {
				out[u.Only] = l.URL
			}
		}
	}
	return out, nil
}

// handleTestLink makes a new test link of one protocol, which replaces the
// old one, or deletes it. The link works on that protocol only, so each one
// can be checked alone; it has no subscription and counts no traffic.
func (s *Server) handleTestLink(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r, true) {
		return
	}
	var req struct {
		Protocol string `json:"protocol"`
		Delete   bool   `json:"delete"`
	}
	if !decode(w, r, maxBody, &req) {
		return
	}
	if !slices.Contains(testProtocols, req.Protocol) {
		badRequest(w, "неизвестный протокол")
		return
	}
	ctx := r.Context()
	uuid := ""
	if !req.Delete {
		var err error
		if uuid, err = newUUID(); err != nil {
			internalError(w)
			return
		}
	}
	s.xrayMu.Lock()
	defer s.xrayMu.Unlock()
	if err := s.store.SetTestUser(ctx, req.Protocol, uuid); err != nil {
		internalError(w)
		return
	}
	warning := s.applyTest(ctx, req.Protocol, uuid)
	resp, err := s.protocolsState(ctx)
	if err != nil {
		internalError(w)
		return
	}
	resp.Warning = warning
	writeJSON(w, http.StatusOK, resp)
}

// applyTest tells the running core of a protocol about its new test link.
// Xray swaps it live; sing-box rereads its config like on a user change.
func (s *Server) applyTest(ctx context.Context, protocol, uuid string) string {
	if protocol != xray.GRPCTag && protocol != xray.XHTTPTag {
		rt := s.Singbox
		if protocol == singbox.MieruTag {
			rt = s.Mieru
		}
		domain, err := s.domain(ctx)
		if err == nil {
			err = rt.Sync(ctx, s.store, domain, true)
		}
		if err != nil {
			log.Printf("%s test link: %v", rt.Name, err)
			return "Ссылка сохранена, но " + err.Error() + "."
		}
		return ""
	}
	if err := s.sync(ctx); err != nil {
		log.Printf("xray sync: %v", err)
	}
	if s.xrayState() != "active" {
		return ""
	}
	set, err := xray.Load(ctx, s.store)
	if err == nil {
		err = s.Xray.RemoveTest(ctx, set, protocol)
	}
	if err == nil && uuid != "" {
		err = s.Xray.AddUser(ctx, set, store.User{UUID: uuid, Enabled: true, Only: protocol})
	}
	if err != nil {
		log.Printf("xray test link: %v", err)
		return "Ссылка сохранена, но Xray её не принял на лету: " + err.Error() + ". Она заработает после перезапуска Xray."
	}
	return ""
}

// handleTestQR draws the test link of a protocol as an SVG QR code.
func (s *Server) handleTestQR(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r, false) {
		return
	}
	ctx := r.Context()
	resp, err := s.protocolsState(ctx)
	if err != nil {
		internalError(w)
		return
	}
	link := resp.Tests[r.URL.Query().Get("protocol")]
	if link == "" {
		http.NotFound(w, r)
		return
	}
	svg, err := qrSVG(link)
	if err != nil {
		internalError(w)
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Write(svg)
}
