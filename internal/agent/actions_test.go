package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/config"
	"github.com/ContractorKeith/machine-monitor/internal/hub"
	"github.com/ContractorKeith/machine-monitor/internal/report"
)

// fakeExec records the power actions it was asked to run.
type fakeExec struct {
	mu  sync.Mutex
	ran []string
}

func (f *fakeExec) run(_ context.Context, action string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ran = append(f.ran, action)
	return nil
}

func (f *fakeExec) actions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.ran)
}

var cmdNow = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

func signed(key, device, id, action string, t time.Time) Command {
	return Command{ID: id, Action: action, T: t.UnixMilli(), Sig: CommandSig(key, device, id, action, t.UnixMilli())}
}

func TestVerify(t *testing.T) {
	good := signed(ourKey, "dev-1", "a1b2c3d4e5f6", "restart", cmdNow)
	cases := []struct {
		name  string
		c     Command
		allow bool
		ok    bool
	}{
		{"valid", good, true, true},
		{"actions off", good, false, false},
		{"unsigned", Command{ID: good.ID, Action: good.Action, T: good.T}, true, false},
		{"wrong key", signed("ffffffffffffffffffffffff", "dev-1", good.ID, "restart", cmdNow), true, false},
		{"other device", signed(ourKey, "dev-2", good.ID, "restart", cmdNow), true, false},
		{"action swapped", Command{ID: good.ID, Action: "shutdown", T: good.T, Sig: good.Sig}, true, false},
		{"time swapped", Command{ID: good.ID, Action: good.Action, T: good.T - 1, Sig: good.Sig}, true, false},
		{"expired", signed(ourKey, "dev-1", good.ID, "restart", cmdNow.Add(-CommandTTL-time.Second)), true, false},
		{"from the future", signed(ourKey, "dev-1", good.ID, "restart", cmdNow.Add(CommandTTL+time.Second)), true, false},
		{"skewed but in time", signed(ourKey, "dev-1", good.ID, "restart", cmdNow.Add(30*time.Second)), true, true},
		{"unknown action", signed(ourKey, "dev-1", good.ID, "format", cmdNow), true, false},
		{"bad id", signed(ourKey, "dev-1", "A1B2C3D4E5F6", "restart", cmdNow), true, false},
	}
	for _, c := range cases {
		g := guard{key: ourKey, allow: c.allow}
		if err := g.verify(c.c, "dev-1", cmdNow); (err == nil) != c.ok {
			t.Errorf("%s: verify = %v, want ok %v", c.name, err, c.ok)
		}
	}

	g := guard{key: ourKey, allow: true}
	if err := g.verify(good, "dev-1", cmdNow); err != nil {
		t.Fatal(err)
	}
	if err := g.verify(good, "dev-1", cmdNow.Add(time.Second)); err == nil {
		t.Error("replayed command accepted")
	}
	if err := g.verify(good, "dev-1", cmdNow.Add(3*CommandTTL)); err == nil {
		t.Error("replay after the id was forgotten accepted (the TTL must refuse it)")
	}
	if len(g.seen) != 0 {
		t.Errorf("seen ids not pruned: %v", g.seen)
	}
}

