package hub

import (
	"context"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/config"
	"github.com/ContractorKeith/machine-monitor/internal/report"
)

// report posts one report for id and returns the hub's reply.
func (th *testHub) report(id string, edit func(m map[string]any)) reportReply {
	th.t.Helper()
	w := th.req("POST", "/api/report", "", th.agentReport(id, edit))
	if w.Code != 200 {
		th.t.Fatalf("report: %d %s", w.Code, w.Body)
	}
	return decode[reportReply](th.t, w)
}

// allowing is a report edit for an agent run with --allow-power-actions.
func allowing(actions ...string) func(m map[string]any) {
	return func(m map[string]any) {
		m["features"] = []string{"actions"}
		m["actions"] = actions
	}
}

func (th *testHub) action(id, action string) (int, map[string]any) {
	th.t.Helper()
	w := th.keyed("POST", "/api/devices/"+id+"/action", `{"action":"`+action+`"}`)
	return w.Code, decode[map[string]any](th.t, w)
}

func TestActionRefusals(t *testing.T) {
	th := newTestHub(t)
	th.report("dev-1", nil)
	cases := []struct {
		id, action string
		want       int
	}{
		{"nope-1", "restart", 404},
		{"dev-1", "reboot", 400},
		{"dev-1", "", 400},
		{"dev-1", "restart", 409}, // actions are off unless the agent allows them
		{"dev-1", "wake", 400},    // no MAC reported
	}
	for _, c := range cases {
		if code, body := th.action(c.id, c.action); code != c.want {
			t.Errorf("%s %q: %d %v, want %d", c.id, c.action, code, body, c.want)
		}
	}
	th.report("dev-1", allowing("sleep"))
	if code, _ := th.action("dev-1", "shutdown"); code != 409 {
		t.Errorf("shutdown while only sleep is allowed: %d, want 409", code)
	}
	th.clk.Add(OfflineAfter + time.Second)
	if code, body := th.action("dev-1", "sleep"); code != 409 {
		t.Errorf("sleep while offline: %d %v, want 409", code, body)
	}
	if w := th.req("POST", "/api/devices/dev-1/action", "", `{"action":"sleep"}`); w.Code != 401 {
		t.Errorf("no key: %d, want 401", w.Code)
	}
}

func TestWake(t *testing.T) {
	var got []string
	old := wakeFunc
	wakeFunc = func(macs []string) error { got = macs; return nil }
	t.Cleanup(func() { wakeFunc = old })

	th := newTestHub(t)
	th.report("dev-1", func(m map[string]any) { m["macs"] = []string{"AA-BB-CC-DD-EE-01"} })
	th.clk.Add(time.Hour) // asleep: wake works on offline devices
	srv := httptest.NewServer(th.h)
	defer srv.Close()
	s := openStream(t, srv, th.Token())
	s.next("snapshot")
	if code, body := th.action("dev-1", "wake"); code != 200 || body["ok"] != true {
		t.Fatalf("wake: %d %v", code, body)
	}
	if !slices.Equal(got, []string{"aa:bb:cc:dd:ee:01"}) {
		t.Errorf("woke %v", got)
	}
	ev := s.next("actionResult")
	if ev["id"] != "dev-1" || ev["result"].(map[string]any)["ok"] != true {
		t.Errorf("actionResult %v", ev)
	}
	th.Close() // ends the stream, so srv.Close doesn't wait on it
	for range s.ch {
	}
}

func TestCommandSignedAndDeliveredOnce(t *testing.T) {
	th := newTestHub(t)
	th.report("dev-1", allowing("sleep", "restart", "shutdown"))
	code, body := th.action("dev-1", "restart")
	id, _ := body["id"].(string)
	if code != 200 || len(id) != 12 {
		t.Fatalf("queue: %d %v", code, body)
	}
	th.clk.Add(2 * time.Second)
	rep := th.report("dev-1", allowing("sleep", "restart", "shutdown"))
	if len(rep.Commands) != 1 {
		t.Fatalf("commands %+v, want one", rep.Commands)
	}
	c := rep.Commands[0]
	queued := th.clk.Now().Add(-2 * time.Second).UnixMilli()
	if c.ID != id || c.Action != "restart" || c.T != queued {
		t.Errorf("command %+v, want id %s restart t %d", c, id, queued)
	}
	if c.Sig != SignCommand(th.Token(), "dev-1", id, "restart", queued) {
		t.Errorf("sig %s doesn't match SignCommand", c.Sig)
	}
	if c.Sig == SignCommand(th.Token(), "dev-2", id, "restart", queued) {
		t.Error("signature doesn't cover the device id")
	}
	if rep := th.report("dev-1", allowing("restart")); len(rep.Commands) != 0 {
		t.Errorf("delivered twice: %+v", rep.Commands)
	}
}

func TestCommandAskedAgainReplaces(t *testing.T) {
	th := newTestHub(t)
	th.report("dev-1", allowing("sleep", "restart"))
	th.action("dev-1", "restart")
	_, body := th.action("dev-1", "restart")
	th.action("dev-1", "sleep")
	rep := th.report("dev-1", allowing("sleep", "restart"))
	if len(rep.Commands) != 2 || rep.Commands[0].ID != body["id"] || rep.Commands[1].Action != "sleep" {
		t.Errorf("commands %+v", rep.Commands)
	}
}

