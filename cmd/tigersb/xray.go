package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/Stilltiger45/TigerSB-panel/internal/edge"
	"github.com/Stilltiger45/TigerSB-panel/internal/service"
	"github.com/Stilltiger45/TigerSB-panel/internal/singbox"
	"github.com/Stilltiger45/TigerSB-panel/internal/xray"
)

func (e *env) runtime() *xray.Runtime {
	return xray.DefaultRuntime(filepath.Dir(e.cfg.DBPath))
}

// cmdXray runs the Xray container (run, stop: called by systemd), writes
// the config and restarts it (apply), or shows the state (status).
func cmdXray(args []string) error {
	if len(args) == 0 {
		return errors.New("использование: tigersb xray status | xray apply | xray image | xray run | xray stop")
	}
	if args[0] == "image" {
		// The installer pulls this image before the first start.
		fmt.Println(xray.DefaultRuntime("").Image)
		return nil
	}
	e, err := openEnv("xray", args[1:])
	if err != nil {
		return err
	}
	rt := e.runtime()
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
			return xray.ErrNoDocker
		}
		rt.Remove(ctx)
		// systemd watches docker itself, so a crash of Xray restarts the unit.
		return syscall.Exec(docker, append([]string{"docker"}, rt.RunArgs()...), os.Environ())
	case "stop":
		e.st.Close()
		_, err := rt.Exec(ctx, nil, "docker", "stop", "-t", "5", xray.Container)
		return err
	case "apply":
		defer e.st.Close()
		_, conf, routes, err := xray.Build(ctx, e.st, e.set.Domain)
		if err != nil {
			return err
		}
		if err := rt.Test(ctx, conf); err != nil {
			return err
		}
		if err := rt.Write(conf, routes); err != nil {
			return err
		}
		if err := rt.Restart(ctx); err != nil {
			return err
		}
		fmt.Println("Настройки Xray применены.")
		return nil
	case "status":
		defer e.st.Close()
		run := service.ExecRunner
		fmt.Println("Xray (Docker):", service.UnitState(run, xray.Unit))
		fmt.Println("Порт 443 (edge):", service.UnitState(run, xray.EdgeUnit))
		s, err := xray.Load(ctx, e.st)
		if err != nil {
			return err
		}
		for _, p := range []struct {
			name    string
			enabled bool
			port    int
			sni     string
		}{{"gRPC", s.GRPC.Enabled, s.GRPC.Port, s.GRPC.Reality.SNI}, {"XHTTP", s.XHTTP.Enabled, s.XHTTP.Port, s.XHTTP.Reality.SNI}} {
			state := "выключен"
			if p.enabled {
				state = fmt.Sprintf("порт %d, SNI %s", p.port, p.sni)
			}
			fmt.Printf("%s: %s\n", p.name, state)
		}
		users, err := e.st.Users(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("Пользователей: %d\n", len(users))
		return nil
	}
	e.st.Close()
	return fmt.Errorf("неизвестная команда xray %s", args[0])
}

// cmdEdge holds port 443 and splits it by SNI between Caddy and Xray.
func cmdEdge(args []string) error {
	e, err := openEnv("edge", args)
	if err != nil {
		return err
	}
	rt := e.runtime()
	// Fresh routes from the database; on failure the file on disk is used.
	if _, _, routes, err := xray.Build(context.Background(), e.st, e.set.Domain); err != nil {
		log.Printf("edge: маршруты из базы: %v", err)
	} else if err := edge.Write(rt.Routes, routes); err != nil {
		log.Printf("edge: %v", err)
	}
	e.st.Close()

	ln, err := net.Listen("tcp", ":"+strconv.Itoa(xray.EdgePort))
	if err != nil {
		return err
	}
	// The ports it holds for sing-box come from the file sing-box's Sync
	// writes.
	srv := &edge.Server{Path: rt.Routes, Forwards: singbox.ForwardsPath(filepath.Dir(e.cfg.DBPath))}
	srv.Reload()
	stop := make(chan struct{})
	go srv.Watch(2*time.Second, stop)
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	go func() {
		<-ctx.Done()
		close(stop)
		ln.Close()
		srv.CloseForwards()
	}()
	log.Printf("edge: слушаю :%d", xray.EdgePort)
	return srv.Serve(ln)
}
