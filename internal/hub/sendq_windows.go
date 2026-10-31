package hub

import "net"

// streamSendBuffer is a compact stream's send buffer: about 3 s at 20 KB/s.
const streamSendBuffer = 64 << 10

// boundSendQueue caps a compact stream's kernel send buffer at streamSendBuffer, so a write
// returns only once the link has carried most of what came before. Windows has no
// TCP_NOTSENT_LOWAT, so this caps bytes in flight too: enough for the compact stream of
// MaxDevices over any link with a round trip under a second or so. Best effort.
func boundSendQueue(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetWriteBuffer(streamSendBuffer)
	}
}
