package discovery

import (
	"bytes"
	"encoding/json"
	"net"
	"net/netip"
	"regexp"
	"strings"
	"testing"
	"time"
)

// startResponder runs a responder on an ephemeral loopback port.
func startResponder(t *testing.T, name string, hubPort int) netip.AddrPort {
	t.Helper()
	r, err := Respond("127.0.0.1:0", name, hubPort)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r.Addr().(*net.UDPAddr).AddrPort()
}

func TestLoopbackProbeReply(t *testing.T) {
	addr := startResponder(t, "Studio PC", 7777)
	hubs, err := probe(500*time.Millisecond, []netip.AddrPort{addr})
	if err != nil {
		t.Fatal(err)
	}
	want := []Hub{{Name: "Studio PC", URL: "http://127.0.0.1:7777", Impl: Impl}}
	if len(hubs) != 1 || hubs[0] != want[0] {
		t.Fatalf("got %+v, want %+v", hubs, want)
	}
}

// An upstream client sends the raw probe string and parses the reply with JSON.parse,
// requiring app "device-monitor", an integer port and a string name.
func TestRawUpstreamProbe(t *testing.T) {
	addr := startResponder(t, "hub", 7777)
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.WriteToUDPAddrPort([]byte("device-monitor:discover"), addr)
	conn.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 1024)
	n, _, err := conn.ReadFromUDPAddrPort(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(buf[:n]), `^\{"app":"device-monitor","name":"hub","port":7777,"impl":"machine-monitor","id":"[0-9a-f]{16}"\}$`; !regexp.MustCompile(want).MatchString(got) {
		t.Fatalf("reply %s, want %s", got, want)
	}
	if n > maxReply {
		t.Fatalf("reply is %d bytes; upstream drops over %d", n, maxReply)
	}
}

func TestResponderIgnoresOtherPayloads(t *testing.T) {
	addr := startResponder(t, "hub", 7777)
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, p := range []string{"device-monitor:discover\n", "DEVICE-MONITOR:DISCOVER", "hello", ""} {
		conn.WriteToUDPAddrPort([]byte(p), addr)
	}
	conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if n, _, err := conn.ReadFromUDPAddrPort(make([]byte, 1024)); err == nil {
		t.Fatalf("got a %d-byte reply to a non-probe", n)
	}
}

