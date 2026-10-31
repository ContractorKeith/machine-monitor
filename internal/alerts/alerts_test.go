package alerts

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/config"
	"github.com/ContractorKeith/machine-monitor/internal/report"
)

// ---- fakes -------------------------------------------------------------------------------

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time           { return c.t }
func (c *fakeClock) add(d time.Duration)      { c.t = c.t.Add(d) }
func newClock() *fakeClock                    { return &fakeClock{t: time.Date(2026, 10, 31, 14, 0, 0, 0, time.Local)} }
func (c *fakeClock) ms(d time.Duration) int64 { return c.t.Add(d).UnixMilli() }

type fakeEnergy struct {
	watts *float64
	cost  Cost
	ok    bool
	calls int // Watts calls
}

func (f *fakeEnergy) Watts(string, *report.Report) *float64 { f.calls++; return f.watts }
func (f *fakeEnergy) CostToday(string) (Cost, bool)         { return f.cost, f.ok }

type sent struct {
	url  string
	body map[string]any
}

// fakeDoer records requests and answers with status.
type fakeDoer struct {
	mu     sync.Mutex
	status int
	reqs   []sent
}

func (f *fakeDoer) Do(r *http.Request) (*http.Response, error) {
	b, _ := io.ReadAll(r.Body)
	var body map[string]any
	json.Unmarshal(b, &body)
	f.mu.Lock()
	f.reqs = append(f.reqs, sent{r.URL.String(), body})
	f.mu.Unlock()
	st := f.status
	if st == 0 {
		st = 200
	}
	return &http.Response{StatusCode: st, Body: io.NopCloser(strings.NewReader("nope")), Header: http.Header{}}, nil
}

func (f *fakeDoer) sent() []sent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sent(nil), f.reqs...)
}

type harness struct {
	e       *Engine
	clock   *fakeClock
	energy  *fakeEnergy
	doer    *fakeDoer
	devices []Device
	msgs    []Message
}

// newHarness builds an engine with one device "pc1" (named "Workstation") and the given
// settings JSON ("" for defaults).
func newHarness(t *testing.T, settings string) *harness {
	t.Helper()
	h := &harness{clock: newClock(), energy: &fakeEnergy{}, doer: &fakeDoer{}}
	h.devices = []Device{{ID: "pc1", Name: "Workstation", Online: true, LastSeen: h.clock.t}}
	h.e = New(Options{
		Devices:   func() []Device { return h.devices },
		Energy:    h.energy,
		Broadcast: func(m Message) { h.msgs = append(h.msgs, m) },
		HubURL:    func() string { return "http://hub.lan:7777" },
		Now:       h.clock.now,
		HTTP:      h.doer,
		LookupIP: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
		},
	})
	if settings != "" {
		if _, err := h.e.Update([]byte(settings)); err != nil {
			t.Fatal(err)
		}
		h.msgs = nil
	}
	t.Cleanup(h.e.Wait)
	return h
}

// events returns the alert events broadcast since the last call.
func (h *harness) events() []Event {
	var out []Event
	for _, m := range h.msgs {
		if m.Type == "alert" {
			out = append(out, *m.Event)
		}
	}
	h.msgs = nil
	return out
}

func rule(typ string, threshold any, minutes int) string {
	b, _ := json.Marshal(map[string]any{"rules": []map[string]any{
		{"id": "aaaaaaaaaaaa", "type": typ, "device": "*", "threshold": threshold, "minutes": minutes}}})
	return string(b)
}

func expectOne(t *testing.T, h *harness, state, title string) Event {
	t.Helper()
	evs := h.events()
	if len(evs) != 1 {
		t.Fatalf("want one %s event, got %+v", state, evs)
	}
	if evs[0].State != state || evs[0].Title != title {
		t.Fatalf("got %s %q, want %s %q", evs[0].State, evs[0].Title, state, title)
	}
	return evs[0]
}

func expectNone(t *testing.T, h *harness) {
	t.Helper()
	if evs := h.events(); len(evs) != 0 {
		t.Fatalf("want no events, got %+v", evs)
	}
}

// ---- rule types --------------------------------------------------------------------------

