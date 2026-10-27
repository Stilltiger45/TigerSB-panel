//go:build !linux

package edge

import (
	"errors"
	"net"
)

func setCongestion(net.Conn, string) error {
	return errors.New("выбор алгоритма TCP есть только в Linux")
}
