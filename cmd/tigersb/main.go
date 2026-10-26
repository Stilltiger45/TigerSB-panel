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
	"github.com/Stilltiger45/TigerSB-panel/internal/web"
	"github.com/Stilltiger45/TigerSB-panel/web/assets"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `TigerSB panel

Использование:
  tigersb init     первичная настройка (вызывает установщик)
  tigersb serve    запустить панель (вызывает systemd)
  tigersb info     показать команду SSH-туннеля и адрес панели
  tigersb passwd   сменить пароль администратора
  tigersb site     заглушка и сертификат: site apply | site status
  tigersb sleep    усыпить панель (VPN продолжает работать)
  tigersb wake     разбудить панель
  tigersb status   состояние панели
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
		err = service.Sleep(service.ExecRunner)
		if err == nil {
			fmt.Println("Панель усыплена. VPN и подписки продолжают работать. Разбудить: tigersb wake")
		}
	case "wake":
		err = service.Wake(service.ExecRunner)
		if err == nil {
			fmt.Println("Панель запущена. Адрес: tigersb info")
		}
	case "status":
		fmt.Println("Панель:", service.State(service.ExecRunner))
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
	if err := config.Save(*cfgPath, cfg); err != nil {
		return err
	}
	printInfo(cfg)
	return nil
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
	fs := flag.NewFlagSet("info", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultPath, "путь к конфигу")
	fs.Parse(args)
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	printInfo(cfg)
	return nil
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

// cmdSite writes the maintenance page and Caddyfile and loads them into Caddy
// (apply), or prints the site and certificate state (status).
func cmdSite(args []string) error {
	if len(args) == 0 || (args[0] != "apply" && args[0] != "status") {
		return errors.New("использование: tigersb site apply | tigersb site status")
	}
	fs := flag.NewFlagSet("site", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultPath, "путь к конфигу")
	fs.Parse(args[1:])
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()
	set, err := site.Load(ctx, st, cfg)
	if err != nil {
		return err
	}
	paths := site.DefaultPaths(filepath.Dir(cfg.DBPath))
	if args[0] == "apply" {
		err := site.Apply(ctx, set, paths, time.Now())
		if err != nil && !errors.Is(err, site.ErrCaddyDown) {
			return err
		}
		if err != nil {
			fmt.Println(err)
		} else {
			fmt.Println("Настройки сайта применены.")
		}
	}
	domain := set.Domain
	if domain == "" {
		domain = "не задан, заглушка открывается по http://<IP сервера>/"
	}
	fmt.Printf("Домен: %s\nЗаглушка: %s «%s»\nCaddy: %s\n", domain, set.Template, set.Name, service.UnitState(service.ExecRunner, service.CaddyUnit))
	c := site.CheckCert(ctx, set.Domain, site.LocalTLS, nil, time.Now())
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

func printInfo(cfg *config.Config) {
	_, port, _ := net.SplitHostPort(cfg.Listen)
	host := cfg.Domain
	if host == "" {
		host = "<IP сервера>"
	}
	fmt.Printf(`
Панель доступна только через SSH-туннель.

1. На своём компьютере выполните:
   ssh -N -L 8080:127.0.0.1:%s <пользователь>@%s

2. Пока туннель открыт, откройте в браузере:
   http://localhost:8080/%s/

Сохраните секретный путь: без него панель не откроется даже через туннель.
`, port, host, cfg.SecretPath)
}