// Each metric rule: fires only after its minutes over the threshold, holds inside the
// hysteresis band, resolves below it.
func TestMetricRulesFireAndResolve(t *testing.T) {
	cases := []struct {
		typ               string
		threshold         float64
		set               func(r *report.Report, h *harness, v float64)
		over, band, under float64
		firing, resolved  string
	}{
		{TypeCPUTemp, 90, func(r *report.Report, _ *harness, v float64) { r.CPU.Temp = &v },
			95, 88, 86, "Workstation: CPU temperature 95°C", "Workstation: CPU temperature back to normal"},
		{TypeGPUTemp, 85, func(r *report.Report, _ *harness, v float64) {
			r.GPUs = []report.GPU{{Temp: report.Ptr(40.0)}, {Temp: &v}, {}}
		}, 90, 83, 81, "Workstation: GPU temperature 90°C", "Workstation: GPU temperature back to normal"},
		{TypeCPU, 80, func(r *report.Report, _ *harness, v float64) { r.CPU.Usage = &v },
			99.6, 76, 74, "Workstation: CPU load 100%", "Workstation: CPU load back to normal"},
		{TypeGPU, 80, func(r *report.Report, _ *harness, v float64) { r.GPUs = []report.GPU{{Usage: &v}} },
			90, 76, 70, "Workstation: GPU load 90%", "Workstation: GPU load back to normal"},
		{TypeMem, 90, func(r *report.Report, _ *harness, v float64) {
			r.Memory = report.Memory{Total: report.Ptr(16e9), Used: report.Ptr(16e9 * v / 100)}
		}, 95, 88, 86, "Workstation: Memory use 95%", "Workstation: Memory use back to normal"},
		{TypeDisk, 90, func(r *report.Report, _ *harness, v float64) {
			r.Disks = []report.Disk{
				{Name: report.Ptr("/boot"), Total: report.Ptr(5e8), Used: report.Ptr(5e8)}, // ignored: under 1 GB
				{Name: report.Ptr("/"), Total: report.Ptr(100e9), Used: report.Ptr(v * 1e9)},
			}
		}, 95, 89.5, 88, "Workstation: disk / is 95% full", "Workstation: disk space back under 90%"},
		{TypePower, 500, func(_ *report.Report, h *harness, v float64) { h.energy.watts = &v },
			1250, 480, 470, "Workstation: Power draw 1.25 kW", "Workstation: Power draw back to normal"},
	}
	for _, c := range cases {
		t.Run(c.typ, func(t *testing.T) {
			h := newHarness(t, rule(c.typ, c.threshold, 2))
			send := func(v float64) {
				r := &report.Report{}
				c.set(r, h, v)
				h.e.OnReport("pc1", r)
			}
			send(c.over)
			expectNone(t, h) // not over for 2 min yet
			h.clock.add(119 * time.Second)
			send(c.over)
			expectNone(t, h)
			h.clock.add(time.Second)
			send(c.over)
			ev := expectOne(t, h, Firing, c.firing)
			if ev.Device != "pc1" || ev.RuleID != "aaaaaaaaaaaa" || ev.Value == nil {
				t.Fatalf("bad event %+v", ev)
			}
			if a := h.e.Active(); len(a) != 1 || a[0].Title != c.firing {
				t.Fatalf("active = %+v", a)
			}
			send(c.over)
			expectNone(t, h) // fires once
			send(c.band)
			expectNone(t, h) // under the threshold but inside hysteresis
			send(c.under)
			expectOne(t, h, Resolved, c.resolved)
			if a := h.e.Active(); len(a) != 0 {
				t.Fatalf("active after resolve = %+v", a)
			}
		})
	}
}

func TestDurationRestartsWhenValueDrops(t *testing.T) {
	h := newHarness(t, rule(TypeCPU, 80, 2))
	cpu := func(v float64) { h.e.OnReport("pc1", &report.Report{CPU: report.CPU{Usage: &v}}) }
	cpu(90)
	h.clock.add(90 * time.Second)
	cpu(50) // dips: the 2 minutes start over
	h.clock.add(30 * time.Second)
	cpu(90)
	h.clock.add(90 * time.Second)
	cpu(90)
	expectNone(t, h)
	h.clock.add(30 * time.Second)
	cpu(90)
	expectOne(t, h, Firing, "Workstation: CPU load 90%")
}

func TestMissingMetricIsIgnored(t *testing.T) {
	h := newHarness(t, rule(TypeCPUTemp, 50, 0))
	h.e.OnReport("pc1", &report.Report{}) // nil temp, never treated as 0
	h.e.OnReport("pc1", &report.Report{Memory: report.Memory{Total: report.Ptr(8e9)}})
	expectNone(t, h)
	if a := h.e.Active(); len(a) != 0 {
		t.Fatalf("active = %+v", a)
	}
}

