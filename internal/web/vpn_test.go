package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/Stilltiger45/TigerSB-client/pkg/profile"

	"github.com/Stilltiger45/TigerSB-panel/internal/edge"
	"github.com/Stilltiger45/TigerSB-panel/internal/health"
	"github.com/Stilltiger45/TigerSB-panel/internal/singbox"
	"github.com/Stilltiger45/TigerSB-panel/internal/store"
	"github.com/Stilltiger45/TigerSB-panel/internal/sub"
	"github.com/Stilltiger45/TigerSB-panel/internal/xray"
)

// fakeXray stands in for docker and systemctl with Xray running.
type fakeXray struct {
	mu      sync.Mutex
	calls   []string
	stdin   []string
	online  string
	traffic string
	// sessions are the open connections of users, by email.
	sessions map[string]int
	test     error // what `xray run -test` says
}

func (f *fakeXray) exec(_ context.Context, stdin []byte, name string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	line := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, line)
	if stdin != nil {
		f.stdin = append(f.stdin, string(stdin))
	}
	switch {
	case strings.Contains(line, " check -c "):
		if f.test != nil {
			return "FATAL " + f.test.Error(), errors.New("exit 1")
		}
		return "", nil
	case strings.Contains(line, " run -test "):
		if f.test != nil {
			return "Failed to start: " + f.test.Error(), errors.New("exit 23")
		}
		return "Configuration OK.", nil
	case strings.Contains(line, " adu "):
		n := strings.Count(string(stdin), `"tag"`)
		return "Added " + string(rune('0'+n)) + " user(s) in total.", nil
	case strings.Contains(line, " rmu "):
		return "Removed 1 user(s) in total.", nil
	case strings.Contains(line, " statsonline "):
		email := args[len(args)-1]
		if f.sessions[email] == 0 {
			return "failed to get stats: rpc error: code = NotFound desc = user>>>" + email + ">>>online not found.", errors.New("exit 1")
		}
		return fmt.Sprintf(`{"stat":{"name":"user>>>%s>>>online","value":%d}}`, email, f.sessions[email]), nil
	case strings.Contains(line, "statsonlineiplist"):
		return f.online, nil
	case strings.Contains(line, "statsquery"):
		return f.traffic, nil
	case name == "systemctl":
		return "", nil
	}
	return "", nil
}

func (f *fakeXray) joined() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.calls, "\n")
}

func vpnServer(t *testing.T) (*Server, *fakeXray, *http.Cookie) {
	t.Helper()
	s := newTestServer(t)
	f := &fakeXray{}
	s.Xray.Exec = f.exec
	s.Singbox.Exec = f.exec
	s.Mieru.Exec = f.exec
	s.Run = func(name string, args ...string) (string, error) {
		if name == "systemctl" && args[0] == "is-active" {
			return "active", nil
		}
		return "", nil
	}
	c := login(t, s, "admin", "very-secret-pass").Result().Cookies()[0]
	return s, f, c
}

func decodeUsers(t *testing.T, body []byte) usersResponse {
	t.Helper()
	var r usersResponse
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return r
}

