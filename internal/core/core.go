// Package core holds what the VPN cores share. Each core runs in its own
// Docker container with the host network: Xray-core (package xray) carries
// VLESS over gRPC and XHTTP, sing-box (package singbox) carries TUIC. A new
// protocol goes into the core that supports it; a new core gets a package
// of its own with the same shape: settings, a config renderer, a runtime
// with Test, Sync and the `docker run` arguments, and share links.
package core

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
)

// Exec runs a command with optional stdin and returns its combined output.
type Exec func(ctx context.Context, stdin []byte, name string, args ...string) (string, error)

// RealExec runs real commands.
func RealExec(ctx context.Context, stdin []byte, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// ErrNoDocker means Docker is not installed or not running.
var ErrNoDocker = errors.New("Docker не установлен или не запущен: переустановите панель (install.sh --update) или запустите systemctl start docker")

// Docker runs the docker CLI and turns "no Docker here" into ErrNoDocker.
func Docker(ctx context.Context, run Exec, stdin []byte, args ...string) (string, error) {
	out, err := run(ctx, stdin, "docker", args...)
	var notFound *exec.Error
	if errors.As(err, &notFound) || strings.Contains(out, "Cannot connect to the Docker daemon") {
		return out, ErrNoDocker
	}
	return out, err
}

// Tail keeps the last lines of tool output for an error message.
func Tail(out string, n int) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// Link is one ready-to-import connection of a user.
type Link struct {
	Protocol string `json:"protocol"` // grpc, xhttp or tuic
	Title    string `json:"title"`
	URL      string `json:"url"`
}