func TestOfflineFiresAndResolves(t *testing.T) {
	h := newHarness(t, rule(TypeOffline, nil, 10))
	lastSeen := h.clock.t
	h.devices[0].Online, h.devices[0].LastSeen = false, lastSeen
	h.clock.add(9 * time.Minute)
	h.e.Tick()
	expectNone(t, h)
	h.clock.add(time.Minute)
	h.e.Tick()
	ev := expectOne(t, h, Firing, "Workstation is offline")
	if want := "No reports since " + lastSeen.Format("3:04 PM") + "."; ev.Message != want {
		t.Fatalf("message %q, want %q", ev.Message, want)
	}
	h.e.Tick()
	expectNone(t, h)

	h.devices[0].Online = true
	h.e.OnReport("pc1", &report.Report{})
	ev = expectOne(t, h, Resolved, "Workstation is back online")
	if !strings.HasPrefix(ev.Message, "It was offline since ") {
		t.Fatalf("message %q", ev.Message)
	}
}

func TestOfflineAtStartupFiresSilently(t *testing.T) {
	clock := newClock()
	var msgs []Message
	e := New(Options{
		Devices: func() []Device {
			return []Device{{ID: "pc1", Name: "Box", LastSeen: clock.t.Add(-time.Hour)}}
		},
		Now:       clock.now,
		Broadcast: func(m Message) { msgs = append(msgs, m) },
		HTTP:      &fakeDoer{},
	})
	if len(msgs) != 0 {
		t.Fatalf("broadcast at startup: %+v", msgs)
	}
	if a := e.Active(); len(a) != 1 || a[0].Type != TypeOffline {
		t.Fatalf("active = %+v", a)
	}
	e.Tick()
	if len(msgs) != 0 {
		t.Fatalf("tick re-fired: %+v", msgs)
	}
}

func TestDailyCostFiresOncePerDay(t *testing.T) {
	h := newHarness(t, rule(TypeDailyCost, 2.5, 0))
	h.energy.ok, h.energy.cost = true, Cost{Amount: 2, Currency: "USD"}
	h.e.CheckCost()
	expectNone(t, h)
	h.energy.cost.Amount = 1234.5
	h.e.CheckCost()
	ev := expectOne(t, h, Firing, "All devices: electricity today is $1,234.50")
	if ev.Message != "Over your $2.50 daily limit." {
		t.Fatalf("message %q", ev.Message)
	}
	h.e.CheckCost()
	expectNone(t, h)

	// Next day: the alert quietly clears, then fires again if still over.
	h.clock.add(24 * time.Hour)
	h.energy.cost.Amount = 0.1
	h.e.CheckCost()
	expectNone(t, h)
	if a := h.e.Active(); len(a) != 0 {
		t.Fatalf("active on new day = %+v", a)
	}
	h.energy.cost.Amount = 3
	h.e.CheckCost()
	expectOne(t, h, Firing, "All devices: electricity today is $3.00")
}

func TestDailyCostNoPriceSet(t *testing.T) {
	h := newHarness(t, rule(TypeDailyCost, 1, 0))
	h.energy.cost = Cost{Amount: 99, Currency: "USD"} // ok false: no price configured
	h.e.CheckCost()
	expectNone(t, h)
}

func TestDisabledRuleAndOtherDevice(t *testing.T) {
	h := newHarness(t, `{"rules":[
		{"type":"cpu","device":"pc1","threshold":50,"minutes":0,"enabled":false},
		{"type":"cpu","device":"pc2","threshold":50,"minutes":0}]}`)
	h.e.OnReport("pc1", &report.Report{CPU: report.CPU{Usage: report.Ptr(99.0)}})
	expectNone(t, h)
}

func TestUpdateForgetsChangedRules(t *testing.T) {
	h := newHarness(t, rule(TypeCPU, 50, 0))
	h.e.OnReport("pc1", &report.Report{CPU: report.CPU{Usage: report.Ptr(99.0)}})
	expectOne(t, h, Firing, "Workstation: CPU load 99%")

	// Same rule re-sent: still firing.
	if _, err := h.e.Update([]byte(rule(TypeCPU, 50, 0))); err != nil {
		t.Fatal(err)
	}
	if len(h.e.Active()) != 1 {
		t.Fatal("unchanged rule lost its state")
	}
	// Threshold changed: state dropped, no resolved event.
	if _, err := h.e.Update([]byte(rule(TypeCPU, 60, 0))); err != nil {
		t.Fatal(err)
	}
	if len(h.e.Active()) != 0 {
		t.Fatal("changed rule kept its state")
	}
	if evs := h.events(); len(evs) != 0 {
		t.Fatalf("events: %+v", evs)
	}
	if m := h.msgs; len(m) != 0 {
		t.Fatalf("leftover: %+v", m)
	}
}

