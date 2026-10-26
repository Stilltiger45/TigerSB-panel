package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Stilltiger45/TigerSB-panel/internal/health"
	"github.com/Stilltiger45/TigerSB-panel/internal/metrics"
	"github.com/Stilltiger45/TigerSB-panel/internal/update"
)

// checkEvery is how often the monitor timer asks GitHub for a new version.
const checkEvery = 6 * time.Hour

func (e *env) updater() *update.Updater {
	return &update.Updater{
		Layout:  update.DefaultLayout(e.cfgPath, e.cfg.DBPath),
		Store:   e.st,
		Version: version,
		PanelUp: update.LoopbackCheck(e.cfg.Listen, e.cfg.SecretPath),
	}
}

func cmdMonitor(args []string) error {
	e, err := openEnv("monitor", args)
	if err != nil {
		return err
	}
	defer e.st.Close()
	ctx := context.Background()
	now := time.Now()
	if _, err := metrics.Collect(ctx, e.st, metrics.Host, now); err != nil {
		fmt.Fprintln(os.Stderr, "показатели:", err)
	}
	if c, _ := health.LastCheck(ctx, e.st); c == nil || now.Sub(c.Checked) > checkEvery {
		c := update.CheckGitHub(ctx, http.DefaultClient, version, now)
		if err := health.SaveCheck(ctx, e.st, c); err != nil {
			return err
		}
	}
	r, err := health.Run(ctx, health.Env{Cfg: e.cfg, Store: e.st, Updater: e.updater(), Now: now})
	if err != nil {
		return err
	}
	for _, res := range r.Results {
		if res.State == health.Warn || res.State == health.Fail {
			fmt.Printf("%s: %s\n", res.Title, res.Detail)
		}
	}
	return nil
}

func cmdUpdate(args []string) error {
	var yes bool
	e, err := openEnv("update", args, func(fs *flag.FlagSet) {
		fs.BoolVar(&yes, "yes", false, "не спрашивать подтверждение")
	})
	if err != nil {
		return err
	}
	defer e.st.Close()
	ctx := context.Background()
	c := update.CheckGitHub(ctx, http.DefaultClient, version, time.Now())
	health.SaveCheck(ctx, e.st, c)
	if c.Error != "" {
		return errors.New(c.Error)
	}
	if !c.Available {
		fmt.Printf("Установлена последняя версия %s.\n", version)
		return nil
	}
	fmt.Printf("Доступна версия %s (сейчас %s). Что изменилось:\n", c.Latest, version)
	for _, ch := range c.Changes {
		fmt.Println("  -", ch)
	}
	if !yes && !confirm("Создать точку восстановления и обновить? [y/N]: ") {
		fmt.Println("Отменено.")
		return nil
	}
	u := e.updater()
	log, err := u.OpenLog()
	if err != nil {
		return err
	}
	defer log.Close()
	return u.RunNow(ctx, update.ActionUpdate, c.Ref, io.MultiWriter(log, os.Stdout))
}

func confirm(prompt string) bool {
	tty, err := os.Open("/dev/tty")
	if err != nil {
		return false
	}
	defer tty.Close()
	fmt.Print(prompt)
	line, _ := bufio.NewReader(tty).ReadString('\n')
	switch strings.TrimSpace(strings.ToLower(line)) {
	case "y", "yes", "д", "да":
		return true
	}
	return false
}

func cmdRestore(args []string) error {
	if len(args) == 0 {
		args = []string{"list"}
	}
	sub, rest := args[0], args[1:]
	var id string
	if sub == "apply" {
		if len(rest) == 0 || strings.HasPrefix(rest[0], "-") {
			return errors.New("укажите имя точки: tigersb restore apply <имя>, список: tigersb restore list")
		}
		id, rest = rest[0], rest[1:]
	}
	e, err := openEnv("restore", rest)
	if err != nil {
		return err
	}
	defer e.st.Close()
	u := e.updater()
	switch sub {
	case "list":
		points, err := update.ListPoints(u.Layout)
		if err != nil {
			return err
		}
		if len(points) == 0 {
			fmt.Println("Точек восстановления нет. Создать: tigersb restore create")
		}
		for _, p := range points {
			fmt.Printf("%s  %s  версия %s, %s, %.1f МБ\n", p.ID, p.Created.Local().Format("02.01.2006 15:04"), p.Version, p.Reason, float64(p.Size)/(1<<20))
		}
		return nil
	case "create":
		p, err := update.CreatePoint(context.Background(), u.Layout, e.st, version, "вручную", time.Now())
		if err != nil {
			return err
		}
		fmt.Printf("Точка восстановления %s создана. Вернуть: tigersb restore apply %s\n", p.ID, p.ID)
		return nil
	case "apply":
		log, err := u.OpenLog()
		if err != nil {
			return err
		}
		defer log.Close()
		return u.RunNow(context.Background(), update.ActionRollback, id, io.MultiWriter(log, os.Stdout))
	}
	return fmt.Errorf("неизвестная команда restore %s: есть list, create, apply", sub)
}

// cmdJob runs the update or rollback the panel queued. tigersb-update.service calls it.
func cmdJob(args []string) error {
	e, err := openEnv("job", args)
	if err != nil {
		return err
	}
	defer e.st.Close()
	u := e.updater()
	log, err := u.OpenLog()
	if err != nil {
		return err
	}
	defer log.Close()
	return u.RunQueued(context.Background(), io.MultiWriter(log, os.Stdout))
}
