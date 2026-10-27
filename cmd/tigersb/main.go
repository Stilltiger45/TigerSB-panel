// Command tigersb is the TigerSB panel: web UI, installer helper and sleep switch.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/Stilltiger45/TigerSB-panel/internal/auth"
	"github.com/Stilltiger45/TigerSB-panel/internal/config"
	"github.com/Stilltiger45/TigerSB-panel/internal/service"
	"github.com/Stilltiger45/TigerSB-panel/internal/site"
	"github.com/Stilltiger45/TigerSB-panel/internal/store"
	"github.com/Stilltiger45/TigerSB-panel/internal/update"
	"github.com/Stilltiger45/TigerSB-panel/internal/web"
	"github.com/Stilltiger45/TigerSB-panel/internal/xray"
	"github.com/Stilltiger45/TigerSB-panel/web/assets"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `TigerSB panel

Использование:
  tigersb init     первичная настройка (вызывает установщик)
  tigersb serve    запустить панель (вызывает systemd)
  tigersb info     показать адрес панели
  tigersb passwd   сменить пароль администратора
  tigersb site     заглушка и сертификат: site status | site apply | site set --domain …
  tigersb open     открыть панель по адресу https://<домен>/<секретный путь>/
  tigersb close    закрыть панель: по секретному пути будет открываться заглушка
  tigersb sleep    усыпить панель (VPN продолжает работать)
  tigersb wake     разбудить панель
  tigersb status   состояние панели
  tigersb update   обновить панель с GitHub (перед этим создаётся точка восстановления)
  tigersb restore  точки восстановления: restore list | restore create | restore apply <имя>
  tigersb monitor  снять показатели и проверить сервер (вызывает таймер раз в 5 минут)
  tigersb xray     VPN-ядро Xray в Docker (gRPC, XHTTP): xray status | xray apply
  tigersb singbox  VPN-ядро sing-box в Docker (TUIC): singbox status | singbox apply
  tigersb edge     делить порт 443 между сайтом и VPN (вызывает systemd)
  tigersb sub      выдавать подписки пользователей (вызывает systemd)
  tigersb version  версия
`

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "init":
		err = cmdInit(os.Args[2:])
	case "serve":
		err = cmdServe(os.Args[2:])
	case "info":
		err = cmdInfo(os.Args[2:])
	case "passwd":
		err = cmdPasswd(os.Args[2:])
	case "site":
		err = cmdSite(os.Args[2:])
	case "sleep":
		err = cmdSleep(os.Args[2:])
	case "wake":
		err = cmdWake(os.Args[2:])
	case "open":
		err = cmdOpen(os.Args[2:], true)
	case "close":
		err = cmdOpen(os.Args[2:], false)
	case "status":
		fmt.Println("Панель:", service.State(service.ExecRunner))
	case "monitor":
		err = cmdMonitor(os.Args[2:])
	case "update":
		err = cmdUpdate(os.Args[2:])
	case "restore":
		err = cmdRestore(os.Args[2:])
	case "job":
		err = cmdJob(os.Args[2:])
	case "xray":
		err = cmdXray(os.Args[2:])
	case "singbox":
		err = cmdSingbox(os.Args[2:])
	case "edge":
		err = cmdEdge(os.Args[2:])
	case "sub":
		err = cmdSub(os.Args[2:])
	case "version":
		fmt.Println(version)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "неизвестная команда %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		log.Fatalf("ошибка: %v", err)
	}
}

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultPath, "путь к конфигу")
	dbPath := fs.String("db", "/var/lib/tigersb/panel.db", "путь к базе")
	role := fs.String("role", "", "роль сервера: main или router")
	domain := fs.String("domain", "", "домен сервера")
	email := fs.String("email", "", "email для сертификата")
	mainServer := fs.String("main-server", "", "адрес основного сервера (для router)")
	admin := fs.String("admin", "", "логин администратора")
	passwordStdin := fs.Bool("password-stdin", false, "прочитать пароль из stdin")
	fs.Parse(args)

	if _, err := os.Stat(*cfgPath); err == nil {
		return fmt.Errorf("%s уже существует; для смены пароля используйте tigersb passwd", *cfgPath)
	}
	if *admin == "" {
		return errors.New("нужен --admin")
	}
	secret, err := config.NewSecretPath()
	if err != nil {
		return err
	}
	port, err := randomPort()
	if err != nil {
		return err
	}
	cfg := &config.Config{
		Role:       config.Role(*role),
		Listen:     net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
		SecretPath: secret,
		Domain:     *domain,
		Email:      *email,
		DBPath:     *dbPath,
		MainServer: *mainServer,
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	password, err := readPassword(*passwordStdin)
	if err != nil {
		return err
	}
	if err := setAdmin(cfg.DBPath, *admin, password); err != nil {
		return err
	}
	return config.Save(*cfgPath, cfg)
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultPath, "путь к конфигу")
	fs.Parse(args)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	h := web.New(cfg, st, assets.FS, version)
	h.Updater = &update.Updater{
		Layout:  update.DefaultLayout(*cfgPath, cfg.DBPath),
		Store:   st,
		Version: version,
		PanelUp: update.LoopbackCheck(cfg.Listen, cfg.SecretPath),
	}
	go h.PruneLoop(ctx)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Printf("TigerSB %s: панель слушает %s (роль %s)", version, cfg.Listen, cfg.Role)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}

