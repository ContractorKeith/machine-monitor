package hub

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/config"
	"github.com/ContractorKeith/machine-monitor/internal/report"
)

// clock is a settable test clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type testHub struct {
	*Hub
	t     *testing.T
	clk   *clock
	paths config.Paths
	h     http.Handler
}

func newTestHub(t *testing.T) *testHub {
	t.Helper()
	return openTestHub(t, config.At(t.TempDir()), &clock{t: time.Date(2026, 10, 31, 12, 0, 30, 0, time.UTC)})
}

func openTestHub(t *testing.T, paths config.Paths, clk *clock) *testHub {
	t.Helper()
	h, err := New(Options{Paths: paths, Now: clk.Now, Hostname: "testbox", Location: time.UTC, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	return &testHub{Hub: h, t: t, clk: clk, paths: paths, h: h.Handler()}
}

// req sends a request from 127.0.0.1 to Host 127.0.0.1:7777. key "" sends no key.
func (th *testHub) req(method, path, key, body string, hdr ...string) *httptest.ResponseRecorder {
	th.t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:50000"
	r.Host = "127.0.0.1:7777"
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		if hdr[i] == "Host" {
			r.Host = hdr[i+1]
		} else if hdr[i] == "RemoteAddr" {
			r.RemoteAddr = hdr[i+1]
		} else {
			r.Header.Set(hdr[i], hdr[i+1])
		}
	}
	w := httptest.NewRecorder()
	th.h.ServeHTTP(w, r)
	return w
}

// keyed sends a request with the hub key.
func (th *testHub) keyed(method, path, body string) *httptest.ResponseRecorder {
	th.t.Helper()
	return th.req(method, path, th.Token(), body)
}

// agentReport is the recorded upstream agent.sh report with this hub's token and id.
func (th *testHub) agentReport(id string, edit func(m map[string]any)) string {
	th.t.Helper()
	b, err := os.ReadFile("../report/testdata/linux-agent.sh.json")
	if err != nil {
		th.t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		th.t.Fatal(err)
	}
	m["token"] = th.Token()
	m["id"] = id
	if edit != nil {
		edit(m)
	}
	out, _ := json.Marshal(m)
	return string(out)
}

func (th *testHub) postReport(id string, edit func(m map[string]any)) {
	th.t.Helper()
	if w := th.req("POST", "/api/report", "", th.agentReport(id, edit)); w.Code != 200 {
		th.t.Fatalf("report: %d %s", w.Code, w.Body)
	}
}

func decode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", w.Body, err)
	}
	return v
}

func (th *testHub) devices() []View {
	th.t.Helper()
	w := th.keyed("GET", "/api/devices", "")
	if w.Code != 200 {
		th.t.Fatalf("devices: %d %s", w.Code, w.Body)
	}
	return decode[[]View](th.t, w)
}

// ---- token and config --------------------------------------------------------------------

func TestTokenCreatedOnceAndPrivate(t *testing.T) {
	th := newTestHub(t)
	tok := th.Token()
	if len(tok) != 24 || strings.Trim(tok, "0123456789abcdef") != "" {
		t.Fatalf("token %q: want 24 hex chars", tok)
	}
	st, err := os.Stat(th.paths.Config)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Errorf("config.json mode %v, want 0600", st.Mode().Perm())
	}
	th.Close()
	again := openTestHub(t, th.paths, th.clk)
	if again.Token() != tok {
		t.Errorf("token changed across restarts: %q → %q", tok, again.Token())
	}
}

func TestConfigKeepsUnknownKeys(t *testing.T) {
	paths := config.At(t.TempDir())
	if err := config.WriteFile0600(paths.Config, []byte(`{"token":"abc","tempUnit":"F"}`)); err != nil {
		t.Fatal(err)
	}
	th := openTestHub(t, paths, &clock{t: time.Unix(1e9, 0)})
	if th.Token() != "abc" {
		t.Fatalf("token %q", th.Token())
	}
	th.keyed("PUT", "/api/energy/settings", `{"region":"US","rate":0.2,"currency":"USD"}`)
	b, _ := os.ReadFile(paths.Config)
	if !strings.Contains(string(b), `"tempUnit": "F"`) || !strings.Contains(string(b), `"energy"`) {
		t.Errorf("config.json = %s", b)
	}
}

func TestDamagedConfigIsAnError(t *testing.T) {
	paths := config.At(t.TempDir())
	config.WriteFile0600(paths.Config, []byte(`{"token":`))
	if _, err := New(Options{Paths: paths}); err == nil {
		t.Fatal("New with a damaged config.json: no error")
	}
}

// ---- report intake -----------------------------------------------------------------------