func TestUsersLifecycle(t *testing.T) {
	s, f, c := vpnServer(t)
	w := do(s, http.MethodPost, "/"+secret+"/api/users/add", `{"name":" Мама ","devices":2}`, c)
	if w.Code != http.StatusOK {
		t.Fatalf("add: %d %s", w.Code, w.Body)
	}
	r := decodeUsers(t, w.Body.Bytes())
	if len(r.Users) != 1 || r.Warning != "" {
		t.Fatalf("users: %+v", r)
	}
	u := r.Users[0]
	if u.Name != "Мама" || u.Devices != 2 || !u.Enabled || u.Subs == nil || r.Domain != "example.com" {
		t.Fatalf("user: %+v", u)
	}
	// Only subscription links: tigersb:// with the panel key, and https://.
	pk, err := sub.PublicKey(context.Background(), s.store)
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimPrefix(u.Subs.HTTPS, "https://example.com/sub/")
	if len(token) != 32 || token == u.Subs.HTTPS {
		t.Fatalf("https link: %s", u.Subs.HTTPS)
	}
	key, err := profile.ParseKey(u.Subs.TigerSB)
	if err != nil || key.URL != u.Subs.HTTPS || profile.EncodeKey(key.PK) != pk {
		t.Fatalf("tigersb link %s: %v %+v", u.Subs.TigerSB, err, key)
	}
	// The user went into the running Xray, into both inbounds, and into the config on disk.
	if !strings.Contains(f.joined(), "docker exec -i tigersb-xray /usr/local/bin/xray api adu --server=127.0.0.1:10085 stdin:") {
		t.Fatalf("no live add:\n%s", f.joined())
	}
	if !strings.Contains(f.stdin[0], `"tag":"grpc"`) || !strings.Contains(f.stdin[0], `"tag":"xhttp"`) || !strings.Contains(f.stdin[0], store.SlotUUID(u.UUID, 1, 0)) {
		t.Fatalf("adu input: %s", f.stdin[0])
	}
	conf, _ := os.ReadFile(s.Xray.ConfigPath())
	if !strings.Contains(string(conf), store.SlotUUID(u.UUID, 1, 0)) || strings.Contains(string(conf), u.UUID) {
		t.Fatal("config on disk lacks the user's key, or has the user's own")
	}

	if w := do(s, http.MethodPost, "/"+secret+"/api/users/add", `{"name":"Мама","devices":1}`, c); w.Code != http.StatusBadRequest {
		t.Fatalf("duplicate name: %d", w.Code)
	}
	if w := do(s, http.MethodPost, "/"+secret+"/api/users/add", `{"name":"x","devices":99}`, c); w.Code != http.StatusBadRequest {
		t.Fatalf("too many devices: %d", w.Code)
	}

	// Live stats: traffic adds to the stored one, three addresses exceed two devices.
	// The keys of the places add up to the user.
	f.online = `{"users":[{"email":"u1s1","ips":[{"ip":"198.51.100.1","lastSeen":"1761500000"},{"ip":"198.51.100.2"}]},{"email":"u1s2","ips":[{"ip":"198.51.100.3"}]}]}`
	f.traffic = `{"stat":[{"name":"user>>>u1s1>>>traffic>>>uplink","value":10},{"name":"user>>>u1s1>>>traffic>>>downlink","value":15},{"name":"user>>>u1s2>>>traffic>>>downlink","value":5}]}`
	r = decodeUsers(t, do(s, http.MethodGet, "/"+secret+"/api/users", "", c).Body.Bytes())
	if !r.Live || r.Users[0].Down != 20 || len(r.Users[0].Online) != 3 || !r.Users[0].Over {
		t.Fatalf("live: %+v", r)
	}
	// sing-box and mbox add what they counted and their addresses.
	at := s.now().Unix()
	os.MkdirAll(s.Singbox.Dir, 0o700)
	os.MkdirAll(s.Mieru.Dir, 0o700)
	os.WriteFile(s.Singbox.OnlinePath(), fmt.Appendf(nil, `{"at":%d,"users":{"1":{"up":5,"down":7,"online":[{"ip":"198.51.100.3"},{"ip":"203.0.113.9"}]}}}`, at), 0o600)
	os.WriteFile(s.Mieru.OnlinePath(), fmt.Appendf(nil, `{"at":%d,"users":{"1":{"up":0,"down":3,"online":[]}}}`, at), 0o600)
	r = decodeUsers(t, do(s, http.MethodGet, "/"+secret+"/api/users", "", c).Body.Bytes())
	if r.Users[0].Down != 20+7+3 || r.Users[0].Up != 10+5 || len(r.Users[0].Online) != 4 {
		t.Fatalf("with sing-box: %+v", r.Users[0])
	}
	os.Remove(s.Singbox.OnlinePath())
	os.Remove(s.Mieru.OnlinePath())

	// A name changes nothing in Xray; a higher limit only adds the keys of
	// the new places, and the open connections keep theirs.
	before := len(f.calls)
	w = do(s, http.MethodPost, "/"+secret+"/api/users/update", `{"id":1,"name":"Папа"}`, c)
	if r := decodeUsers(t, w.Body.Bytes()); r.Users[0].Name != "Папа" {
		t.Fatalf("rename: %s", w.Body)
	}
	if calls := strings.Join(f.calls[before:], "\n"); strings.Contains(calls, " adu ") || strings.Contains(calls, " rmu ") {
		t.Fatalf("rename touched Xray:\n%s", calls)
	}
	before, stdin := len(f.calls), len(f.stdin)
	w = do(s, http.MethodPost, "/"+secret+"/api/users/update", `{"id":1,"devices":3}`, c)
	if r := decodeUsers(t, w.Body.Bytes()); r.Users[0].Over {
		t.Fatalf("devices: %s", w.Body)
	}
	if calls := strings.Join(f.calls[before:], "\n"); strings.Contains(calls, " rmu ") || len(f.stdin) != stdin+1 || !strings.Contains(f.stdin[stdin], `"email":"u1s3"`) {
		t.Fatalf("higher limit:\n%s", calls)
	}

	// Devices that fetched the subscription show up and can be removed.
	h := &sub.Handler{Store: s.store, Domain: func(context.Context) (string, error) { return "example.com", nil }}
	fetch := func(hwid string) int {
		req := httptest.NewRequest(http.MethodGet, "/sub/"+token, nil)
		req.Header.Set("User-Agent", "TigerSB-android/1.0")
		req.Header.Set("X-Hwid", hwid)
		req.Header.Set("X-Device-Model", "Pixel "+hwid)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	for _, hw := range []string{"a", "b", "c", "d"} {
		fetch(hw)
	}
	r = decodeUsers(t, do(s, http.MethodGet, "/"+secret+"/api/users", "", c).Body.Bytes())
	list := r.Users[0].List
	if len(list) != 3 || list[0].Model != "Pixel a" || list[2].Model != "Pixel c" {
		t.Fatalf("devices (limit 3): %+v", list)
	}
	w = do(s, http.MethodPost, "/"+secret+"/api/users/device/delete", fmt.Sprintf(`{"user_id":1,"id":%d}`, list[0].ID), c)
	if r := decodeUsers(t, w.Body.Bytes()); len(r.Users[0].List) != 2 {
		t.Fatalf("device delete: %s", w.Body)
	}
	if fetch("d") != http.StatusOK {
		t.Fatal("a freed place was not taken")
	}
	if w := do(s, http.MethodPost, "/"+secret+"/api/users/device/delete", `{"user_id":2,"id":1}`, c); w.Code != http.StatusBadRequest {
		t.Fatalf("device of another user: %d", w.Code)
	}

	// New links: a new key and a new address; the old address is gone and
	// the devices start over.
	w = do(s, http.MethodPost, "/"+secret+"/api/users/update", `{"id":1,"reissue":true}`, c)
	r = decodeUsers(t, w.Body.Bytes())
	if r.Users[0].UUID == u.UUID || r.Users[0].Subs.HTTPS == u.Subs.HTTPS || len(r.Users[0].List) != 0 {
		t.Fatalf("reissue: %+v", r.Users[0])
	}
	if fetch("a") != http.StatusNotFound {
		t.Fatal("old subscription address still works")
	}
	tail := strings.Join(f.calls[len(f.calls)-30:], "\n")
	if !strings.Contains(tail, "rmu --server=127.0.0.1:10085 -tag=grpc u1s1") || !strings.Contains(tail, "-tag=xhttp u1s3") {
		t.Fatalf("reissue:\n%s", tail)
	}

	// Off takes the user out of Xray and the config.
	do(s, http.MethodPost, "/"+secret+"/api/users/update", `{"id":1,"enabled":false}`, c)
	conf, _ = os.ReadFile(s.Xray.ConfigPath())
	if strings.Contains(string(conf), store.SlotUUID(r.Users[0].UUID, 1, 0)) || strings.Contains(string(conf), "u1s") {
		t.Fatal("disabled user is still in the config")
	}

	// QR of the subscription links.
	for _, kind := range []string{"tigersb", "https"} {
		if w := do(s, http.MethodGet, "/"+secret+"/api/users/qr?id=1&kind="+kind, "", c); w.Code != http.StatusOK || !strings.HasPrefix(w.Body.String(), "<svg") {
			t.Fatalf("qr %s: %d %.60s", kind, w.Code, w.Body)
		}
	}
	if w := do(s, http.MethodGet, "/"+secret+"/api/users/qr?id=9&kind=https", "", c); w.Code != http.StatusNotFound {
		t.Fatalf("qr of unknown user: %d", w.Code)
	}

	w = do(s, http.MethodPost, "/"+secret+"/api/users/delete", `{"id":1}`, c)
	if r := decodeUsers(t, w.Body.Bytes()); len(r.Users) != 0 {
		t.Fatalf("delete: %s", w.Body)
	}
	if w := do(s, http.MethodPost, "/"+secret+"/api/users/delete", `{"id":1}`, c); w.Code != http.StatusBadRequest {
		t.Fatalf("delete twice: %d", w.Code)
	}
}

func TestUsersNeedLogin(t *testing.T) {
	s, _, _ := vpnServer(t)
	for _, p := range []string{"/api/users", "/api/protocols", "/api/users/qr?id=1&kind=https"} {
		if w := do(s, http.MethodGet, "/"+secret+p, ""); w.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
	if w := do(s, http.MethodPost, "/"+secret+"/api/users/add", `{"name":"a","devices":1}`); w.Code != http.StatusUnauthorized {
		t.Errorf("add: %d", w.Code)
	}
	if w := do(s, http.MethodPost, "/"+secret+"/api/users/device/delete", `{"user_id":1,"id":1}`); w.Code != http.StatusUnauthorized {
		t.Errorf("device delete: %d", w.Code)
	}
}

func TestProtocolsSave(t *testing.T) {
	s, f, c := vpnServer(t)
	w := do(s, http.MethodGet, "/"+secret+"/api/protocols", "", c)
	var p protocolsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil || p.Settings.GRPC.Port != 8443 || p.AutoAddress != "203.0.113.9" {
		t.Fatalf("get: %v %s", err, w.Body)
	}

	set := p.Settings
	set.GRPC.Port = 443
	set.GRPC.Reality.SNI = "www.microsoft.com"
	body, _ := json.Marshal(set)
	w = do(s, http.MethodPost, "/"+secret+"/api/protocols", string(body), c)
	if w.Code != http.StatusOK {
		t.Fatalf("save: %d %s", w.Code, w.Body)
	}
	joined := f.joined()
	if !strings.Contains(joined, "docker run --rm --network none --log-driver none -v ") || !strings.Contains(joined, "systemctl restart tigersb-xray.service") {
		t.Fatalf("not tested and restarted:\n%s", joined)
	}
	routes, err := edge.Read(s.Xray.Routes)
	if err != nil {
		t.Fatal(err)
	}
	if b := routes.Pick("www.microsoft.com"); b == nil || b.Addr != "127.0.0.1:10441" {
		t.Fatalf("grpc on 443 needs an edge route: %+v", routes)
	}
	if b := routes.Pick("example.com"); b == nil || b.Addr != "127.0.0.1:8444" {
		t.Fatalf("the panel domain must stay with Caddy: %+v", routes)
	}

	// Xray refuses: nothing is saved.
	f.test = errors.New("invalid REALITY config")
	set.GRPC.ServiceName = "other"
	body, _ = json.Marshal(set)
	w = do(s, http.MethodPost, "/"+secret+"/api/protocols", string(body), c)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid REALITY config") {
		t.Fatalf("refused config: %d %s", w.Code, w.Body)
	}
	got, _ := xray.Load(context.Background(), s.store)
	if got.GRPC.ServiceName == "other" {
		t.Fatal("refused settings were saved")
	}

	// Validation errors never reach Xray.
	set.XHTTP.Reality.SNI = "example.com"
	body, _ = json.Marshal(set)
	if w := do(s, http.MethodPost, "/"+secret+"/api/protocols", string(body), c); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "доменом панели") {
		t.Fatalf("own domain as SNI: %d %s", w.Code, w.Body)
	}

	// A panel domain that is a protocol's SNI is refused on the site tab.
	w = do(s, http.MethodPost, "/"+secret+"/api/site", `{"domain":"cloudflare.com","template":"bakery","name":"x"}`, c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("domain = SNI: %d %s", w.Code, w.Body)
	}
}

