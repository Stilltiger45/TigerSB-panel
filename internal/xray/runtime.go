package xray

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Stilltiger45/TigerSB-panel/internal/edge"
	"github.com/Stilltiger45/TigerSB-panel/internal/store"
)

// Image is the official Xray-core image (MPL-2.0), unmodified, pinned by
// digest so every server runs the same build.
const Image = "ghcr.io/xtls/xray-core:26.9.30@sha256:6d30597c3e729b5dc1faac8dbc0138fbbe6d4d787bfb54120ea48f14e2e7b5ca"

// Container and Unit name the running Xray.
const (
	Container = "tigersb-xray"
	Unit      = "tigersb-xray.service"
	EdgeUnit  = "tigersb-edge.service"
)

// The image runs Xray as the distroless "nonroot" user; its config must be
// readable by that user.
const containerUID = 65532

// mountDir is where the config directory appears inside the container.
const mountDir = "/etc/tigersb-xray"

// Exec runs a command with optional stdin and returns its combined output.
type Exec func(ctx context.Context, stdin []byte, name string, args ...string) (string, error)

// RealExec runs real commands.
func RealExec(ctx context.Context, stdin []byte, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// Runtime is the Xray container and the edge router on this server.
type Runtime struct {
	Dir    string // config directory, /var/lib/tigersb/xray
	Routes string // edge routes file, /var/lib/tigersb/edge/routes.json
	Image  string
	Exec   Exec
}

// DefaultRuntime is the layout the installer creates under dataDir. The image
// can be replaced with TIGERSB_XRAY_IMAGE, which tests use.
func DefaultRuntime(dataDir string) *Runtime {
	img := os.Getenv("TIGERSB_XRAY_IMAGE")
	if img == "" {
		img = Image
	}
	return &Runtime{
		Dir:    filepath.Join(dataDir, "xray"),
		Routes: filepath.Join(dataDir, "edge", "routes.json"),
		Image:  img,
		Exec:   RealExec,
	}
}

// ConfigPath is the live Xray config on the host.
func (r *Runtime) ConfigPath() string { return filepath.Join(r.Dir, "config.json") }

// writeFile writes data atomically and gives it to the container user when
// running as root.
func (r *Runtime) writeFile(name string, data []byte) error {
	if err := os.MkdirAll(r.Dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(r.Dir, name)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		for _, p := range []string{r.Dir, tmp} {
			if err := os.Chown(p, containerUID, containerUID); err != nil {
				os.Remove(tmp)
				return err
			}
		}
	}
	return os.Rename(tmp, path)
}

// Write saves the Xray config and the edge routes. Xray reads the config when
// it starts; the edge picks up routes within seconds.
func (r *Runtime) Write(conf []byte, routes edge.Routes) error {
	if err := r.writeFile("config.json", conf); err != nil {
		return err
	}
	return edge.Write(r.Routes, routes)
}

// ErrNoDocker means Docker is not installed or not running.
var ErrNoDocker = errors.New("Docker не установлен или не запущен: переустановите панель (install.sh --update) или запустите systemctl start docker")

func (r *Runtime) docker(ctx context.Context, stdin []byte, args ...string) (string, error) {
	out, err := r.Exec(ctx, stdin, "docker", args...)
	var notFound *exec.Error
	if errors.As(err, &notFound) || strings.Contains(out, "Cannot connect to the Docker daemon") {
		return out, ErrNoDocker
	}
	return out, err
}

// tail keeps the last lines of tool output for an error message.
func tail(out string, n int) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// Test asks Xray in a throwaway container whether it accepts conf.
func (r *Runtime) Test(ctx context.Context, conf []byte) error {
	if err := r.writeFile("test.json", conf); err != nil {
		return err
	}
	defer os.Remove(filepath.Join(r.Dir, "test.json"))
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, err := r.docker(ctx, nil, "run", "--rm", "--network", "none", "--log-driver", "none",
		"-v", r.Dir+":"+mountDir+":ro", r.Image, "run", "-test", "-c", mountDir+"/test.json")
	if errors.Is(err, ErrNoDocker) {
		return err
	}
	if err != nil {
		return fmt.Errorf("Xray не принял настройки: %s", tail(out, 6))
	}
	return nil
}

// RunArgs are the `docker run` arguments of the long-running container. The
// host network lets Xray see real client addresses and bind its ports
// directly; everything else is locked down.
func (r *Runtime) RunArgs() []string {
	return []string{"run", "--rm", "--name", Container,
		"--network", "host", "--log-driver", "none",
		"--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"-v", r.Dir + ":" + mountDir + ":ro",
		r.Image, "run", "-c", mountDir + "/config.json"}
}

// Remove deletes a container left by a crash, so the name is free.
func (r *Runtime) Remove(ctx context.Context) {
	r.docker(ctx, nil, "rm", "-f", Container)
}

func (r *Runtime) api(ctx context.Context, stdin []byte, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	exec := []string{"exec"}
	if stdin != nil {
		exec = append(exec, "-i")
	}
	exec = append(exec, Container, "/usr/local/bin/xray", "api")
	exec = append(exec, args[0], "--server="+APIAddr)
	return r.docker(ctx, stdin, append(exec, args[1:]...)...)
}

// tags lists the inbounds that exist in s.
func tags(s Settings) []string {
	var out []string
	if s.GRPC.Enabled {
		out = append(out, GRPCTag)
	}
	if s.XHTTP.Enabled {
		out = append(out, XHTTPTag)
	}
	return out
}

// AddUser adds an enabled user to the running Xray without a restart.
func (r *Runtime) AddUser(ctx context.Context, s Settings, u store.User) error {
	var inbounds []obj
	for _, tag := range tags(s) {
		// Only the tag and the clients matter; Xray still wants a valid
		// listen address and port to parse the inbound.
		inbounds = append(inbounds, obj{
			"tag": tag, "protocol": "vless", "listen": "127.0.0.1", "port": 1,
			"settings": obj{"clients": clients([]store.User{u}), "decryption": "none"},
		})
	}
	if len(inbounds) == 0 || !u.Enabled {
		return nil
	}
	b, _ := json.Marshal(obj{"inbounds": inbounds})
	out, err := r.api(ctx, b, "adu", "stdin:")
	if err == nil && !strings.Contains(out, fmt.Sprintf("Added %d user", len(inbounds))) {
		err = errors.New(tail(out, 3))
	}
	if err != nil && !errors.Is(err, ErrNoDocker) {
		err = fmt.Errorf("Xray не добавил пользователя: %v", err)
	}
	return err
}

// RemoveUser takes a user out of the running Xray; existing connections of
// that user end.
func (r *Runtime) RemoveUser(ctx context.Context, s Settings, id int64) error {
	for _, tag := range tags(s) {
		out, err := r.api(ctx, nil, "rmu", "-tag="+tag, Email(id))
		if errors.Is(err, ErrNoDocker) {
			return err
		}
		// A user who was not there is already removed.
		if err != nil || !strings.Contains(out, "Removed 1 user") && !strings.Contains(out, "not found") {
			return fmt.Errorf("Xray не удалил пользователя: %v %s", err, tail(out, 3))
		}
	}
	return nil
}

// Address is one place a user is connected from.
type Address struct {
	IP       string `json:"ip"`
	LastSeen int64  `json:"last_seen"` // unix seconds
}

// UserStats is what Xray reports about one user.
type UserStats struct {
	ID     int64
	Up     int64
	Down   int64
	Online []Address
}

// onlineReply is GetUsersStatsResponse as `xray api statsonlineiplist -all`
// prints it: only users with open connections, loopback addresses left out.
type onlineReply struct {
	Users []struct {
		Email string `json:"email"`
		IPs   []struct {
			IP       string          `json:"ip"`
			LastSeen json.RawMessage `json:"lastSeen"`
		} `json:"ips"`
	} `json:"users"`
}

// trafficReply is QueryStatsResponse as `xray api statsquery` prints it.
type trafficReply struct {
	Stat []struct {
		Name  string          `json:"name"`
		Value json.RawMessage `json:"value"`
	} `json:"stat"`
}

// number reads an int64 that protobuf JSON may print as a string.
func number(raw json.RawMessage) int64 {
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return n
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		n, _ = strconv.ParseInt(s, 10, 64)
	}
	return n
}

