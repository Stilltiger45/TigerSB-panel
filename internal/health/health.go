// Package health checks that every part of the server works: services, the
// maintenance page, the certificate, the panel route, disk, memory, clock,
// internet access, the database, monitoring, updates and restore points.
//
// Each later stage (Xray, sing-box, routing points) adds its own checks here;
// until then they are listed as coming.
package health

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/Stilltiger45/TigerSB-panel/internal/config"
	"github.com/Stilltiger45/TigerSB-panel/internal/metrics"
	"github.com/Stilltiger45/TigerSB-panel/internal/service"
	"github.com/Stilltiger45/TigerSB-panel/internal/site"
	"github.com/Stilltiger45/TigerSB-panel/internal/store"
	"github.com/Stilltiger45/TigerSB-panel/internal/update"
)

// States of a check, from best to worst; "soon" is a part not installed yet.
const (
	OK   = "ok"
	Off  = "off"
	Soon = "soon"
	Warn = "warn"
	Fail = "fail"
)

// Result is one check.
type Result struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	State  string `json:"state"`
	Detail string `json:"detail"`
}

// Report is a full run.
type Report struct {
	Checked time.Time `json:"checked"`
	State   string    `json:"state"` // the worst result
	Results []Result  `json:"results"`
}

// Key is where the last report is stored.
const Key = "health.last"

// Env is what the checks look at. Zero fields fall back to the real server.
type Env struct {
	Cfg       *config.Config
	Store     *store.Store
	Updater   *update.Updater
	Run       service.Runner
	HTTPSAddr string // where Caddy serves TLS, 127.0.0.1:443
	HTTPAddr  string // plain HTTP without a domain, 127.0.0.1:80
	Roots     *x509.CertPool
	Lookup    func(ctx context.Context, host string) ([]net.IP, error)
	LocalIPs  func() ([]net.IP, error)
	Internet  string // host:port that must be reachable, github.com:443
	Now       time.Time
}

func (e *Env) defaults() {
	if e.Run == nil {
		e.Run = service.ExecRunner
	}
	if e.HTTPSAddr == "" {
		e.HTTPSAddr = "127.0.0.1:443"
	}
	if e.HTTPAddr == "" {
		e.HTTPAddr = "127.0.0.1:80"
	}
	if e.Lookup == nil {
		e.Lookup = func(ctx context.Context, host string) ([]net.IP, error) {
			return net.DefaultResolver.LookupIP(ctx, "ip", host)
		}
	}
	if e.LocalIPs == nil {
		e.LocalIPs = localIPs
	}
	if e.Internet == "" {
		e.Internet = "github.com:443"
	}
	if e.Now.IsZero() {
		e.Now = time.Now()
	}
}

// Run performs every check and stores the report.
func Run(ctx context.Context, e Env) (*Report, error) {
	e.defaults()
	set, err := site.Load(ctx, e.Store, e.Cfg)
	if err != nil {
		return nil, err
	}
	open, err := site.PanelOpen(ctx, e.Store)
	if err != nil {
		return nil, err
	}
	panel := service.State(e.Run)
	r := &Report{Checked: e.Now.UTC()}
	add := func(res Result) { r.Results = append(r.Results, res) }

	add(checkPanel(e, panel))
	caddy := service.UnitState(e.Run, service.CaddyUnit)
	add(checkCaddy(caddy))
	add(checkStub(ctx, e, set, caddy))
	add(checkCert(ctx, e, set))
	add(checkDNS(ctx, e, set))
	add(checkRoute(ctx, e, set, open, panel))
	sample, _ := metrics.Last(ctx, e.Store)
	add(checkDisk(sample))
	add(checkMemory(sample))
	add(checkMonitor(e, sample))
	add(checkClock(e))
	add(checkInternet(ctx, e))
	add(checkDB(ctx, e))
	add(checkUpdate(ctx, e))
	add(checkPoints(e))
	add(Result{ID: "xray", Title: "Xray (gRPC, XHTTP, каскад)", State: Soon, Detail: "Появится вместе с протоколами."})
	add(Result{ID: "singbox", Title: "sing-box (TUIC)", State: Soon, Detail: "Появится вместе с TUIC."})
	add(Result{ID: "routers", Title: "Точки маршрутизации", State: Soon, Detail: "Появятся вместе с каскадом: панель будет проверять каждую по SSH."})

	r.State = OK
	for _, res := range r.Results {
		if rank(res.State) > rank(r.State) {
			r.State = res.State
		}
	}
	b, _ := json.Marshal(r)
	return r, e.Store.SetSettings(ctx, map[string]string{Key: string(b)})
}