func TestRemoveDevice(t *testing.T) {
	h := newHarness(t, rule(TypeCPU, 50, 0))
	h.e.OnReport("pc1", &report.Report{CPU: report.CPU{Usage: report.Ptr(99.0)}})
	h.e.RemoveDevice("pc1")
	if len(h.e.Active()) != 0 {
		t.Fatal("removed device still active")
	}
}

func TestEventLogCapAndOrder(t *testing.T) {
	h := newHarness(t, rule(TypeCPU, 50, 0))
	for i := 0; i < 200; i++ {
		h.e.OnReport("pc1", &report.Report{CPU: report.CPU{Usage: report.Ptr(99.0)}})
		h.e.OnReport("pc1", &report.Report{CPU: report.CPU{Usage: report.Ptr(10.0)}})
	}
	evs := h.e.Events(0)
	if len(evs) != maxLog {
		t.Fatalf("log has %d events, want %d", len(evs), maxLog)
	}
	if evs[0].State != Resolved || len(h.e.Events(5)) != 5 {
		t.Fatalf("newest first? %+v", evs[0])
	}
	h.e.ClearLog()
	if len(h.e.Events(0)) != 0 {
		t.Fatal("log not cleared")
	}
}

func TestFahrenheitWording(t *testing.T) {
	h := newHarness(t, `{"tempUnit":"F","rules":[{"id":"aaaaaaaaaaaa","type":"cpuTemp","device":"*","threshold":90,"minutes":0}]}`)
	h.e.OnReport("pc1", &report.Report{CPU: report.CPU{Temp: report.Ptr(100.0)}})
	ev := expectOne(t, h, Firing, "Workstation: CPU temperature 212°F")
	if ev.Message != "Above 194°F." {
		t.Fatalf("message %q", ev.Message)
	}
}

// ---- delivery ----------------------------------------------------------------------------

const channels = `"channels":{"ntfy":{"enabled":true,"topic":"my-alerts","email":"me@example.com"},
	"webhook":{"enabled":true,"url":"https://discord.example/api/webhooks/1/x"}}`

func TestDeliveryPayloads(t *testing.T) {
	h := newHarness(t, `{"rules":[{"type":"cpu","device":"*","threshold":50,"minutes":0}],`+channels+`}`)
	h.e.OnReport("pc1", &report.Report{CPU: report.CPU{Usage: report.Ptr(99.0)}})
	h.e.Wait()
	reqs := h.doer.sent()
	if len(reqs) != 2 {
		t.Fatalf("want ntfy + webhook, got %+v", reqs)
	}
	n, w := reqs[0], reqs[1]
	if n.url != "https://ntfy.sh" || n.body["topic"] != "my-alerts" || n.body["priority"] != 4.0 ||
		n.body["title"] != "Workstation: CPU load 99%" || n.body["click"] != "http://hub.lan:7777/#/alerts" ||
		n.body["email"] != "me@example.com" {
		t.Fatalf("ntfy: %+v", n)
	}
	line := "⚠️ Workstation: CPU load 99%\nAbove 50%."
	if w.url != "https://discord.example/api/webhooks/1/x" || w.body["content"] != line || w.body["text"] != line ||
		w.body["state"] != Firing || w.body["device"] != "Workstation" {
		t.Fatalf("webhook: %+v", w)
	}

	// Resolved: lower priority; and nothing at all when notifyResolved is off.
	h.e.OnReport("pc1", &report.Report{CPU: report.CPU{Usage: report.Ptr(1.0)}})
	h.e.Wait()
	if reqs = h.doer.sent(); len(reqs) != 4 || reqs[2].body["priority"] != 3.0 {
		t.Fatalf("resolved delivery: %+v", reqs)
	}
}

func TestNotifyResolvedOff(t *testing.T) {
	h := newHarness(t, `{"notifyResolved":false,"rules":[{"type":"cpu","device":"*","threshold":50,"minutes":0}],`+channels+`}`)
	h.e.OnReport("pc1", &report.Report{CPU: report.CPU{Usage: report.Ptr(99.0)}})
	h.e.OnReport("pc1", &report.Report{CPU: report.CPU{Usage: report.Ptr(1.0)}})
	h.e.Wait()
	if n := len(h.doer.sent()); n != 2 {
		t.Fatalf("want only the firing delivered, got %d requests", n)
	}
	for _, m := range h.msgs {
		if m.Event.State == Resolved && m.Notify {
			t.Fatal("resolved event asked for a banner")
		}
	}
}