func cmdInfo(args []string) error {
	env, err := openEnv("info", args)
	if err != nil {
		return err
	}
	defer env.st.Close()
	return env.printInfo()
}

func cmdPasswd(args []string) error {
	fs := flag.NewFlagSet("passwd", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultPath, "путь к конфигу")
	admin := fs.String("admin", "", "логин администратора")
	passwordStdin := fs.Bool("password-stdin", false, "прочитать пароль из stdin")
	fs.Parse(args)
	if *admin == "" {
		return errors.New("нужен --admin")
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	password, err := readPassword(*passwordStdin)
	if err != nil {
		return err
	}
	if err := setAdmin(cfg.DBPath, *admin, password); err != nil {
		return err
	}
	fmt.Println("Пароль обновлён, все активные сессии этого администратора завершены.")
	return nil
}

// env is what the site and access commands need.
type env struct {
	cfgPath string
	cfg     *config.Config
	st      *store.Store
	set     site.Settings
	paths   site.Paths
	open    bool
}

func openEnv(name string, args []string, extra ...func(*flag.FlagSet)) (*env, error) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultPath, "путь к конфигу")
	for _, f := range extra {
		f(fs)
	}
	fs.Parse(args)
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return nil, err
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	set, err := site.Load(ctx, st, cfg)
	if err == nil {
		var open bool
		open, err = site.PanelOpen(ctx, st)
		if err == nil {
			return &env{cfgPath: *cfgPath, cfg: cfg, st: st, set: set, paths: site.DefaultPaths(filepath.Dir(cfg.DBPath)), open: open}, nil
		}
	}
	st.Close()
	return nil, err
}

// panelUp is false only when systemd says the panel is stopped, so a sleeping
// panel never gets a route that would answer 502 instead of the page.
func panelUp() bool {
	switch service.State(service.ExecRunner) {
	case "inactive", "failed":
		return false
	}
	return true
}

// apply writes the site and loads it into Caddy, publishing the panel when
// published is true.
func (e *env) apply(published bool) error {
	err := site.Apply(context.Background(), e.set, e.paths, site.PanelRoute(e.cfg, published), time.Now())
	// Port 443 sends the domain to Caddy; keep that in step with the site.
	// Caddy goes first: it must already accept the PROXY header the new
	// routes add.
	if err := e.runtime().Sync(context.Background(), e.st, e.set.Domain, false); err != nil {
		fmt.Fprintln(os.Stderr, "маршруты порта 443:", err)
	}
	// TUIC uses the domain's certificate and name.
	if err := e.singbox().Sync(context.Background(), e.st, e.set.Domain, true); err != nil {
		fmt.Fprintln(os.Stderr, "sing-box:", err)
	}
	if errors.Is(err, site.ErrCaddyDown) {
		fmt.Println(err)
		return nil
	}
	return err
}

func (e *env) printInfo() error {
	url := site.PanelURL(e.set, e.cfg)
	switch {
	case url == "":
		fmt.Println("Домен не задан, поэтому панель открывается только через SSH-туннель (ниже).")
		fmt.Println("Задать домен: tigersb site set --domain vpn.example.com --email you@example.com")
	case !e.open:
		fmt.Println("Панель закрыта: по секретному пути открывается заглушка.")
		fmt.Println("Открыть: tigersb open")
	case !panelUp():
		fmt.Println("Панель спит. Разбудить: tigersb wake")
	default:
		fmt.Printf("Панель открыта:\n   %s\n\nЗакрыть после настройки: tigersb close\n", url)
	}
	_, port, _ := net.SplitHostPort(e.cfg.Listen)
	fmt.Printf(`
Запасной вход через SSH-туннель работает всегда, даже когда панель закрыта.
На своём компьютере (не на сервере) выполните:
   ssh -N -L 8080:127.0.0.1:%s root@<IP сервера>
и, пока команда работает, откройте http://localhost:8080/%s/

Сохраните секретный путь: без него панель не откроется.
`, port, e.cfg.SecretPath)
	return nil
}

