package web

import (
	"log"
	"net/http"

	"github.com/Stilltiger45/TigerSB-panel/internal/health"
	"github.com/Stilltiger45/TigerSB-panel/internal/metrics"
	"github.com/Stilltiger45/TigerSB-panel/internal/service"
	"github.com/Stilltiger45/TigerSB-panel/internal/sub"
	"github.com/Stilltiger45/TigerSB-panel/internal/xray"
)

// usersSummary counts users for the "Обзор" tab.
type usersSummary struct {
	Total   int  `json:"total"`
	Enabled int  `json:"enabled"`
	Online  int  `json:"online"`  // users with an address connected now
	Devices int  `json:"devices"` // devices that fetched a subscription
	Over    int  `json:"over"`    // users with more addresses than devices allowed
	Live    bool `json:"live"`    // from the running Xray, not the last collection
}

// overviewResponse is the whole panel at a glance: the parts the other tabs
// show in full.
type overviewResponse struct {
	Protocols []health.Result `json:"protocols"`
	Users     usersSummary    `json:"users"`
	Cores     []coreView      `json:"cores"`
	Edge      string          `json:"edge"`
	Sub       string          `json:"sub"`
	Health    *health.Report  `json:"health"` // the last full check
	Latest    *metrics.Sample `json:"latest"`
	Update    updateInfo      `json:"update"`
}

// handleOverview is the "Обзор" tab: protocols and cores now, users, the
// last check, resources and updates.
func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r, false) {
		return
	}
	ctx := r.Context()
	users, err := s.usersState(ctx)
	if err != nil {
		log.Printf("overview: %v", err)
		internalError(w)
		return
	}
	resp := overviewResponse{
		Protocols: users.Protocols,
		Users:     usersSummary{Total: len(users.Users), Live: users.Live},
		Cores:     s.cores(),
		Edge:      service.UnitState(s.Run, xray.EdgeUnit),
		Sub:       service.UnitState(s.Run, sub.Unit),
		Update:    s.updateInfo(ctx),
	}
	for _, u := range users.Users {
		if u.Enabled {
			resp.Users.Enabled++
		}
		if len(u.Online) > 0 {
			resp.Users.Online++
		}
		if u.Over {
			resp.Users.Over++
		}
		resp.Users.Devices += len(u.List)
	}
	if resp.Health, err = health.Last(ctx, s.store); err != nil {
		internalError(w)
		return
	}
	if resp.Latest, err = metrics.Last(ctx, s.store); err != nil {
		internalError(w)
		return
	}
	resp.Update.Log = ""
	writeJSON(w, http.StatusOK, resp)
}
