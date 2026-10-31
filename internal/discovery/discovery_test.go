package discovery

import (
	"bytes"
	"encoding/json"
	"net"
	"net/netip"
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
	if got, want := string(buf[:n]), `{"app":"device-monitor","name":"hub","port":7777,"impl":"machine-monitor"}`; got != want {
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
