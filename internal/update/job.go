package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/Stilltiger45/TigerSB-panel/internal/service"
	"github.com/Stilltiger45/TigerSB-panel/internal/store"
)

// Repo is the GitHub repository updates come from.
const Repo = "Stilltiger45/TigerSB-panel"

// Unit runs queued updates and rollbacks outside the panel's cgroup.
const Unit = "tigersb-update.service"

// Units of the edge router on 443, the subscriptions and the VPN containers,
// restarted after a rollback.
const (
	EdgeUnit    = "tigersb-edge.service"
	SubUnit     = "tigersb-sub.service"
	XrayUnit    = "tigersb-xray.service"
	SingboxUnit = "tigersb-singbox.service"
)

// Phases of a job.
const (
	PhaseQueued      = "queued"
	PhaseRunning     = "running"
	PhaseDone        = "done"
	PhaseFailed      = "failed"
	PhaseInterrupted = "interrupted"
)

// Actions a job can take.
const (
	ActionUpdate   = "update"
	ActionRollback = "rollback"
)

// queueTTL is how long a queued job stays valid for the unit to pick it up.
const queueTTL = 5 * time.Minute

// Status is the state of the last update or rollback. It lives in a file, not
// in the database, because a rollback replaces the database.
type Status struct {
	Phase    string `json:"phase"`
	Action   string `json:"action,omitempty"`
	Target   string `json:"target,omitempty"`
	From     string `json:"from,omitempty"`
	To       string `json:"to,omitempty"`
	Point    string `json:"point,omitempty"`
	Message  string `json:"message,omitempty"`
	Queued   int64  `json:"queued,omitempty"`
	Started  int64  `json:"started,omitempty"`
	Finished int64  `json:"finished,omitempty"`
}

// Busy reports a job that is queued or running.
func (s Status) Busy() bool { return s.Phase == PhaseQueued || s.Phase == PhaseRunning }

// Updater runs jobs. Fields other than Layout and Store have working defaults.
type Updater struct {
	Layout  Layout
	Store   *store.Store
	Version string
	Run     service.Runner
	Now     func() time.Time
	// Install runs the installer in update mode for the commit ref.
	Install func(ctx context.Context, ref string, log io.Writer) error
	// PanelUp reports whether the panel answers; used after a restart.
	PanelUp func(ctx context.Context) error
	// Wait is how long the panel may take to come up after an update.
	Wait time.Duration
}

func (u *Updater) now() time.Time {
	if u.Now != nil {
		return u.Now()
	}
	return time.Now()
}

func (u *Updater) run(name string, args ...string) (string, error) {
	if u.Run != nil {
		return u.Run(name, args...)
	}
	return service.ExecRunner(name, args...)
}

func (u *Updater) statusPath() string { return filepath.Join(u.Layout.jobDir(), "status.json") }

// LogPath is the log of the last job.
func (u *Updater) LogPath() string { return filepath.Join(u.Layout.jobDir(), "update.log") }

// Status returns the last job's state. A job whose unit died is reported as
// interrupted rather than running forever.
func (u *Updater) Status() Status {
	var s Status
	b, err := os.ReadFile(u.statusPath())
	if err == nil {
		json.Unmarshal(b, &s)
	}
	if s.Busy() && u.now().Unix()-max(s.Queued, s.Started) > 120 {
		if state := service.UnitState(u.run, Unit); state != "active" && state != "activating" {
			s.Phase = PhaseInterrupted
			s.Message = "Задача прервалась, не закончив работу. Посмотрите журнал ниже или на сервере: journalctl -u " + Unit
		}
	}
	return s
}

func (u *Updater) save(s Status) error {
	if err := os.MkdirAll(u.Layout.jobDir(), 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(s)
	tmp := u.statusPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, u.statusPath())
}