// body cuts the JSON out of tool output; empty output means "nothing yet".
func body(out string) []byte {
	if i := strings.Index(out, "{"); i >= 0 {
		return []byte(out[i:])
	}
	return []byte("{}")
}

// ParseStats merges the outputs of `statsonlineiplist -all` and
// `statsquery -pattern user>>>` into one entry per user.
func ParseStats(online, traffic string) ([]UserStats, error) {
	var on onlineReply
	var tr trafficReply
	if err := json.Unmarshal(body(online), &on); err != nil {
		return nil, fmt.Errorf("непонятный ответ Xray: %w", err)
	}
	if err := json.Unmarshal(body(traffic), &tr); err != nil {
		return nil, fmt.Errorf("непонятный ответ Xray: %w", err)
	}
	byID := map[int64]*UserStats{}
	get := func(email string) *UserStats {
		id, ok := UserID(email)
		if !ok {
			return nil
		}
		if byID[id] == nil {
			byID[id] = &UserStats{ID: id, Online: []Address{}}
		}
		return byID[id]
	}
	for _, u := range on.Users {
		if s := get(u.Email); s != nil {
			for _, ip := range u.IPs {
				s.Online = append(s.Online, Address{IP: ip.IP, LastSeen: number(ip.LastSeen)})
			}
		}
	}
	for _, st := range tr.Stat {
		// user>>>EMAIL>>>traffic>>>uplink
		parts := strings.Split(st.Name, ">>>")
		if len(parts) != 4 || parts[0] != "user" || parts[2] != "traffic" {
			continue
		}
		if s := get(parts[1]); s != nil {
			switch parts[3] {
			case "uplink":
				s.Up += number(st.Value)
			case "downlink":
				s.Down += number(st.Value)
			}
		}
	}
	list := make([]UserStats, 0, len(byID))
	for _, s := range byID {
		list = append(list, *s)
	}
	slices.SortFunc(list, func(a, b UserStats) int { return int(a.ID - b.ID) })
	return list, nil
}

