package singbox

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Stilltiger45/TigerSB-panel/internal/store"
	"github.com/Stilltiger45/TigerSB-panel/internal/xray"
)

// ClashPort and MieruClashPort are the Clash API of sing-box and mbox, on
// loopback; xray.Reserved keeps protocols off them. The official builds
// have no per-user counters (v2ray_api is left out), so the runner asks the
// API for the open connections every second instead, and adds up what each
// user's connections carried. A connection that opens and closes between
// two looks, and the last second of one that closes, go uncounted.
const (
	ClashPort      = 10087
	MieruClashPort = 10088
)

// userOutbound starts the tag of a user's own outbound.
const userOutbound = "user-"

const (
	pollEvery  = time.Second
	storeEvery = 30 * time.Second
	// onlineFresh is how old the online file may be: an older one was
	// left by a runner that stopped.
	onlineFresh = 10 * time.Second
)

// clashPort is the port of this core's Clash API.
func (r *Runtime) clashPort() int {
	if r.Binary != "" {
		return MieruClashPort
	}
	return ClashPort
}

// OnlinePath is where the runner writes what it knows now, for the panel.
func (r *Runtime) OnlinePath() string { return filepath.Join(r.Dir, "online.json") }

// Run runs the container until it exits and meanwhile counts the traffic
// of users. A stop signal goes on to docker, which passes it to the core.
func (r *Runtime) Run(st *store.Store, docker string) error {
	cmd := exec.Command(docker, r.RunArgs()...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sig)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() {
		for s := range sig {
			cmd.Process.Signal(s)
		}
	}()
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.watch(ctx, st)
		close(done)
	}()
	err := cmd.Wait()
	stop()
	<-done
	return err
}

// watch polls the Clash API until ctx ends, stores the traffic every
// storeEvery and on the way out, and keeps the online file fresh.
func (r *Runtime) watch(ctx context.Context, st *store.Store) {
	s, err := Load(ctx, st)
	if err != nil {
		log.Printf("%s: статистика: %v", r.Name, err)
		return
	}
	c := newCounter()
	url := "http://127.0.0.1:" + strconv.Itoa(r.clashPort()) + "/connections"
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil}}
	tick := time.NewTicker(pollEvery)
	defer tick.Stop()
	stored := time.Now()
	for {
		select {
		case <-ctx.Done():
			if err := c.store(context.Background(), st, time.Now()); err != nil {
				log.Printf("%s: статистика: %v", r.Name, err)
			}
			os.Remove(r.OnlinePath())
			return
		case now := <-tick.C:
			// The API is down while the core starts or rereads its config.
			if list, err := connections(ctx, client, url, s.ClashSecret()); err == nil {
				c.add(list, now)
			}
			if now.Sub(stored) >= storeEvery {
				if err := c.store(ctx, st, now); err != nil {
					log.Printf("%s: статистика: %v", r.Name, err)
				}
				stored = now
			}
			if err := c.write(r.OnlinePath(), now); err != nil {
				log.Printf("%s: статистика: %v", r.Name, err)
			}
		}
	}
}

// clashConn is one open connection as the Clash API lists it.
type clashConn struct {
	ID       string   `json:"id"`
	Upload   int64    `json:"upload"`
	Download int64    `json:"download"`
	Chains   []string `json:"chains"`
	Metadata struct {
		SourceIP string `json:"sourceIP"`
	} `json:"metadata"`
}

func connections(ctx context.Context, client *http.Client, url, secret string) ([]clashConn, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Clash API: %s", resp.Status)
	}
	var body struct {
		Connections []clashConn `json:"connections"`
	}
	err = json.NewDecoder(resp.Body).Decode(&body)
	return body.Connections, err
}

// UserNow is what the runner knows about a user now: traffic it has not
// stored yet, and the addresses connected.
type UserNow struct {
	Up     int64          `json:"up"`
	Down   int64          `json:"down"`
	Online []xray.Address `json:"online"`
}

// onlineFile is the runner's file for the panel.
type onlineFile struct {
	At    int64             `json:"at"`
	Users map[int64]UserNow `json:"users"`
}

// counter adds up the traffic of users from looks at the open connections.
type counter struct {
	last  map[string]clashConn // by connection, at the previous look
	users map[int64]*UserNow   // traffic since the last store; addresses now
}

func newCounter() *counter {
	return &counter{last: map[string]clashConn{}, users: map[int64]*UserNow{}}
}

// add takes a look at the open connections. A connection seen before adds
// what it carried since; a new one adds all it carried.
func (c *counter) add(list []clashConn, now time.Time) {
	for _, u := range c.users {
		u.Online = nil
	}
	next := make(map[string]clashConn, len(list))
	for _, conn := range list {
		next[conn.ID] = conn
		if len(conn.Chains) == 0 || !strings.HasPrefix(conn.Chains[0], userOutbound) {
			continue
		}
		id, ok := xray.UserID(strings.TrimPrefix(conn.Chains[0], userOutbound))
		if !ok {
			continue
		}
		u := c.users[id]
		if u == nil {
			u = &UserNow{}
			c.users[id] = u
		}
		prev := c.last[conn.ID]
		u.Up += max(conn.Upload-prev.Upload, 0)
		u.Down += max(conn.Download-prev.Download, 0)
		// NaïveProxy over TCP comes through the edge, from loopback.
		ip := net.ParseIP(conn.Metadata.SourceIP)
		if ip == nil || ip.IsLoopback() {
			continue
		}
		addr := ip.String()
		if !slices.ContainsFunc(u.Online, func(a xray.Address) bool { return a.IP == addr }) {
			u.Online = append(u.Online, xray.Address{IP: addr, LastSeen: now.Unix()})
		}
	}
	c.last = next
}

// store adds the counted traffic to the users in the database.
func (c *counter) store(ctx context.Context, st *store.Store, now time.Time) error {
	var list []store.UserActivity
	for id, u := range c.users {
		if u.Up > 0 || u.Down > 0 || len(u.Online) > 0 {
			list = append(list, store.UserActivity{ID: id, Up: u.Up, Down: u.Down, Seen: true})
		}
	}
	if len(list) > 0 {
		if err := st.AddTraffic(ctx, list, now); err != nil {
			return err
		}
	}
	for id, u := range c.users {
		u.Up, u.Down = 0, 0
		if len(u.Online) == 0 {
			delete(c.users, id)
		}
	}
	return nil
}

// write puts what is known now into the online file.
func (c *counter) write(path string, now time.Time) error {
	f := onlineFile{At: now.Unix(), Users: map[int64]UserNow{}}
	for id, u := range c.users {
		v := *u
		if v.Online == nil {
			v.Online = []xray.Address{}
		}
		f.Users[id] = v
	}
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadOnline reads the runner's file: nil when there is none or the
// runner stopped.
func (r *Runtime) ReadOnline(now time.Time) map[int64]UserNow {
	b, err := os.ReadFile(r.OnlinePath())
	if err != nil {
		return nil
	}
	var f onlineFile
	if json.Unmarshal(b, &f) != nil || now.Sub(time.Unix(f.At, 0)) > onlineFresh {
		return nil
	}
	return f.Users
}
