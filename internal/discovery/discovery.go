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
	Port = 7778

	Probe = "device-monitor:discover"

	App = "device-monitor"

	Impl = "machine-monitor"

	maxReply = 512

	replyPerSource  = 5
	replySourceRate = 2.0
	replyPerNetwork = 25
	replyTotal      = 500
	replyMaxTracked = replyTotal * 60
	replyIdle       = 30 * time.Second
	replyQuiet      = 2 * time.Second
)

type Reply struct {
	App  string `json:"app"`
	Name string `json:"name"`
	Port int    `json:"port"`
	Impl string `json:"impl,omitempty"`

	ID string `json:"id,omitempty"`
}

type Hub struct {
	Name string
	URL  string
	Impl string

	Virtual bool
}

type Responder struct {
	conn *net.UDPConn
	done chan struct{}
}

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
				continue
			}
			if string(buf[:n]) != Probe || !isPrivate(from.Addr()) {
				continue
			}
			if !limit.allow(from.Addr()) {
				continue
			}
			conn.WriteToUDPAddrPort(reply, from)
		}
	}()
	return r, nil
}

type replyLimiter struct {
	now    func() time.Time
	mu     sync.Mutex
	total  bucket
	pruned time.Time
	per    map[netip.Addr]*bucket
	nets   map[netip.Prefix]*bucket
}

type bucket struct {
	tokens  float64
	at      time.Time
	seen    time.Time
	reserve bucketReserve
}

type bucketReserve struct {
	tokens float64
	at     time.Time
}

func (b *bucket) take(now time.Time, rate, burst float64, spend bool) bool {
	if d := now.Sub(b.at).Seconds(); d > 0 {
		b.tokens = min(burst, b.tokens+d*rate)
	}
	b.at, b.seen = now, now
	if b.tokens < 1 {
		return false
	}
	if spend {
		b.tokens--
	}
	return true
}

func newReplyLimiter(now func() time.Time) *replyLimiter {
	t := now()
	return &replyLimiter{now: now, pruned: t, total: bucket{tokens: replyTotal, at: t},
		per: map[netip.Addr]*bucket{}, nets: map[netip.Prefix]*bucket{}}
}

func network(from netip.Addr) netip.Prefix {
	from = from.Unmap()
	bits := 64
	if from.Is4() {
		bits = 24
	}
	p, _ := from.Prefix(bits)
	return p
}

func lookup[K comparable](m map[K]*bucket, k K, now time.Time, burst, reserve float64) *bucket {
	if b := m[k]; b != nil && now.Sub(b.seen) < replyIdle {
		return b
	}
	return &bucket{tokens: burst, at: now, reserve: bucketReserve{tokens: reserve, at: now}}
}

func (l *replyLimiter) allow(from netip.Addr) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if now.Sub(l.pruned) >= replyIdle {
		l.pruned = now
		for k, b := range l.per {
			if now.Sub(b.seen) >= replyIdle {
				delete(l.per, k)
			}
		}
		for k, b := range l.nets {
			if now.Sub(b.seen) >= replyIdle {
				delete(l.nets, k)
			}
		}
	}
	nk := network(from)
	if l.per[from] == nil && len(l.per) >= replyMaxTracked ||
		l.nets[nk] == nil && len(l.nets) >= replyMaxTracked {
		return false
	}
	src := lookup(l.per, from, now, replyPerSource, 1)
	nb := lookup(l.nets, nk, now, replyPerNetwork, replyPerNetwork)

	src.reserve.tokens = min(1, src.reserve.tokens+max(0, now.Sub(src.reserve.at).Seconds())/5)
	src.reserve.at = now
	nb.reserve.tokens = min(replyPerNetwork, nb.reserve.tokens+max(0, now.Sub(nb.reserve.at).Seconds())*replyPerNetwork)
	nb.reserve.at = now
	quiet := now.Sub(src.seen) >= replyQuiet
	sourceOK := src.take(now, replySourceRate, replyPerSource, false)
	networkOK := nb.take(now, replyPerNetwork, replyPerNetwork, false)
	ok := sourceOK && (networkOK || quiet && src.reserve.tokens >= 1 && nb.reserve.tokens >= 1) &&
		l.total.take(now, replyTotal, replyTotal, false)
	if _, tracked := l.per[from]; ok || tracked {
		l.per[from] = src
	}
	if !ok {
		return false
	}
	src.tokens--
	if networkOK {
		nb.tokens--
	} else {
		src.reserve.tokens--
		nb.reserve.tokens--
	}
	l.total.tokens--
	l.nets[nk] = nb
	return true
}

func (r *Responder) Addr() net.Addr { return r.conn.LocalAddr() }

func (r *Responder) Close() error {
	err := r.conn.Close()
	<-r.done
	return err
}

func Discover(timeout time.Duration) ([]Hub, error) {
	return probe(timeout, targets(broadcastAddresses(), Port))
}

func probe(timeout time.Duration, to []netip.AddrPort) ([]Hub, error) {
	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	for _, t := range to {
		conn.WriteToUDPAddrPort([]byte(Probe), t)
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

type hubSet struct {
	order []string
	byKey map[string]Hub
}

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

var nameJunk = regexp.MustCompile(`[^A-Za-z0-9_ .()'-]`)

func cleanName(name string) string {
	name = nameJunk.ReplaceAllString(name, "")
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}

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
		impl = ""
	}
	return Hub{
		Name:    name,
		URL:     "http://" + netip.AddrPortFrom(from, uint16(port)).String(),
		Impl:    impl,
		Virtual: isVirtual(from),
	}, true
}

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

func isVirtual(a netip.Addr) bool {
	if !a.Is4() {
		return false
	}
	b := a.As4()
	return b[0] == 172 && b[1] >= 16 && b[1] <= 31
}

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

func targets(addrs []netip.Addr, port uint16) []netip.AddrPort {
	out := make([]netip.AddrPort, len(addrs))
	for i, a := range addrs {
		out[i] = netip.AddrPortFrom(a, port)
	}
	return out
}
