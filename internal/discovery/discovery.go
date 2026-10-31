// Package discovery finds hubs on the LAN with a UDP broadcast probe, answers that probe on
// a hub, and sends Wake-on-LAN magic packets. The probe and reply are wire-compatible with
// upstream device-monitor, so either app finds the other's hubs.
//
// Ported from Forward-Future/device-monitor discovery.js (respond, broadcastAddresses,
// discover, wake) and security.js (isPrivateAddress) at c3a4c41.
// MIT License, Copyright (c) 2026 Matt: https://github.com/Forward-Future/device-monitor
package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net"
	"net/netip"
	"regexp"
	"time"
)

const (
	// Port is the UDP port hubs listen on for probes.
	Port = 7778
	// Probe is the exact datagram a client broadcasts.
	Probe = "device-monitor:discover"
	// App is the "app" value in every reply; clients ignore replies without it.
	App = "device-monitor"
	// Impl marks replies from this implementation (additive; upstream ignores it).
	Impl = "machine-monitor"

	maxReply = 512 // upstream drops larger replies
)

// Reply is the JSON a hub sends back to a probe: {"app","name","port"} as upstream, plus "impl".
type Reply struct {
	App  string `json:"app"`
	Name string `json:"name"`
	Port int    `json:"port"`
	Impl string `json:"impl,omitempty"`
}

// Hub is one hub that answered a probe.
type Hub struct {
	Name string // sanitized, at most 64 chars; the sender's IP when nothing is left
	URL  string // http://<sender IP>:<port>
	Impl string // "machine-monitor", or "" for upstream hubs
	// Virtual is true for 172.16-31.x senders (WSL/Hyper-V adapters); a real LAN address
	// for the same name replaces it.
	Virtual bool
}

// Responder answers probes until closed.
type Responder struct {
	conn *net.UDPConn
	done chan struct{}
}

// Respond listens on addr (":7778" in production) and answers each exact probe from a
// private or loopback IPv4 sender with a Reply naming this hub and its HTTP port. name is
// cleaned the way clients clean it, which also keeps the reply under upstream's 512 bytes.
// The port is bound with address reuse, as upstream, so an upstream hub or a second
// instance can share it.
func Respond(addr, name string, hubPort int) (*Responder, error) {
	pc, err := (&net.ListenConfig{Control: reuseAddr}).ListenPacket(context.Background(), "udp4", addr)
	if err != nil {
		return nil, err
	}
	conn := pc.(*net.UDPConn)
	reply, _ := json.Marshal(Reply{App: App, Name: cleanName(name), Port: hubPort, Impl: Impl})
	r := &Responder{conn: conn, done: make(chan struct{})}
	go func() {
		defer close(r.done)
		buf := make([]byte, 1500)
		for {
			n, from, err := conn.ReadFromUDPAddrPort(buf)
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					return
				}
				continue // e.g. Windows ICMP port-unreachable resets; keep serving
			}
			if string(buf[:n]) != Probe || !isPrivate(from.Addr()) {
				continue // only answer LAN peers
			}
			conn.WriteToUDPAddrPort(reply, from)
		}
	}()
	return r, nil
}

// Addr is the local address the responder is bound to.
func (r *Responder) Addr() net.Addr { return r.conn.LocalAddr() }

// Close stops the responder and waits for its goroutine to exit.
func (r *Responder) Close() error {
	err := r.conn.Close()
	<-r.done
	return err
}

// Discover broadcasts a probe on every local IPv4 network and returns the hubs that
// answered within timeout (upstream uses 1.5 s).
func Discover(timeout time.Duration) ([]Hub, error) {
	return probe(timeout, targets(broadcastAddresses(), Port))
}

// probe sends Probe to each target, then collects valid replies until timeout.
func probe(timeout time.Duration, to []netip.AddrPort) ([]Hub, error) {
	conn, err := net.ListenUDP("udp4", nil) // Go enables SO_BROADCAST on UDP sockets
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	for _, t := range to {
		conn.WriteToUDPAddrPort([]byte(Probe), t) // fail soft: an unreachable network just stays silent
	}
	conn.SetReadDeadline(time.Now().Add(timeout))

	found := map[string]Hub{}
	var order []string
	buf := make([]byte, 2048)
	for {
		n, from, err := conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				break
			}
			if errors.Is(err, net.ErrClosed) {
				break
			}
			continue
		}
		h, ok := parseReply(buf[:n], from.Addr().Unmap())
		if !ok {
			continue
		}
		prev, seen := found[h.Name]
		if !seen {
			order = append(order, h.Name)
		}
		if !seen || (prev.Virtual && !h.Virtual) {
			found[h.Name] = h
		}
	}
	hubs := make([]Hub, 0, len(order))
	for _, name := range order {
		hubs = append(hubs, found[name])
	}
	return hubs, nil
}

