//go:build linux || darwin

package hub

import (
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

const streamUnsent = 16 << 10

func boundSendQueue(c net.Conn) {
	sc, ok := c.(syscall.Conn)
	if !ok {
		return
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return
	}
	raw.Control(func(fd uintptr) {
		unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_NOTSENT_LOWAT, streamUnsent)
	})
}
