package sub

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stilltiger45/TigerSB-client/pkg/profile"

	"github.com/Stilltiger45/TigerSB-panel/internal/singbox"
	"github.com/Stilltiger45/TigerSB-panel/internal/store"
)

func setup(t *testing.T) (*Handler, *store.Store, *store.User) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	token, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	u := &store.User{Name: "Мама", UUID: "5783a3e7-e373-41cd-8642-c83782b807c5", Devices: 2, Enabled: true, Created: time.Now(), SubToken: token}
	if err := st.AddUser(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	h := &Handler{
		Store:  st,
		Domain: func(context.Context) (string, error) { return "vpn.example.com", nil },
		Now:    func() time.Time { return time.Date(2026, 10, 27, 6, 0, 0, 0, time.UTC) },
	}
	return h, st, u
}

func get(h http.Handler, method, path, ua, hwid string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = "127.0.0.1:40000"
	req.Header.Set("X-Forwarded-For", "203.0.113.50")
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	if hwid != "" {
		req.Header.Set("X-Hwid", hwid)
		req.Header.Set("X-Device-Os", "Android")
		req.Header.Set("X-Ver-Os", "14")
		req.Header.Set("X-Device-Model", "Pixel 8")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestProfileForTigerSB(t *testing.T) {
	h, st, u := setup(t)
	w := get(h, http.MethodGet, "/sub/"+u.SubToken, "TigerSB-android/0.1", "hw1")
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %v %s", w.Code, w.Header(), w.Body)
	}
	pk, err := PublicKey(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	pinned, _ := profile.DecodePublicKey(pk)
	p, _, err := profile.Open(w.Body.Bytes(), pinned)
	if err != nil {
		t.Fatalf("client rejects the profile: %v\n%s", err, w.Body)
	}
	if p.Update == nil || p.Update.URL != "https://vpn.example.com/sub/"+u.SubToken || len(p.Exits) != 1 {
		t.Fatalf("profile: %+v", p)
	}
	tr := p.Exits[0].Transports
	// Without a certificate only Xray and Mieru, which needs none, are there.
	if len(tr) != 3 || tr[0].ID != "grpc" || tr[0].Prio != 20 || tr[1].ID != "xhttp" || tr[1].Prio != 10 || tr[2].ID != "mieru" || tr[2].Prio != 60 {
		t.Fatalf("transports: %+v", tr)
	}
	// The first device gets the key of the first place, not the user's own.
	key := store.SlotUUID(u.UUID, 1, 0)
	if !strings.Contains(string(tr[1].Xray), key) || strings.Contains(string(tr[1].Xray), u.UUID) || !strings.Contains(string(tr[1].Xray), `"network":"xhttp"`) {
		t.Errorf("xhttp outbound: %s", tr[1].Xray)
	}
	// A key signed by anyone else is refused, so a forged subscription fails.
	other, _, _ := profile.GenerateKey()
	if _, _, err := profile.Open(w.Body.Bytes(), other); err == nil {
		t.Error("profile accepted under another key")
	}
}

func TestPlainForOtherClients(t *testing.T) {
	h, _, u := setup(t)
	w := get(h, http.MethodGet, "/sub/"+u.SubToken, "v2rayNG/1.10.16", "hw1")
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	raw, err := base64.StdEncoding.DecodeString(w.Body.String())
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "vless://"+store.SlotUUID(u.UUID, 1, 0)+"@") {
		t.Fatalf("links: %q", lines)
	}
	// Mieru: the key's name inside mbox and the whole port range.
	if !strings.HasPrefix(lines[2], "mierus://u1s1:") || !strings.Contains(lines[2], "@vpn.example.com?profile=TigerSB&port=2012-2022&protocol=TCP#") {
		t.Fatalf("mieru link: %q", lines[2])
	}
	if w.Header().Get("Profile-Update-Interval") != "6" || !strings.HasPrefix(w.Header().Get("Subscription-Userinfo"), "upload=0; download=0") {
		t.Errorf("headers: %v", w.Header())
	}
}

// Everything that is not a live subscription is a plain 404, which Caddy
// shows as the maintenance page.
func TestMissesLookLikeTheSite(t *testing.T) {
	h, st, u := setup(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/sub/" + strings.Repeat("A", 32)},
		{http.MethodGet, "/sub/short"},
		{http.MethodGet, "/sub/" + u.SubToken + "/x"},
		{http.MethodGet, "/sub/../" + u.SubToken},
		{http.MethodGet, "/other/" + u.SubToken},
		{http.MethodPost, "/sub/" + u.SubToken},
	} {
		if w := get(h, tc.method, tc.path, "v2rayNG/1", "hw"); w.Code != http.StatusNotFound {
			t.Errorf("%s %s: %d", tc.method, tc.path, w.Code)
		}
	}
	u.Enabled = false
	if err := st.UpdateUser(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	if w := get(h, http.MethodGet, "/sub/"+u.SubToken, "v2rayNG/1", "hw"); w.Code != http.StatusNotFound {
		t.Errorf("disabled user: %d", w.Code)
	}
}

func TestDeviceLimit(t *testing.T) {
	h, st, u := setup(t)
	path := "/sub/" + u.SubToken
	for _, hw := range []string{"one", "two", "one"} {
		if w := get(h, http.MethodGet, path, "TigerSB-android/0.1", hw); w.Code != http.StatusOK {
			t.Fatalf("%s: %d", hw, w.Code)
		}
	}
	if w := get(h, http.MethodGet, path, "TigerSB-android/0.1", "three"); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "Лимит устройств (2)") {
		t.Fatalf("third device: %d %s", w.Code, w.Body)
	}
	// Other clients get one dead server whose name explains the limit.
	w := get(h, http.MethodGet, path, "Hiddify/2.0", "three")
	raw, _ := base64.StdEncoding.DecodeString(w.Body.String())
	if w.Code != http.StatusOK || !strings.HasPrefix(string(raw), "vless://00000000-0000-0000-0000-000000000000@0.0.0.0:1") || strings.Contains(string(raw), u.UUID) {
		t.Fatalf("limit for other clients: %d %s", w.Code, raw)
	}
	list, err := st.Devices(context.Background())
	if err != nil || len(list) != 2 || list[0].Model != "Pixel 8" || list[0].IP != "203.0.113.50" || list[0].OSVersion != "14" {
		t.Fatalf("devices: %v %+v", err, list)
	}
	// A freed place is taken by the next device.
	if err := st.DeleteDevice(context.Background(), u.ID, list[0].ID); err != nil {
		t.Fatal(err)
	}
	if w := get(h, http.MethodGet, path, "TigerSB-android/0.1", "three"); w.Code != http.StatusOK {
		t.Fatalf("after delete: %d", w.Code)
	}
	// It holds the freed place with a new key; the forgotten device's key
	// is gone, and the other device keeps its own.
	list, _ = st.Devices(context.Background())
	got := map[string]int{}
	for _, d := range list {
		got[d.HWID] = d.Slot
	}
	if len(list) != 2 || got["three"] != 1 || got["two"] != 2 {
		t.Fatalf("places: %+v", got)
	}
	w = get(h, http.MethodGet, path, "Hiddify/2.0", "three")
	raw, _ = base64.StdEncoding.DecodeString(w.Body.String())
	if !strings.Contains(string(raw), store.SlotUUID(u.UUID, 1, 1)) || strings.Contains(string(raw), store.SlotUUID(u.UUID, 1, 0)) {
		t.Fatalf("new key: %s", raw)
	}
}

// Clients without x-hwid take one place per app; a forwarded address counts
// only when Caddy on loopback sent it.
func TestDeviceWithoutHWID(t *testing.T) {
	h, st, u := setup(t)
	get(h, http.MethodGet, "/sub/"+u.SubToken, "v2rayNG/1.10.16", "")
	get(h, http.MethodGet, "/sub/"+u.SubToken, "v2rayNG/1.10.17", "")
	req := httptest.NewRequest(http.MethodGet, "/sub/"+u.SubToken, nil)
	req.RemoteAddr = "198.51.100.7:1234"
	req.Header.Set("X-Forwarded-For", "10.0.0.1")
	req.Header.Set("User-Agent", "Happ/1.0")
	h.ServeHTTP(httptest.NewRecorder(), req)
	list, _ := st.Devices(context.Background())
	if len(list) != 2 || list[0].HWID != "app:v2rayNG" || list[0].App != "v2rayNG/1.10.17" || list[1].IP != "198.51.100.7" {
		t.Fatalf("devices: %+v", list)
	}
}

func TestSigningKeyStays(t *testing.T) {
	_, st, _ := setup(t)
	a, err := PublicKey(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := PublicKey(context.Background(), st)
	if a != b || len(a) != 43 {
		t.Fatalf("key changed or malformed: %q %q", a, b)
	}
	u := store.User{Name: "Мама", SubToken: "tok"}
	if LinksFor("", a, u) != nil {
		t.Error("links without a domain")
	}
	l := LinksFor("vpn.example.com", a, u)
	k, err := profile.ParseKey(l.TigerSB)
	if err != nil || k.URL != "https://vpn.example.com/sub/tok" || k.Name != "Мама" {
		t.Fatalf("key %s: %v %+v", l.TigerSB, err, k)
	}
}

// With sing-box holding the certificate, TUIC and Hysteria 2 join both
// subscriptions.
func TestSingboxInSubscriptions(t *testing.T) {
	h, st, u := setup(t)
	h.TUICCert = func() bool { return true }
	w := get(h, http.MethodGet, "/sub/"+u.SubToken, "TigerSB-android/0.1", "hw1")
	pk, _ := PublicKey(context.Background(), st)
	pinned, _ := profile.DecodePublicKey(pk)
	p, _, err := profile.Open(w.Body.Bytes(), pinned)
	if err != nil {
		t.Fatalf("%v\n%s", err, w.Body)
	}
	// The client shows the profile name: the user's, not the brand.
	if p.Name != "Мама" || w.Header().Get("Profile-Title") != "base64:"+base64.StdEncoding.EncodeToString([]byte("TigerSB · Мама")) {
		t.Errorf("name %q, title %q", p.Name, w.Header().Get("Profile-Title"))
	}
	tr := p.Exits[0].Transports
	if len(tr) != 6 || tr[2].ID != "tuic" || tr[2].Prio != 30 || !strings.Contains(string(tr[2].Xray), `"serverName":"vpn.example.com"`) {
		t.Fatalf("transports: %+v", tr)
	}
	// Hysteria 2 reaches the client with Salamander and its password.
	if hy := string(tr[3].Xray); tr[3].ID != "hysteria2" || tr[3].Prio != 40 || !strings.Contains(hy, `"type":"salamander"`) || !strings.Contains(hy, `"port":443`) {
		t.Fatalf("hysteria2: %s", tr[3].Xray)
	}
	w = get(h, http.MethodGet, "/sub/"+u.SubToken, "Hiddify/2.0", "hw1")
	raw, _ := base64.StdEncoding.DecodeString(w.Body.String())
	lines := strings.Split(string(raw), "\n")
	if len(lines) != 6 || !strings.HasPrefix(lines[2], "tuic://"+store.SlotUUID(u.UUID, 1, 0)+":") || !strings.Contains(lines[2], "sni=vpn.example.com") {
		t.Fatalf("links: %q", lines)
	}
	if !strings.HasPrefix(lines[3], "hysteria2://") || !strings.Contains(lines[3], "@vpn.example.com:443/?sni=vpn.example.com&obfs=salamander&obfs-password=") {
		t.Fatalf("hysteria2 link: %q", lines[3])
	}
	// NaïveProxy: the key's name inside sing-box, the TCP port and padding.
	if !strings.HasPrefix(lines[4], "naive+https://u1s1:") || !strings.Contains(lines[4], "@vpn.example.com:2443?sni=vpn.example.com&padding=true#") {
		t.Fatalf("naive link: %q", lines[4])
	}
	if nv := string(tr[4].Xray); tr[4].ID != "naive" || tr[4].Prio != 50 || !strings.Contains(nv, `"port":2443`) || !strings.Contains(nv, `"padding":true`) {
		t.Fatalf("naive: %s", nv)
	}

	// NaïveProxy on TCP and QUIC: both links, two transports in the profile.
	sb, _ := singbox.Load(context.Background(), st)
	sb.Naive.Network = singbox.NaiveBoth
	singbox.Save(context.Background(), st, sb)
	w = get(h, http.MethodGet, "/sub/"+u.SubToken, "Hiddify/2.0", "hw1")
	raw, _ = base64.StdEncoding.DecodeString(w.Body.String())
	lines = strings.Split(string(raw), "\n")
	if len(lines) != 7 || !strings.HasPrefix(lines[4], "naive+https://") || !strings.HasPrefix(lines[5], "naive+quic://") {
		t.Fatalf("links: %q", lines)
	}
	w = get(h, http.MethodGet, "/sub/"+u.SubToken, "TigerSB-android/0.1", "hw1")
	if p, _, err = profile.Open(w.Body.Bytes(), pinned); err != nil || len(p.Exits[0].Transports) != 7 {
		t.Fatalf("%v: %+v", err, p)
	}
	if q := p.Exits[0].Transports[5]; q.ID != "naive-quic" || q.Prio != 55 || !strings.Contains(string(q.Xray), `"quic":true`) {
		t.Fatalf("naive QUIC: %+v %s", q, q.Xray)
	}

	// Mieru on TCP and UDP: two links, two transports.
	sb.Mieru.Transport = singbox.MieruBoth
	singbox.Save(context.Background(), st, sb)
	w = get(h, http.MethodGet, "/sub/"+u.SubToken, "Hiddify/2.0", "hw1")
	raw, _ = base64.StdEncoding.DecodeString(w.Body.String())
	lines = strings.Split(string(raw), "\n")
	if len(lines) != 8 || !strings.Contains(lines[6], "&protocol=TCP#") || !strings.Contains(lines[7], "&protocol=UDP#") {
		t.Fatalf("links: %q", lines)
	}
	w = get(h, http.MethodGet, "/sub/"+u.SubToken, "TigerSB-android/0.1", "hw1")
	if p, _, err = profile.Open(w.Body.Bytes(), pinned); err != nil || len(p.Exits[0].Transports) != 8 {
		t.Fatalf("%v: %+v", err, p)
	}
	m, mu := p.Exits[0].Transports[6], p.Exits[0].Transports[7]
	if m.ID != "mieru" || m.Prio != 60 || mu.ID != "mieru-udp" || mu.Prio != 65 || !strings.Contains(string(mu.Xray), `"protocol":"UDP"`) {
		t.Fatalf("mieru: %+v %+v", m, mu)
	}
}