// Stats asks the running Xray for the traffic of every user and the
// addresses connected now. reset zeroes the traffic counters, for the
// collector that stores them.
func (r *Runtime) Stats(ctx context.Context, reset bool) ([]UserStats, error) {
	online, err := r.api(ctx, nil, "statsonlineiplist", "-all")
	if err != nil {
		return nil, apiErr(err, online)
	}
	args := []string{"statsquery", "-pattern", "user>>>"}
	if reset {
		args = append(args, "-reset")
	}
	traffic, err := r.api(ctx, nil, args...)
	if err != nil {
		return nil, apiErr(err, traffic)
	}
	return ParseStats(online, traffic)
}

func apiErr(err error, out string) error {
	if errors.Is(err, ErrNoDocker) {
		return err
	}
	return fmt.Errorf("Xray не ответил: %s", tail(out, 2))
}

// Restart restarts the container with the config on disk. Connections drop
// for a second, so user changes go through AddUser and RemoveUser instead.
func (r *Runtime) Restart(ctx context.Context) error {
	out, err := r.Exec(ctx, nil, "systemctl", "restart", Unit)
	if err != nil {
		return fmt.Errorf("Xray не перезапустился: %v %s. Подробности: journalctl -u %s", err, out, Unit)
	}
	return nil
}

// Build reads the settings and users and renders the Xray config and the
// edge routes. domain is the panel's domain, which keeps going to Caddy.
func Build(ctx context.Context, st *store.Store, domain string) (Settings, []byte, edge.Routes, error) {
	s, err := Load(ctx, st)
	if err != nil {
		return s, nil, edge.Routes{}, err
	}
	users, err := st.Users(ctx)
	if err != nil {
		return s, nil, edge.Routes{}, err
	}
	conf, err := Render(s, users)
	return s, conf, Routes(s, domain), err
}

// Sync writes the config and routes built from the store. With restart it
// also restarts Xray, so it reads them.
func (r *Runtime) Sync(ctx context.Context, st *store.Store, domain string, restart bool) error {
	_, conf, routes, err := Build(ctx, st, domain)
	if err != nil {
		return err
	}
	if err := r.Write(conf, routes); err != nil {
		return err
	}
	if restart {
		return r.Restart(ctx)
	}
	return nil
}

// Collect stores the traffic since the previous collection and who is
// connected now. The monitor timer calls it every five minutes.
func (r *Runtime) Collect(ctx context.Context, st *store.Store, now time.Time) error {
	list, err := r.Stats(ctx, true)
	if err != nil {
		return err
	}
	var acts []store.UserActivity
	for _, u := range list {
		online, _ := json.Marshal(u.Online)
		acts = append(acts, store.UserActivity{ID: u.ID, Up: u.Up, Down: u.Down, Online: string(online), Seen: len(u.Online) > 0})
	}
	return st.RecordActivity(ctx, acts, now)
}