func TestProtocolKeys(t *testing.T) {
	s, _, c := vpnServer(t)
	w := do(s, http.MethodPost, "/"+secret+"/api/protocols/keys", `{}`, c)
	var k struct {
		Private string   `json:"private_key"`
		Public  string   `json:"public_key"`
		IDs     []string `json:"short_ids"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &k); err != nil || len(k.Private) != 43 || len(k.Public) != 43 || len(k.IDs) != 1 {
		t.Fatalf("keys: %s", w.Body)
	}
}

// TUIC is saved apart from Xray: only sing-box is checked and reloaded, and
// the password secret never leaves the server.
func TestProtocolsSaveTUIC(t *testing.T) {
	s, f, c := vpnServer(t)
	w := do(s, http.MethodGet, "/"+secret+"/api/protocols", "", c)
	var p protocolsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if !p.Singbox.TUIC.Enabled || p.Singbox.TUIC.Port != 8443 || p.Singbox.Secret != "" || strings.Contains(w.Body.String(), `"secret"`) {
		t.Fatalf("get: %s", w.Body)
	}
	stored, _ := singbox.Load(context.Background(), s.store)

	sb := p.Singbox
	sb.TUIC.Port = 9443
	sb.TUIC.CongestionControl = "cubic"
	sb.Secret = "attacker-chosen-secret-attacker-chosen-secret"
	body, _ := json.Marshal(map[string]any{"address": p.Settings.Address, "grpc": p.Settings.GRPC, "xhttp": p.Settings.XHTTP, "singbox": sb})
	w = do(s, http.MethodPost, "/"+secret+"/api/protocols", string(body), c)
	if w.Code != http.StatusOK {
		t.Fatalf("save: %d %s", w.Code, w.Body)
	}
	var got protocolsResponse
	json.Unmarshal(w.Body.Bytes(), &got)
	joined := f.joined()
	if !strings.Contains(joined, "singbox:test check -c /etc/tigersb-singbox/test.json") || !strings.Contains(joined, "systemctl try-reload-or-restart tigersb-singbox.service") {
		t.Fatalf("sing-box not checked and reloaded:\n%s", joined)
	}
	if strings.Contains(joined, "tigersb-xray.service") || strings.Contains(joined, " run -test ") {
		t.Fatalf("a TUIC change touched Xray:\n%s", joined)
	}
	if got.Singbox.TUIC.Port != 9443 || got.Applied != "sing-box перечитал настройки" {
		t.Fatalf("response: %s", w.Body)
	}
	after, _ := singbox.Load(context.Background(), s.store)
	if after.Secret != stored.Secret || after.TUIC.CongestionControl != "cubic" {
		t.Fatalf("stored: %+v", after)
	}

	sb.TUIC.CongestionControl = "brutal"
	body, _ = json.Marshal(map[string]any{"address": p.Settings.Address, "grpc": p.Settings.GRPC, "xhttp": p.Settings.XHTTP, "singbox": sb})
	if w := do(s, http.MethodPost, "/"+secret+"/api/protocols", string(body), c); w.Code != http.StatusBadRequest {
		t.Fatalf("bad TUIC: %d %s", w.Code, w.Body)
	}
}

// Hysteria 2 comes on by itself with Salamander; a cleared password gets a
// new one from the secret, and TUIC and Hysteria 2 cannot share a UDP port.
func TestProtocolsSaveHysteria2(t *testing.T) {
	s, _, c := vpnServer(t)
	w := do(s, http.MethodGet, "/"+secret+"/api/protocols", "", c)
	var p protocolsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	h := p.Singbox.Hysteria2
	if !h.Enabled || h.Port != 443 || !h.Obfs || len(h.ObfsPassword) < 8 || len(p.BBRProfiles) != 3 {
		t.Fatalf("get: %s", w.Body)
	}
	save := func(sb singbox.Settings) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"address": p.Settings.Address, "grpc": p.Settings.GRPC, "xhttp": p.Settings.XHTTP, "singbox": sb})
		return do(s, http.MethodPost, "/"+secret+"/api/protocols", string(body), c)
	}
	sb := p.Singbox
	sb.Hysteria2.ObfsPassword = ""
	sb.Hysteria2.BBRProfile = "aggressive"
	if w := save(sb); w.Code != http.StatusOK {
		t.Fatalf("save: %d %s", w.Code, w.Body)
	}
	after, _ := singbox.Load(context.Background(), s.store)
	if after.Hysteria2.ObfsPassword != h.ObfsPassword || after.Hysteria2.BBRProfile != "aggressive" {
		t.Fatalf("stored: %+v", after.Hysteria2)
	}
	sb.Hysteria2.Port = sb.TUIC.Port
	if w := save(sb); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "разные порты") {
		t.Fatalf("same UDP port: %d %s", w.Code, w.Body)
	}
}

func TestCoreRestart(t *testing.T) {
	s, f, c := vpnServer(t)
	old := coreSettle
	coreSettle = 0
	defer func() { coreSettle = old }()
	api := "/" + secret + "/api/control/core"
	for core, unit := range map[string]string{"xray": xray.Unit, "singbox": singbox.Unit, "mieru": singbox.MieruUnit} {
		w := do(s, http.MethodPost, api, `{"core":"`+core+`"}`, c)
		if w.Code != http.StatusOK || !strings.Contains(f.joined(), "systemctl restart "+unit) {
			t.Fatalf("%s: %d %s\n%s", core, w.Code, w.Body, f.joined())
		}
		var resp struct {
			Cores  []coreView `json:"cores"`
			Health struct {
				Results []struct{ ID string } `json:"results"`
			} `json:"health"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || len(resp.Cores) != 3 || resp.Cores[0].State != "active" || len(resp.Health.Results) == 0 {
			t.Fatalf("%s: %s", core, w.Body)
		}
	}
	// Xray restarts with a config rebuilt from the database.
	if _, err := os.Stat(s.Xray.ConfigPath()); err != nil {
		t.Errorf("xray config not written: %v", err)
	}
	if w := do(s, http.MethodPost, api, `{"core":"nginx"}`, c); w.Code != http.StatusBadRequest {
		t.Errorf("unknown core: %d", w.Code)
	}
	if w := do(s, http.MethodPost, api, `{"core":"xray"}`); w.Code == http.StatusOK {
		t.Error("restart without login")
	}
}

