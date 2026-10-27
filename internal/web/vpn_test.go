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
	test    error // what `xray run -test` says
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
	if !strings.Contains(f.stdin[0], `"tag":"grpc"`) || !strings.Contains(f.stdin[0], `"tag":"xhttp"`) || !strings.Contains(f.stdin[0], u.UUID) {
		t.Fatalf("adu input: %s", f.stdin[0])
	}
	conf, _ := os.ReadFile(s.Xray.ConfigPath())
	if !strings.Contains(string(conf), u.UUID) {
		t.Fatal("config on disk lacks the user")
	}

	if w := do(s, http.MethodPost, "/"+secret+"/api/users/add", `{"name":"Мама","devices":1}`, c); w.Code != http.StatusBadRequest {
		t.Fatalf("duplicate name: %d", w.Code)
	}
	if w := do(s, http.MethodPost, "/"+secret+"/api/users/add", `{"name":"x","devices":99}`, c); w.Code != http.StatusBadRequest {
		t.Fatalf("too many devices: %d", w.Code)
	}

	// Live stats: traffic adds to the stored one, three addresses exceed two devices.
	f.online = `{"users":[{"email":"u1","ips":[{"ip":"198.51.100.1","lastSeen":"1761500000"},{"ip":"198.51.100.2"},{"ip":"198.51.100.3"}]}]}`
	f.traffic = `{"stat":[{"name":"user>>>u1>>>traffic>>>uplink","value":10},{"name":"user>>>u1>>>traffic>>>downlink","value":20}]}`
	r = decodeUsers(t, do(s, http.MethodGet, "/"+secret+"/api/users", "", c).Body.Bytes())
	if !r.Live || r.Users[0].Down != 20 || len(r.Users[0].Online) != 3 || !r.Users[0].Over {
		t.Fatalf("live: %+v", r)
	}

	// Name and devices change nothing in Xray.
	before := len(f.calls)
	w = do(s, http.MethodPost, "/"+secret+"/api/users/update", `{"id":1,"name":"Папа","devices":3}`, c)
	if r := decodeUsers(t, w.Body.Bytes()); r.Users[0].Name != "Папа" || r.Users[0].Over {
		t.Fatalf("rename: %s", w.Body)
	}
	for _, call := range f.calls[before:] {
		if strings.Contains(call, " adu ") || strings.Contains(call, " rmu ") {
			t.Fatalf("rename touched Xray: %s", call)
		}
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
	tail := strings.Join(f.calls[len(f.calls)-6:], "\n")
	if !strings.Contains(tail, "rmu --server=127.0.0.1:10085 -tag=grpc u1") || !strings.Contains(tail, "-tag=xhttp u1") {
		t.Fatalf("reissue:\n%s", tail)
	}

	// Off takes the user out of Xray and the config.
	do(s, http.MethodPost, "/"+secret+"/api/users/update", `{"id":1,"enabled":false}`, c)
	conf, _ = os.ReadFile(s.Xray.ConfigPath())
	if strings.Contains(string(conf), r.Users[0].UUID) {
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
