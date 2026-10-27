package update

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stilltiger45/TigerSB-panel/internal/store"
)

type fixture struct {
	l   Layout
	st  *store.Store
	sys *fakeSystemd
}

// fakeSystemd answers systemctl and records what was asked.
type fakeSystemd struct {
	calls   []string
	version string
	caddy   string
}

func (f *fakeSystemd) run(name string, args ...string) (string, error) {
	f.calls = append(f.calls, strings.Join(append([]string{filepath.Base(name)}, args...), " "))
	if strings.HasSuffix(name, "tigersb") && len(args) == 1 && args[0] == "version" {
		return f.version, nil
	}
	if name == "systemctl" && args[0] == "is-active" {
		if args[1] == "tigersb-caddy.service" {
			return f.caddy, nil
		}
		return "active", nil
	}
	return "", nil
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	l := Layout{
		Config: filepath.Join(root, "etc/tigersb/config.json"),
		DB:     filepath.Join(root, "var/lib/tigersb/panel.db"),
		Bin:    filepath.Join(root, "usr/local/bin/tigersb"),
		Units:  filepath.Join(root, "etc/systemd/system"),
	}
	write(t, l.Config, "config v1")
	write(t, l.Bin, "binary v1")
	write(t, filepath.Join(l.Units, "tigersb.service"), "unit v1")
	write(t, filepath.Join(l.Units, "other.service"), "not ours")
	write(t, filepath.Join(l.www(), "index.html"), "page v1")
	write(t, filepath.Join(l.caddy(), "certificates/a/cert.pem"), "cert v1")
	st, err := store.Open(l.DB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.SetSettings(context.Background(), map[string]string{"k": "v1"}); err != nil {
		t.Fatal(err)
	}
	return &fixture{l: l, st: st, sys: &fakeSystemd{caddy: "active"}}
}

func TestRestorePointRoundTrip(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	p, err := CreatePoint(ctx, f.l, f.st, "main-1111111", "вручную", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if p.Size == 0 || !ValidID(p.ID) {
		t.Fatalf("point: %+v", p)
	}

	// Everything changes after the point.
	write(t, f.l.Config, "config v2")
	write(t, f.l.Bin, "binary v2")
	write(t, filepath.Join(f.l.Units, "tigersb-monitor.timer"), "new timer")
	write(t, filepath.Join(f.l.www(), "extra.html"), "new")
	f.st.SetSettings(ctx, map[string]string{"k": "v2"})
	f.st.Close()

	got, err := RestorePoint(f.l, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "main-1111111" || got.Reason != "вручную" {
		t.Fatalf("manifest: %+v", got)
	}
	for path, want := range map[string]string{
		f.l.Config: "config v1", f.l.Bin: "binary v1",
		filepath.Join(f.l.www(), "index.html"):                "page v1",
		filepath.Join(f.l.caddy(), "certificates/a/cert.pem"): "cert v1",
		filepath.Join(f.l.Units, "tigersb.service"):           "unit v1",
		filepath.Join(f.l.Units, "other.service"):             "not ours",
	} {
		if s := read(t, path); s != want {
			t.Errorf("%s: %q, want %q", path, s, want)
		}
	}
	for _, gone := range []string{filepath.Join(f.l.Units, "tigersb-monitor.timer"), filepath.Join(f.l.www(), "extra.html")} {
		if fileExists(gone) {
			t.Errorf("%s survived the restore", gone)
		}
	}
	st, err := store.Open(f.l.DB)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if vals, _ := st.Settings(ctx); vals["k"] != "v1" {
		t.Fatalf("database not restored: %v", vals)
	}
}

func TestPointsArePrunedAndListed(t *testing.T) {
	f := newFixture(t)
	t0 := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	for i := 0; i < KeepPoints+2; i++ {
		if _, err := CreatePoint(context.Background(), f.l, f.st, "v", "r", t0.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	// Two in the same second get distinct names.
	if p, err := CreatePoint(context.Background(), f.l, f.st, "v", "r", t0.Add(time.Duration(KeepPoints+1)*time.Minute)); err != nil || !strings.HasSuffix(p.ID, "-1") {
		t.Fatalf("same second: %+v %v", p, err)
	}
	points, err := ListPoints(f.l)
	if err != nil || len(points) != KeepPoints {
		t.Fatalf("points: %d %v", len(points), err)
	}
	if points[0].ID < points[1].ID {
		t.Fatalf("not newest first: %s, %s", points[0].ID, points[1].ID)
	}
}

func TestRestoreRejectsForeignPaths(t *testing.T) {
	f := newFixture(t)
	os.MkdirAll(f.l.points(), 0o700)
	id := "20261026-120000"
	out, _ := os.Create(f.l.archive(id))
	gz := gzip.NewWriter(out)
	tw := tar.NewWriter(gz)
	add := func(name, body string) {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg})
		io.WriteString(tw, body)
	}
	add(manifestName, `{"version":"x"}`)
	add(tarName(f.l.DB), "db")
	add("etc/cron.d/evil", "* * * * * root sh")
	tw.Close()
	gz.Close()
	out.Close()
	if _, err := RestorePoint(f.l, id); err == nil || !strings.Contains(err.Error(), "лишний путь") {
		t.Fatalf("want rejection, got %v", err)
	}
	if read(t, f.l.Config) != "config v1" {
		t.Fatal("a rejected point must not touch anything")
	}
	if _, err := RestorePoint(f.l, "../../etc"); err == nil {
		t.Fatal("path in id accepted")
	}
}

func newUpdater(f *fixture) *Updater {
	return &Updater{Layout: f.l, Store: f.st, Version: "main-1111111", Run: f.sys.run, Wait: time.Millisecond,
		PanelUp: func(context.Context) error { return nil }}
}

const newSHA = "abcdef0123456789abcdef0123456789abcdef01"

func TestUpdateSucceeds(t *testing.T) {
	f := newFixture(t)
	u := newUpdater(f)
	f.sys.version = "main-abcdef0"
	u.Install = func(_ context.Context, ref string, log io.Writer) error {
		io.WriteString(log, "building "+ref+"\n")
		write(t, f.l.Bin, "binary v2")
		return nil
	}
	log, _ := u.OpenLog()
	defer log.Close()
	if err := u.RunNow(context.Background(), ActionUpdate, newSHA, log); err != nil {
		t.Fatal(err)
	}
	s := u.Status()
	if s.Phase != PhaseDone || s.To != "main-abcdef0" || s.Point == "" || !strings.Contains(s.Message, "main-1111111 → main-abcdef0") {
		t.Fatalf("status: %+v", s)
	}
	if read(t, f.l.Bin) != "binary v2" {
		t.Fatal("update was rolled back")
	}
	if !strings.Contains(u.LogTail(20), "building "+newSHA) {
		t.Fatalf("log: %s", u.LogTail(20))
	}
}

func TestFailedUpdateRollsBack(t *testing.T) {
	f := newFixture(t)
	u := newUpdater(f)
	u.Install = func(_ context.Context, _ string, log io.Writer) error {
		io.WriteString(log, "curl: (6) Could not resolve host: github.com\n")
		write(t, f.l.Bin, "half-written binary")
		return errors.New("exit status 1")
	}
	log, _ := u.OpenLog()
	defer log.Close()
	err := u.RunNow(context.Background(), ActionUpdate, newSHA, log)
	if err == nil {
		t.Fatal("want failure")
	}
	if read(t, f.l.Bin) != "binary v1" {
		t.Fatal("binary not restored")
	}
	s := u.Status()
	if s.Phase != PhaseFailed || !strings.Contains(s.Message, "GitHub недоступен") || !strings.Contains(s.Message, "возвращена к версии main-1111111") {
		t.Fatalf("status: %+v", s)
	}
	joined := strings.Join(f.sys.calls, "\n")
	for _, want := range []string{"systemctl stop tigersb.service", "systemctl daemon-reload", "systemctl restart tigersb-caddy.service", "systemctl start tigersb.service"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no %q in:\n%s", want, joined)
		}
	}
}

func TestQueue(t *testing.T) {
	f := newFixture(t)
	u := newUpdater(f)
	if _, err := u.Queue(ActionUpdate, "main"); err == nil {
		t.Fatal("non-sha target accepted")
	}
	if _, err := u.Queue(ActionRollback, "20990101-000000"); err == nil {
		t.Fatal("missing point accepted")
	}
	s, err := u.Queue(ActionUpdate, newSHA)
	if err != nil || s.Phase != PhaseQueued {
		t.Fatalf("queue: %+v %v", s, err)
	}
	if !strings.Contains(strings.Join(f.sys.calls, "\n"), "systemctl start --no-block tigersb-update.service") {
		t.Fatalf("unit not started: %v", f.sys.calls)
	}
	if _, err := u.Queue(ActionUpdate, newSHA); err == nil {
		t.Fatal("second job queued while the first waits")
	}

	// A queued job whose unit never ran shows up as interrupted.
	u.Now = func() time.Time { return time.Now().Add(10 * time.Minute) }
	f.sys.caddy = "inactive"
	u.Run = func(name string, args ...string) (string, error) { return "inactive", nil }
	if s := u.Status(); s.Phase != PhaseInterrupted {
		t.Fatalf("stale job: %+v", s)
	}
	if err := u.RunQueued(context.Background(), io.Discard); err == nil {
		t.Fatal("stale job ran")
	}
}

func TestCheckGitHub(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/"+Repo+"/commits/main":
			io.WriteString(w, `{"sha":"`+newSHA+`","commit":{"message":"Merge pull request #7 from x/y\n\nВкладка Контроль","committer":{"date":"2026-10-26T12:00:00Z"}}}`)
		case strings.HasPrefix(r.URL.Path, "/repos/"+Repo+"/compare/1111111..."):
			io.WriteString(w, `{"commits":[{"parents":[{}],"commit":{"message":"Fix A"}},{"parents":[{},{}],"commit":{"message":"Merge pull request #7 from x/y\n\nВкладка Контроль"}},{"parents":[{}],"commit":{"message":"Inside PR 8"}},{"parents":[{},{}],"commit":{"message":"Merge pull request #8 from x/z\n\nОткат"}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	old := APIBase
	APIBase = srv.URL
	defer func() { APIBase = old }()

	now := time.Now()
	c := CheckGitHub(context.Background(), srv.Client(), "main-1111111", now)
	if !c.Available || c.Latest != "main-abcdef0" || c.Ref != newSHA || len(c.Changes) != 2 || c.Changes[0] != "Откат (#8)" || c.Changes[1] != "Вкладка Контроль (#7)" {
		t.Fatalf("check: %+v", c)
	}
	if c := CheckGitHub(context.Background(), srv.Client(), "main-abcdef0", now); c.Available || c.Error != "" {
		t.Fatalf("up to date: %+v", c)
	}
	if c := CheckGitHub(context.Background(), srv.Client(), "main", now); !c.Available || len(c.Changes) != 1 {
		t.Fatalf("unknown build: %+v", c)
	}
	APIBase = "http://127.0.0.1:1"
	if c := CheckGitHub(context.Background(), srv.Client(), "main", now); c.Error == "" || c.Available {
		t.Fatalf("offline: %+v", c)
	}
}

// A rollback to a version without the edge router stops it, so the older
// Caddy can take port 443 back; going forward restarts edge and Xray.
func TestRollbackAcrossEdge(t *testing.T) {
	f := newFixture(t)
	u := newUpdater(f)
	ctx := context.Background()
	old, err := CreatePoint(ctx, f.l, f.st, "main-1111111", "до edge", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(f.l.Units, EdgeUnit), "edge")
	write(t, filepath.Join(f.l.Units, XrayUnit), "xray")
	write(t, filepath.Join(f.l.Units, SingboxUnit), "singbox")
	newer, err := CreatePoint(ctx, f.l, f.st, "main-2222222", "с edge", time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}

	if err := u.restore(ctx, old.ID, false, io.Discard); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(f.sys.calls, "\n")
	stop := strings.Index(joined, "systemctl stop "+EdgeUnit)
	caddy := strings.Index(joined, "systemctl restart tigersb-caddy.service")
	if stop < 0 || caddy < 0 || stop > caddy || !strings.Contains(joined, "systemctl stop "+XrayUnit) || !strings.Contains(joined, "systemctl stop "+SingboxUnit) {
		t.Fatalf("edge must stop before Caddy restarts:\n%s", joined)
	}
	if strings.Contains(joined, "restart "+EdgeUnit) {
		t.Fatalf("edge restarted after rollback to a version without it:\n%s", joined)
	}

	f.sys.calls = nil
	if err := u.restore(ctx, newer.ID, false, io.Discard); err != nil {
		t.Fatal(err)
	}
	joined = strings.Join(f.sys.calls, "\n")
	for _, want := range []string{"systemctl enable " + EdgeUnit, "systemctl restart " + EdgeUnit, "systemctl restart " + XrayUnit, "systemctl restart " + SingboxUnit} {
		if !strings.Contains(joined, want) {
			t.Errorf("no %q in:\n%s", want, joined)
		}
	}
}