func TestReportAuthAndValidation(t *testing.T) {
	th := newTestHub(t)
	for _, tt := range []struct {
		name, body string
		code       int
		errText    string
	}{
		{"no token", `{"id":"abc"}`, 401, "bad token"},
		{"wrong token", `{"id":"abc","token":"nope"}`, 401, "bad token"},
		{"token not a string", `{"id":"abc","token":1}`, 401, "bad token"},
		{"array", `[1]`, 401, "bad token"},
		{"empty", ``, 401, "bad token"},
		{"bad json", `{"id":`, 400, "invalid JSON"},
		{"bad id", fmt.Sprintf(`{"id":"../x","token":%q}`, th.Token()), 400, "invalid id"},
		{"too large", fmt.Sprintf(`{"token":%q,"pad":"%s"}`, th.Token(), strings.Repeat("x", maxBody)), 400, "too large"},
	} {
		w := th.req("POST", "/api/report", "", tt.body)
		if w.Code != tt.code || !strings.Contains(w.Body.String(), tt.errText) {
			t.Errorf("%s: %d %s, want %d %q", tt.name, w.Code, w.Body, tt.code, tt.errText)
		}
	}
	if n := len(th.devices()); n != 0 {
		t.Errorf("%d devices after bad reports", n)
	}
}

