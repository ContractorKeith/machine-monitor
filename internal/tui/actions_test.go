package tui

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/exp/golden"

	"github.com/ContractorKeith/machine-monitor/internal/client"
	"github.com/ContractorKeith/machine-monitor/internal/hub"
	"github.com/ContractorKeith/machine-monitor/internal/report"
)

// actionFake is a fakeHub that also serves processes and actions.
type actionFake struct {
	*fakeHub
	pmu   sync.Mutex
	procs map[string]*client.Processes
}

func (f *actionFake) Processes(ctx context.Context, id string) (*client.Processes, error) {
	if err := f.record("processes " + id); err != nil {
		return nil, err
	}
	f.pmu.Lock()
	defer f.pmu.Unlock()
	if p := f.procs[id]; p != nil {
		return p, nil
	}
	return &client.Processes{}, nil
}

func (f *actionFake) Action(ctx context.Context, id, action string) error {
	return f.record("action " + id + " " + action)
}

// actionDevices is threeDevices with the workstation allowing restart and shutdown.
func actionDevices() []client.Device {
	devs := threeDevices()
	devs[0].Latest.Actions = []string{"restart", "shutdown"}
	devs[0].Latest.Features = []string{"procs", "actions"}
	return devs
}

func newActionFake() *actionFake {
	devs := actionDevices()
	ws := devs[0].ID
	return &actionFake{fakeHub: newFakeHub(devs), procs: map[string]*client.Processes{
		ws: {Supported: true, Procs: &hub.ProcSnapshot{T: hubNow.UnixMilli(), List: []report.Process{
			{PID: p(4242), Name: report.Ptr("ollama"), User: report.Ptr("keith"), CPU: p(88), Mem: p(6 * gib)},
			{PID: p(17), Name: report.Ptr("sshd"), User: report.Ptr("root"), CPU: p(0.1), Mem: p(8 * mib)},
		}}},
		"mac-0001": {Supported: false},
	}}
}

// actModel is a hub-mode model on f at w×h with its snapshot applied.
func actModel(t *testing.T, f *actionFake, w, h int) model {
	t.Helper()
	m := New(Options{Hub: f, Interval: time.Hour, Now: func() time.Time { return hubNow }}).(model)
	next, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	m = next.(model)
	for _, e := range f.events {
		next, _ = m.Update(hubMsg{e})
		m = next.(model)
	}
	return m
}

// runActs is run for the actions' messages: it feeds back what hub requests answer.
func runActs(t *testing.T, m model, msgs ...tea.Msg) model {
	t.Helper()
	for _, msg := range msgs {
		next, cmd := m.Update(msg)
		m = next.(model)
		if cmd == nil {
			continue
		}
		out := make(chan tea.Msg, 1)
		go func() { out <- cmd() }()
		select {
		case res := <-out:
			switch res.(type) {
			case actionMsg, remoteProcsMsg:
				next, _ = m.Update(res)
				m = next.(model)
			}
		case <-time.After(100 * time.Millisecond):
		}
	}
	return m
}

func TestRemoteProcesses(t *testing.T) {
	f := newActionFake()
	ws := actionDevices()[0].ID
	m := actModel(t, f, 120, 30)
	m = runActs(t, m, keyMsg("3"))
	if got := f.Calls(); !slices.Equal(got, []string{"processes " + ws}) {
		t.Fatalf("calls = %q", got)
	}
	out := plain(m)
	if !strings.Contains(out, "ollama") || !strings.Contains(out, "2 of 2 processes") {
		t.Fatalf("processes view:\n%s", out)
	}
	m = runActs(t, m, keyMsg("/"), keyMsg("s"), keyMsg("s"), keyMsg("h"), keyMsg("enter"))
	if v := m.visibleProcs(); len(v) != 1 || *v[0].Name != "sshd" {
		t.Errorf("filter on a remote list: %d rows", len(v))
	}

	// The poll goes on while the view is open and stops when it closes.
	gen := m.act.procsGen
	next, cmd := m.Update(remoteTickMsg{gen})
	if cmd == nil {
		t.Fatal("no poll while Processes is open")
	}
	m = runActs(t, next.(model), keyMsg("1"))
	if _, cmd := m.Update(remoteTickMsg{gen}); cmd != nil {
		t.Error("still polling after Processes closed")
	}
	if _, cmd := m.Update(remoteProcsMsg{gen: gen, id: ws}); cmd != nil {
		t.Error("a late reply restarted the poll")
	}

	// Another device: its list, never the last one's.
	m = runActs(t, m, keyMsg("l"), keyMsg("3"))
	if out := plain(m); strings.Contains(out, "ollama") || !strings.Contains(out, "studio doesn't send its process list") {
		t.Errorf("unsupported device:\n%s", out)
	}
}

