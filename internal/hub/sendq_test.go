//go:build linux || darwin || windows

package hub

import (
	"net"
	"testing"
)

func TestBoundSendQueueReportsFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if err := boundSendQueue(c); err != nil {
		t.Fatalf("open conn: %v", err)
	}
	c.Close()
	if err := boundSendQueue(c); err == nil {
		t.Fatal("closed conn: want an error, got nil")
	}
}