func TestReportRegistersDevice(t *testing.T) {
	th := newTestHub(t)
	w := th.req("POST", "/api/report", "", th.agentReport("dev-1", func(m map[string]any) {
		// A small display card listed first: the big GPU should be sorted to the front.
		gpus := m["gpus"].([]any)
		m["gpus"] = append([]any{map[string]any{"name": "Tiny", "memTotal": 1 << 20}}, gpus...)
		m["processes"] = []any{map[string]any{"pid": 1, "name": "init"}}
	}))
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"ok":true}` {
		t.Fatalf("reply %d %s, want {\"ok\":true}", w.Code, w.Body)
	}
	devs := th.devices()
	if len(devs) != 1 {
		t.Fatalf("%d devices, want 1", len(devs))
	}
	d := devs[0]
	if d.ID != "dev-1" || d.Name != "atlas" || !d.Online || d.Platform != "linux" || d.Order != 0 {
		t.Errorf("device = %+v", d.Device)
	}
	if d.Latest == nil || d.Latest.PowerEst == nil || d.Latest.PowerEst.Watts <= 0 {
		t.Fatalf("latest/powerEst missing: %+v", d.Latest)
	}
	if d.Latest.Token != "" || d.Latest.Processes != nil {
		t.Errorf("token or processes stored with the report")
	}
	if n := *d.Latest.GPUs[0].Name; n == "Tiny" {
		t.Errorf("GPUs not sorted by memory: first is %q", n)
	}
	if d.Latest.ReceivedAt != th.clk.Now().UnixMilli() {
		t.Errorf("receivedAt %d", d.Latest.ReceivedAt)
	}
	if len(d.History) != 1 || d.History[0].Power == nil || d.History[0].Mem == nil {
		t.Errorf("history = %+v", d.History)
	}
	if d.HW == nil || d.HW.Threads == nil || *d.HW.Threads != 16 {
		t.Errorf("hw = %+v", d.HW)
	}

	th.clk.Add(time.Second)
	th.postReport("dev-2", nil)
	if devs := th.devices(); len(devs) != 2 || devs[1].ID != "dev-2" || devs[1].Order != 1 {
		t.Errorf("second device: %+v", devs)
	}
}

func TestOfflineAfterSilence(t *testing.T) {
	th := newTestHub(t)
	th.postReport("dev-1", nil)
	th.clk.Add(OfflineAfter - time.Millisecond)
	if !th.devices()[0].Online {
		t.Fatal("offline too early")
	}
	th.clk.Add(time.Millisecond)
	if th.devices()[0].Online {
		t.Fatal("still online after OfflineAfter")
	}
	th.postReport("dev-1", nil)
	if !th.devices()[0].Online {
		t.Fatal("not online after a new report")
	}
}

func TestMaxDevices(t *testing.T) {
	th := newTestHub(t)
	for i := range MaxDevices {
		th.postReport(fmt.Sprintf("d%d", i), nil)
	}
	w := th.req("POST", "/api/report", "", th.agentReport("one-more", nil))
	if w.Code != 400 || !strings.Contains(w.Body.String(), "too many devices") {
		t.Fatalf("device %d: %d %s", MaxDevices+1, w.Code, w.Body)
	}
	th.postReport("d0", nil) // known devices still report
}

func TestRegistryPersists(t *testing.T) {
	th := newTestHub(t)
	th.postReport("dev-1", nil)
	th.keyed("PATCH", "/api/devices/dev-1", `{"name":"Kitchen PC"}`)
	if err := th.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(th.paths.Devices)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Errorf("devices.json mode %v", st.Mode().Perm())
	}
	again := openTestHub(t, th.paths, th.clk)
	devs := again.devices()
	if len(devs) != 1 || devs[0].Name != "Kitchen PC" || !devs[0].Renamed || devs[0].Latest != nil {
		t.Fatalf("after restart: %+v", devs)
	}
	if len(devs[0].History) != 0 {
		t.Errorf("raw history should not survive a restart")
	}
}

type fakeCollector struct{ id string }

func (f fakeCollector) Collect(context.Context) (*report.Report, error) {
	return &report.Report{ID: f.id, Platform: report.PlatformLinux, Hostname: report.Ptr("hubhost"),
		CPU: report.CPU{Usage: report.Ptr(5.0)}}, nil
}

func TestHubMonitorsItself(t *testing.T) {
	clk := &clock{t: time.Now()}
	h, err := New(Options{Paths: config.At(t.TempDir()), Now: clk.Now, Hostname: "testbox",
		Collector: fakeCollector{"hub-self"}, Interval: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		h.mu.Lock()
		d := h.registry["hub-self"]
		h.mu.Unlock()
		if d != nil {
			if d.Name != "hubhost" {
				t.Errorf("name %q", d.Name)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("hub never ingested its own report")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
}

// ---- info --------------------------------------------------------------------------------

func TestInfo(t *testing.T) {
	th := newTestHub(t)
	nonce := "0123456789abcdef0123"
	locked := decode[map[string]any](t, th.req("GET", "/api/info?nonce="+nonce, "", ""))
	if locked["locked"] != true || locked["app"] != "device-monitor" || locked["hostname"] != "testbox" ||
		locked["token"] != nil || locked["port"] != float64(DefaultPort) {
		t.Errorf("locked info = %v", locked)
	}
	if locked["proof"] != hubProof(th.Token(), nonce) {
		t.Errorf("proof = %v", locked["proof"])
	}
	// Computed with upstream's hubProof in Node: hex(HMAC-SHA256(key, "device-monitor-hub:" + nonce)).
	if got := hubProof("k", "abc"); got != "edde3dc2cc87ea46276b01ea252bdf3a8b538dcdd0e475e0f1b53e070c0745fe" {
		t.Errorf("hubProof(k, abc) = %s", got)
	}
	for _, bad := range []string{"", "short", "ABCDEF0123456789", strings.Repeat("a", 65)} {
		if v := decode[map[string]any](t, th.req("GET", "/api/info?nonce="+bad, "", "")); v["proof"] != nil {
			t.Errorf("nonce %q got a proof", bad)
		}
	}
	open := decode[map[string]any](t, th.keyed("GET", "/api/info", ""))
	if open["token"] != th.Token() || open["addresses"] == nil || open["remote"] == nil {
		t.Errorf("unlocked info = %v", open)
	}
}

// ---- security ----------------------------------------------------------------------------

func TestKeyRequired(t *testing.T) {
	th := newTestHub(t)
	th.postReport("dev-1", nil)
	for _, rt := range [][2]string{
		{"GET", "/api/devices"}, {"GET", "/api/stream"}, {"GET", "/api/history/dev-1"},
		{"GET", "/api/energy?bounds=0,1"}, {"GET", "/api/energy/settings"}, {"PUT", "/api/energy/settings"},
		{"GET", "/api/alerts"}, {"PUT", "/api/alerts"}, {"POST", "/api/alerts/test"}, {"DELETE", "/api/alerts/log"},
		{"PUT", "/api/order"}, {"PATCH", "/api/devices/dev-1"}, {"DELETE", "/api/devices/dev-1"},
	} {
		if w := th.req(rt[0], rt[1], "", "{}"); w.Code != 401 {
			t.Errorf("%s %s without key: %d", rt[0], rt[1], w.Code)
		}
		if w := th.req(rt[0], rt[1], "wrong-key", "{}"); w.Code != 401 {
			t.Errorf("%s %s with wrong key: %d", rt[0], rt[1], w.Code)
		}
	}
	// The cookie works as well as the header.
	if w := th.req("GET", "/api/devices", "", "", "Cookie", "dm_key="+th.Token()); w.Code != 200 {
		t.Errorf("cookie key: %d", w.Code)
	}
}

func TestGuard(t *testing.T) {
	th := newTestHub(t)
	for _, tt := range []struct {
		name string
		hdr  []string
		code int
	}{
		{"loopback", nil, 200},
		{"rebinding host", []string{"Host", "evil.example.com"}, 421},
		{"host with quote", []string{"Host", `testbox"`}, 421},
		{"no host", []string{"Host", ""}, 421},
		{"own name", []string{"Host", "testbox.local:7777"}, 200},
		{"magicdns", []string{"Host", "testbox.tail1234.ts.net"}, 200},
		{"ipv6 host", []string{"Host", "[fe80::1]:7777"}, 200},
		{"public source", []string{"RemoteAddr", "8.8.8.8:4000"}, 403},
		{"public v6 source", []string{"RemoteAddr", "[2001:db8::1]:4000"}, 403},
		{"lan source", []string{"RemoteAddr", "192.168.1.20:4000"}, 200},
		{"tailscale source", []string{"RemoteAddr", "100.101.102.103:4000"}, 200},
		{"ula source", []string{"RemoteAddr", "[fd12::5]:4000"}, 200},
		{"mapped source", []string{"RemoteAddr", "[::ffff:10.0.0.2]:4000"}, 200},
	} {
		w := th.req("GET", "/api/devices", th.Token(), "", tt.hdr...)
		if w.Code != tt.code {
			t.Errorf("%s: %d %s, want %d", tt.name, w.Code, w.Body, tt.code)
		}
		if w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Content-Security-Policy") == "" {
			t.Errorf("%s: base headers missing", tt.name)
		}
	}
}

