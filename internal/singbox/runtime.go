package singbox

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Stilltiger45/TigerSB-panel/internal/core"
	"github.com/Stilltiger45/TigerSB-panel/internal/store"
)

// Image is the official sing-box image (GPL-3.0), unmodified, pinned by
// digest so every server runs the same build.
const Image = "ghcr.io/sagernet/sing-box:v1.14.4@sha256:0788ffbddab2c68a99a2dd4780bbc50902f159af6f589df3ee74a8389d278279"

// Container and Unit name the running sing-box.
const (
	Container = "tigersb-singbox"
	Unit      = "tigersb-singbox.service"
)

// Runtime is the sing-box container on this server.
type Runtime struct {
	Dir string // config directory with the certificate copies, /var/lib/tigersb/singbox
	// Certs is Caddy's storage, /var/lib/tigersb/caddy: the certificate of
	// the panel's domain is taken from there.
	Certs string
	Image string
	Exec  core.Exec
}

// DefaultRuntime is the layout the installer creates under dataDir. The image
// can be replaced with TIGERSB_SINGBOX_IMAGE, which tests use.
func DefaultRuntime(dataDir string) *Runtime {
	img := os.Getenv("TIGERSB_SINGBOX_IMAGE")
	if img == "" {
		img = Image
	}
	return &Runtime{
		Dir:   filepath.Join(dataDir, "singbox"),
		Certs: filepath.Join(dataDir, "caddy"),
		Image: img,
		Exec:  core.RealExec,
	}
}

// ConfigPath is the live sing-box config on the host.
func (r *Runtime) ConfigPath() string { return filepath.Join(r.Dir, "config.json") }

// writeFile writes data atomically unless the file already holds it, and
// reports whether it changed. sing-box runs as root in the container, so
// the files stay root's.
func (r *Runtime) writeFile(name string, data []byte) (bool, error) {
	path := filepath.Join(r.Dir, name)
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		return false, nil
	}
	if err := os.MkdirAll(r.Dir, 0o700); err != nil {
		return false, err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, path)
}

// HaveCert reports whether the certificate copies are in place.
func (r *Runtime) HaveCert() bool {
	for _, name := range []string{certFile, keyFile} {
		if _, err := os.Stat(filepath.Join(r.Dir, name)); err != nil {
			return false
		}
	}
	return true
}

// caddyCert finds the newest valid certificate Caddy keeps for domain, in
// certificates/<issuer>/<domain>/<domain>.crt, with its key next to it.
func (r *Runtime) caddyCert(domain string, now time.Time) (cert, key []byte, err error) {
	paths, _ := filepath.Glob(filepath.Join(r.Certs, "certificates", "*", domain, domain+".crt"))
	var best time.Time
	for _, p := range paths {
		c, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		block, _ := pem.Decode(c)
		if block == nil {
			continue
		}
		leaf, err := x509.ParseCertificate(block.Bytes)
		if err != nil || leaf.VerifyHostname(domain) != nil || now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) || !leaf.NotAfter.After(best) {
			continue
		}
		k, err := os.ReadFile(filepath.Join(filepath.Dir(p), domain+".key"))
		if err != nil {
			continue
		}
		cert, key, best = c, k, leaf.NotAfter
	}
	if cert == nil {
		return nil, nil, errors.New("Caddy ещё не получил сертификат домена")
	}
	return cert, key, nil
}

// CopyCert puts Caddy's current certificate for domain next to the config
// and reports whether it changed. Without a domain or a certificate the old
// copies are removed, so TUIC is never served under a stale name.
func (r *Runtime) CopyCert(domain string, now time.Time) (bool, error) {
	var cert, key []byte
	if domain != "" {
		cert, key, _ = r.caddyCert(domain, now)
	}
	if cert == nil {
		had := r.HaveCert()
		for _, name := range []string{certFile, keyFile} {
			os.Remove(filepath.Join(r.Dir, name))
		}
		return had, nil
	}
	changedKey, err := r.writeFile(keyFile, key)
	if err != nil {
		return false, err
	}
	changedCert, err := r.writeFile(certFile, cert)
	return changedKey || changedCert, err
}