func TestTestAlertAndHTTPErrors(t *testing.T) {
	h := newHarness(t, `{"rules":[],`+channels+`}`)
	h.doer.status = http.StatusFound // a redirect is a failure, never followed
	d := h.e.Test(context.Background())
	if d.Ntfy == nil || d.Ntfy.OK || d.Ntfy.Error != "HTTP 302 nope" || d.Webhook == nil || d.Webhook.OK {
		t.Fatalf("delivery = %+v %+v", d.Ntfy, d.Webhook)
	}
	evs := h.events()
	if len(evs) != 1 || !evs[0].Test {
		t.Fatalf("test event not broadcast: %+v", evs)
	}
}

// Acceptance: a webhook to 127.0.0.1 is refused, both when saved and when sent.
func TestWebhookToLoopbackRefused(t *testing.T) {
	for _, u := range []string{"http://127.0.0.1:8080/hook", "http://[::1]/x", "http://localhost/x",
		"http://foo.localhost/x", "http://0.0.0.0/x", "http://[::ffff:127.0.0.1]/x", "http://169.254.169.254/x",
		"http://2130706433/x", "http://0x7f.1/x", "http://127.1/x", "ftp://example.com/x"} {
		if _, err := ParseSettings([]byte(`{"rules":[],"channels":{"webhook":{"enabled":true,"url":"` + u + `"}}}`)); err == nil {
			t.Errorf("%s: accepted", u)
		}
	}
	h := newHarness(t, "")
	if r := h.e.post(context.Background(), "http://127.0.0.1:8080/hook", map[string]any{}); r.OK ||
		!strings.HasPrefix(r.Error, "blocked") {
		t.Fatalf("post to loopback: %+v", r)
	}
	if n := len(h.doer.sent()); n != 0 {
		t.Fatalf("%d requests sent", n)
	}
}

// Acceptance: a webhook to a name that resolves to 169.254.169.254 is refused.
func TestWebhookToNameResolvingToMetadataRefused(t *testing.T) {
	h := newHarness(t, `{"rules":[],"channels":{"webhook":{"enabled":true,"url":"https://metadata.example/hook"}}}`)
	h.e.opt.LookupIP = func(_ context.Context, host string) ([]netip.Addr, error) {
		if host != "metadata.example" {
			t.Errorf("looked up %q", host)
		}
		// One public, one metadata: any blocked address refuses.
		return []netip.Addr{netip.MustParseAddr("93.184.216.34"), netip.MustParseAddr("169.254.169.254")}, nil
	}
	d := h.e.Test(context.Background())
	if d.Webhook == nil || d.Webhook.OK || d.Webhook.Error != "blocked: resolves to a local address" {
		t.Fatalf("webhook = %+v", d.Webhook)
	}
	if n := len(h.doer.sent()); n != 0 {
		t.Fatalf("%d requests sent", n)
	}

	h.e.opt.LookupIP = func(context.Context, string) ([]netip.Addr, error) { return nil, errors.New("no such host") }
	if d = h.e.Test(context.Background()); d.Webhook.OK || !strings.HasPrefix(d.Webhook.Error, "lookup") {
		t.Fatalf("lookup failure: %+v", d.Webhook)
	}
}

func TestPrivateAddresses(t *testing.T) {
	lan := netip.MustParseAddr("192.168.1.20")
	if blockedAddr(lan, false) {
		t.Fatal("LAN address blocked by default (upstream allows self-hosted ntfy)")
	}
	if !blockedAddr(lan, true) || !blockedAddr(netip.MustParseAddr("fd00::1"), true) {
		t.Fatal("BlockPrivate didn't block")
	}
	for _, a := range []string{"127.0.0.1", "0.1.2.3", "169.254.169.254", "::1", "::", "fe80::1", "::ffff:169.254.169.254", "::127.0.0.1"} {
		if !blockedAddr(netip.MustParseAddr(a), false) {
			t.Errorf("%s not blocked", a)
		}
	}
	for _, a := range []string{"8.8.8.8", "100.64.0.1", "10.0.0.1", "2606:4700::1111"} {
		if blockedAddr(netip.MustParseAddr(a), false) {
			t.Errorf("%s blocked", a)
		}
	}
}