func TestParseReply(t *testing.T) {
	lan := netip.MustParseAddr("192.168.1.20")
	cases := []struct {
		in   string
		ok   bool
		want Hub
	}{
		{`{"app":"device-monitor","name":"Office","port":7777}`, true, Hub{Name: "Office", URL: "http://192.168.1.20:7777"}},
		{`{"app":"device-monitor","name":"Office","port":7777,"impl":"machine-monitor"}`, true, Hub{Name: "Office", URL: "http://192.168.1.20:7777", Impl: Impl}},
		{`{"app":"device-monitor","name":"Office","port":7777,"impl":"<script>"}`, true, Hub{Name: "Office", URL: "http://192.168.1.20:7777"}},
		{`{"app":"device-monitor","name":"Office","port":7777.0}`, true, Hub{Name: "Office", URL: "http://192.168.1.20:7777"}},
		{`{"app":"device-monitor","name":"<b>Kev's PC</b>!","port":80}`, true, Hub{Name: "bKev's PCb", URL: "http://192.168.1.20:80"}},
		{`{"app":"device-monitor","name":"***","port":80}`, true, Hub{Name: "192.168.1.20", URL: "http://192.168.1.20:80"}},
		{`{"app":"device-monitor","name":"` + strings.Repeat("a", 100) + `","port":80}`, true, Hub{Name: strings.Repeat("a", 64), URL: "http://192.168.1.20:80"}},
		{`{"app":"other","name":"x","port":80}`, false, Hub{}},
		{`{"name":"x","port":80}`, false, Hub{}},
		{`{"app":"device-monitor","name":5,"port":80}`, false, Hub{}},
		{`{"app":"device-monitor","name":"x","port":"80"}`, false, Hub{}},
		{`{"app":"device-monitor","name":"x","port":80.5}`, false, Hub{}},
		{`{"app":"device-monitor","name":"x","port":0}`, false, Hub{}},
		{`{"app":"device-monitor","name":"x","port":65536}`, false, Hub{}},
		{`not json`, false, Hub{}},
		{`[]`, false, Hub{}},
		{`{"app":"device-monitor","name":"` + strings.Repeat("a", 500) + `","port":80}`, false, Hub{}},
	}
	for _, c := range cases {
		got, ok := parseReply([]byte(c.in), lan)
		if ok != c.ok || got != c.want {
			t.Errorf("parseReply(%.60s) = %+v, %v; want %+v, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestVirtualAdapterLoses(t *testing.T) {
	// Same hub name from a WSL address and a real LAN address: keep the LAN one.
	wsl, _ := parseReply([]byte(`{"app":"device-monitor","name":"pc","port":7777}`), netip.MustParseAddr("172.20.0.1"))
	if !wsl.Virtual {
		t.Fatal("172.20.x should be virtual")
	}
	lan, _ := parseReply([]byte(`{"app":"device-monitor","name":"pc","port":7777}`), netip.MustParseAddr("192.168.0.5"))
	if lan.Virtual {
		t.Fatal("192.168.x should not be virtual")
	}
}

func TestIsPrivate(t *testing.T) {
	for ip, want := range map[string]bool{
		"10.1.2.3": true, "127.0.0.1": true, "172.16.0.1": true, "172.31.255.255": true,
		"172.32.0.1": false, "192.168.0.1": true, "169.254.1.1": true, "100.64.0.1": true,
		"100.127.0.1": true, "100.128.0.1": false, "8.8.8.8": false, "::1": false,
		"::ffff:192.168.1.1": true, "fe80::1": false,
	} {
		if got := isPrivate(netip.MustParseAddr(ip)); got != want {
			t.Errorf("isPrivate(%s) = %v, want %v", ip, got, want)
		}
	}
}

func TestBroadcastOf(t *testing.T) {
	for cidr, want := range map[string]string{
		"192.168.1.20/24": "192.168.1.255",
		"10.0.5.9/8":      "10.255.255.255",
		"172.20.3.4/20":   "172.20.15.255",
		"100.100.1.1/32":  "100.100.1.1",
	} {
		ip, n, _ := net.ParseCIDR(cidr)
		n.IP = ip
		got, ok := broadcastOf(n)
		if !ok || got.String() != want {
			t.Errorf("broadcastOf(%s) = %v, %v; want %s", cidr, got, ok, want)
		}
	}
	_, v6, _ := net.ParseCIDR("fd00::1/64")
	if _, ok := broadcastOf(v6); ok {
		t.Error("IPv6 network should have no broadcast")
	}
	if got := broadcastAddresses(); len(got) == 0 || got[0].String() != "255.255.255.255" {
		t.Errorf("broadcastAddresses() = %v, want 255.255.255.255 first", got)
	}
}

func TestMagicPacket(t *testing.T) {
	p, err := MagicPacket("aa:bb:cc:dd:ee:0f")
	if err != nil {
		t.Fatal(err)
	}
	mac := []byte{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0x0f}
	want := append(bytes.Repeat([]byte{0xff}, 6), bytes.Repeat(mac, 16)...)
	if len(p) != 102 || !bytes.Equal(p, want) {
		t.Fatalf("packet % x", p)
	}
	if q, err := MagicPacket("AA-BB-CC-DD-EE-0F"); err != nil || !bytes.Equal(q, p) {
		t.Errorf("dash form: %v", err)
	}
	for _, bad := range []string{"", "aa:bb:cc", "zz:bb:cc:dd:ee:ff", "00:00:5e:10:00:00:00:01"} {
		if _, err := MagicPacket(bad); err == nil {
			t.Errorf("MagicPacket(%q) should fail", bad)
		}
	}
}

func TestWakeSendsPackets(t *testing.T) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	port := uint16(conn.LocalAddr().(*net.UDPAddr).Port)
	macs := []string{"01:02:03:04:05:06", "aa:bb:cc:dd:ee:ff"}
	if err := wake(macs, []netip.Addr{netip.MustParseAddr("127.0.0.1")}, []uint16{port}); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 256)
	for _, m := range macs {
		n, _, err := conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			t.Fatal(err)
		}
		want, _ := MagicPacket(m)
		if !bytes.Equal(buf[:n], want) {
			t.Errorf("got % x, want packet for %s", buf[:n], m)
		}
	}
	if err := wake([]string{"01:02:03:04:05:06", "bogus"}, nil, nil); err == nil {
		t.Error("an invalid MAC should fail before sending")
	}
}

