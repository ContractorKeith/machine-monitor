package discovery

import "syscall"

// reuseAddr sets SO_REUSEADDR so several responders (ours or upstream's) can bind UDP 7778,
// as Node's reuseAddr does on Windows.
func reuseAddr(_, _ string, c syscall.RawConn) error {
	var serr error
	err := c.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
	})
	if err != nil {
		return err
	}
	return serr
}
