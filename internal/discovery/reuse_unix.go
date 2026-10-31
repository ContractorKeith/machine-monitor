//go:build !windows && !darwin

package discovery

import "syscall"

// reuseAddr sets SO_REUSEADDR so several responders (ours or upstream's) can bind UDP 7778;
// on Linux that is enough for UDP sockets that all set it, which is what Node does. They
// don't all hear every probe: a broadcast probe reaches every one of them, but a unicast
// probe (to the machine's own address, or a Tailscale /32) reaches only the socket bound
// last. Clients probe the broadcast addresses, so each hub is still found.
func reuseAddr(_, _ string, c syscall.RawConn) error {
	var serr error
	err := c.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
	})
	if err != nil {
		return err
	}
	return serr
}
