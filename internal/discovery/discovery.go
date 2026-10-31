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
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net"
	"net/netip"
	"regexp"
	"strings"
	"sync"
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

	// Reply limits, so the responder can't be used to reflect traffic at a spoofed source,
	// and a flood from a few sources can't hide the hub from everyone else (skeptic report
	// N2). Each source has a token bucket of replyPerSource replies that refills at
	// replySourceRate a second. Every reply also spends from one of two global buckets, each
	// refilled at replyGlobal/2 a second: one for sources first answered within replyNewFor,
	// one for the rest. A flood soon counts as established, so it can drain only that half,
	// and a client that has just started probing still gets an answer. A client's probe
	// reaches a hub a few times at most (255.255.255.255 plus a directed broadcast per
	// network).
	replyPerSource  = 5
	replySourceRate = 2.0 // replies a second, per source
	replyGlobal     = 50  // replies a second in all
	replyNewFor     = 5 * time.Second
	replyIdle       = 30 * time.Second // a source silent this long is forgotten (new again)
)

// Reply is the JSON a hub sends back to a probe: {"app","name","port"} as upstream, plus
// "impl" and "id" (additive; upstream ignores them).
type Reply struct {
	App  string `json:"app"`
	Name string `json:"name"`
	Port int    `json:"port"`
	Impl string `json:"impl,omitempty"`
	// ID is random per responder, so a client can tell one hub answering from several
	// addresses (LAN and Tailscale) from two machines that share a hostname.
	ID string `json:"id,omitempty"`
}

// Hub is one hub that answered a probe.
type Hub struct {
	Name string // sanitized, at most 64 chars; the sender's IP when nothing is left
	URL  string // http://<sender IP>:<port>
	Impl string // "machine-monitor", or "" for upstream hubs
	// Virtual is true for 172.16-31.x senders (WSL/Hyper-V adapters); a real LAN address
	// for the same hub replaces it.
	Virtual bool
}

// Responder answers probes until closed.
type Responder struct {
	conn *net.UDPConn
	done chan struct{}
}

// Respond listens on addr (":7778" in production) and answers each exact probe from a
// private or loopback IPv4 sender with a Reply naming this hub and its HTTP port, within the
// reply limits (replyPerSource, replyGlobal). name is cleaned the way clients clean it, which
// also keeps the reply under upstream's 512 bytes. The port is bound with address reuse, as
// upstream, so an upstream hub or a second instance can share it (see reuseAddr for what
// each one then receives).
func Respond(addr, name string, hubPort int) (*Responder, error) {
	pc, err := (&net.ListenConfig{Control: reuseAddr}).ListenPacket(context.Background(), "udp4", addr)
	if err != nil {
		return nil, err
	}
	conn := pc.(*net.UDPConn)
	id := make([]byte, 8)
	rand.Read(id)
	reply, _ := json.Marshal(Reply{App: App, Name: cleanName(name), Port: hubPort, Impl: Impl, ID: hex.EncodeToString(id)})
	r := &Responder{conn: conn, done: make(chan struct{})}
	limit := newReplyLimiter(time.Now)
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
			if !limit.allow(from.Addr()) {
				continue
			}
			conn.WriteToUDPAddrPort(reply, from)
		}
	}()
	return r, nil
}

// replyLimiter holds the reply token buckets: one per source, and the two global ones.
// Only sources that have been answered are tracked, so the map holds at most about
// replyGlobal*replyIdle entries however many addresses a flood uses.
type replyLimiter struct {
	now    func() time.Time
	mu     sync.Mutex
	at     time.Time // when the global buckets were last refilled
	fresh  float64   // global tokens for sources first answered within replyNewFor
	old    float64   // global tokens for every other source
	pruned time.Time
	per    map[netip.Addr]*replySource
}

// replySource is one source's bucket.
type replySource struct {
	first  time.Time // first reply
	seen   time.Time // last probe
	at     time.Time // last refill
	tokens float64
}