// The default client re-checks the address it dials, so DNS rebinding can't reach loopback.
func TestGuardedClientRefusesLoopbackDial(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("request reached the loopback server")
	}))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL, nil)
	if _, err := newGuardedClient(func(a netip.Addr) bool { return blockedAddr(a, false) }).Do(req); err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("err = %v", err)
	}
}

// The hub's own LAN and Tailscale addresses are refused, as a literal, behind a name, and by
// the dial-time check.
func TestWebhookToOwnAddressRefused(t *testing.T) {
	h := newHarness(t, "")
	h.e.opt.LocalAddrs = func() []netip.Addr {
		return []netip.Addr{netip.MustParseAddr("192.168.0.105"), netip.MustParseAddr("100.127.35.0")}
	}
	for _, u := range []string{"http://192.168.0.105:17820/api/pair/code", "http://100.127.35.0:1/x", "http://[::ffff:192.168.0.105]/x"} {
		if r := h.e.post(context.Background(), u, map[string]any{}); r.OK || !strings.HasPrefix(r.Error, "blocked") {
			t.Errorf("%s: %+v", u, r)
		}
	}
	h.e.opt.LookupIP = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("100.127.35.0")}, nil
	}
	if r := h.e.post(context.Background(), "http://hub.tail.example/x", map[string]any{}); r.OK || !strings.HasPrefix(r.Error, "blocked") {
		t.Errorf("name for own address: %+v", r)
	}
	if !h.e.blocked(netip.MustParseAddr("100.127.35.0")) || h.e.blocked(netip.MustParseAddr("192.168.0.77")) {
		t.Error("dial-time check wrong: own address must be blocked, another LAN host not")
	}
	if n := len(h.doer.sent()); n != 0 {
		t.Fatalf("%d requests sent", n)
	}
}

// allowAll lets the guarded client reach httptest servers on loopback, so the tests below
// exercise only its redirect and proxy rules.
func allowAll(netip.Addr) bool { return false }

// A redirect is reported, never followed.
func TestGuardedClientDoesNotFollowRedirects(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("redirect target was requested")
	}))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirector.Close()
	req, _ := http.NewRequest(http.MethodPost, redirector.URL, nil)
	res, err := newGuardedClient(allowAll).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want the 302 itself", res.StatusCode)
	}
}

// The client never uses a proxy: one would hide where the request really goes.
func TestGuardedClientIgnoresProxyEnvironment(t *testing.T) {
	tr := newGuardedClient(allowAll).Transport.(*http.Transport)
	if tr.Proxy != nil {
		t.Fatal("guarded client has a proxy function")
	}
}

// ---- settings and persistence ------------------------------------------------------------

func TestDefaultSettings(t *testing.T) {
	s := DefaultSettings()
	if len(s.Rules) != 4 || s.Rules[0].Type != TypeOffline || s.Rules[0].Minutes != 10 || s.Rules[0].Threshold != nil ||
		*s.Rules[1].Threshold != 90 || *s.Rules[2].Threshold != 85 || *s.Rules[3].Threshold != 90 {
		t.Fatalf("rules = %+v", s.Rules)
	}
	if !s.Channels.Desktop || s.Channels.Ntfy.Server != DefaultNtfyServer || s.Channels.Ntfy.Enabled ||
		!s.NotifyResolved || s.TempUnit != "C" {
		t.Fatalf("settings = %+v", s)
	}
	for _, r := range s.Rules {
		if !ruleIDRE.MatchString(r.ID) || !r.Enabled {
			t.Fatalf("rule %+v", r)
		}
	}
}

