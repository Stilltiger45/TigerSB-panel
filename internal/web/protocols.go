package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/Stilltiger45/TigerSB-panel/internal/core"
	"github.com/Stilltiger45/TigerSB-panel/internal/service"
	"github.com/Stilltiger45/TigerSB-panel/internal/singbox"
	"github.com/Stilltiger45/TigerSB-panel/internal/xray"
)

type protocolsResponse struct {
	Settings xray.Settings `json:"settings"`
	// Singbox holds TUIC and Hysteria 2; its secret is left out.
	Singbox singbox.Settings `json:"singbox"`
	// TUICCert tells whether sing-box has the domain's certificate yet,
	// which TUIC and Hysteria 2 need.
	TUICCert     bool           `json:"tuic_cert"`
	AutoAddress  string         `json:"auto_address"`
	Domain       string         `json:"domain"`
	Fingerprints []string       `json:"fingerprints"`
	Modes        []string       `json:"modes"`
	Methods      []string       `json:"methods"`
	Reserved     map[int]string `json:"reserved"`
	Congestion   []string       `json:"congestion_controls"`
	RelayModes   []string       `json:"udp_relay_modes"`
	BBRProfiles  []string       `json:"bbr_profiles"`
	MieruModes   []string       `json:"mieru_transports"`
	MieruState   string         `json:"mieru_state"`
	Xray         string         `json:"xray"`
	SingboxState string         `json:"singbox_state"`
	Edge         string         `json:"edge"`
	// BBRReady is false when the kernel does not let the cores pick BBR.
	BBRReady bool `json:"bbr_ready"`
	// Tests holds the test link of each protocol that has one (testLinks).
	Tests map[string]string `json:"tests"`
	// Applied says what the save did, for the message under the button.
	Applied string `json:"applied,omitempty"`
	Warning string `json:"warning,omitempty"`
}