func TestReplyShape(t *testing.T) {
	b, _ := json.Marshal(Reply{App: App, Name: "n", Port: 1})
	if string(b) != `{"app":"device-monitor","name":"n","port":1}` {
		t.Errorf("reply without impl: %s", b)
	}
}

// Two responders (ours twice here; ours and upstream's in the field) share one UDP port.
func TestRespondersShareAPort(t *testing.T) {
	first := startResponder(t, "one", 7777)
	second, err := Respond(first.String(), "two", 7777)
	if err != nil {
		t.Fatalf("second bind of %s: %v", first, err)
	}
	defer second.Close()
	if got := second.Addr().(*net.UDPAddr).AddrPort(); got != first {
		t.Fatalf("second bound %s, want %s", got, first)
	}
	// The OS picks which socket gets a unicast probe; either way someone answers.
	hubs, err := probe(500*time.Millisecond, []netip.AddrPort{first})
	if err != nil || len(hubs) == 0 {
		t.Fatalf("no reply on the shared port: %v %v", hubs, err)
	}
}

// A long or junk-filled hostname is cleaned before it goes out, so the reply stays under
// upstream's 512-byte limit and carries the name clients would show anyway.
func TestReplyNameCleaned(t *testing.T) {
	addr := startResponder(t, strings.Repeat("<é\"\\", 200)+strings.Repeat("x", 600), 7777)
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.WriteToUDPAddrPort([]byte(Probe), addr)
	conn.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 4096)
	n, _, err := conn.ReadFromUDPAddrPort(buf)
	if err != nil {
		t.Fatal(err)
	}
	if n > maxReply {
		t.Fatalf("reply is %d bytes, over %d", n, maxReply)
	}
	var r Reply
	if err := json.Unmarshal(buf[:n], &r); err != nil || r.Name != strings.Repeat("x", 64) {
		t.Fatalf("reply %s: %v", buf[:n], err)
	}
}

// One source gets at most replyPerSource replies a window, so a spoofed source can't turn
// the responder into a reflector.
func TestResponderRateLimited(t *testing.T) {
	addr := startResponder(t, "hub", 7777)
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for range 3 * replyPerSource {
		conn.WriteToUDPAddrPort([]byte(Probe), addr)
	}
	conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	buf := make([]byte, 1024)
	n := 0
	for {
		if _, _, err := conn.ReadFromUDPAddrPort(buf); err != nil {
			break
		}
		n++
	}
	if n == 0 || n > replyPerSource {
		t.Fatalf("%d replies to %d probes; want 1-%d", n, 3*replyPerSource, replyPerSource)
	}
}

