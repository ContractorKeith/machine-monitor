package hub

import "net"

const streamSendBuffer = 64 << 10

func boundSendQueue(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetWriteBuffer(streamSendBuffer)
	}
}