func TestRemoteProcessesNotes(t *testing.T) {
	f := newActionFake()
	m := actModel(t, f, 120, 30)
	m = runActs(t, m, keyMsg("j"), keyMsg("j"), keyMsg("3")) // the offline NAS
	if out := plain(m); !strings.Contains(out, "nas-02 is offline.") {
		t.Errorf("offline device:\n%s", out)
	}
	// A hub without the actions API (an older client) says so.
	m = hubModel(t, newFakeHub(threeDevices()), 120, 30)
	m = run(t, m, keyMsg("3"))
	if out := plain(m); !strings.Contains(out, "not available") {
		t.Errorf("no ActionHub:\n%s", out)
	}
}

func TestWakeKey(t *testing.T) {
	f := newActionFake()
	m := actModel(t, f, 160, 48)
	m = runActs(t, m, keyMsg("w")) // the workstation is on
	if len(f.Calls()) != 0 || !strings.Contains(m.hubStatus(), "already on") {
		t.Errorf("wake on an online device: %q, footer %q", f.Calls(), m.hubStatus())
	}
	m = runActs(t, m, keyMsg("j"), keyMsg("j"), keyMsg("w"))
	if got := f.Calls(); !slices.Equal(got, []string{"action nas-0002 wake"}) {
		t.Errorf("calls = %q", got)
	}
	if !strings.Contains(m.hubStatus(), "Wake-up signal sent to nas-02") {
		t.Errorf("footer = %q", m.hubStatus())
	}
}

func TestPowerMenu(t *testing.T) {
	f := newActionFake()
	ws := actionDevices()[0].ID
	m := actModel(t, f, 160, 48)

	m = runActs(t, m, keyMsg("P"))
	if s := m.hubStatus(); !strings.Contains(s, "r restart · S shutdown") || strings.Contains(s, "sleep") {
		t.Errorf("menu = %q", s)
	}
	m = runActs(t, m, keyMsg("s")) // sleep isn't allowed: closes the menu
	if m.act.menu != "" || len(f.Calls()) != 0 {
		t.Errorf("disallowed key: menu %q calls %q", m.act.menu, f.Calls())
	}
	m = runActs(t, m, keyMsg("P"), keyMsg("r"))
	if s := m.hubStatus(); !strings.Contains(s, "Restart ws-01? y to confirm") {
		t.Errorf("confirm = %q", s)
	}
	m = runActs(t, m, keyMsg("n"))
	if len(f.Calls()) != 0 {
		t.Errorf("cancelled restart sent: %q", f.Calls())
	}
	m = runActs(t, m, keyMsg("P"), keyMsg("S"), keyMsg("y"))
	if got := f.Calls(); !slices.Equal(got, []string{"action " + ws + " shutdown"}) {
		t.Errorf("calls = %q", got)
	}
	if !strings.Contains(m.hubStatus(), "Shutdown sent to ws-01") {
		t.Errorf("footer = %q", m.hubStatus())
	}

	raw, _ := json.Marshal(map[string]any{"type": "actionResult", "id": ws, "name": "ws-01",
		"result": map[string]any{"id": "0123456789ab", "ok": true, "message": "Shutting\x1b down."}})
	m = runActs(t, m, hubMsg{client.Event{Type: client.EventActionResult, ID: ws, Raw: raw}})
	if s := m.hubStatus(); s != "ws-01: Shutting down." {
		t.Errorf("actionResult footer = %q", s)
	}

	// The device drops off (or turns actions off) while the confirmation is up: not sent.
	m = runActs(t, m, keyMsg("P"), keyMsg("r"))
	m = runActs(t, m, hubMsg{client.Event{Type: client.EventStatus, Online: map[string]bool{ws: false}}})
	m = runActs(t, m, keyMsg("y"))
	if len(f.Calls()) != 1 || !strings.Contains(m.hubStatus(), "went offline") {
		t.Errorf("sent to an offline device: %q, footer %q", f.Calls(), m.hubStatus())
	}
	m = runActs(t, m, hubMsg{client.Event{Type: client.EventStatus, Online: map[string]bool{ws: true}}})
	m = runActs(t, m, keyMsg("P"), keyMsg("r"))
	m.editDev(ws, func(d *device) { r := *d.last; r.Actions = []string{"shutdown"}; d.last = &r })
	m = runActs(t, m, keyMsg("y"))
	if len(f.Calls()) != 1 || !strings.Contains(m.hubStatus(), "no longer allowed") {
		t.Errorf("sent a withdrawn action: %q, footer %q", f.Calls(), m.hubStatus())
	}

	m = runActs(t, m, keyMsg("l"), keyMsg("P")) // the Mac doesn't allow power actions
	if m.act.menu != "" || !strings.Contains(m.hubStatus(), "--allow-power-actions") {
		t.Errorf("menu %q footer %q", m.act.menu, m.hubStatus())
	}
}

func TestActionsGolden(t *testing.T) {
	cases := []struct {
		name string
		keys []string
	}{
		{"processes_remote_80x24", []string{"3"}},
		{"power_menu_80x24", []string{"P"}},
		{"power_confirm_80x24", []string{"P", "r"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := runActs(t, actModel(t, newActionFake(), 80, 24), keys(c.keys...)...)
			golden.RequireEqual(t, plain(m))
		})
	}
}

var _ ActionHub = (*client.Client)(nil) // the connected TUI's hub serves actions