func TestReplyLimiter(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newReplyLimiter(func() time.Time { return now })
	src := func(i int) netip.Addr { return netip.AddrFrom4([4]byte{10, 0, byte(i >> 8), byte(i)}) }
	for i := range replyPerSource {
		if !l.allow(src(0)) {
			t.Fatalf("reply %d to one source refused", i+1)
		}
	}
	if l.allow(src(0)) {
		t.Fatal("one source over its limit")
	}
	// New sources in one /24 can use their fallback after the shared budget runs out.
	n := 0
	for i := 1; i < 256; i++ {
		if l.allow(src(i)) {
			n++
		}
	}
	if n != 255 {
		t.Fatalf("%d replies to 255 new sources in one /24; want %d", n, 255)
	}
	if !l.allow(src(256)) { // 10.0.1.0: another /24
		t.Fatal("another network refused")
	}
	now = now.Add(time.Second)
	if !l.allow(src(0)) {
		t.Fatal("not refilled after a second")
	}
	// IPv6 sources are grouped by /64.
	a, b, c := netip.MustParseAddr("fd00::1"), netip.MustParseAddr("fd00::ffff:2"), netip.MustParseAddr("fd00:0:0:1::1")
	if network(a) != network(b) || network(a) == network(c) || network(netip.MustParseAddr("192.168.1.9")).String() != "192.168.1.0/24" {
		t.Fatalf("networks: %v %v %v", network(a), network(b), network(c))
	}
}

// floodRun probes l every step for 20 s from flood(k), the k-th flood probe, plus a
// legitimate prober (127.0.0.1) sending three probes at 8, 11 and 14 s, as `connect` run
// three times in a row. It fails if any second has more than replyTotal replies, and
// returns the flood's replies and how many legitimate probes were answered of how many.
func floodRun(t *testing.T, step time.Duration, flood func(k int) netip.Addr) (replies, answered, probes int) {
	t.Helper()
	now := time.Unix(1000, 0)
	l := newReplyLimiter(func() time.Time { return now })
	legit := netip.MustParseAddr("127.0.0.1")
	start, second, perSecond := now, now, 0
	for k := 0; now.Sub(start) < 20*time.Second; k++ {
		now = now.Add(step)
		if now.Sub(second) >= time.Second {
			if perSecond > replyTotal {
				t.Fatalf("%d replies in one second; want at most %d", perSecond, replyTotal)
			}
			second, perSecond = now, 0
		}
		if l.allow(flood(k)) {
			replies++
			perSecond++
		}
		if at := now.Sub(start); at == 8*time.Second || at == 11*time.Second || at == 14*time.Second {
			for range 3 {
				probes++
				if l.allow(legit) {
					answered++
				}
			}
		}
	}
	return replies, answered, probes
}

// TestReplyFloodLeavesRoom is skeptic report N2: ten sources probing 50 times a second each
// (500 pps, unprivileged on one machine) used up the global limit, so every legitimate
// probe went unanswered. The flood must stay within its sources' and network's limits, and
// a prober on the same network must still be answered.
func TestReplyFloodLeavesRoom(t *testing.T) {
	replies, answered, probes := floodRun(t, 2*time.Millisecond, func(k int) netip.Addr {
		return netip.AddrFrom4([4]byte{127, 0, 0, byte(2 + k%10)})
	})
	if answered != probes {
		t.Errorf("the legitimate prober got %d replies to %d probes during the flood; want all", answered, probes)
	}
	if max := 20*10*replySourceRate + 10*replyPerSource; float64(replies) > max {
		t.Errorf("the flood got %d replies in 20 s; want at most %v", replies, max)
	}
	if replies == 0 {
		t.Error("the flood's sources got no replies at all; each should keep a small share")
	}
}

// TestReplyRotatingFlood (review of #70): a flood from a new address each probe (400 pps)
// drained the budget kept for new sources, so a legitimate prober got 0 of 8 replies. Each
// /24 now has its own budget: the flood gets at most that per network it uses, and the
// prober, on a network the flood isn't using at the time, is answered.
func TestReplyRotatingFlood(t *testing.T) {
	for name, flood := range map[string]func(k int) netip.Addr{
		// 127.0.0.2, 127.0.0.3, …: a new address each probe, a new /24 every 256.
		"sequential": func(k int) netip.Addr {
			i := k + 2
			return netip.AddrFrom4([4]byte{127, byte(i >> 16), byte(i >> 8), byte(i)})
		},
		// A new /24 each probe, spread over 127.0.0.0/8.
		"scattered": func(k int) netip.Addr {
			i := uint32(k)*2654435761 | 1<<8 // never 127.0.0.x, the prober's network
			return netip.AddrFrom4([4]byte{127, byte(i >> 16), byte(i >> 8), byte(i)})
		},
	} {
		t.Run(name, func(t *testing.T) {
			replies, answered, probes := floodRun(t, 2500*time.Microsecond, flood)
			if answered != probes {
				t.Errorf("the legitimate prober got %d replies to %d probes during the flood; want all", answered, probes)
			}
			t.Logf("the flood got %d replies to 8000 probes", replies)
		})
	}
}

