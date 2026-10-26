package web

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"strconv"

	"github.com/Stilltiger45/TigerSB-panel/internal/service"
	"github.com/Stilltiger45/TigerSB-panel/internal/xray"
)

type protocolsResponse struct {
	Settings     xray.Settings  `json:"settings"`
	AutoAddress  string         `json:"auto_address"`
	Domain       string         `json:"domain"`
	Fingerprints []string       `json:"fingerprints"`
	Modes        []string       `json:"modes"`
	Methods      []string       `json:"methods"`
	Reserved     map[int]string `json:"reserved"`
	Xray         string         `json:"xray"`
	Edge         string         `json:"edge"`
	Warning      string         `json:"warning,omitempty"`
}

func (s *Server) panelPort() int {
	_, p, _ := net.SplitHostPort(s.cfg.Listen)
	n, _ := strconv.Atoi(p)
	return n
}

func (s *Server) protocolsState(ctx context.Context) (*protocolsResponse, error) {
	set, err := xray.Load(ctx, s.store)
	if err != nil {
		return nil, err
	}
	domain, err := s.domain(ctx)
	if err != nil {
		return nil, err
	}
	auto := set
	auto.Address = ""
	return &protocolsResponse{
		Settings:     set,
		AutoAddress:  s.address(ctx, auto),
		Domain:       domain,
		Fingerprints: xray.Fingerprints,
		Modes:        xray.XHTTPModes,
		Methods:      xray.HTTPMethods,
		Reserved:     xray.Reserved(s.panelPort()),
		Xray:         s.xrayState(),
		Edge:         service.UnitState(s.Run, xray.EdgeUnit),
	}, nil
}

// handleProtocols shows (GET) or saves and applies (POST) the protocol
// settings. A save checks the config in a throwaway Xray container first and
// then restarts Xray, so every connection drops for a moment.
func (s *Server) handleProtocols(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.Method == http.MethodGet {
		if !s.authed(w, r, false) {
			return
		}
		resp, err := s.protocolsState(ctx)
		if err != nil {
			log.Printf("protocols: %v", err)
			internalError(w)
			return
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}
	if !s.authed(w, r, true) {
		return
	}
	var set xray.Settings
	if !decode(w, r, 32<<10, &set) {
		return
	}
	set.Normalize()
	domain, err := s.domain(ctx)
	if err != nil {
		internalError(w)
		return
	}
	if err := set.Validate(domain, s.panelPort()); err != nil {
		badRequest(w, err.Error())
		return
	}
	s.xrayMu.Lock()
	defer s.xrayMu.Unlock()
	users, err := s.store.Users(ctx)
	if err != nil {
		internalError(w)
		return
	}
	conf, err := xray.Render(set, users)
	if err != nil {
		internalError(w)
		return
	}
	warning := ""
	if err := s.Xray.Test(ctx, conf); errors.Is(err, xray.ErrNoDocker) {
		warning = "Настройки сохранены, но не проверены: " + err.Error()
	} else if err != nil {
		badRequest(w, err.Error())
		return
	}
	if err := xray.Save(ctx, s.store, set); err != nil {
		internalError(w)
		return
	}
	if err := s.Xray.Write(conf, xray.Routes(set, domain)); err != nil {
		log.Printf("xray write: %v", err)
		internalError(w)
		return
	}
	if warning == "" {
		if err := s.Xray.Restart(ctx); err != nil {
			warning = "Настройки сохранены, но " + err.Error()
		}
	}
	resp, err := s.protocolsState(ctx)
	if err != nil {
		internalError(w)
		return
	}
	resp.Warning = warning
	writeJSON(w, http.StatusOK, resp)
}

// handleKeys makes a new REALITY key pair and short ID for the form; nothing
// is saved until the admin presses "Сохранить".
func (s *Server) handleKeys(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r, true) {
		return
	}
	var rk xray.Reality
	if err := rk.NewKeys(); err != nil {
		internalError(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"private_key": rk.PrivateKey, "public_key": rk.PublicKey, "short_ids": rk.ShortIDs,
	})
}

// handleSNICheck tests a site from this server as a REALITY disguise.
func (s *Server) handleSNICheck(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r, true) {
		return
	}
	var req struct {
		SNI string `json:"sni"`
	}
	if !decode(w, r, maxBody, &req) {
		return
	}
	writeJSON(w, http.StatusOK, xray.CheckSNI(r.Context(), req.SNI))
}

// handleScan looks for disguise sites next to this server, or in the given
// address, subnet or domain.
func (s *Server) handleScan(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r, true) {
		return
	}
	var req struct {
		Target string `json:"target"`
	}
	if !decode(w, r, maxBody, &req) {
		return
	}
	ctx := r.Context()
	if req.Target == "" {
		set, err := xray.Load(ctx, s.store)
		if err != nil {
			internalError(w)
			return
		}
		set.Address = ""
		req.Target = s.address(ctx, set)
	}
	if req.Target == "" {
		badRequest(w, "не удалось узнать IP сервера: укажите адрес или подсеть")
		return
	}
	results, err := xray.Scan(ctx, req.Target)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	if results == nil {
		results = []xray.Probe{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"target": req.Target, "results": results})
}