func rank(state string) int {
	switch state {
	case Fail:
		return 2
	case Warn:
		return 1
	}
	return 0
}

// Last returns the stored report, or nil.
func Last(ctx context.Context, st *store.Store) (*Report, error) {
	vals, err := st.Settings(ctx)
	if err != nil || vals[Key] == "" {
		return nil, err
	}
	var r Report
	if err := json.Unmarshal([]byte(vals[Key]), &r); err != nil {
		return nil, nil
	}
	return &r, nil
}

func checkPanel(e Env, state string) Result {
	r := Result{ID: "panel", Title: "Панель (tigersb)"}
	switch {
	case state == "active":
		r.State, r.Detail = OK, "Работает."
	case state == "inactive" && unitDisabled(e, service.Unit):
		r.State, r.Detail = Off, "Спит. Разбудить: tigersb wake."
	default:
		r.State, r.Detail = Fail, fmt.Sprintf("Служба в состоянии %s. Подробности: journalctl -u %s", state, service.Unit)
	}
	return r
}

func unitDisabled(e Env, unit string) bool {
	out, _ := e.Run("systemctl", "is-enabled", unit)
	return strings.TrimSpace(out) == "disabled"
}

func checkCaddy(state string) Result {
	r := Result{ID: "caddy", Title: "Caddy (сайт и сертификат)"}
	if state == "active" {
		r.State, r.Detail = OK, "Работает."
	} else {
		r.State, r.Detail = Fail, fmt.Sprintf("Служба в состоянии %s: заглушка, сертификат и вход по домену не работают. Подробности: journalctl -u %s", state, service.CaddyUnit)
	}
	return r
}

// probe makes one request to Caddy on loopback, as if it came for host.
func probe(ctx context.Context, e Env, host, path string) (*http.Response, error) {
	addr, scheme := e.HTTPSAddr, "https"
	if host == "" {
		addr, scheme, host = e.HTTPAddr, "http", "localhost"
	}
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
			// The certificate has its own check; here only the answer matters.
			TLSClientConfig: &tls.Config{ServerName: host, InsecureSkipVerify: true},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, scheme+"://"+host+path, nil)
	if err != nil {
		return nil, err
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	res.Body.Close()
	return res, nil
}

func checkStub(ctx context.Context, e Env, set site.Settings, caddy string) Result {
	r := Result{ID: "stub", Title: "Заглушка «Технические работы»"}
	if caddy != "active" {
		r.State, r.Detail = Off, "Не проверялась: Caddy не работает."
		return r
	}
	res, err := probe(ctx, e, set.Domain, "/")
	switch {
	case err != nil:
		r.State, r.Detail = Fail, "Не отвечает: "+err.Error()
	case res.StatusCode != http.StatusOK:
		r.State, r.Detail = Fail, "Отвечает "+res.Status+" вместо страницы."
	case set.Domain == "":
		r.State, r.Detail = OK, "Открывается по http://IP/."
	default:
		r.State, r.Detail = OK, "Открывается на https://"+set.Domain+"/."
	}
	return r
}

func checkCert(ctx context.Context, e Env, set site.Settings) Result {
	r := Result{ID: "cert", Title: "Сертификат"}
	c := site.CheckCert(ctx, set.Domain, e.HTTPSAddr, e.Roots, e.Now)
	switch c.State {
	case site.CertOK:
		r.State, r.Detail = OK, fmt.Sprintf("%s, ещё %d дн.", c.Issuer, c.DaysLeft)
	case site.CertExpiring:
		r.State, r.Detail = Warn, fmt.Sprintf("Истекает через %d дн., а Caddy его ещё не продлил. Проверьте, что порт 443 открыт.", c.DaysLeft)
	case site.CertNoDomain:
		r.State, r.Detail = Off, "Домен не задан."
	case site.CertUntrusted:
		r.State, r.Detail = Fail, "Браузеры ему не доверяют. Обычно это проходит, когда Caddy получит настоящий."
	default:
		r.State, r.Detail = Fail, "Ещё не выпущен. Проверьте, что домен указывает на сервер и порт 443 открыт."
	}
	return r
}