func TestCommandExpiresAndNeedsPermission(t *testing.T) {
	th := newTestHub(t)
	th.report("dev-1", allowing("restart"))
	th.action("dev-1", "restart")
	th.clk.Add(CommandTTL)
	if rep := th.report("dev-1", allowing("restart")); len(rep.Commands) != 0 {
		t.Errorf("expired command delivered: %+v", rep.Commands)
	}

	th.action("dev-1", "restart")
	// The user turned power actions off on the device before it picked the command up.
	if rep := th.report("dev-1", nil); len(rep.Commands) != 0 {
		t.Errorf("command delivered to a device that no longer allows it: %+v", rep.Commands)
	}
}

func TestRemoveForgetsCommands(t *testing.T) {
	th := newTestHub(t)
	th.report("dev-1", allowing("restart"))
	th.action("dev-1", "restart")
	if w := th.keyed("DELETE", "/api/devices/dev-1", ""); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if rep := th.report("dev-1", allowing("restart")); len(rep.Commands) != 0 {
		t.Errorf("removed device got its old command: %+v", rep.Commands)
	}
}

func TestProcessesOnDemand(t *testing.T) {
	th := newTestHub(t)
	withProcs := func(m map[string]any) {
		m["features"] = []string{"procs"}
		m["processes"] = []map[string]any{{"pid": 42, "name": "ollama", "cpu": 80}, {"pid": 7}}
	}
	get := func() ProcessesReply {
		t.Helper()
		w := th.keyed("GET", "/api/devices/dev-1/processes", "")
		if w.Code != 200 {
			t.Fatalf("processes: %d %s", w.Code, w.Body)
		}
		return decode[ProcessesReply](t, w)
	}

	if rep := th.report("dev-1", withProcs); rep.Procs {
		t.Error("procs asked for before anyone watched")
	}
	if p := get(); p.Procs != nil || !p.Supported {
		t.Errorf("first GET: %+v; want no list yet (unwatched lists are dropped), supported", p)
	}
	if rep := th.report("dev-1", withProcs); !rep.Procs {
		t.Error("procs not asked for while watched")
	}
	p := get()
	if p.Procs == nil || len(p.Procs.List) != 1 || *p.Procs.List[0].Name != "ollama" {
		t.Fatalf("watched GET: %+v", p.Procs)
	}
	if rep := th.report("dev-1", withProcs); !rep.Procs {
		t.Error("procs not asked for while still watched")
	}
	for _, v := range th.devices() {
		if v.Latest != nil && v.Latest.Processes != nil {
			t.Error("processes stored with the report")
		}
	}

	th.clk.Add(ProcsWatch) // the viewer left
	if rep := th.report("dev-1", withProcs); rep.Procs {
		t.Error("procs still asked for after the watch ran out")
	}
	th.act.mu.Lock()
	left := th.act.procs["dev-1"]
	th.act.mu.Unlock()
	if left != nil {
		t.Error("process list kept after the watch ran out")
	}

	th.report("dev-2", nil)
	if p := th.keyed("GET", "/api/devices/dev-2/processes", ""); decode[ProcessesReply](t, p).Supported {
		t.Error("dev-2 doesn't send processes but is supported")
	}
	if rep := th.report("dev-2", nil); rep.Procs {
		t.Error("a device that can't send processes was asked to")
	}
	if w := th.keyed("GET", "/api/devices/nope-1/processes", ""); w.Code != 404 {
		t.Errorf("unknown device: %d", w.Code)
	}
}

// TestHubOwnProcesses: the hub lists its own processes, busiest first, only while watched.
func TestHubOwnProcesses(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	procs := func(context.Context) ([]report.Process, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		var out []report.Process
		for i := range maxProcs + 10 {
			out = append(out, report.Process{PID: report.Ptr(float64(i)), Name: report.Ptr("p"), CPU: report.Ptr(float64(i) / 2)})
		}
		return out, nil
	}
	count := func() int { mu.Lock(); defer mu.Unlock(); return calls }
	th := openHub(t, Options{Collector: fakeCollector{"hub-self"}, Procs: procs, Interval: 5 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { th.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	waitHub(t, "the hub's own report", func() bool { return len(th.devices()) == 1 })
	time.Sleep(30 * time.Millisecond)
	if n := count(); n != 0 {
		t.Fatalf("processes listed %d times unwatched", n)
	}
	var p ProcessesReply
	waitHub(t, "the hub's own process list", func() bool {
		p = decode[ProcessesReply](t, th.keyed("GET", "/api/devices/hub-self/processes", ""))
		return p.Procs != nil
	})
	if !p.Supported || len(p.Procs.List) != maxProcs || *p.Procs.List[0].CPU != float64(maxProcs+9)/2 {
		t.Errorf("list: supported %v, %d rows, first %v", p.Supported, len(p.Procs.List), *p.Procs.List[0].CPU)
	}
}

// openHub is a test hub with extra options and the real clock.
func openHub(t *testing.T, opt Options) *testHub {
	t.Helper()
	clk := &clock{t: time.Now()}
	opt.Paths, opt.Now, opt.Hostname, opt.Logf = config.At(t.TempDir()), clk.Now, "testbox", t.Logf
	h, err := New(opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	return &testHub{Hub: h, t: t, clk: clk, paths: opt.Paths, h: h.Handler()}
}

func waitHub(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}