func TestAllowedHostsFromConfig(t *testing.T) {
	paths := config.At(t.TempDir())
	config.WriteFile0600(paths.Config, []byte(`{"token":"abc","allowedHosts":["monitor.example.lan"]}`))
	th := openTestHub(t, paths, &clock{t: time.Unix(1e9, 0)})
	if w := th.req("GET", "/api/devices", "abc", "", "Host", "Monitor.Example.LAN:7777"); w.Code != 200 {
		t.Errorf("allowed host: %d", w.Code)
	}
}

func TestCrossOriginRefused(t *testing.T) {
	th := newTestHub(t)
	body := th.agentReport("dev-1", nil)
	if w := th.req("POST", "/api/report", "", body, "Origin", "http://evil.example.com"); w.Code != 403 ||
		!strings.Contains(w.Body.String(), "cross-site") {
		t.Errorf("cross-origin POST: %d %s", w.Code, w.Body)
	}
	if w := th.req("PUT", "/api/order", th.Token(), `{}`, "Origin", "null"); w.Code != 403 {
		t.Errorf("Origin null PUT: %d", w.Code)
	}
	if w := th.req("DELETE", "/api/alerts/log", th.Token(), ``, "Origin", "http://127.0.0.1:7778"); w.Code != 403 {
		t.Errorf("other-port DELETE: %d", w.Code)
	}
	if w := th.req("POST", "/api/report", "", body, "Origin", "http://127.0.0.1:7777"); w.Code != 200 {
		t.Errorf("same-origin POST: %d %s", w.Code, w.Body)
	}
	if w := th.req("GET", "/api/devices", th.Token(), "", "Origin", "http://evil.example.com"); w.Code != 200 {
		t.Errorf("cross-origin GET: %d (GETs are allowed; CORS keeps the reply from the page)", w.Code)
	}
}

func TestSameOriginDefaultPort(t *testing.T) {
	r := httptest.NewRequest("POST", "/", nil)
	r.Host = "hub.lan"
	r.Header.Set("Origin", "http://HUB.lan:80")
	if !sameOrigin(r) {
		t.Error("http://hub.lan:80 should match Host hub.lan")
	}
}

func TestRateLimit(t *testing.T) {
	th := newTestHub(t)
	for i := range RateMax {
		if w := th.req("GET", "/api/info", "", ""); w.Code != 200 {
			t.Fatalf("request %d: %d", i+1, w.Code)
		}
	}
	if w := th.req("GET", "/api/info", "", ""); w.Code != 429 {
		t.Fatalf("request %d: %d, want 429", RateMax+1, w.Code)
	}
	// Other addresses have their own allowance; a LAN peer can't pick one with X-Forwarded-For.
	if w := th.req("GET", "/api/info", "", "", "RemoteAddr", "192.168.1.9:1"); w.Code != 200 {
		t.Errorf("other address: %d", w.Code)
	}
	spoof := func() int {
		return th.req("GET", "/api/info", "", "", "RemoteAddr", "192.168.1.9:1", "X-Forwarded-For", "10.9.9.9").Code
	}
	for range RateMax - 1 {
		spoof()
	}
	if c := spoof(); c != 429 {
		t.Errorf("X-Forwarded-For from a LAN peer dodged the limit: %d", c)
	}
	// From loopback (tailscale serve) the forwarded address is the client.
	if w := th.req("GET", "/api/info", "", "", "X-Forwarded-For", "100.64.0.7"); w.Code != 200 {
		t.Errorf("forwarded tailnet client: %d", w.Code)
	}
	// The key skips the limit; the limit resets after the window.
	if w := th.keyed("GET", "/api/info", ""); w.Code != 200 {
		t.Errorf("with key: %d", w.Code)
	}
	th.clk.Add(RateWindow + time.Second)
	th.rate.prune()
	if w := th.req("GET", "/api/info", "", ""); w.Code != 200 {
		t.Errorf("after the window: %d", w.Code)
	}
}