func cmdOpen(args []string, open bool) error {
	name := "close"
	if open {
		name = "open"
	}
	e, err := openEnv(name, args)
	if err != nil {
		return err
	}
	defer e.st.Close()
	if open && e.set.Domain == "" {
		return errors.New("сначала задайте домен: tigersb site set --domain vpn.example.com")
	}
	if err := site.SetPanelOpen(context.Background(), e.st, open); err != nil {
		return err
	}
	e.open = open
	if open && !panelUp() {
		if err := service.Wake(service.ExecRunner); err != nil {
			return err
		}
	}
	if err := e.apply(open); err != nil {
		return err
	}
	if !open {
		fmt.Println("Панель закрыта: по секретному пути теперь открывается заглушка.")
		fmt.Println("Открыть снова: tigersb open")
		return nil
	}
	return e.printInfo()
}

func cmdSleep(args []string) error {
	e, err := openEnv("sleep", args)
	if err != nil {
		return err
	}
	defer e.st.Close()
	if err := service.Sleep(service.ExecRunner); err != nil {
		return err
	}
	if err := e.apply(false); err != nil {
		return err
	}
	fmt.Println("Панель усыплена: по секретному пути открывается заглушка, VPN и подписки работают. Разбудить: tigersb wake")
	return nil
}

func cmdWake(args []string) error {
	e, err := openEnv("wake", args)
	if err != nil {
		return err
	}
	defer e.st.Close()
	if err := service.Wake(service.ExecRunner); err != nil {
		return err
	}
	if err := e.apply(e.open); err != nil {
		return err
	}
	fmt.Println("Панель запущена.")
	return e.printInfo()
}

// cmdSite prints the site and certificate state (status), writes and loads
// it again (apply), or changes the domain and email (set).
func cmdSite(args []string) error {
	if len(args) == 0 || (args[0] != "apply" && args[0] != "status" && args[0] != "set") {
		return errors.New("использование: tigersb site status | site apply | site set --domain vpn.example.com [--email you@example.com]")
	}
	var domain, email string
	e, err := openEnv("site", args[1:], func(fs *flag.FlagSet) {
		fs.StringVar(&domain, "domain", "", "домен сервера")
		fs.StringVar(&email, "email", "", "email для Let's Encrypt")
	})
	if err != nil {
		return err
	}
	defer e.st.Close()
	ctx := context.Background()
	switch args[0] {
	case "set":
		if domain == "" && email == "" {
			return errors.New("укажите --domain и/или --email")
		}
		if domain != "" {
			e.set.Domain = domain
			if err := xray.CheckDomainFree(ctx, e.st, strings.ToLower(domain)); err != nil {
				return err
			}
		}
		if email != "" {
			e.set.Email = email
		}
		e.set.Normalize()
		if err := site.Save(ctx, e.st, e.set); err != nil {
			return err
		}
		fallthrough
	case "apply":
		if err := e.apply(e.open && panelUp()); err != nil {
			return err
		}
		fmt.Println("Настройки сайта применены.")
	}
	domainText := e.set.Domain
	if domainText == "" {
		domainText = "не задан, заглушка открывается по http://<IP сервера>/"
	}
	fmt.Printf("Домен: %s\nЗаглушка: %s «%s»\nCaddy: %s\n", domainText, e.set.Template, e.set.Name, service.UnitState(service.ExecRunner, service.CaddyUnit))
	c := site.CheckCert(ctx, e.set.Domain, site.LocalTLS, nil, time.Now())
	switch c.State {
	case site.CertOK, site.CertExpiring:
		fmt.Printf("Сертификат: %s, действует до %s (%d дн.)\n", c.Issuer, c.NotAfter.Format("2006-01-02"), c.DaysLeft)
	case site.CertUntrusted:
		fmt.Printf("Сертификат: не доверенный (%s): %s\n", c.Issuer, c.Error)
	case site.CertMissing:
		fmt.Println("Сертификат: ещё не выпущен. Caddy получает его через порт 443, проверьте, что домен указывает на этот сервер.")
	}
	return nil
}

func setAdmin(dbPath, username, password string) error {
	if len(password) < auth.MinPasswordLen {
		return fmt.Errorf("пароль короче %d символов", auth.MinPasswordLen)
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	return st.UpsertAdmin(context.Background(), username, hash)
}

func readPassword(fromStdin bool) (string, error) {
	if fromStdin || !term.IsTerminal(int(os.Stdin.Fd())) {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	fmt.Fprint(os.Stderr, "Пароль: ")
	p1, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	fmt.Fprint(os.Stderr, "Повторите пароль: ")
	p2, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	if string(p1) != string(p2) {
		return "", errors.New("пароли не совпадают")
	}
	return string(p1), nil
}

// randomPort picks a loopback port in 20000–59999 so the panel is not on a well-known port.
func randomPort() (int, error) {
	for i := 0; i < 20; i++ {
		n, err := rand.Int(rand.Reader, big.NewInt(40000))
		if err != nil {
			return 0, err
		}
		port := 20000 + int(n.Int64())
		l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err == nil {
			l.Close()
			return port, nil
		}
	}
	return 0, errors.New("не удалось найти свободный порт")
}