func TestParseSettings(t *testing.T) {
	bad := []string{
		`[]`, `{}`, `{"rules":[{"type":"nope","device":"*"}]}`,
		`{"rules":[{"type":"cpu","device":"*","threshold":0}]}`,
		`{"rules":[{"type":"cpu","device":"*","threshold":101}]}`,
		`{"rules":[{"type":"cpu","device":"bad id!","threshold":50}]}`,
		`{"rules":[{"type":"offline","device":"*","minutes":0}]}`,
		`{"rules":[{"type":"cpu","device":"*","threshold":50,"minutes":1441}]}`,
		`{"rules":[{"type":"dailyCost","device":"*","threshold":null}]}`,
		`{"rules":[],"channels":{"ntfy":{"topic":"has space"}}}`,
		`{"rules":[],"channels":{"ntfy":{"email":"nope"}}}`,
		`{"rules":[],"channels":{"ntfy":{"server":"http://127.0.0.1"}}}`,
	}
	for _, in := range bad {
		if _, err := ParseSettings([]byte(in)); err == nil {
			t.Errorf("accepted %s", in)
		}
	}
	s, err := ParseSettings([]byte(`{"rules":[
		{"id":"ABC","type":"cpu","device":"pc-1.lan","threshold":"75","minutes":2.5},
		{"id":"0123456789ab","type":"offline","device":"*","minutes":1,"enabled":false}],
		"channels":{"desktop":false,"ntfy":{"enabled":true,"topic":""},"webhook":{"enabled":true,"url":"https://hooks.slack.com/x/"}},
		"tempUnit":"K"}`))
	if err != nil {
		t.Fatal(err)
	}
	r := s.Rules[0]
	if r.ID == "ABC" || !ruleIDRE.MatchString(r.ID) || *r.Threshold != 75 || r.Minutes != 3 || !r.Enabled {
		t.Fatalf("rule 0 = %+v", r)
	}
	if s.Rules[1].ID != "0123456789ab" || s.Rules[1].Enabled {
		t.Fatalf("rule 1 = %+v", s.Rules[1])
	}
	if s.Channels.Desktop || s.Channels.Ntfy.Enabled || !s.Channels.Webhook.Enabled ||
		s.Channels.Webhook.URL != "https://hooks.slack.com/x" || s.TempUnit != "C" {
		t.Fatalf("channels = %+v", s)
	}
}

func TestPersistence(t *testing.T) {
	paths := config.At(t.TempDir())
	clock := newClock()
	devices := func() []Device { return []Device{{ID: "pc1", Name: "Box", Online: true, LastSeen: clock.t}} }
	opts := Options{Paths: paths, Devices: devices, Now: clock.now, HTTP: &fakeDoer{}}

	e := New(opts)
	if _, err := os.Stat(paths.Alerts); err != nil {
		t.Fatalf("defaults not saved: %v", err)
	}
	if _, err := e.Update([]byte(rule(TypeCPU, 50, 0))); err != nil {
		t.Fatal(err)
	}
	e.OnReport("pc1", &report.Report{CPU: report.CPU{Usage: report.Ptr(99.0)}})
	e.Wait()

	e2 := New(opts)
	if a := e2.Active(); len(a) != 1 || a[0].Title != "Box: CPU load 99%" || a[0].Since != clock.ms(0) {
		t.Fatalf("active after reload = %+v", a)
	}
	if evs := e2.Events(0); len(evs) != 1 || evs[0].State != Firing {
		t.Fatalf("log after reload = %+v", evs)
	}
	// Still firing after reload, so it resolves rather than firing again.
	var got []Message
	e2.opt.Broadcast = func(m Message) { got = append(got, m) }
	e2.OnReport("pc1", &report.Report{CPU: report.CPU{Usage: report.Ptr(1.0)}})
	if len(got) != 1 || got[0].Event.State != Resolved {
		t.Fatalf("after reload: %+v", got)
	}
	if runtime.GOOS != "windows" {
		for _, p := range []string{paths.Alerts, paths.AlertsLog} {
			if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 {
				t.Fatalf("%s: %v %v", p, fi.Mode(), err)
			}
		}
	}
}

// A burst of events writes alerts-log.json once, logDelay later; Wait writes it at once.
func TestLogWriteDebounced(t *testing.T) {
	paths := config.At(t.TempDir())
	clock := newClock()
	devices := func() []Device { return []Device{{ID: "pc1", Name: "Box", Online: true, LastSeen: clock.t}} }
	e := New(Options{Paths: paths, Devices: devices, Now: clock.now, HTTP: &fakeDoer{}})
	if _, err := e.Update([]byte(rule(TypeCPU, 50, 0))); err != nil {
		t.Fatal(err)
	}
	for _, v := range []float64{99, 1, 99} {
		e.OnReport("pc1", &report.Report{CPU: report.CPU{Usage: report.Ptr(v)}})
	}
	if _, err := os.Stat(paths.AlertsLog); err == nil {
		t.Fatal("log written synchronously")
	}
	e.Wait()
	var evs []Event
	b, _ := os.ReadFile(paths.AlertsLog)
	if json.Unmarshal(b, &evs) != nil || len(evs) != 3 {
		t.Fatalf("log after Wait = %s", b)
	}

	// Without Wait, the timer writes it.
	e.OnReport("pc1", &report.Report{CPU: report.CPU{Usage: report.Ptr(1.0)}})
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, _ = os.ReadFile(paths.AlertsLog)
		if json.Unmarshal(b, &evs) == nil && len(evs) == 4 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("debounced write never happened: %s", b)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Clearing writes at once.
	e.ClearLog()
	if b, _ = os.ReadFile(paths.AlertsLog); string(b) != "[]" {
		t.Fatalf("log after clear = %s", b)
	}
}