func TestNotFound(t *testing.T) {
	th := newTestHub(t)
	for _, rt := range [][2]string{{"GET", "/nope"}, {"DELETE", "/api/info"}, {"GET", "/api/report"},
		{"GET", "/api/history/unknown"}, {"PATCH", "/api/devices/unknown"}, {"DELETE", "/api/devices/..%2f"}} {
		w := th.keyed(rt[0], rt[1], "")
		if w.Code != 404 || !strings.Contains(w.Body.String(), `"error"`) {
			t.Errorf("%s %s: %d %s, want JSON 404", rt[0], rt[1], w.Code, w.Body)
		}
	}
}

func TestHostAllowed(t *testing.T) {
	for host, want := range map[string]bool{
		"127.0.0.1": true, "127.0.0.1:7777": true, "[::1]:7777": true, "::1": true, "10.1.2.3": true,
		"localhost": true, "LOCALHOST:80": true, "box": true, "box.home.arpa": true, "box.lan": true,
		"box.x.ts.net": true, "other.x.ts.net": false, "evil.com": false, "box.evil.com": false,
		"": false, "a b": false, "box'": false, "[::1": false, "trusted.lan": true,
	} {
		if got := hostAllowed(host, "Box", []string{"Trusted.lan"}); got != want {
			t.Errorf("hostAllowed(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestIsLocalAddress(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:1": true, "10.0.0.1:1": true, "172.16.0.1:1": true, "172.31.255.255:1": true,
		"172.32.0.1:1": false, "192.168.0.1:1": true, "169.254.1.1:1": true, "100.64.0.1:1": true,
		"100.128.0.1:1": false, "8.8.8.8:1": false, "[::1]:1": true, "[fe80::1%eth0]:1": true,
		"[febf::1]:1": true, "[fec0::1]:1": false, "[fc00::1]:1": true, "[2001:db8::1]:1": false,
		"[::ffff:192.168.1.1]:1": true, "[::ffff:8.8.8.8]:1": false, "garbage": false, "": false,
	} {
		if got := isLocalAddress(addr); got != want {
			t.Errorf("isLocalAddress(%q) = %v, want %v", addr, got, want)
		}
	}
}

// ---- devices: history, rename, remove, order ---------------------------------------------

func TestHistoryEndpoint(t *testing.T) {
	th := newTestHub(t)
	for range 5 {
		th.postReport("dev-1", nil)
		th.clk.Add(2 * time.Second)
	}
	w := th.keyed("GET", "/api/history/dev-1?range=10m", "")
	res := decode[struct {
		From, To, Resolution float64
		Points               []map[string]any
	}](t, w)
	if w.Code != 200 || len(res.Points) != 5 || res.Resolution != 2000 {
		t.Fatalf("history: %d %s", w.Code, w.Body)
	}
	if w := th.keyed("GET", "/api/history/dev-1?from=10&to=5", ""); w.Code != 400 {
		t.Errorf("from > to: %d", w.Code)
	}
}

func TestRenameRemoveOrder(t *testing.T) {
	th := newTestHub(t)
	th.postReport("a", nil)
	th.clk.Add(time.Millisecond)
	th.postReport("b", nil)

	w := th.keyed("PATCH", "/api/devices/a", `{"name":"  Den\u0007 PC "}`)
	if v := decode[View](t, w); w.Code != 200 || v.Name != "Den PC" || !v.Renamed {
		t.Fatalf("rename: %d %s", w.Code, w.Body)
	}
	th.postReport("a", nil) // a report no longer overrides the user's name
	if d := th.devices()[0]; d.Name != "Den PC" {
		t.Errorf("name after report: %q", d.Name)
	}
	if w := th.keyed("PATCH", "/api/devices/a", `{"name":5}`); w.Code != 200 || decode[View](t, w).Name != "Den PC" {
		t.Errorf("non-string name changed it: %s", w.Body)
	}
	if w := th.keyed("PATCH", "/api/devices/a", `{"name":`); w.Code != 400 {
		t.Errorf("bad JSON: %d", w.Code)
	}

	if w := th.keyed("PUT", "/api/order", `{"ids":["b","nope",7,"a"]}`); w.Code != 200 {
		t.Fatalf("order: %d", w.Code)
	}
	for _, d := range th.devices() {
		if want := map[string]int{"a": 1, "b": 0}[d.ID]; d.Order != want {
			t.Errorf("%s order %d, want %d", d.ID, d.Order, want)
		}
	}

	if w := th.keyed("DELETE", "/api/devices/a", ""); w.Code != 200 {
		t.Fatalf("remove: %d", w.Code)
	}
	if devs := th.devices(); len(devs) != 1 || devs[0].ID != "b" {
		t.Errorf("after remove: %+v", devs)
	}
	if w := th.keyed("GET", "/api/history/a", ""); w.Code != 404 {
		t.Errorf("history of removed device: %d", w.Code)
	}
}

// ---- energy ------------------------------------------------------------------------------

func TestEnergy(t *testing.T) {
	th := newTestHub(t)
	if w := th.keyed("GET", "/api/energy/settings", ""); strings.TrimSpace(w.Body.String()) != "null" {
		t.Errorf("settings before any: %s", w.Body)
	}
	for _, b := range []string{"", "5", "1,1", "2,1", "0,x", "0,Infinity", fmt.Sprintf("0,%d", 33*86400000)} {
		if w := th.keyed("GET", "/api/energy?bounds="+b, ""); w.Code != 400 {
			t.Errorf("bounds %q: %d", b, w.Code)
		}
	}
	if w := th.keyed("PUT", "/api/energy/settings", `{"region":"US","rate":-1,"currency":"USD"}`); w.Code != 400 {
		t.Errorf("invalid settings: %d", w.Code)
	}

	// Two minutes of reports, so the first minute is committed.
	start := th.clk.Now().Truncate(time.Minute)
	for range 60 {
		th.postReport("dev-1", nil)
		th.clk.Add(2 * time.Second)
	}
	bounds := fmt.Sprintf("%d,%d", start.UnixMilli(), start.Add(time.Hour).UnixMilli())

	w := th.keyed("GET", "/api/energy?bounds="+bounds, "")
	res := decode[struct {
		Settings *json.RawMessage
		Devices  []Usage
	}](t, w)
	if w.Code != 200 || len(res.Devices) != 1 || res.Settings != nil {
		t.Fatalf("energy: %d %s", w.Code, w.Body)
	}
	u := res.Devices[0]
	if u.Minutes < 2 || u.Wh <= 0 || u.Cost != nil || u.Stats["cpu"].Avg != 12.6 || u.Bytes["rx"] <= 0 {
		t.Errorf("usage without price: %s", w.Body)
	}

	if w := th.keyed("PUT", "/api/energy/settings", `{"region":"US","rate":0.25,"currency":"USD"}`); w.Code != 200 {
		t.Fatalf("put settings: %d %s", w.Code, w.Body)
	}
	if w := th.keyed("GET", "/api/energy/settings", ""); !strings.Contains(w.Body.String(), `"rate":0.25`) {
		t.Errorf("settings: %s", w.Body)
	}
	u = decode[struct{ Devices []Usage }](t, th.keyed("GET", "/api/energy?bounds="+bounds, "")).Devices[0]
	if u.Cost == nil || *u.Cost <= 0 {
		t.Errorf("cost with price: %v", u.Cost)
	}
	c, ok := energySource{th.Hub}.CostToday("*")
	if !ok || c.Currency != "USD" || c.Amount != *u.Cost {
		t.Errorf("CostToday = %+v %v, want %v USD", c, ok, *u.Cost)
	}
	if c, ok := (energySource{th.Hub}).CostToday("unknown"); !ok || c.Amount != 0 {
		t.Errorf("CostToday(unknown) = %+v %v", c, ok)
	}
}

// ---- alerts ------------------------------------------------------------------------------

func TestAlertsEndpoints(t *testing.T) {
	th := newTestHub(t)
	got := decode[map[string]any](t, th.keyed("GET", "/api/alerts", ""))
	if got["settings"] == nil || got["active"] == nil || got["events"] == nil || got["priceSet"] != false {
		t.Fatalf("GET /api/alerts = %v", got)
	}
	if w := th.keyed("PUT", "/api/alerts", `{"rules":"nope"}`); w.Code != 400 {
		t.Errorf("invalid settings: %d", w.Code)
	}
	w := th.keyed("PUT", "/api/alerts", `{"rules":[{"type":"cpu","device":"*","threshold":5,"minutes":0}],"channels":{"desktop":true}}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"active"`) {
		t.Fatalf("PUT /api/alerts: %d %s", w.Code, w.Body)
	}
	th.postReport("dev-1", nil) // CPU 12.6 % > 5 %: fires at once
	got = decode[map[string]any](t, th.keyed("GET", "/api/alerts", ""))
	if len(got["active"].([]any)) != 1 || len(got["events"].([]any)) != 1 {
		t.Fatalf("after report: %v", got)
	}
	if w := th.keyed("POST", "/api/alerts/test", ""); w.Code != 200 {
		t.Errorf("test: %d", w.Code)
	}
	if w := th.keyed("DELETE", "/api/alerts/log", ""); w.Code != 200 {
		t.Errorf("clear log: %d", w.Code)
	}
	if got := decode[map[string]any](t, th.keyed("GET", "/api/alerts", "")); len(got["events"].([]any)) != 0 {
		t.Errorf("log after clear: %v", got["events"])
	}
}

func TestOfflineAlertFromDevices(t *testing.T) {
	th := newTestHub(t)
	th.postReport("dev-1", nil)
	th.clk.Add(11 * time.Minute) // default rule: offline for 10 minutes
	th.alerts.Tick()
	act := th.alerts.Active()
	if len(act) != 1 || act[0].Type != "offline" || act[0].DeviceName != "atlas" {
		t.Fatalf("active = %+v", act)
	}
}

// ---- SSE ---------------------------------------------------------------------------------

// sse reads events from a live /api/stream.
type sse struct {
	t    *testing.T
	body io.Closer
	ch   chan map[string]any
}

func openStream(t *testing.T, srv *httptest.Server, key string) *sse {
	t.Helper()
	req, _ := http.NewRequest("GET", srv.URL+"/api/stream", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	s := &sse{t: t, body: resp.Body, ch: make(chan map[string]any, 100)}
	go func() {
		defer close(s.ch)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(nil, 1<<22)
		for sc.Scan() {
			line, ok := strings.CutPrefix(sc.Text(), "data: ")
			if !ok {
				continue
			}
			var m map[string]any
			if json.Unmarshal([]byte(line), &m) == nil {
				s.ch <- m
			}
		}
	}()
	t.Cleanup(func() { resp.Body.Close() })
	return s
}

// next returns the next event of type typ, skipping others.
func (s *sse) next(typ string) map[string]any {
	s.t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case m, ok := <-s.ch:
			if !ok {
				s.t.Fatalf("stream closed waiting for %q", typ)
			}
			if m["type"] == typ {
				return m
			}
		case <-timeout:
			s.t.Fatalf("no %q event", typ)
		}
	}
}

func TestStreamEvents(t *testing.T) {
	th := newTestHub(t)
	th.postReport("dev-1", nil)
	srv := httptest.NewServer(th.h)
	defer srv.Close()
	s := openStream(t, srv, th.Token())

	snap := s.next("snapshot")
	devs := snap["devices"].([]any)
	if len(devs) != 1 || devs[0].(map[string]any)["history"] == nil {
		t.Fatalf("snapshot = %v", snap)
	}

	th.clk.Add(time.Second)
	th.postReport("dev-1", func(m map[string]any) {
		m["actionResult"] = map[string]any{"id": "0123456789ab", "ok": true, "message": "Restarting"}
	})
	rep := s.next("report")
	if rep["id"] != "dev-1" || rep["sample"] == nil {
		t.Errorf("report = %v", rep)
	}
	if dev := rep["device"].(map[string]any); dev["online"] != true || dev["latest"] == nil || dev["history"] != nil {
		t.Errorf("report device = %v", dev)
	}
	if ar := s.next("actionResult"); ar["name"] != "atlas" || ar["result"].(map[string]any)["ok"] != true {
		t.Errorf("actionResult = %v", ar)
	}

	th.broadcastStatus()
	if st := s.next("status"); st["online"].(map[string]any)["dev-1"] != true {
		t.Errorf("status = %v", st)
	}
	th.keyed("PATCH", "/api/devices/dev-1", `{"name":"Den"}`)
	if d := s.next("device"); d["device"].(map[string]any)["name"] != "Den" {
		t.Errorf("device = %v", d)
	}
	th.keyed("PUT", "/api/order", `{"ids":["dev-1"]}`)
	if o := s.next("order"); o["order"].(map[string]any)["dev-1"] != float64(0) {
		t.Errorf("order = %v", o)
	}
	th.keyed("PUT", "/api/energy/settings", `{"region":"GB","rate":0.3,"currency":"GBP"}`)
	if e := s.next("energySettings"); e["settings"].(map[string]any)["currency"] != "GBP" {
		t.Errorf("energySettings = %v", e)
	}
	th.keyed("PUT", "/api/alerts", `{"rules":[],"channels":{"desktop":true}}`)
	s.next("alerts")
	th.keyed("POST", "/api/alerts/test", "")
	if a := s.next("alert"); a["event"].(map[string]any)["test"] != true || a["notify"] != true {
		t.Errorf("alert = %v", a)
	}
	th.keyed("DELETE", "/api/devices/dev-1", "")
	if r := s.next("removed"); r["id"] != "dev-1" {
		t.Errorf("removed = %v", r)
	}

	// Close ends open streams.
	th.Close()
	for range s.ch {
	}
}

func TestStreamLimit(t *testing.T) {
	th := newTestHub(t)
	for range MaxStreams {
		if th.addStream() == nil {
			t.Fatal("stream refused under the limit")
		}
	}
	if w := th.keyed("GET", "/api/stream", ""); w.Code != 503 {
		t.Errorf("stream over the limit: %d", w.Code)
	}
}

func TestSlowStreamDropped(t *testing.T) {
	th := newTestHub(t)
	s := th.addStream()
	for range streamBuffer + 1 {
		th.broadcast(map[string]string{"type": "status"})
	}
	select {
	case <-s.done:
	default:
		t.Fatal("stream not dropped after overflowing its buffer")
	}
	if n := len(s.ch); n != streamBuffer {
		t.Errorf("got %d buffered messages, want %d", n, streamBuffer)
	}
	// The slot stays taken until the handler finishes.
	th.smu.Lock()
	active := th.activeStreams
	th.smu.Unlock()
	if active != 1 {
		t.Errorf("activeStreams = %d after drop, want 1 until release", active)
	}
	th.releaseStream(s)
	th.releaseStream(th.addStream())
	if th.activeStreams != 0 {
		t.Errorf("activeStreams = %d after release, want 0", th.activeStreams)
	}
}

// TestStreamClientStopsReading: a keyed client that opens the stream and never reads must not
// pin its handler and connection forever. The write deadline ends the handler.
func TestStreamClientStopsReading(t *testing.T) {
	th := newTestHub(t)
	th.streamWriteTimeout = 200 * time.Millisecond
	srv := httptest.NewServer(th.h)
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET /api/stream HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\n\r\n",
		srv.Listener.Addr(), th.Token())
	active := func() int {
		th.smu.Lock()
		defer th.smu.Unlock()
		return th.activeStreams
	}
	for deadline := time.Now().Add(5 * time.Second); active() == 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("stream never opened")
		}
	}
	// Never read from conn. Big messages fill the socket buffers, so the handler blocks in a
	// write; without a deadline it would stay there for good.
	pad := strings.Repeat("x", 256<<10)
	for deadline := time.Now().Add(15 * time.Second); active() != 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("handler still running for a client that stopped reading")
		}
		th.broadcast(map[string]string{"type": "status", "pad": pad})
	}
}