func (r *Runtime) docker(ctx context.Context, args ...string) (string, error) {
	return core.Docker(ctx, r.Exec, nil, args...)
}

// Test asks sing-box in a throwaway container whether it accepts conf.
func (r *Runtime) Test(ctx context.Context, conf []byte) error {
	if _, err := r.writeFile("test.json", conf); err != nil {
		return err
	}
	defer os.Remove(filepath.Join(r.Dir, "test.json"))
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, err := r.docker(ctx, "run", "--rm", "--network", "none", "--log-driver", "none",
		"-v", r.Dir+":"+mountDir+":ro", r.Image, "check", "-c", mountDir+"/test.json")
	if errors.Is(err, core.ErrNoDocker) {
		return err
	}
	if err != nil {
		return fmt.Errorf("sing-box не принял настройки: %s", core.Tail(out, 6))
	}
	return nil
}

// RunArgs are the `docker run` arguments of the long-running container: the
// host network, so sing-box sees real client addresses, and nothing else.
// NET_BIND_SERVICE lets TUIC take UDP 443.
func (r *Runtime) RunArgs() []string {
	return []string{"run", "--rm", "--name", Container,
		"--network", "host", "--log-driver", "none",
		"--read-only", "--cap-drop", "ALL", "--cap-add", "NET_BIND_SERVICE", "--security-opt", "no-new-privileges",
		"-v", r.Dir + ":" + mountDir + ":ro",
		r.Image, "run", "-c", mountDir + "/config.json"}
}

// Remove deletes a container left by a crash, so the name is free.
func (r *Runtime) Remove(ctx context.Context) {
	r.docker(ctx, "rm", "-f", Container)
}

// Signal asks the running sing-box to reread its config and certificate.
// Open TUIC connections drop; clients notice by the idle timeout.
func (r *Runtime) Signal(ctx context.Context) error {
	out, err := r.docker(ctx, "kill", "-s", "HUP", Container)
	if err != nil && !errors.Is(err, core.ErrNoDocker) {
		return fmt.Errorf("sing-box не перечитал настройки: %s", core.Tail(out, 2))
	}
	return err
}

// Restart restarts the container; on start it rebuilds the config and the
// certificate copy from the database and Caddy's storage.
func (r *Runtime) Restart(ctx context.Context) error {
	out, err := r.Exec(ctx, nil, "systemctl", "restart", Unit)
	if err != nil {
		return fmt.Errorf("sing-box не перезапустился: %v %s. Подробности: journalctl -u %s", err, out, Unit)
	}
	return nil
}

// Reload makes a running sing-box reread its files; a stopped one reads
// them when it starts.
func (r *Runtime) Reload(ctx context.Context) error {
	out, err := r.Exec(ctx, nil, "systemctl", "try-reload-or-restart", Unit)
	if err != nil && (strings.Contains(out, "not found") || strings.Contains(out, "not loaded")) {
		// The installer has not put the unit in place yet.
		return nil
	}
	if err != nil {
		return fmt.Errorf("sing-box не перечитал настройки: %v %s. Подробности: journalctl -u %s", err, out, Unit)
	}
	return nil
}

// Build reads the settings and users and renders the config.
func Build(ctx context.Context, st *store.Store, domain string, haveCert bool) (Settings, []byte, error) {
	s, err := Load(ctx, st)
	if err != nil {
		return s, nil, err
	}
	users, err := st.CoreUsers(ctx)
	if err != nil {
		return s, nil, err
	}
	conf, err := Render(s, users, domain, haveCert)
	return s, conf, err
}

// Sync copies the certificate and writes the config built from the store.
// With reload a running sing-box rereads them, but only when something
// changed, so the monitor timer can call it every few minutes.
func (r *Runtime) Sync(ctx context.Context, st *store.Store, domain string, reload bool) error {
	changedCert, err := r.CopyCert(domain, time.Now())
	if err != nil {
		return err
	}
	_, conf, err := Build(ctx, st, domain, r.HaveCert())
	if err != nil {
		return err
	}
	changedConf, err := r.writeFile("config.json", conf)
	if err != nil {
		return err
	}
	if reload && (changedCert || changedConf) {
		return r.Reload(ctx)
	}
	return nil
}