// The power estimate is computed only when an enabled power rule covers the device.
func TestWattsOnlyForPowerRules(t *testing.T) {
	h := newHarness(t, rule(TypeCPU, 50, 0))
	h.e.OnReport("pc1", &report.Report{})
	if h.energy.calls != 0 {
		t.Fatalf("Watts called %d times without a power rule", h.energy.calls)
	}
	if _, err := h.e.Update([]byte(`{"rules":[{"type":"power","device":"other","threshold":100}]}`)); err != nil {
		t.Fatal(err)
	}
	h.e.OnReport("pc1", &report.Report{})
	if h.energy.calls != 0 {
		t.Fatal("Watts called for a power rule on another device")
	}
	if _, err := h.e.Update([]byte(rule(TypePower, 100, 0))); err != nil {
		t.Fatal(err)
	}
	h.e.OnReport("pc1", &report.Report{})
	if h.energy.calls != 1 {
		t.Fatalf("Watts called %d times with a power rule, want 1", h.energy.calls)
	}
}

// Without a hub URL, click and link still go out (upstream sends the bare path).
func TestLinkWithoutHubURL(t *testing.T) {
	h := newHarness(t, `{"rules":[],`+channels+`}`)
	h.e.opt.HubURL = nil
	h.e.Test(context.Background())
	reqs := h.doer.sent()
	if len(reqs) != 2 || reqs[0].body["click"] != "/#/alerts" || reqs[1].body["link"] != "/#/alerts" {
		t.Fatalf("requests = %+v", reqs)
	}
}

func TestMoney(t *testing.T) {
	cases := map[string]string{
		money(1234567.891, "USD"): "$1,234,567.89",
		money(0.5, "eur"):         "€0.50",
		money(1500, "JPY"):        "¥1,500",
		money(12, "CHF"):          "CHF 12.00",
		money(3, ""):              "$3.00",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

// After a hub restart every device looks offline; none of them coming back is news, since the
// hub never saw them go (skeptic pass 2, restart flood).
func TestNoBackOnlineForDevicesNeverSeenOffline(t *testing.T) {
	clock := newClock()
	dev := []Device{{ID: "pc1", Name: "Box", LastSeen: clock.t.Add(-time.Minute)}}
	var msgs []Message
	e := New(Options{
		Devices:   func() []Device { return dev },
		Now:       clock.now,
		Broadcast: func(m Message) { msgs = append(msgs, m) },
		HTTP:      &fakeDoer{},
	})
	e.mu.Lock()
	e.cfg.Rules = []Rule{{ID: "aaaaaaaaaaaa", Type: TypeOffline, Device: AllDevices, Enabled: true, Minutes: 10}}
	e.state[key("aaaaaaaaaaaa", "pc1")] = &state{since: clock.t, firing: true, firedAt: clock.t, carried: true}
	e.mu.Unlock()
	dev[0].Online = true
	e.OnReport("pc1", &report.Report{})
	e.Wait()
	if len(msgs) != 0 {
		t.Fatalf("broadcast on return: %+v", msgs)
	}
	if a := e.Active(); len(a) != 0 {
		t.Fatalf("still active: %+v", a)
	}
}

// A failed delivery logs and reports the host, never the path or query that holds the secret.
func TestDeliveryErrorsHideWebhookSecret(t *testing.T) {
	h := newHarness(t, "")
	const full = "https://hooks.example/api/webhooks/123456/SECRET-TOKEN?k=SECRET2"
	h.e.client = errDoer{}
	r := h.e.postRedacted(context.Background(), full, map[string]any{})
	if r.OK || strings.Contains(r.Error, "SECRET") || !strings.Contains(r.Error, "https://hooks.example") {
		t.Fatalf("error = %q", r.Error)
	}
}

type errDoer struct{}

func (errDoer) Do(r *http.Request) (*http.Response, error) {
	return nil, &url.Error{Op: "Post", URL: r.URL.String(), Err: errors.New("connection refused")}
}
