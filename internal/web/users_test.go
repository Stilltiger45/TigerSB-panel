package web

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stilltiger45/TigerSB-panel/internal/singbox"
	"github.com/Stilltiger45/TigerSB-panel/internal/sub"
)

// caddyCert puts a certificate for domain where Caddy keeps it, so sing-box
// serves TUIC, Hysteria 2 and NaïveProxy.
func caddyCert(t *testing.T, storage, domain string) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: domain},
		DNSNames: []string{domain}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(30 * 24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(storage, "certificates", "acme", domain)
	os.MkdirAll(dir, 0o700)
	kb, _ := x509.MarshalECPrivateKey(key)
	os.WriteFile(filepath.Join(dir, domain+".crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(filepath.Join(dir, domain+".key"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
}

// TestUsersComeAndGo adds, reissues and deletes users and checks that every
// core serves exactly the users there are, with their current keys: an old
// key, a removed user and an old subscription address stop working, while
// the other users and new ones work.
func TestUsersComeAndGo(t *testing.T) {
	s, f, c := vpnServer(t)
	caddyCert(t, s.Site.Dir, "example.com")
	ctx := context.Background()
	sb, err := singbox.Load(ctx, s.store)
	if err != nil {
		t.Fatal(err)
	}
	// keys are what each core knows a user by.
	keys := func(uuid string) map[string]string {
		return map[string]string{
			"Xray":       uuid,
			"TUIC":       sb.Password(uuid),
			"Hysteria 2": sb.Hy2Password(uuid),
			"NaïveProxy": sb.NaivePassword(uuid),
			"Mieru":      sb.MieruPassword(uuid),
		}
	}
	configs := func() map[string]string {
		t.Helper()
		out := map[string]string{}
		for name, path := range map[string]string{"Xray": s.Xray.ConfigPath(), "sing-box": s.Singbox.ConfigPath(), "Mieru": s.Mieru.ConfigPath()} {
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			out[name] = string(b)
		}
		return out
	}
	core := map[string]string{"Xray": "Xray", "TUIC": "sing-box", "Hysteria 2": "sing-box", "NaïveProxy": "sing-box", "Mieru": "Mieru"}
	served := func(who, uuid string, want bool) {
		t.Helper()
		conf := configs()
		for proto, key := range keys(uuid) {
			if got := strings.Contains(conf[core[proto]], key); got != want {
				t.Errorf("%s in %s: served %v, want %v", who, proto, got, want)
			}
		}
	}
	h := &sub.Handler{Store: s.store, Domain: func(context.Context) (string, error) { return "example.com", nil }, TUICCert: s.Singbox.HaveCert}
	fetch := func(link string) (int, []string) {
		req := httptest.NewRequest(http.MethodGet, strings.TrimPrefix(link, "https://example.com"), nil)
		req.Header.Set("User-Agent", "Hiddify/2.0")
		req.Header.Set("X-Hwid", "phone")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		raw, _ := base64.StdEncoding.DecodeString(rec.Body.String())
		return rec.Code, strings.Split(string(raw), "\n")
	}
	add := func(name string) userView {
		t.Helper()
		r := decodeUsers(t, do(s, http.MethodPost, "/"+secret+"/api/users/add", `{"name":"`+name+`","devices":2}`, c).Body.Bytes())
		if r.Warning != "" {
			t.Fatalf("add %s: %s", name, r.Warning)
		}
		return r.Users[len(r.Users)-1]
	}
	since := func(n int) string { return strings.Join(f.calls[n:], "\n") }

	mama, papa := add("Мама"), add("Папа")
	served("Мама", mama.UUID, true)
	served("Папа", papa.UUID, true)
	for _, u := range []userView{mama, papa} {
		code, links := fetch(u.Subs.HTTPS)
		if code != http.StatusOK || len(links) != 6 || !strings.Contains(strings.Join(links, "\n"), u.UUID) {
			t.Fatalf("%s subscription: %d %q", u.Name, code, links)
		}
	}

	// New links for Мама while she downloads over XHTTP: Xray drops the user,
	// sees her connections still open and restarts to end them; the restart
	// reads her new key, so it is not added again.
	f.sessions = map[string]int{"u1": 2}
	n := len(f.calls)
	r := decodeUsers(t, do(s, http.MethodPost, "/"+secret+"/api/users/update", `{"id":1,"reissue":true}`, c).Body.Bytes())
	mama2 := r.Users[0]
	calls := since(n)
	if r.Warning != "" || !strings.Contains(calls, "rmu --server=127.0.0.1:10085 -tag=xhttp u1") ||
		!strings.Contains(calls, "statsonline --server=127.0.0.1:10085 -email u1") || !strings.Contains(calls, "systemctl restart tigersb-xray.service") ||
		strings.Contains(calls, " adu ") {
		t.Fatalf("reissue (%s):\n%s", r.Warning, calls)
	}
	// sing-box rereads its config, mbox restarts: both end the old connections.
	if !strings.Contains(calls, "systemctl try-reload-or-restart tigersb-singbox.service") || !strings.Contains(calls, "systemctl try-restart tigersb-mieru.service") {
		t.Fatalf("reissue did not reach sing-box or mbox:\n%s", calls)
	}
	served("Мама with the old key", mama.UUID, false)
	served("Мама", mama2.UUID, true)
	served("Папа", papa.UUID, true)
	if code, _ := fetch(mama.Subs.HTTPS); code != http.StatusNotFound {
		t.Fatalf("old subscription address: %d", code)
	}
	if code, links := fetch(mama2.Subs.HTTPS); code != http.StatusOK || !strings.Contains(strings.Join(links, "\n"), mama2.UUID) {
		t.Fatalf("new subscription: %d %q", code, links)
	}

	// Папа is idle: deleting him needs no restart of Xray.
	f.sessions = nil
	n = len(f.calls)
	r = decodeUsers(t, do(s, http.MethodPost, "/"+secret+"/api/users/delete", `{"id":2}`, c).Body.Bytes())
	if calls := since(n); len(r.Users) != 1 || !strings.Contains(calls, "-tag=grpc u2") || strings.Contains(calls, "restart tigersb-xray") {
		t.Fatalf("delete:\n%s", calls)
	}
	served("Папа after delete", papa.UUID, false)
	served("Мама", mama2.UUID, true)
	if code, _ := fetch(papa.Subs.HTTPS); code != http.StatusNotFound {
		t.Fatalf("subscription of a deleted user: %d", code)
	}

	// A user added after them works everywhere and goes into the running Xray.
	// He does not get Папа's ID: the cores know users by it, and Xray could
	// still count Папа's traffic under it.
	n = len(f.calls)
	son := add("Сын")
	if son.ID != 3 {
		t.Fatalf("new user got ID %d, the deleted one's", son.ID)
	}
	if !strings.Contains(since(n), " adu ") || !strings.Contains(f.stdin[len(f.stdin)-1], son.UUID) {
		t.Fatalf("new user not added live:\n%s", since(n))
	}
	served("Сын", son.UUID, true)
	if code, links := fetch(son.Subs.HTTPS); code != http.StatusOK || len(links) != 6 {
		t.Fatalf("new user subscription: %d %q", code, links)
	}

	// Off and on again: the same key goes away and comes back.
	f.sessions = map[string]int{"u3": 1}
	do(s, http.MethodPost, "/"+secret+"/api/users/update", `{"id":3,"enabled":false}`, c)
	served("Сын turned off", son.UUID, false)
	f.sessions = nil
	do(s, http.MethodPost, "/"+secret+"/api/users/update", `{"id":3,"enabled":true}`, c)
	served("Сын turned on", son.UUID, true)
}