// nameJunk matches what upstream strips from names: anything but [A-Za-z0-9_ .()'-].
var nameJunk = regexp.MustCompile(`[^A-Za-z0-9_ .()'-]`)

// cleanName strips nameJunk and caps the result at 64 characters, as upstream clients do.
func cleanName(name string) string {
	name = nameJunk.ReplaceAllString(name, "")
	if len(name) > 64 {
		name = name[:64] // ASCII only after the strip, so bytes are chars
	}
	return name
}

// parseReply validates one reply. Replies come from anyone on the LAN: accept only
// app "device-monitor", an integer port 1-65535 and a string name, which is sanitized.
func parseReply(b []byte, from netip.Addr) (Hub, bool) {
	if len(b) > maxReply {
		return Hub{}, false
	}
	var r map[string]any
	if json.Unmarshal(b, &r) != nil {
		return Hub{}, false
	}
	port, ok := r["port"].(float64)
	if !ok || port != math.Trunc(port) || port < 1 || port > 65535 {
		return Hub{}, false
	}
	name, ok := r["name"].(string)
	if !ok || r["app"] != App {
		return Hub{}, false
	}
	name = cleanName(name)
	if name == "" {
		name = from.String()
	}
	impl, _ := r["impl"].(string)
	if impl != Impl {
		impl = "" // only our own marker is meaningful
	}
	return Hub{
		Name:    name,
		URL:     "http://" + netip.AddrPortFrom(from, uint16(port)).String(),
		Impl:    impl,
		Virtual: isVirtual(from),
	}, true
}

// isPrivate reports whether a is an IPv4 LAN, loopback, link-local or Tailscale CGNAT address.
func isPrivate(a netip.Addr) bool {
	a = a.Unmap()
	if !a.Is4() {
		return false
	}
	b := a.As4()
	return b[0] == 10 || b[0] == 127 ||
		(b[0] == 172 && b[1] >= 16 && b[1] <= 31) ||
		(b[0] == 192 && b[1] == 168) ||
		(b[0] == 169 && b[1] == 254) ||
		(b[0] == 100 && b[1] >= 64 && b[1] <= 127)
}

// isVirtual reports a 172.16.0.0/12 address, which upstream treats as a WSL/Hyper-V adapter.
func isVirtual(a netip.Addr) bool {
	if !a.Is4() {
		return false
	}
	b := a.As4()
	return b[0] == 172 && b[1] >= 16 && b[1] <= 31
}

// broadcastAddresses is 255.255.255.255 plus the directed broadcast of every IPv4 address
// on an interface that is up and not loopback, deduplicated.
func broadcastAddresses() []netip.Addr {
	out := []netip.Addr{netip.AddrFrom4([4]byte{255, 255, 255, 255})}
	seen := map[netip.Addr]bool{out[0]: true}
	ifaces, _ := net.Interfaces()
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagLoopback != 0 || ifi.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, _ := ifi.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			if b, ok := broadcastOf(ipn); ok && !seen[b] {
				seen[b] = true
				out = append(out, b)
			}
		}
	}
	return out
}

// broadcastOf returns ip | ^mask for an IPv4 network, or false for anything else.
func broadcastOf(n *net.IPNet) (netip.Addr, bool) {
	ip, mask := n.IP.To4(), n.Mask
	if ip == nil {
		return netip.Addr{}, false
	}
	if len(mask) == net.IPv6len {
		mask = mask[12:]
	}
	if len(mask) != net.IPv4len {
		return netip.Addr{}, false
	}
	var b [4]byte
	for i := range b {
		b[i] = ip[i] | ^mask[i]
	}
	return netip.AddrFrom4(b), true
}

// targets pairs each address with port.
func targets(addrs []netip.Addr, port uint16) []netip.AddrPort {
	out := make([]netip.AddrPort, len(addrs))
	for i, a := range addrs {
		out[i] = netip.AddrPortFrom(a, port)
	}
	return out
}
