package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/Stilltiger45/TigerSB-panel/internal/site"
	"github.com/Stilltiger45/TigerSB-panel/internal/sub"
	"github.com/Stilltiger45/TigerSB-panel/internal/xray"
)

// cmdSub serves subscriptions on loopback for Caddy. It runs apart from the
// panel, so subscriptions and the device limit keep working while the panel
// sleeps or is closed.
func cmdSub(args []string) error {
	e, err := openEnv("sub", args)
	if err != nil {
		return err
	}
	defer e.st.Close()
	h := &sub.Handler{
		Store: e.st,
		Domain: func(ctx context.Context) (string, error) {
			set, err := site.Load(ctx, e.st, e.cfg)
			return set.Domain, err
		},
		LocalIPs: xray.LocalIPs,
		TUICCert: e.singbox().HaveCert,
	}
	// The key is made now rather than on the first request, so the panel and
	// this service never race to create it.
	if _, err := sub.SigningKey(context.Background(), e.st); err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              net.JoinHostPort("127.0.0.1", strconv.Itoa(site.SubPort)),
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       20 * time.Second,
		WriteTimeout:      30 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	go func() {
		<-ctx.Done()
		shut, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		srv.Shutdown(shut)
	}()
	log.Printf("sub: слушаю %s", srv.Addr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
