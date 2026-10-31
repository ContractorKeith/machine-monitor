package discovery

import "syscall"

// reuseAddr sets SO_REUSEADDR and SO_REUSEPORT: BSD sockets need both before a second
// socket may bind the same UDP port (Node sets both for reuseAddr on macOS).
func reuseAddr(_, _ string, c syscall.RawConn) error {
	var serr error
	err := c.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
		if serr == nil {
			serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEPORT, 1)
		}
	})
	if err != nil {
		return err
	}
	return serr
}
