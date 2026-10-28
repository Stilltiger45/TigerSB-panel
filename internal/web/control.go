package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/Stilltiger45/TigerSB-panel/internal/health"
	"github.com/Stilltiger45/TigerSB-panel/internal/metrics"
	"github.com/Stilltiger45/TigerSB-panel/internal/service"
	"github.com/Stilltiger45/TigerSB-panel/internal/singbox"
	"github.com/Stilltiger45/TigerSB-panel/internal/update"
	"github.com/Stilltiger45/TigerSB-panel/internal/xray"
)

// maxPoints caps a chart; longer ranges are averaged into this many points.
const maxPoints = 288

// point is one chart sample: percentages and bytes per second.
type point struct {
	T    int64    `json:"t"`
	CPU  *float64 `json:"cpu"`
	Mem  float64  `json:"mem"`
	Disk float64  `json:"disk"`
	Rx   *float64 `json:"rx"`
	Tx   *float64 `json:"tx"`
}

type updateInfo struct {
	Status update.Status `json:"status"`
	Check  *update.Check `json:"check"`
	Log    string        `json:"log,omitempty"`
}

// coreView is a VPN core container on the "Контроль" tab. Its health check
// has the same ID.
type coreView struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Protocols string `json:"protocols"`
	State     string `json:"state"` // systemd state of the unit
}

type controlResponse struct {
	Latest  *metrics.Sample `json:"latest"`
	History []point         `json:"history"`
	Health  *health.Report  `json:"health"`
	Update  updateInfo      `json:"update"`
	Points  []update.Point  `json:"points"`
	Cores   []coreView      `json:"cores"`
}

func (s *Server) cores() []coreView {
	return []coreView{
		{ID: "xray", Title: "Xray", Protocols: "gRPC, XHTTP", State: service.UnitState(s.Run, xray.Unit)},
		{ID: "singbox", Title: "sing-box", Protocols: "TUIC, Hysteria 2, NaïveProxy", State: service.UnitState(s.Run, singbox.Unit)},
		{ID: "mieru", Title: "mbox", Protocols: "Mieru", State: service.UnitState(s.Run, singbox.MieruUnit)},
	}
}

func pct(used, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return 100 * float64(used) / float64(total)
}

// chart turns samples into at most maxPoints points, averaging neighbours.
func chart(samples []metrics.Sample) []point {
	step := (len(samples) + maxPoints - 1) / maxPoints
	step = max(step, 1)
	out := make([]point, 0, len(samples)/step+1)
	for i := 0; i < len(samples); i += step {
		group := samples[i:min(i+step, len(samples))]
		p := point{T: group[len(group)-1].Time}
		var cpu, rx, tx float64
		var nCPU, nNet int
		for _, s := range group {
			p.Mem += pct(s.MemUsed, s.MemTotal)
			p.Disk += pct(s.DiskUsed, s.DiskTotal)
			if s.CPU != nil {
				cpu += *s.CPU
				nCPU++
			}
			if s.RxRate != nil && s.TxRate != nil {
				rx += *s.RxRate
				tx += *s.TxRate
				nNet++
			}
		}
		p.Mem /= float64(len(group))
		p.Disk /= float64(len(group))
		if nCPU > 0 {
			v := cpu / float64(nCPU)
			p.CPU = &v
		}
		if nNet > 0 {
			r, t := rx/float64(nNet), tx/float64(nNet)
			p.Rx, p.Tx = &r, &t
		}
		out = append(out, p)
	}
	return out
}

func (s *Server) updateInfo(ctx context.Context) updateInfo {
	info := updateInfo{}
	if s.Updater == nil {
		return info
	}
	info.Status = s.Updater.Status()
	info.Check, _ = health.LastCheck(ctx, s.store)
	if info.Status.Phase != "" {
		info.Log = s.Updater.LogTail(40)
	}
	return info
}

func (s *Server) authed(w http.ResponseWriter, r *http.Request, post bool) bool {
	if post {
		if !s.checkPost(w, r) {
			return false
		}
	} else if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return false
	}
	if _, err := s.currentAdmin(r); err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "требуется вход"})
		return false
	}
	return true
}

func internalError(w http.ResponseWriter) {
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "внутренняя ошибка"})
}

func badRequest(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
}

