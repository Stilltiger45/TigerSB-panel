package web

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Stilltiger45/TigerSB-panel/internal/service"
	"github.com/Stilltiger45/TigerSB-panel/internal/site"
	"github.com/Stilltiger45/TigerSB-panel/internal/store"
	"github.com/Stilltiger45/TigerSB-panel/internal/xray"
)

// MaxDevices is the largest device limit a user can get.
const MaxDevices = 20

type userView struct {
	ID      int64          `json:"id"`
	Name    string         `json:"name"`
	UUID    string         `json:"uuid"`
	Devices int            `json:"devices"`
	Enabled bool           `json:"enabled"`
	Created time.Time      `json:"created"`
	Up      int64          `json:"up"`
	Down    int64          `json:"down"`
	Online  []xray.Address `json:"online"`
	SeenAt  int64          `json:"seen_at"`
	Over    bool           `json:"over"` // more addresses online than devices allowed
	Links   []xray.Link    `json:"links"`
}

type usersResponse struct {
	Users   []userView `json:"users"`
	Address string     `json:"address"`
	Xray    string     `json:"xray"`
	Live    bool       `json:"live"` // traffic and online come from the running Xray
	Warning string     `json:"warning,omitempty"`
}

func newUUID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

func checkName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > 40 {
		return "", errors.New("имя должно быть от 1 до 40 символов")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", errors.New("в имени есть управляющие символы")
		}
	}
	return name, nil
}

func checkDevices(n int) error {
	if n < 1 || n > MaxDevices {
		return fmt.Errorf("устройств может быть от 1 до %d", MaxDevices)
	}
	return nil
}

func (s *Server) domain(ctx context.Context) (string, error) {
	set, err := site.Load(ctx, s.store, s.cfg)
	return set.Domain, err
}

// address is where links point to.
func (s *Server) address(ctx context.Context, set xray.Settings) string {
	domain, _ := s.domain(ctx)
	var ips []net.IP
	if s.LocalIPs != nil {
		ips, _ = s.LocalIPs()
	}
	return xray.PublicAddress(set, ips, domain)
}

func (s *Server) xrayState() string { return service.UnitState(s.Run, xray.Unit) }

func (s *Server) usersState(ctx context.Context) (*usersResponse, error) {
	set, err := xray.Load(ctx, s.store)
	if err != nil {
		return nil, err
	}
	users, err := s.store.Users(ctx)
	if err != nil {
		return nil, err
	}
	resp := &usersResponse{Users: []userView{}, Address: s.address(ctx, set), Xray: s.xrayState()}
	live := map[int64]xray.UserStats{}
	if resp.Xray == "active" {
		list, err := s.Xray.Stats(ctx, false)
		if err == nil {
			resp.Live = true
			for _, u := range list {
				live[u.ID] = u
			}
		} else {
			resp.Warning = "Данные о подключениях пятиминутной давности: " + err.Error()
		}
	}
	for _, u := range users {
		v := userView{
			ID: u.ID, Name: u.Name, UUID: u.UUID, Devices: u.Devices, Enabled: u.Enabled,
			Created: u.Created, Up: u.Up, Down: u.Down, SeenAt: u.SeenAt, Online: []xray.Address{},
			Links: xray.Links(set, resp.Address, u),
		}
		if resp.Live {
			if l, ok := live[u.ID]; ok {
				v.Up += l.Up
				v.Down += l.Down
				v.Online = l.Online
				if len(l.Online) > 0 {
					v.SeenAt = s.now().Unix()
				}
			}
		} else {
			json.Unmarshal([]byte(u.Online), &v.Online)
		}
		v.Over = len(v.Online) > u.Devices
		resp.Users = append(resp.Users, v)
	}
	return resp, nil
}

// sync rewrites the Xray config and edge routes from the database. Xray
// itself is changed live by the caller.
func (s *Server) sync(ctx context.Context) error {
	domain, err := s.domain(ctx)
	if err != nil {
		return err
	}
	return s.Xray.Sync(ctx, s.store, domain, false)
}

// live applies a user change to the running Xray; a stopped Xray reads the
// new config when it starts, so only a running one is told.
func (s *Server) live(ctx context.Context, f func(xray.Settings) error) string {
	if s.xrayState() != "active" {
		return ""
	}
	set, err := xray.Load(ctx, s.store)
	if err == nil {
		err = f(set)
	}
	if err != nil {
		log.Printf("xray live change: %v", err)
		return "Изменение сохранено, но Xray его не принял на лету: " + err.Error() + ". Оно вступит в силу после перезапуска Xray."
	}
	return ""
}

func (s *Server) respondUsers(w http.ResponseWriter, ctx context.Context, warning string) {
	resp, err := s.usersState(ctx)
	if err != nil {
		log.Printf("users: %v", err)
		internalError(w)
		return
	}
	if warning != "" {
		resp.Warning = warning
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleUsers lists users with their links, traffic and devices online.
func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r, false) {
		return
	}
	s.respondUsers(w, r.Context(), "")
}

func decode(w http.ResponseWriter, r *http.Request, limit int64, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit)).Decode(v); err != nil {
		badRequest(w, "неверный запрос")
		return false
	}
	return true
}