func checkDNS(ctx context.Context, e Env, set site.Settings) Result {
	r := Result{ID: "dns", Title: "Домен указывает на сервер"}
	if set.Domain == "" {
		r.State, r.Detail = Off, "Домен не задан."
		return r
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ips, err := e.Lookup(ctx, set.Domain)
	if err != nil || len(ips) == 0 {
		r.State, r.Detail = Fail, "Домен "+set.Domain+" не находится в DNS. Проверьте A-запись."
		return r
	}
	local, _ := e.LocalIPs()
	for _, ip := range ips {
		for _, l := range local {
			if ip.Equal(l) {
				r.State, r.Detail = OK, set.Domain+" → "+ip.String()+"."
				return r
			}
		}
	}
	var names []string
	for _, ip := range ips {
		names = append(names, ip.String())
	}
	r.State = Warn
	r.Detail = set.Domain + " указывает на " + strings.Join(names, ", ") + ", а этого адреса нет у сервера. Если сервер за NAT, это нормально; иначе исправьте A-запись."
	return r
}

func localIPs() ([]net.IP, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	var out []net.IP
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			out = append(out, n.IP)
		}
	}
	return out, nil
}

// checkRoute asks Caddy for the secret path and tells the panel from the
// stub by the panel's Content-Security-Policy header.
func checkRoute(ctx context.Context, e Env, set site.Settings, open bool, panel string) Result {
	r := Result{ID: "route", Title: "Вход в панель по домену"}
	if set.Domain == "" {
		r.State, r.Detail = Off, "Без домена панель открывается только через SSH-туннель."
		return r
	}
	res, err := probe(ctx, e, set.Domain, "/"+e.Cfg.SecretPath+"/")
	if err != nil {
		r.State, r.Detail = Off, "Не проверялся: Caddy не отвечает."
		return r
	}
	isPanel := strings.Contains(res.Header.Get("Content-Security-Policy"), "form-action")
	switch {
	case open && panel == "active" && isPanel:
		r.State, r.Detail = OK, "Открыт. Закройте после настройки: tigersb close."
	case open && panel == "active":
		r.State, r.Detail = Fail, "Панель включена на домене, но по секретному пути открывается не она. Выполните tigersb site apply."
	case isPanel:
		r.State, r.Detail = Fail, "Панель закрыта, но по секретному пути всё ещё открывается. Выполните tigersb close."
	case open:
		r.State, r.Detail = Off, "Панель спит, по секретному пути заглушка."
	default:
		r.State, r.Detail = OK, "Закрыт: по секретному пути заглушка. Открыть: tigersb open."
	}
	return r
}

func percent(used, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return 100 * float64(used) / float64(total)
}

func checkDisk(s *metrics.Sample) Result {
	r := Result{ID: "disk", Title: "Место на диске"}
	if s == nil || s.DiskTotal == 0 {
		r.State, r.Detail = Off, "Нет данных."
		return r
	}
	p := percent(s.DiskUsed, s.DiskTotal)
	free := fmt.Sprintf("Занято %.0f%%, свободно %s.", p, bytes(s.DiskTotal-s.DiskUsed))
	switch {
	case p >= 95:
		r.State, r.Detail = Fail, free+" Обновления и сертификаты могут перестать записываться."
	case p >= 85:
		r.State, r.Detail = Warn, free
	default:
		r.State, r.Detail = OK, free
	}
	return r
}

func checkMemory(s *metrics.Sample) Result {
	r := Result{ID: "memory", Title: "Память"}
	if s == nil || s.MemTotal == 0 {
		r.State, r.Detail = Off, "Нет данных."
		return r
	}
	p := percent(s.MemUsed, s.MemTotal)
	r.Detail = fmt.Sprintf("Занято %.0f%% из %s.", p, bytes(s.MemTotal))
	r.State = OK
	if p >= 90 {
		r.State = Warn
	}
	return r
}