// protocolsRequest is the Xray settings with the sing-box ones next to
// them; without "singbox" those stay as they are.
type protocolsRequest struct {
	xray.Settings
	Singbox *singbox.Settings `json:"singbox"`
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
	sb, err := singbox.Load(ctx, s.store)
	if err != nil {
		return nil, err
	}
	tests, err := s.testLinks(ctx, set, sb, domain)
	if err != nil {
		return nil, err
	}
	sb.Secret = ""
	auto := set
	auto.Address = ""
	return &protocolsResponse{
		Settings:     set,
		Singbox:      sb,
		TUICCert:     s.Singbox.HaveCert(),
		AutoAddress:  s.address(ctx, auto),
		Domain:       domain,
		Fingerprints: xray.Fingerprints,
		Modes:        xray.XHTTPModes,
		Methods:      xray.HTTPMethods,
		Reserved:     xray.Reserved(s.panelPort()),
		Congestion:   singbox.CongestionControls,
		RelayModes:   singbox.UDPRelayModes,
		BBRProfiles:  singbox.BBRProfiles,
		MieruModes:   singbox.MieruTransports,
		MieruState:   service.UnitState(s.Run, singbox.MieruUnit),
		Xray:         s.xrayState(),
		SingboxState: service.UnitState(s.Run, singbox.Unit),
		Edge:         service.UnitState(s.Run, xray.EdgeUnit),
		BBRReady:     xray.BBRReady(),
		Tests:        tests,
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
	var req protocolsRequest
	if !decode(w, r, 32<<10, &req) {
		return
	}
	set := req.Settings
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
	oldSet, err := xray.Load(ctx, s.store)
	if err != nil {
		internalError(w)
		return
	}
	oldSB, err := singbox.Load(ctx, s.store)
	if err != nil {
		internalError(w)
		return
	}
	sb := oldSB
	if req.Singbox != nil {
		sb = *req.Singbox
		sb.Secret = oldSB.Secret
		sb.Normalize()
		if err := sb.Validate(); err != nil {
			badRequest(w, err.Error())
			return
		}
	}
	if err := tcpClash(set, sb, s.panelPort()); err != nil {
		badRequest(w, err.Error())
		return
	}
	// Each core restarts only when its own settings changed, so tuning TUIC
	// or Hysteria 2 keeps Xray's connections and the other way round.
	// Mieru lives in the same settings but in a container of its own.
	noMieru := func(x singbox.Settings) singbox.Settings {
		x.Mieru = singbox.Mieru{}
		return x
	}
	xrayChanged, sbChanged := !sameJSON(set, oldSet), !sameJSON(noMieru(sb), noMieru(oldSB))
	mieruChanged := !sameJSON(sb.Mieru, oldSB.Mieru)
	users, err := s.store.CoreUsers(ctx)
	if err != nil {
		internalError(w)
		return
	}
	var xrayConf, sbConf, mieruConf []byte
	if xrayChanged {
		if xrayConf, err = xray.Render(set, users); err != nil {
			internalError(w)
			return
		}
	}
	if sbChanged {
		if sbConf, err = s.Singbox.Render(sb, users, domain); err != nil {
			internalError(w)
			return
		}
	}
	if mieruChanged {
		if mieruConf, err = s.Mieru.Render(sb, users, domain); err != nil {
			internalError(w)
			return
		}
	}
	// Both configs are checked before anything is saved.
	var unchecked []string
	if xrayChanged {
		if err := s.Xray.Test(ctx, xrayConf); errors.Is(err, xray.ErrNoDocker) {
			unchecked = append(unchecked, err.Error())
		} else if err != nil {
			badRequest(w, err.Error())
			return
		}
	}
	if sbChanged {
		if err := s.Singbox.Test(ctx, sbConf); errors.Is(err, core.ErrNoDocker) {
			unchecked = append(unchecked, err.Error())
		} else if err != nil {
			badRequest(w, err.Error())
			return
		}
	}
	if mieruChanged {
		if err := s.Mieru.Test(ctx, mieruConf); errors.Is(err, core.ErrNoDocker) {
			unchecked = append(unchecked, err.Error())
		} else if err != nil {
			badRequest(w, err.Error())
			return
		}
	}
	warning := ""
	if len(unchecked) > 0 {
		warning = "Настройки сохранены, но не проверены: " + unchecked[0]
	}
	var applied []string
	if xrayChanged {
		if err := xray.Save(ctx, s.store, set); err != nil {
			internalError(w)
			return
		}
		if err := s.Xray.Write(xrayConf, xray.Routes(set, domain)); err != nil {
			log.Printf("xray write: %v", err)
			internalError(w)
			return
		}
		if warning == "" {
			if err := s.Xray.Restart(ctx); err != nil {
				warning = "Настройки сохранены, но " + err.Error()
			} else {
				applied = append(applied, "Xray перезапущен")
			}
		}
	}
	if sbChanged || mieruChanged {
		if err := singbox.Save(ctx, s.store, sb); err != nil {
			internalError(w)
			return
		}
	}
	for _, c := range []struct {
		changed bool
		rt      *singbox.Runtime
	}{{sbChanged, s.Singbox}, {mieruChanged, s.Mieru}} {
		if !c.changed {
			continue
		}
		if err := c.rt.Sync(ctx, s.store, domain, warning == ""); err != nil {
			warning = "Настройки сохранены, но " + err.Error()
		} else if warning == "" {
			applied = append(applied, c.rt.Name+" перечитал настройки")
		}
	}
	resp, err := s.protocolsState(ctx)
	if err != nil {
		internalError(w)
		return
	}
	resp.Warning = warning
	resp.Applied = strings.Join(applied, ", ")
	writeJSON(w, http.StatusOK, resp)
}

// tcpClash catches NaïveProxy or Mieru on a TCP port that Xray or the panel
// holds; each core checks only its own ports.
func tcpClash(set xray.Settings, sb singbox.Settings, panelPort int) error {
	type user struct {
		name string
		has  func(int) bool
	}
	var users []user
	if n := sb.Naive; n.Enabled {
		users = append(users, user{"NaïveProxy", func(p int) bool { return p == n.Port }})
	}
	if m := sb.Mieru; m.Enabled && m.Transport == "TCP" {
		users = append(users, user{"Mieru", sb.MieruHas})
	}
	for _, u := range users {
		for port, busy := range xray.Reserved(panelPort) {
			if u.has(port) {
				return fmt.Errorf("%s: порт %d занят: %s", u.name, port, busy)
			}
		}
		for _, p := range []struct {
			name    string
			enabled bool
			port    int
		}{{"gRPC", set.GRPC.Enabled, set.GRPC.Port}, {"XHTTP", set.XHTTP.Enabled, set.XHTTP.Port}} {
			if p.enabled && u.has(p.port) {
				return fmt.Errorf("%s и %s оба работают по TCP, им нужны разные порты, а не %d", u.name, p.name, p.port)
			}
		}
	}
	return nil
}

// sameJSON compares settings the way they are stored.
func sameJSON(a, b any) bool {
	x, errA := json.Marshal(a)
	y, errB := json.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(x, y)
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
