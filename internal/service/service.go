// Package service puts the panel to sleep and wakes it up through systemd.
//
// Sleep stops only the panel process. Xray, sing-box and Caddy are separate
// units, so VPN traffic and pre-rendered subscriptions keep working.
package service

import (
	"fmt"
	"os/exec"
	"strings"
)

// Unit is the systemd unit of the panel.
const Unit = "tigersb.service"

// Runner executes a command and returns its combined output.
type Runner func(name string, args ...string) (string, error)

// ExecRunner runs real commands.
func ExecRunner(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// Sleep stops the panel and keeps it from starting on boot.
func Sleep(run Runner) error {
	if out, err := run("systemctl", "disable", "--now", Unit); err != nil {
		return fmt.Errorf("systemctl disable --now: %v: %s", err, out)
	}
	return nil
}

// Wake starts the panel and enables it on boot.
func Wake(run Runner) error {
	if out, err := run("systemctl", "enable", "--now", Unit); err != nil {
		return fmt.Errorf("systemctl enable --now: %v: %s", err, out)
	}
	return nil
}

// State is "active", "inactive", "failed" and so on, as systemd reports it.
func State(run Runner) string {
	// is-active exits non-zero for anything but active; the output is still the state.
	out, _ := run("systemctl", "is-active", Unit)
	if out == "" {
		return "unknown"
	}
	return out
}