func TestTestLinks(t *testing.T) {
	s, f, c := vpnServer(t)
	if w := do(s, http.MethodPost, "/"+secret+"/api/users/add", `{"name":"Мама","devices":1}`, c); w.Code != http.StatusOK {
		t.Fatalf("add: %d %s", w.Code, w.Body)
	}
	test := func(body string) protocolsResponse {
		t.Helper()
		w := do(s, http.MethodPost, "/"+secret+"/api/protocols/test", body, c)
		var p protocolsResponse
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &p) != nil {
			t.Fatalf("%s: %d %s", body, w.Code, w.Body)
		}
		return p
	}
	p := test(`{"protocol":"grpc"}`)
	first := p.Tests["grpc"]
	if !strings.HasPrefix(first, "vless://") || len(p.Tests) != 1 {
		t.Fatalf("tests: %v", p.Tests)
	}
	if !strings.Contains(f.joined(), " adu ") || !strings.Contains(f.stdin[len(f.stdin)-1], `"test-grpc"`) || strings.Contains(f.stdin[len(f.stdin)-1], `"xhttp"`) {
		t.Fatalf("live add: %v", f.stdin)
	}
	conf, _ := os.ReadFile(s.Xray.ConfigPath())
	if strings.Count(string(conf), `"test-grpc"`) != 1 {
		t.Fatalf("config: %s", conf)
	}
	if u := decodeUsers(t, do(s, http.MethodGet, "/"+secret+"/api/users", "", c).Body.Bytes()); len(u.Users) != 1 {
		t.Fatalf("a test link shows up among the users: %+v", u.Users)
	}
	if again := test(`{"protocol":"grpc"}`).Tests["grpc"]; again == first || !strings.Contains(f.joined(), "-tag=grpc test-grpc") {
		t.Fatalf("new link: %q, calls %s", again, f.joined())
	}
	// TUIC has no certificate here: the link is kept and shows up once it works.
	if p := test(`{"protocol":"tuic"}`); p.Tests["tuic"] != "" || len(p.Tests) != 2 {
		t.Fatalf("tuic: %v", p.Tests)
	}
	if p := test(`{"protocol":"grpc","delete":true}`); len(p.Tests) != 1 {
		t.Fatalf("delete: %v", p.Tests)
	}
	if w := do(s, http.MethodPost, "/"+secret+"/api/protocols/test", `{"protocol":"ssh"}`, c); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown protocol: %d", w.Code)
	}
}

