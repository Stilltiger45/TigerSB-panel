package edge

import (
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func congestion(c net.Conn) (string, error) {
	raw, err := c.(*net.TCPConn).SyscallConn()
	if err != nil {
		return "", err
	}
	var name string
	var serr error
	if err := raw.Control(func(fd uintptr) {
		name, serr = unix.GetsockoptString(int(fd), unix.IPPROTO_TCP, unix.TCP_CONGESTION)
	}); err != nil {
		return "", err
	}
	return name, serr
}

// A route with BBR switches the client's connection to it, the others keep
// the kernel's default.
func TestRouteCongestion(t *testing.T) {
	allowed, _ := os.ReadFile("/proc/sys/net/ipv4/tcp_allowed_congestion_control")
	if !strings.Contains(" "+strings.TrimSpace(string(allowed))+" ", " bbr ") {
		t.Skip("this kernel does not allow bbr: " + string(allowed))
	}
	def, _ := os.ReadFile("/proc/sys/net/ipv4/tcp_congestion_control")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			got <- err.Error()
			return
		}
		defer c.Close()
		// reno first, so the test proves a switch even where bbr is the default.
		for _, cc := range []string{"reno", "bbr"} {
			if err := setCongestion(c, cc); err != nil {
				got <- err.Error()
				return
			}
		}
		name, err := congestion(c)
		if err != nil {
			name = err.Error()
		}
		got <- name
	}()
	c, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if name := <-got; name != "bbr" {
		t.Fatalf("congestion %q, kernel default %q", name, strings.TrimSpace(string(def)))
	}
}