func checkMonitor(e Env, s *metrics.Sample) Result {
	r := Result{ID: "monitor", Title: "Сбор данных о ресурсах"}
	if s == nil {
		r.State, r.Detail = Warn, "Данных ещё нет. Сбор идёт раз в 5 минут: systemctl status tigersb-monitor.timer"
		return r
	}
	age := e.Now.Sub(time.Unix(s.Time, 0))
	if age > 3*metrics.Interval {
		r.State, r.Detail = Warn, fmt.Sprintf("Последние данные %d мин назад. Проверьте: systemctl status tigersb-monitor.timer", int(age.Minutes()))
		return r
	}
	r.State, r.Detail = OK, "Раз в 5 минут, хранятся 7 дней."
	return r
}

func checkClock(e Env) Result {
	r := Result{ID: "clock", Title: "Точное время"}
	out, err := e.Run("timedatectl", "show", "-p", "NTPSynchronized", "--value")
	switch strings.TrimSpace(out) {
	case "yes":
		r.State, r.Detail = OK, "Часы синхронизированы."
	case "no":
		r.State, r.Detail = Warn, "Часы не синхронизированы. Сертификаты и протоколы VPN не любят неточное время: timedatectl set-ntp true"
	default:
		r.State, r.Detail = Off, "Не удалось проверить"
		if err != nil {
			r.Detail += ": " + err.Error()
		}
		r.Detail += "."
	}
	return r
}

func checkInternet(ctx context.Context, e Env) Result {
	r := Result{ID: "internet", Title: "Связь с GitHub"}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", e.Internet)
	if err != nil {
		r.State, r.Detail = Warn, "Нет связи: обновления не скачаются. "+err.Error()
		return r
	}
	conn.Close()
	r.State, r.Detail = OK, "Есть, обновления скачаются."
	return r
}

func checkDB(ctx context.Context, e Env) Result {
	r := Result{ID: "db", Title: "База данных"}
	res, err := e.Store.QuickCheck(ctx)
	if err != nil || res != "ok" {
		if err != nil {
			res = err.Error()
		}
		r.State, r.Detail = Fail, "Повреждена: "+res+". Верните точку восстановления."
		return r
	}
	r.State, r.Detail = OK, "Целая."
	return r
}

func checkUpdate(ctx context.Context, e Env) Result {
	r := Result{ID: "update", Title: "Обновления"}
	if e.Updater == nil {
		r.State, r.Detail = Off, "Нет данных."
		return r
	}
	s := e.Updater.Status()
	if s.Phase == update.PhaseFailed || s.Phase == update.PhaseInterrupted {
		r.State, r.Detail = Warn, "Последнее обновление: "+s.Message
		return r
	}
	c, _ := LastCheck(ctx, e.Store)
	switch {
	case s.Busy():
		r.State, r.Detail = OK, "Идёт обновление."
	case c != nil && c.Available:
		r.State, r.Detail = Warn, "Доступна версия "+c.Latest+". Обновить можно кнопкой ниже."
	default:
		r.State, r.Detail = OK, "Установлена версия "+e.Updater.Version+"."
	}
	return r
}

func checkPoints(e Env) Result {
	r := Result{ID: "restore", Title: "Точки восстановления"}
	if e.Updater == nil {
		r.State, r.Detail = Off, "Нет данных."
		return r
	}
	points, err := update.ListPoints(e.Updater.Layout)
	if err != nil || len(points) == 0 {
		r.State, r.Detail = Warn, "Ни одной нет. Создайте точку, прежде чем что-то менять."
		return r
	}
	r.State = OK
	r.Detail = fmt.Sprintf("%d шт., последняя %s.", len(points), points[0].Created.Local().Format("02.01.2006 15:04"))
	return r
}

// CheckKey is where the last GitHub check is stored.
const CheckKey = "update.check"

// LastCheck returns the stored GitHub check, or nil.
func LastCheck(ctx context.Context, st *store.Store) (*update.Check, error) {
	vals, err := st.Settings(ctx)
	if err != nil || vals[CheckKey] == "" {
		return nil, err
	}
	var c update.Check
	if err := json.Unmarshal([]byte(vals[CheckKey]), &c); err != nil {
		return nil, nil
	}
	return &c, nil
}

// SaveCheck stores a GitHub check.
func SaveCheck(ctx context.Context, st *store.Store, c update.Check) error {
	b, _ := json.Marshal(c)
	return st.SetSettings(ctx, map[string]string{CheckKey: string(b)})
}

func bytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d Б", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cБ", float64(n)/float64(div), []rune("КМГТП")[exp])
}