func TestTCPClash(t *testing.T) {
	set, _ := xray.Defaults()
	sb, _ := singbox.Defaults()
	if err := tcpClash(set, sb, 2053); err != nil {
		t.Fatalf("defaults: %v", err)
	}
	sb.Naive.Port = set.GRPC.Port
	if err := tcpClash(set, sb, 2053); err == nil || !strings.Contains(err.Error(), "NaïveProxy и gRPC") {
		t.Fatalf("naive on gRPC: %v", err)
	}
	sb, _ = singbox.Defaults()
	if err := tcpClash(set, sb, 2015); err == nil || !strings.Contains(err.Error(), "Mieru: порт 2015 занят: панель") {
		t.Fatalf("panel in Mieru's range: %v", err)
	}
	sb.Mieru.Transport = "UDP"
	if err := tcpClash(set, sb, 2015); err != nil {
		t.Fatalf("Mieru over UDP leaves TCP alone: %v", err)
	}
}

func TestOverview(t *testing.T) {
	s, f, c := vpnServer(t)
	s.HealthBase = health.Env{Listening: func(int) bool { return true }, ListeningUDP: func(int) bool { return true }, TUICCert: func() bool { return true }}
	if w := do(s, http.MethodPost, "/"+secret+"/api/users/add", `{"name":"Мама","devices":1}`, c); w.Code != http.StatusOK {
		t.Fatalf("add: %d %s", w.Code, w.Body)
	}
	do(s, http.MethodPost, "/"+secret+"/api/users/add", `{"name":"Папа","devices":1}`, c)
	f.online = `{"users":[{"email":"u1","ips":[{"ip":"198.51.100.1"},{"ip":"198.51.100.2"}]}]}`
	w := do(s, http.MethodGet, "/"+secret+"/api/overview", "", c)
	var r overviewResponse
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil || w.Code != http.StatusOK {
		t.Fatalf("%d %v: %s", w.Code, err, w.Body)
	}
	if u := r.Users; u.Total != 2 || u.Enabled != 2 || u.Online != 1 || u.Over != 1 || !u.Live {
		t.Fatalf("users: %+v", u)
	}
	if len(r.Protocols) != 6 || r.Protocols[0].ID != "xhttp" || r.Protocols[0].State != health.OK {
		t.Fatalf("protocols: %+v", r.Protocols)
	}
	if len(r.Cores) != 3 || r.Edge != "active" || r.Sub != "active" {
		t.Fatalf("services: %+v %s %s", r.Cores, r.Edge, r.Sub)
	}
	// The Users tab shows the same protocols.
	if u := decodeUsers(t, do(s, http.MethodGet, "/"+secret+"/api/users", "", c).Body.Bytes()); len(u.Protocols) != 6 {
		t.Fatalf("users protocols: %+v", u.Protocols)
	}
	if w := do(s, http.MethodGet, "/"+secret+"/api/overview", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("without login: %d", w.Code)
	}
}