func newReplyLimiter(now func() time.Time) *replyLimiter {
	t := now()
	return &replyLimiter{now: now, at: t, pruned: t, fresh: replyGlobal / 2, old: replyGlobal / 2, per: map[netip.Addr]*replySource{}}
}

// refill adds the tokens earned since at, up to burst.
func refill(tokens *float64, at *time.Time, now time.Time, rate, burst float64) {
	if d := now.Sub(*at).Seconds(); d > 0 {
		*tokens = min(burst, *tokens+d*rate)
	}
	*at = now
}

// allow reports whether a reply to from is within the limits, and if so spends it.
func (l *replyLimiter) allow(from netip.Addr) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	at := l.at
	refill(&l.fresh, &at, now, replyGlobal/2, replyGlobal/2)
	refill(&l.old, &l.at, now, replyGlobal/2, replyGlobal/2)
	if now.Sub(l.pruned) >= replyIdle {
		l.pruned = now
		for a, s := range l.per {
			if now.Sub(s.seen) >= replyIdle {
				delete(l.per, a)
			}
		}
	}
	s := l.per[from]
	if s != nil && now.Sub(s.seen) >= replyIdle {
		s = nil // silent long enough to count as new
	}
	if s == nil {
		s = &replySource{first: now, at: now, tokens: replyPerSource}
	}
	s.seen = now
	refill(&s.tokens, &s.at, now, replySourceRate, replyPerSource)
	global := &l.old
	if now.Sub(s.first) < replyNewFor {
		global = &l.fresh
	}
	if s.tokens < 1 || *global < 1 {
		if _, tracked := l.per[from]; tracked {
			l.per[from] = s
		}
		return false
	}
	s.tokens--
	*global--
	l.per[from] = s
	return true
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

	var found hubSet
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
		found.add(buf[:n], from.Addr().Unmap())
	}
	return found.list(), nil
}

// hubSet merges replies into one entry per hub, in the order hubs first answered, keeping
// each hub's best address (rank).
type hubSet struct {
	order []string
	byKey map[string]Hub
}

// add takes one reply from from; invalid replies are ignored.
func (s *hubSet) add(b []byte, from netip.Addr) {
	h, ok := parseReply(b, from)
	if !ok {
		return
	}
	if s.byKey == nil {
		s.byKey = map[string]Hub{}
	}
	key := hubKey(b, h)
	prev, seen := s.byKey[key]
	if !seen {
		s.order = append(s.order, key)
	}
	if !seen || rank(h.URL) > rank(prev.URL) {
		s.byKey[key] = h
	}
}

func (s *hubSet) list() []Hub {
	hubs := make([]Hub, 0, len(s.order))
	for _, k := range s.order {
		hubs = append(hubs, s.byKey[k])
	}
	return hubs
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

// hubKey is which hub a valid reply b (parsed as h) is from. Our hubs send a random id. For
// upstream hubs it is the name and port: one machine answers from each of its addresses, and
// two hubs on one machine (sharing UDP 7778) differ by port.
func hubKey(b []byte, h Hub) string {
	var r struct {
		ID string `json:"id"`
	}
	json.Unmarshal(b, &r)
	if h.Impl == Impl && r.ID != "" && len(r.ID) <= 32 {
		return "id:" + r.ID
	}
	return "name:" + h.Name + ":" + h.URL[strings.LastIndex(h.URL, ":")+1:]
}

// rank orders the addresses one hub answered from, best first: a LAN address works for
// every device at home and keeps working when Tailscale is off; a Tailscale (100.64/10)
// address works from anywhere on the tailnet; loopback only on the hub's machine; a
// WSL/Hyper-V adapter (172.16/12) usually not at all.
func rank(hubURL string) int {
	ap, err := netip.ParseAddrPort(strings.TrimPrefix(hubURL, "http://"))
	if err != nil {
		return 0
	}
	a := ap.Addr()
	b := a.As4()
	switch {
	case isVirtual(a):
		return 0
	case a.IsLoopback():
		return 1
	case b[0] == 100 && b[1] >= 64 && b[1] <= 127:
		return 2
	default:
		return 3
	}
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
