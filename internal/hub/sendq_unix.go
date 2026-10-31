//go:build linux || darwin

package hub

import (
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// streamUnsent caps the bytes a compact stream's connection may hold unsent in the kernel:
// under a second of a 250-device stream at 20 KB/s.
const streamUnsent = 16 << 10

// boundSendQueue caps the bytes a compact stream's connection holds unsent in the kernel at
// streamUnsent (TCP_NOTSENT_LOWAT), so a write returns only once the link has carried
// the rest. Bytes in flight aren't capped, so a fast link keeps its speed. Best effort.
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