// handleControl is the whole "Контроль" tab: resources, checks, updates.
func (s *Server) handleControl(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r, false) {
		return
	}
	ctx := r.Context()
	hours, _ := strconv.Atoi(r.URL.Query().Get("hours"))
	if hours <= 0 || hours > int(metrics.Keep/time.Hour) {
		hours = 24
	}
	latest, err := metrics.Last(ctx, s.store)
	if err != nil {
		internalError(w)
		return
	}
	history, err := metrics.History(ctx, s.store, s.now().Add(-time.Duration(hours)*time.Hour))
	if err != nil {
		internalError(w)
		return
	}
	report, err := health.Last(ctx, s.store)
	if err != nil {
		internalError(w)
		return
	}
	resp := controlResponse{Latest: latest, History: chart(history), Health: report, Update: s.updateInfo(ctx), Points: []update.Point{}, Cores: s.cores()}
	if s.Updater != nil {
		if points, err := update.ListPoints(s.Updater.Layout); err == nil && points != nil {
			resp.Points = points
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) healthEnv() health.Env {
	e := s.HealthBase
	e.Cfg, e.Store, e.Updater, e.Run, e.Now = s.cfg, s.store, s.Updater, s.Run, s.now()
	if e.HTTPSAddr == "" {
		e.HTTPSAddr = s.CertAddr
	}
	return e
}

// handleHealth runs every check now.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r, true) {
		return
	}
	report, err := health.Run(r.Context(), s.healthEnv())
	if err != nil {
		internalError(w)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// coreSettle is how long a restarted core gets to open its ports before the
// health check looks at them.
var coreSettle = 2 * time.Second

// handleCoreRestart restarts a VPN core container, then checks the server,
// so the page shows at once whether that helped. Xray first gets its config
// rebuilt from the database; sing-box rebuilds its own when it starts.
func (s *Server) handleCoreRestart(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r, true) {
		return
	}
	var req struct {
		Core string `json:"core"`
	}
	if !decode(w, r, maxBody, &req) {
		return
	}
	ctx := r.Context()
	var err error
	switch req.Core {
	case "xray":
		var domain string
		if domain, err = s.domain(ctx); err == nil {
			err = s.Xray.Sync(ctx, s.store, domain, true)
		}
	case "singbox":
		err = s.Singbox.Restart(ctx)
	case "mieru":
		err = s.Mieru.Restart(ctx)
	default:
		badRequest(w, "нет такого ядра")
		return
	}
	resp := struct {
		Cores   []coreView     `json:"cores"`
		Health  *health.Report `json:"health"`
		Warning string         `json:"warning,omitempty"`
	}{}
	if err != nil {
		resp.Warning = err.Error()
	} else {
		select {
		case <-time.After(coreSettle):
		case <-ctx.Done():
		}
	}
	if resp.Health, err = health.Run(ctx, s.healthEnv()); err != nil {
		internalError(w)
		return
	}
	resp.Cores = s.cores()
	writeJSON(w, http.StatusOK, resp)
}

// handleJob reports the running update; the page polls it.
func (s *Server) handleJob(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r, false) {
		return
	}
	writeJSON(w, http.StatusOK, s.updateInfo(r.Context()))
}

// handleCheck asks GitHub for a new version.
func (s *Server) handleCheck(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r, true) {
		return
	}
	if s.Updater == nil {
		badRequest(w, "обновление из панели недоступно")
		return
	}
	c := update.CheckGitHub(r.Context(), s.GitHub, s.version, s.now())
	if err := health.SaveCheck(r.Context(), s.store, c); err != nil {
		internalError(w)
		return
	}
	writeJSON(w, http.StatusOK, s.updateInfo(r.Context()))
}

// handleUpdate queues the update to the version the last check found.
func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r, true) {
		return
	}
	if s.Updater == nil {
		badRequest(w, "обновление из панели недоступно")
		return
	}
	c, err := health.LastCheck(r.Context(), s.store)
	if err != nil {
		internalError(w)
		return
	}
	if c == nil || !c.Available || s.now().Sub(c.Checked) > time.Hour {
		badRequest(w, "сначала проверьте обновления")
		return
	}
	if _, err := s.Updater.Queue(update.ActionUpdate, c.Ref); err != nil {
		badRequest(w, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.updateInfo(r.Context()))
}

// handleRestore creates a restore point (POST {}) or rolls back to one
// (POST {"id": "..."}).
func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r, true) {
		return
	}
	if s.Updater == nil {
		badRequest(w, "точки восстановления недоступны")
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&req); err != nil {
		badRequest(w, "неверный запрос")
		return
	}
	if req.ID != "" {
		if _, err := s.Updater.Queue(update.ActionRollback, req.ID); err != nil {
			badRequest(w, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, s.updateInfo(r.Context()))
		return
	}
	if s.Updater.Status().Busy() {
		badRequest(w, "дождитесь окончания обновления")
		return
	}
	p, err := update.CreatePoint(r.Context(), s.Updater.Layout, s.store, s.version, "вручную", s.now())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "не удалось создать точку: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, p)
}