// TestConcurrentReportsStayOrdered: reports for one device racing each other are applied one
// at a time: LastSeen never goes backwards, history and "report" events stay in time order.
func TestConcurrentReportsStayOrdered(t *testing.T) {
	th := newTestHub(t)
	s := th.addStream()
	raw := []byte(th.agentReport("dev-1", nil))
	const workers, each = 8, 25 // 200 events fit in streamBuffer
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for range each {
				th.clk.Add(time.Millisecond)
				if _, err := th.ingest(raw); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()

	last := 0.0
	for range workers * each {
		var ev struct{ Sample struct{ T float64 } }
		data := <-s.ch
		if err := json.Unmarshal(data[len("data: "):], &ev); err != nil {
			t.Fatal(err)
		}
		if ev.Sample.T < last {
			t.Fatalf("report events out of order: %v after %v", ev.Sample.T, last)
		}
		last = ev.Sample.T
	}
	hist := th.history.Recent("dev-1", LiveHistory)
	if len(hist) != workers*each {
		t.Fatalf("%d history samples, want %d", len(hist), workers*each)
	}
	for i := 1; i < len(hist); i++ {
		if hist[i].T < hist[i-1].T {
			t.Fatalf("history out of order at %d: %v after %v", i, hist[i].T, hist[i-1].T)
		}
	}
	th.mu.Lock()
	seen := th.registry["dev-1"].LastSeen
	th.mu.Unlock()
	if float64(seen) != last || seen != th.clk.Now().UnixMilli() {
		t.Errorf("LastSeen %d, last event %v, clock %d", seen, last, th.clk.Now().UnixMilli())
	}
}

func TestParseBoundsLimit(t *testing.T) {
	if _, ok := parseBounds("8999999999999,9000000000000"); ok {
		t.Error("bound at 9e12 ms accepted")
	}
	b, ok := parseBounds("8999999999998,8999999999999")
	if !ok {
		t.Fatal("bounds just under 9e12 ms refused")
	}
	if got := msTime(b[1]).UnixMilli(); got != 8999999999999 {
		t.Errorf("msTime(%v) = %d ms: overflowed", b[1], got)
	}
	if got := msTime(-8999999999999).UnixMilli(); got != -8999999999999 {
		t.Errorf("msTime(-8999999999999) = %d ms", got)
	}
}

func TestPlatformAlwaysSent(t *testing.T) {
	b, _ := json.Marshal(Device{ID: "x"})
	if !strings.Contains(string(b), `"platform":""`) {
		t.Errorf("Device JSON %s has no platform key", b)
	}
}