// lock serialises jobs between the panel, the unit and the command line.
func (u *Updater) lock(wait bool) (func(), error) {
	if err := os.MkdirAll(u.Layout.jobDir(), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(u.Layout.jobDir(), "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	how := syscall.LOCK_EX
	if !wait {
		how |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		f.Close()
		return nil, errors.New("другое обновление или откат уже выполняется")
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

var shaRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Queue records a job and starts the unit that runs it.
func (u *Updater) Queue(action, target string) (Status, error) {
	switch action {
	case ActionUpdate:
		if !shaRe.MatchString(target) {
			return Status{}, errors.New("сначала проверьте обновления")
		}
	case ActionRollback:
		if !ValidID(target) || !fileExists(u.Layout.archive(target)) {
			return Status{}, errors.New("такой точки восстановления нет")
		}
	default:
		return Status{}, errors.New("неизвестное действие")
	}
	unlock, err := u.lock(false)
	if err != nil {
		return Status{}, err
	}
	defer unlock()
	if u.Status().Busy() {
		return Status{}, errors.New("обновление или откат уже выполняется")
	}
	s := Status{Phase: PhaseQueued, Action: action, Target: target, From: u.Version, Queued: u.now().Unix(), Message: "Задача поставлена в очередь."}
	if err := u.save(s); err != nil {
		return Status{}, err
	}
	if out, err := u.run("systemctl", "start", "--no-block", Unit); err != nil {
		s.Phase, s.Message = PhaseFailed, "Не удалось запустить службу обновления: "+out
		u.save(s)
		return s, errors.New(s.Message)
	}
	return s, nil
}

// RunQueued runs the job the panel queued. The unit calls it.
func (u *Updater) RunQueued(ctx context.Context, log io.Writer) error {
	unlock, err := u.lock(true)
	if err != nil {
		return err
	}
	defer unlock()
	s := u.Status()
	if s.Phase != PhaseQueued || u.now().Unix()-s.Queued > int64(queueTTL/time.Second) {
		return errors.New("нет задачи в очереди")
	}
	return u.do(ctx, s, log)
}

// RunNow runs a job right away, from the command line.
func (u *Updater) RunNow(ctx context.Context, action, target string, log io.Writer) error {
	unlock, err := u.lock(false)
	if err != nil {
		return err
	}
	defer unlock()
	return u.do(ctx, Status{Action: action, Target: target, From: u.Version}, log)
}

func (u *Updater) do(ctx context.Context, s Status, log io.Writer) error {
	s.Phase, s.Started, s.Finished = PhaseRunning, u.now().Unix(), 0
	var err error
	switch s.Action {
	case ActionUpdate:
		err = u.update(ctx, &s, log)
	case ActionRollback:
		err = u.rollback(ctx, &s, log)
	default:
		err = errors.New("неизвестное действие")
	}
	s.Finished = u.now().Unix()
	if err != nil {
		s.Phase, s.Message = PhaseFailed, err.Error()
	} else {
		s.Phase = PhaseDone
	}
	fmt.Fprintln(log, s.Message)
	if serr := u.save(s); err == nil {
		err = serr
	}
	return err
}

func (u *Updater) step(s *Status, log io.Writer, msg string) {
	s.Message = msg
	fmt.Fprintf(log, "== %s\n", msg)
	u.save(*s)
}

func (u *Updater) update(ctx context.Context, s *Status, log io.Writer) error {
	if !shaRe.MatchString(s.Target) {
		return errors.New("неверная версия для обновления")
	}
	wasUp := service.State(u.run) == "active"
	u.step(s, log, "Создаю точку восстановления")
	p, err := CreatePoint(ctx, u.Layout, u.Store, u.Version, "перед обновлением", u.now())
	if err != nil {
		return fmt.Errorf("не удалось создать точку восстановления, обновление не начато: %w", err)
	}
	s.Point = p.ID
	u.step(s, log, "Скачиваю и собираю новую версию, это займёт пару минут")
	install := u.Install
	if install == nil {
		install = RunInstaller
	}
	err = install(ctx, s.Target, log)
	if err == nil {
		u.step(s, log, "Проверяю, что всё запустилось")
		err = u.verify(ctx, s, wasUp)
	}
	if err == nil {
		s.Message = fmt.Sprintf("Панель обновлена: %s → %s.", s.From, s.To)
		return nil
	}
	fmt.Fprintf(log, "Ошибка: %v\n", err)
	reason := explain(u.LogPath(), err)
	u.step(s, log, "Обновление не удалось, возвращаю точку восстановления "+p.ID)
	if rerr := u.restore(ctx, p.ID, wasUp, log); rerr != nil {
		s.Message = fmt.Sprintf("Обновление не удалось (%s), и откат тоже: %v. На сервере выполните tigersb restore apply %s.", reason, rerr, p.ID)
		return errors.New(s.Message)
	}
	s.Message = fmt.Sprintf("Обновление не удалось: %s Панель возвращена к версии %s.", reason, u.Version)
	return errors.New(s.Message)
}

func (u *Updater) verify(ctx context.Context, s *Status, wasUp bool) error {
	out, err := u.run(u.Layout.Bin, "version")
	if err != nil {
		return fmt.Errorf("новая программа панели не запускается: %v", err)
	}
	s.To = strings.TrimSpace(out)
	if !strings.Contains(s.To, s.Target[:7]) {
		return fmt.Errorf("установилась версия %s вместо %s", s.To, s.Target[:7])
	}
	if state := service.UnitState(u.run, service.CaddyUnit); state != "active" {
		return fmt.Errorf("Caddy после обновления в состоянии %s", state)
	}
	if !wasUp || u.PanelUp == nil {
		return nil
	}
	wait := u.Wait
	if wait == 0 {
		wait = time.Minute
	}
	deadline := time.Now().Add(wait)
	for {
		err := u.PanelUp(ctx)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("панель не отвечает после обновления: %v", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (u *Updater) rollback(ctx context.Context, s *Status, log io.Writer) error {
	wasUp := service.State(u.run) == "active"
	u.step(s, log, "Возвращаю точку восстановления "+s.Target)
	p, err := readManifest(u.Layout.archive(s.Target))
	if err != nil || !ValidID(s.Target) {
		return errors.New("такой точки восстановления нет")
	}
	if err := u.restore(ctx, s.Target, wasUp, log); err != nil {
		return err
	}
	s.To = p.Version
	s.Message = fmt.Sprintf("Панель возвращена к точке %s (версия %s).", s.Target, p.Version)
	return nil
}

// restore stops the panel, puts the point back and starts the services again.
func (u *Updater) restore(ctx context.Context, id string, start bool, log io.Writer) error {
	if out, err := u.run("systemctl", "stop", service.Unit); err != nil {
		fmt.Fprintf(log, "systemctl stop: %v %s\n", err, out)
	}
	before := map[string]bool{}
	for _, f := range u.Layout.unitFiles() {
		before[filepath.Base(f)] = true
	}
	if _, err := RestorePoint(u.Layout, id); err != nil {
		return err
	}
	if out, err := u.run("systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %v %s", err, out)
	}
	after := map[string]bool{}
	for _, f := range u.Layout.unitFiles() {
		after[filepath.Base(f)] = true
	}
	// Units the point does not have must stop: an older version may want
	// port 443 for Caddy back from the edge router.
	for name := range before {
		if !after[name] && name != Unit {
			u.run("systemctl", "stop", name)
		}
	}
	if out, err := u.run("systemctl", "restart", service.CaddyUnit); err != nil {
		return fmt.Errorf("Caddy не перезапустился: %v %s", err, out)
	}
	// The edge router, the subscriptions and the VPN cores rebuild their
	// state from the restored database.
	for _, name := range []string{EdgeUnit, SubUnit, XrayUnit, SingboxUnit} {
		if after[name] {
			if out, err := u.run("systemctl", "enable", name); err != nil {
				fmt.Fprintf(log, "systemctl enable %s: %v %s\n", name, err, out)
			}
			if out, err := u.run("systemctl", "restart", name); err != nil {
				fmt.Fprintf(log, "%s не перезапустился: %v %s\n", name, err, out)
			}
		}
	}
	for _, timer := range u.Layout.unitFiles() {
		if strings.HasSuffix(timer, ".timer") {
			u.run("systemctl", "enable", "--now", filepath.Base(timer))
		}
	}
	if start {
		if out, err := u.run("systemctl", "start", service.Unit); err != nil {
			return fmt.Errorf("панель не запустилась: %v %s", err, out)
		}
	}
	return nil
}

// RunInstaller downloads the installer of commit ref and runs it in update mode.
func RunInstaller(ctx context.Context, ref string, log io.Writer) error {
	url := "https://raw.githubusercontent.com/" + Repo + "/" + ref + "/scripts/install.sh"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	res, err := (&http.Client{Timeout: time.Minute}).Do(req)
	if err != nil {
		return fmt.Errorf("не удалось скачать установщик: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("не удалось скачать установщик: GitHub ответил %s", res.Status)
	}
	f, err := os.CreateTemp("", "tigersb-install-*.sh")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = io.Copy(f, io.LimitReader(res.Body, 1<<20))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "bash", f.Name(), "--update", "--ref", ref)
	cmd.Stdout, cmd.Stderr = log, log
	cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("установщик завершился с ошибкой: %w", err)
	}
	return nil
}

// explain turns the end of the log into a short reason a person can act on.
func explain(logPath string, err error) string {
	b, _ := os.ReadFile(logPath)
	tail := strings.ToLower(string(b[max(0, len(b)-16000):]))
	cases := []struct {
		needles []string
		msg     string
	}{
		{[]string{"could not resolve host", "connection timed out", "failed to connect", "не удалось скачать"}, "GitHub недоступен с сервера или соединение прервалось."},
		{[]string{"no space left on device"}, "на диске закончилось место."},
		{[]string{"go: ", "build failed", "cannot find package"}, "новая версия не собралась."},
		{[]string{"caddy не принял", "caddy не перезапустился"}, "Caddy не принял новые настройки."},
		{[]string{"панель не отвечает"}, "новая версия панели не запустилась."},
	}
	for _, c := range cases {
		for _, n := range c.needles {
			if strings.Contains(tail, n) {
				return c.msg
			}
		}
	}
	return err.Error() + "."
}

// OpenLog starts a fresh log for a job.
func (u *Updater) OpenLog() (*os.File, error) {
	if err := os.MkdirAll(u.Layout.jobDir(), 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(u.LogPath(), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
}

// LogTail returns the last lines of the job log.
func (u *Updater) LogTail(lines int) string {
	b, err := os.ReadFile(u.LogPath())
	if err != nil {
		return ""
	}
	all := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	return strings.Join(all[max(0, len(all)-lines):], "\n")
}

// LoopbackCheck returns a PanelUp function that asks the panel itself.
func LoopbackCheck(listen, secret string) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+listen+"/"+secret+"/", nil)
		if err != nil {
			return err
		}
		res, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			return err
		}
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return fmt.Errorf("панель ответила %s", res.Status)
		}
		return nil
	}
}
