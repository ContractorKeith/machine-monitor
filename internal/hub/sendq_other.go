//go:build !linux && !darwin && !windows

package hub

import "net"

func boundSendQueue(net.Conn) error { return nil }
