package edge

import (
	"errors"
	"net"

	"golang.org/x/sys/unix"
)

// setCongestion switches the TCP congestion control of a connection. The
// kernel allows it without privileges for the algorithms listed in
// net.ipv4.tcp_allowed_congestion_control.
func setCongestion(c net.Conn, name string) error {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		return errors.New("не TCP")
	}
	raw, err := tc.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	if err := raw.Control(func(fd uintptr) {
		serr = unix.SetsockoptString(int(fd), unix.IPPROTO_TCP, unix.TCP_CONGESTION, name)
	}); err != nil {
		return err
	}
	return serr
}
