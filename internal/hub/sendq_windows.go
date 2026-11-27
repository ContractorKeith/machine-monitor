package hub

import "net"

const streamSendBuffer = 64 << 10

func boundSendQueue(c net.Conn) error {
	if tc, ok := c.(*net.TCPConn); ok {
		return tc.SetWriteBuffer(streamSendBuffer)
	}
	return nil
}