// TestBadCommandsIgnored: a hub reply carrying unsigned, wrongly signed, expired and
// replayed commands runs only the one good command, once, and reports its result.
func TestBadCommandsIgnored(t *testing.T) {
	f := &fakeHub{t: t, hubKey: ourKey}
	u := startFake(t, f)
	ex := &fakeExec{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var logs []string
	var lmu sync.Mutex
	logf := func(format string, args ...any) {
		lmu.Lock()
		defer lmu.Unlock()
		logs = append(logs, format)
	}
	go Run(ctx, Options{HubURL: u, Key: ourKey, Collector: fakeCollector{"x"}, AllowPowerActions: true,
		Exec: ex.run, Now: func() time.Time { return cmdNow }, Interval: time.Millisecond, Logf: logf})
	waitFor(t, "a report", func() bool { return len(f.reports()) >= 1 })

	var first struct {
		Features []string `json:"features"`
		Actions  []string `json:"actions"`
	}
	json.Unmarshal(f.reports()[0], &first)
	if !slices.Equal(first.Features, []string{"actions"}) || !slices.Equal(first.Actions, PowerActions) {
		t.Fatalf("report advertises %v %v", first.Features, first.Actions)
	}

	good := signed(ourKey, "x", "0123456789ab", "sleep", cmdNow)
	f.set(func() {
		f.cmds = []Command{
			{ID: "aaaaaaaaaaaa", Action: "shutdown", T: cmdNow.UnixMilli()}, // unsigned
			signed("ffffffffffffffffffffffff", "x", "bbbbbbbbbbbb", "shutdown", cmdNow),
			signed(ourKey, "x", "cccccccccccc", "shutdown", cmdNow.Add(-2*CommandTTL)),
			good,
		}
	})
	waitFor(t, "the good command to run", func() bool { return len(ex.actions()) == 1 })
	f.set(func() { f.cmds = []Command{good} }) // replayed
	n := len(f.reports())
	waitFor(t, "two more reports", func() bool { return len(f.reports()) >= n+2 })
	if got := ex.actions(); !slices.Equal(got, []string{"sleep"}) {
		t.Errorf("ran %v, want only the one good sleep", got)
	}

	sawResult := false
	for _, b := range f.reports() {
		var r report.Report
		json.Unmarshal(b, &r)
		if r.ActionResult != nil {
			if sawResult || r.ActionResult.ID != good.ID || !r.ActionResult.OK {
				t.Errorf("actionResult %+v", r.ActionResult)
			}
			sawResult = true
		}
	}
	if !sawResult {
		t.Error("no actionResult reported")
	}
	lmu.Lock()
	ignored := 0
	for _, l := range logs {
		if strings.Contains(l, "ignored") {
			ignored++
		}
	}
	lmu.Unlock()
	if ignored != 4 {
		t.Errorf("%d ignored commands logged, want 4", ignored)
	}
}

func TestActionsOffByDefault(t *testing.T) {
	f := &fakeHub{t: t, hubKey: ourKey}
	u := startFake(t, f)
	ex := &fakeExec{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, Options{HubURL: u, Key: ourKey, Collector: fakeCollector{"x"}, Exec: ex.run,
		Now: func() time.Time { return cmdNow }, Interval: time.Millisecond, Logf: quiet})
	waitFor(t, "a report", func() bool { return len(f.reports()) >= 1 })
	f.set(func() { f.cmds = []Command{signed(ourKey, "x", "0123456789ab", "shutdown", cmdNow)} })
	n := len(f.reports())
	waitFor(t, "two more reports", func() bool { return len(f.reports()) >= n+2 })
	if got := ex.actions(); len(got) != 0 {
		t.Errorf("ran %v with power actions off", got)
	}
	var r report.Report
	json.Unmarshal(f.reports()[0], &r)
	if len(r.Actions) != 0 || slices.Contains(r.Features, "actions") {
		t.Errorf("report advertises actions %v features %v", r.Actions, r.Features)
	}
}

// testClock is a settable clock for the real hub.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *testClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// clockedHub serves a real hub on clk over httptest.
func clockedHub(t *testing.T, clk func() time.Time) (*hub.Hub, *httptest.Server) {
	t.Helper()
	h, err := hub.New(hub.Options{Paths: config.At(t.TempDir()), Hostname: "testhub", Now: clk, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(func() { srv.Close(); h.Close() })
	return h, srv
}

func postKeyed(t *testing.T, srv *httptest.Server, key, path, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func latestOf(t *testing.T, srv *httptest.Server, key, id string) *hub.Latest {
	t.Helper()
	var devs []hub.View
	json.NewDecoder(keyed(t, srv, key, "GET", "/api/devices").Body).Decode(&devs)
	for _, d := range devs {
		if d.ID == id {
			return d.Latest
		}
	}
	return nil
}

// TestPowerActionEndToEnd: the TUI asks the hub to restart a device; the agent gets the
// signed command in its report reply, runs it through the executor and the hub sees the
// result.
func TestPowerActionEndToEnd(t *testing.T) {
	h, srv := realHub(t)
	ex := &fakeExec{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, Options{HubURL: srv.URL, Key: h.Token(), Collector: fakeCollector{"e2e"}, AllowPowerActions: true,
		Exec: ex.run, Interval: 10 * time.Millisecond, Logf: t.Logf})
	waitFor(t, "the device on the hub", func() bool { return latestOf(t, srv, h.Token(), "e2e") != nil })

	code, body := postKeyed(t, srv, h.Token(), "/api/devices/e2e/action", `{"action":"restart"}`)
	if code != 200 {
		t.Fatalf("action: %d %v", code, body)
	}
	waitFor(t, "the restart to run", func() bool { return len(ex.actions()) == 1 })
	waitFor(t, "the result on the hub", func() bool {
		l := latestOf(t, srv, h.Token(), "e2e")
		return l != nil && l.ActionResult != nil && l.ActionResult.ID == body["id"] && l.ActionResult.OK
	})
	if got := ex.actions(); !slices.Equal(got, []string{"restart"}) {
		t.Errorf("ran %v", got)
	}
}

func TestPowerActionRefusedByHubWhenOff(t *testing.T) {
	h, srv := realHub(t)
	ex := &fakeExec{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, Options{HubURL: srv.URL, Key: h.Token(), Collector: fakeCollector{"off"}, Exec: ex.run,
		Interval: 10 * time.Millisecond, Logf: t.Logf})
	waitFor(t, "the device on the hub", func() bool { return latestOf(t, srv, h.Token(), "off") != nil })
	if code, body := postKeyed(t, srv, h.Token(), "/api/devices/off/action", `{"action":"shutdown"}`); code != 409 {
		t.Errorf("shutdown with actions off: %d %v, want 409", code, body)
	}
}

// TestProcessesEndToEnd: the agent lists processes only while someone keeps asking the hub
// for them, and stops once the hub's watch runs out.
func TestProcessesEndToEnd(t *testing.T) {
	clk := &testClock{t: time.Now()}
	h, srv := clockedHub(t, clk.Now)
	var mu sync.Mutex
	calls := 0
	procs := func(context.Context) ([]report.Process, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		return []report.Process{{PID: report.Ptr(4.0), Name: report.Ptr("ollama"), CPU: report.Ptr(90.0)}}, nil
	}
	count := func() int { mu.Lock(); defer mu.Unlock(); return calls }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, Options{HubURL: srv.URL, Key: h.Token(), Collector: fakeCollector{"pp"}, Procs: procs,
		Interval: 5 * time.Millisecond, Logf: t.Logf})
	waitFor(t, "the device on the hub", func() bool { return latestOf(t, srv, h.Token(), "pp") != nil })
	time.Sleep(50 * time.Millisecond)
	if n := count(); n != 0 {
		t.Fatalf("processes listed %d times before anyone watched", n)
	}

	get := func() hub.ProcessesReply {
		var p hub.ProcessesReply
		json.NewDecoder(keyed(t, srv, h.Token(), "GET", "/api/devices/pp/processes").Body).Decode(&p)
		return p
	}
	if p := get(); !p.Supported {
		t.Error("agent with Procs not marked supported")
	}
	waitFor(t, "the list on the hub", func() bool {
		p := get()
		return p.Procs != nil && len(p.Procs.List) == 1 && *p.Procs.List[0].Name == "ollama"
	})

	clk.Add(hub.ProcsWatch + time.Second) // the viewer left
	time.Sleep(50 * time.Millisecond)     // the next reply says stop
	n := count()
	time.Sleep(100 * time.Millisecond)
	if m := count(); m > n+1 {
		t.Errorf("processes still listed after the viewer left: %d more times", m-n)
	}
}

func TestPowerCommand(t *testing.T) {
	for _, c := range []struct{ goos, action, want string }{
		{"linux", "sleep", "systemctl suspend"},
		{"linux", "restart", "systemctl reboot"},
		{"linux", "shutdown", "systemctl poweroff"},
		{"darwin", "sleep", "/usr/bin/pmset sleepnow"},
		{"darwin", "restart", "to restart"},
		{"darwin", "shutdown", "to shut down"},
		{"windows", "sleep", "SetSuspendState"},
		{"windows", "restart", "shutdown.exe /r /t 0"},
		{"windows", "shutdown", "shutdown.exe /s /t 0"},
	} {
		argv, err := powerCommand(c.goos, c.action)
		if err != nil || !strings.Contains(strings.Join(argv, " "), c.want) {
			t.Errorf("%s %s: %q %v, want %q", c.goos, c.action, argv, err, c.want)
		}
	}
	if _, err := powerCommand("linux", "format"); err == nil {
		t.Error("unknown action accepted")
	}
	if _, err := powerCommand("plan9", "sleep"); err == nil {
		t.Error("unsupported OS accepted")
	}
}
