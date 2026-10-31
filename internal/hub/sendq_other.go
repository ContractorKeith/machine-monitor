//go:build !linux && !darwin && !windows

package hub

import "net"

// boundSendQueue is a no-op here: the stream's send queue is the system's default.
func boundSendQueue(net.Conn) {}