// One hub answering from its LAN and Tailscale addresses is one entry with the LAN URL,
// whichever reply came first. Two hubs on one machine (sharing UDP 7778, different HTTP
// ports) are two entries, and so are two of our hubs that share a hostname.
func TestHubSetMerge(t *testing.T) {
	var s hubSet
	ours := `{"app":"device-monitor","name":"pc","port":7777,"impl":"machine-monitor","id":"aaaa"}`
	s.add([]byte(ours), netip.MustParseAddr("100.127.35.0"))
	s.add([]byte(ours), netip.MustParseAddr("192.168.0.105"))
	s.add([]byte(ours), netip.MustParseAddr("127.0.0.1"))
	s.add([]byte(`{"app":"device-monitor","name":"pc","port":7777,"impl":"machine-monitor","id":"bbbb"}`), netip.MustParseAddr("192.168.0.106"))
	s.add([]byte(`{"app":"device-monitor","name":"pc","port":17801}`), netip.MustParseAddr("100.127.35.0"))
	s.add([]byte(`{"app":"device-monitor","name":"pc","port":17801}`), netip.MustParseAddr("192.168.0.105"))
	s.add([]byte(`{"app":"device-monitor","name":"pc","port":17802}`), netip.MustParseAddr("172.20.0.1"))
	s.add([]byte(`{"app":"device-monitor","name":"pc","port":17802}`), netip.MustParseAddr("100.127.35.0"))
	got := []string{}
	for _, h := range s.list() {
		got = append(got, h.URL)
	}
	want := []string{"http://192.168.0.105:7777", "http://192.168.0.106:7777", "http://192.168.0.105:17801", "http://100.127.35.0:17802"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// Two of our responders on one port at different HTTP ports are both found by a probe that
// reaches both (a broadcast in the field; here one probe to each).
func TestTwoHubsOnOneMachine(t *testing.T) {
	a := startResponder(t, "pc", 17800)
	b := startResponder(t, "pc", 17801)
	hubs, err := probe(500*time.Millisecond, []netip.AddrPort{a, b, a})
	if err != nil || len(hubs) != 2 {
		t.Fatalf("got %+v, %v; want both hubs", hubs, err)
	}
}

func TestReplySameNetworkFlood(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newReplyLimiter(func() time.Time { return now })
	for tick := 0; tick < 10000; tick++ {
		now = now.Add(2 * time.Millisecond)
		l.allow(netip.AddrFrom4([4]byte{127, 0, 0, byte(2 + tick%13)}))
		if tick == 4000 || tick == 7000 {
			if !l.allow(netip.MustParseAddr("127.0.0.1")) {
				t.Fatal("same-/24 prober starved")
			}
		}
	}
	// Successful replies alone allocate entries, bounded by the total rate and idle window.
	for i := 0; i < 100000; i++ {
		l.allow(netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)}))
	}
	if len(l.per) > replyTotal*31 || len(l.nets) > replyTotal*31 {
		t.Fatal("unbounded limiter state")
	}
}

func TestReplyLimiterHardCap(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newReplyLimiter(func() time.Time { return now })
	for i := 0; i < replyMaxTracked; i++ {
		a := netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)})
		l.per[a] = &bucket{seen: now}
	}
	if l.allow(netip.MustParseAddr("192.168.1.1")) || len(l.per) != replyMaxTracked {
		t.Fatal("source cap exceeded")
	}
	now = now.Add(replyIdle)
	if !l.allow(netip.MustParseAddr("192.168.1.1")) {
		t.Fatal("idle entries did not expire")
	}
}