// handleUserAdd creates a user in every protocol at once.
func (s *Server) handleUserAdd(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r, true) {
		return
	}
	var req struct {
		Name    string `json:"name"`
		Devices int    `json:"devices"`
	}
	if !decode(w, r, maxBody, &req) {
		return
	}
	name, err := checkName(req.Name)
	if err == nil {
		err = checkDevices(req.Devices)
	}
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	s.xrayMu.Lock()
	defer s.xrayMu.Unlock()
	ctx := r.Context()
	id, err := newUUID()
	if err != nil {
		internalError(w)
		return
	}
	u := &store.User{Name: name, UUID: id, Devices: req.Devices, Enabled: true, Created: s.now().UTC()}
	if err := s.store.AddUser(ctx, u); errors.Is(err, store.ErrExists) {
		badRequest(w, "пользователь с таким именем уже есть")
		return
	} else if err != nil {
		internalError(w)
		return
	}
	if err := s.sync(ctx); err != nil {
		log.Printf("xray sync: %v", err)
	}
	warning := s.live(ctx, func(set xray.Settings) error { return s.Xray.AddUser(ctx, set, *u) })
	s.respondUsers(w, ctx, warning)
}

// handleUserUpdate changes a user: name, device limit, on/off, a new key
// (which cuts off every device that had the old links) or zero traffic.
func (s *Server) handleUserUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r, true) {
		return
	}
	var req struct {
		ID           int64   `json:"id"`
		Name         *string `json:"name"`
		Devices      *int    `json:"devices"`
		Enabled      *bool   `json:"enabled"`
		Reissue      bool    `json:"reissue"`
		ResetTraffic bool    `json:"reset_traffic"`
	}
	if !decode(w, r, maxBody, &req) {
		return
	}
	s.xrayMu.Lock()
	defer s.xrayMu.Unlock()
	ctx := r.Context()
	u, err := s.store.UserByID(ctx, req.ID)
	if errors.Is(err, store.ErrNotFound) {
		badRequest(w, "пользователь не найден")
		return
	} else if err != nil {
		internalError(w)
		return
	}
	before := *u
	if req.Name != nil {
		if u.Name, err = checkName(*req.Name); err != nil {
			badRequest(w, err.Error())
			return
		}
	}
	if req.Devices != nil {
		if err := checkDevices(*req.Devices); err != nil {
			badRequest(w, err.Error())
			return
		}
		u.Devices = *req.Devices
	}
	if req.Enabled != nil {
		u.Enabled = *req.Enabled
	}
	if req.Reissue {
		if u.UUID, err = newUUID(); err != nil {
			internalError(w)
			return
		}
	}
	if err := s.store.UpdateUser(ctx, u); errors.Is(err, store.ErrExists) {
		badRequest(w, "пользователь с таким именем уже есть")
		return
	} else if err != nil {
		internalError(w)
		return
	}
	if req.ResetTraffic {
		if err := s.store.ResetTraffic(ctx, u.ID); err != nil {
			internalError(w)
			return
		}
	}
	warning := ""
	if before.UUID != u.UUID || before.Enabled != u.Enabled {
		if err := s.sync(ctx); err != nil {
			log.Printf("xray sync: %v", err)
		}
		warning = s.live(ctx, func(set xray.Settings) error {
			if before.Enabled {
				if err := s.Xray.RemoveUser(ctx, set, u.ID); err != nil {
					return err
				}
			}
			return s.Xray.AddUser(ctx, set, *u)
		})
	}
	s.respondUsers(w, ctx, warning)
}

// handleUserDelete removes a user; its devices are cut off at once.
func (s *Server) handleUserDelete(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r, true) {
		return
	}
	var req struct {
		ID int64 `json:"id"`
	}
	if !decode(w, r, maxBody, &req) {
		return
	}
	s.xrayMu.Lock()
	defer s.xrayMu.Unlock()
	ctx := r.Context()
	u, err := s.store.UserByID(ctx, req.ID)
	if errors.Is(err, store.ErrNotFound) {
		badRequest(w, "пользователь не найден")
		return
	} else if err != nil {
		internalError(w)
		return
	}
	if err := s.store.DeleteUser(ctx, u.ID); err != nil {
		internalError(w)
		return
	}
	if err := s.sync(ctx); err != nil {
		log.Printf("xray sync: %v", err)
	}
	warning := ""
	if u.Enabled {
		warning = s.live(ctx, func(set xray.Settings) error { return s.Xray.RemoveUser(ctx, set, u.ID) })
	}
	s.respondUsers(w, ctx, warning)
}

// handleUserQR draws one link of a user as an SVG QR code.
func (s *Server) handleUserQR(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r, false) {
		return
	}
	ctx := r.Context()
	id, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	u, err := s.store.UserByID(ctx, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	set, err := xray.Load(ctx, s.store)
	if err != nil {
		internalError(w)
		return
	}
	for _, l := range xray.Links(set, s.address(ctx, set), *u) {
		if l.Protocol == r.URL.Query().Get("p") {
			svg, err := qrSVG(l.URL)
			if err != nil {
				internalError(w)
				return
			}
			w.Header().Set("Content-Type", "image/svg+xml")
			w.Write(svg)
			return
		}
	}
	http.NotFound(w, r)
}
