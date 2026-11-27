//go:build linux || darwin

package hub

import (
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

const streamUnsent = 16 << 10

func boundSendQueue(c net.Conn) error {
	sc, ok := c.(syscall.Conn)
	if !ok {
		return nil
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return err
	}
	var setErr error
	if err := raw.Control(func(fd uintptr) {
		setErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_NOTSENT_LOWAT, streamUnsent)
	}); err != nil {
		return err
	}
	return setErr
}
