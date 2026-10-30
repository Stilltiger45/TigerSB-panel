package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Stilltiger45/TigerSB-panel/internal/core"
	"github.com/Stilltiger45/TigerSB-panel/internal/service"
	"github.com/Stilltiger45/TigerSB-panel/internal/singbox"
)

func (e *env) singbox() *singbox.Runtime {
	return singbox.DefaultRuntime(filepath.Dir(e.cfg.DBPath))
}

func (e *env) mieru() *singbox.Runtime {
	return singbox.MieruRuntime(filepath.Dir(e.cfg.DBPath))
}

// cmdSingbox runs a sing-box container (run, stop, reload: called by
// systemd), writes the config and reloads it (apply), or shows the state.
// cmd is "singbox" for sing-box itself or "mieru" for mbox, which serves
// Mieru from the same image.
func cmdSingbox(cmd string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("использование: tigersb %[1]s status | %[1]s apply | %[1]s image | %[1]s run | %[1]s stop | %[1]s reload", cmd)
	}
	if args[0] == "image" {
		// The installer pulls this image before the first start.
		fmt.Println(singbox.DefaultRuntime("").Image)
		return nil
	}
	e, err := openEnv(cmd, args[1:])
	if err != nil {
		return err
	}
	rt := e.singbox()
	if cmd == "mieru" {
		rt = e.mieru()
	}
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
		// systemd watches docker itself, so a crash of the core restarts the unit.
		return syscall.Exec(docker, append([]string{"docker"}, rt.RunArgs()...), os.Environ())
	case "stop":
		e.st.Close()
		_, err := rt.Exec(ctx, nil, "docker", "stop", "-t", "5", rt.Container)
		return err
	case "reload":
		e.st.Close()
		return rt.Signal(ctx)
	case "apply":
		defer e.st.Close()
		if rt.Certs != "" {
			if _, err := rt.CopyCert(e.set.Domain, time.Now()); err != nil {
				return err
			}
		}
		_, conf, err := rt.Build(ctx, e.st, e.set.Domain)
		if err != nil {
			return err
		}
		if err := rt.Test(ctx, conf); err != nil {
			return err
		}
		if err := rt.Sync(ctx, e.st, e.set.Domain, false); err != nil {
			return err
		}
		if err := rt.Restart(ctx); err != nil {
			return err
		}
		fmt.Printf("Настройки %s применены.\n", rt.Name)
		return nil
	case "status":
		defer e.st.Close()
		fmt.Printf("%s (Docker): %s\n", rt.Name, service.UnitState(service.ExecRunner, rt.Unit))
		s, err := singbox.Load(ctx, e.st)
		if err != nil {
			return err
		}
		if cmd == "mieru" {
			m := s.Mieru
			if !m.Enabled {
				fmt.Println("Mieru: выключен")
			} else {
				fmt.Printf("Mieru: порты %d-%d/%s\n", m.PortFrom, m.PortTo, m.Label())
			}
			return nil
		}
		switch {
		case !s.TUIC.Enabled:
			fmt.Println("TUIC: выключен")
		case !singbox.Serving(s, e.set.Domain, rt.HaveCert()):
			fmt.Printf("TUIC: порт %d/udp, ждёт домен и сертификат\n", s.TUIC.Port)
		default:
			fmt.Printf("TUIC: порт %d/udp, SNI %s\n", s.TUIC.Port, e.set.Domain)
		}
		h := s.Hysteria2
		obfs := "без Salamander"
		if h.Obfs {
			obfs = "Salamander"
		}
		switch {
		case !h.Enabled:
			fmt.Println("Hysteria 2: выключен")
		case !singbox.ServingHy2(s, e.set.Domain, rt.HaveCert()):
			fmt.Printf("Hysteria 2: порт %d/udp, ждёт домен и сертификат\n", h.Port)
		default:
			fmt.Printf("Hysteria 2: порт %d/udp, SNI %s, %s\n", h.Port, e.set.Domain, obfs)
		}
		n := s.Naive
		var nets []string
		if n.TCP() {
			nets = append(nets, fmt.Sprintf("%d/tcp через tigersb-edge", n.Port))
		}
		if n.QUIC() {
			nets = append(nets, fmt.Sprintf("%d/udp (QUIC)", n.Port))
		}
		bbr := "без BBR"
		if n.BBR {
			bbr = "BBR"
		}
		switch {
		case !n.Enabled:
			fmt.Println("NaïveProxy: выключен")
		case !singbox.ServingNaive(s, e.set.Domain, rt.HaveCert()):
			fmt.Printf("NaïveProxy: порт %s, ждёт домен и сертификат\n", strings.Join(nets, ", "))
		default:
			fmt.Printf("NaïveProxy: порт %s, SNI %s, %s\n", strings.Join(nets, ", "), e.set.Domain, bbr)
		}
		return nil
	}
	e.st.Close()
	return fmt.Errorf("неизвестная команда %s %s", cmd, args[0])
}
