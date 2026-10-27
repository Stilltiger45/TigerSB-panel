package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Stilltiger45/TigerSB-panel/internal/core"
	"github.com/Stilltiger45/TigerSB-panel/internal/service"
	"github.com/Stilltiger45/TigerSB-panel/internal/singbox"
)

func (e *env) singbox() *singbox.Runtime {
	return singbox.DefaultRuntime(filepath.Dir(e.cfg.DBPath))
}

// cmdSingbox runs the sing-box container (run, stop, reload: called by
// systemd), writes the config and reloads it (apply), or shows the state.
func cmdSingbox(args []string) error {
	if len(args) == 0 {
		return errors.New("использование: tigersb singbox status | singbox apply | singbox image | singbox run | singbox stop | singbox reload")
	}
	if args[0] == "image" {
		// The installer pulls this image before the first start.
		fmt.Println(singbox.DefaultRuntime("").Image)
		return nil
	}
	e, err := openEnv("singbox", args[1:])
	if err != nil {
		return err
	}
	rt := e.singbox()
	ctx := context.Background()
	switch args[0] {
	case "run":
		err := rt.Sync(ctx, e.st, e.set.Domain, false)
		e.st.Close()
		if err != nil {
			return err
		}
		docker, err := exec.LookPath("docker")
		if err != nil {
			return core.ErrNoDocker
		}
		rt.Remove(ctx)
		// systemd watches docker itself, so a crash of sing-box restarts the unit.
		return syscall.Exec(docker, append([]string{"docker"}, rt.RunArgs()...), os.Environ())
	case "stop":
		e.st.Close()
		_, err := rt.Exec(ctx, nil, "docker", "stop", "-t", "5", singbox.Container)
		return err
	case "reload":
		e.st.Close()
		return rt.Signal(ctx)
	case "apply":
		defer e.st.Close()
		if _, err := rt.CopyCert(e.set.Domain, time.Now()); err != nil {
			return err
		}
		_, conf, err := singbox.Build(ctx, e.st, e.set.Domain, rt.HaveCert())
		if err != nil {
			return err
		}
		if err := rt.Test(ctx, conf); err != nil {
			return err
		}
		if err := rt.Sync(ctx, e.st, e.set.Domain, false); err != nil {
			return err
		}
		if _, err := rt.Exec(ctx, nil, "systemctl", "restart", singbox.Unit); err != nil {
			return fmt.Errorf("sing-box не перезапустился. Подробности: journalctl -u %s", singbox.Unit)
		}
		fmt.Println("Настройки sing-box применены.")
		return nil
	case "status":
		defer e.st.Close()
		fmt.Println("sing-box (Docker):", service.UnitState(service.ExecRunner, singbox.Unit))
		s, err := singbox.Load(ctx, e.st)
		if err != nil {
			return err
		}
		switch {
		case !s.TUIC.Enabled:
			fmt.Println("TUIC: выключен")
		case !singbox.Serving(s, e.set.Domain, rt.HaveCert()):
			fmt.Printf("TUIC: порт %d/udp, ждёт домен и сертификат\n", s.TUIC.Port)
		default:
			fmt.Printf("TUIC: порт %d/udp, SNI %s\n", s.TUIC.Port, e.set.Domain)
		}
		return nil
	}
	e.st.Close()
	return fmt.Errorf("неизвестная команда singbox %s", args[0])
}
