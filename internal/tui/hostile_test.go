package tui

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/ContractorKeith/machine-monitor/internal/alerts"
	"github.com/ContractorKeith/machine-monitor/internal/client"
	"github.com/ContractorKeith/machine-monitor/internal/hub"
	"github.com/ContractorKeith/machine-monitor/internal/power"
	"github.com/ContractorKeith/machine-monitor/internal/report"
	"github.com/ContractorKeith/machine-monitor/internal/tailscale"
)

// evil holds an OSC 8 link, a C1 CSI and C1 OSC, a DCS, a bell, a bidi override and a
// zero-width space: anything a hostile hub or report could use against the terminal.
const evil = "\x1b]8;;http://evil.example/\x07LINKPWN\x1b]8;;\x07\u009b31m\u009d0;t\x07\x1bP1$r\x1b\\\u202e\u200bEND"

// keepFields are values the TUI matches on or parses, so poisoning them would test nothing.
var keepFields = map[string]bool{"ID": true, "Device": true, "RuleID": true, "Type": true, "State": true,
	"Platform": true, "Region": true, "Action": true, "Kind": true}

// poison sets every string reachable from v (a pointer) to evil, except keepFields.
func poison(v reflect.Value, name string) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			poison(v.Elem(), name)
		}
	case reflect.String:
		if v.CanSet() && !keepFields[name] && v.String() != "" {
			v.SetString("x" + evil + v.String())
		}
	case reflect.Struct:
		for i := range v.NumField() {
			if f := v.Type().Field(i); f.IsExported() {
				poison(v.Field(i), f.Name)
			}
		}
	case reflect.Slice, reflect.Array:
		if k := v.Type().Elem().Kind(); k == reflect.Float64 || k == reflect.Uint8 {
			return
		}
		for i := range v.Len() {
			poison(v.Index(i), name)
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			if e := v.MapIndex(k); e.Kind() == reflect.String && !keepFields[name] {
				v.SetMapIndex(k, reflect.ValueOf("x"+evil+e.String()).Convert(e.Type()))
			}
		}
	}
}

func poisoned[T any](v T) T {
	poison(reflect.ValueOf(&v), "")
	return v
}

// evilHub is a hub whose every response is hostile.
type evilHub struct {
	*fakeAlertsHub
	fail bool // answer every request with an error holding evil
}

var errEvil = errors.New("boom " + evil)

func (h *evilHub) Processes(context.Context, string) (*client.Processes, error) {
	if h.fail {
		return nil, errEvil
	}
	return poisoned(&client.Processes{Supported: true, Procs: &hub.ProcSnapshot{T: hubNow.UnixMilli(), List: []report.Process{
		{PID: p(4242), Name: report.Ptr("ollama"), Cmd: report.Ptr("ollama serve"), User: report.Ptr("keith"), CPU: p(88), Mem: p(gib)}}}}), nil
}

func (h *evilHub) Action(context.Context, string, string) error {
	if h.fail {
		return errEvil
	}
	return nil
}

func (h *evilHub) EnergySettings(context.Context) (*power.Settings, error) {
	if h.fail {
		return nil, errEvil
	}
	return &power.Settings{Region: "US", Rate: 0.165, Currency: "x" + evil}, nil
}

func (h *evilHub) SetEnergySettings(_ context.Context, s power.Settings) (power.Settings, error) {
	if h.fail {
		return s, errEvil
	}
	s.Currency = "x" + evil
	return s, nil
}

func (h *evilHub) Remote(context.Context) (*client.Remote, error) {
	if h.fail {
		return nil, errEvil
	}
	return poisoned(&client.Remote{Status: tailscale.Status{Installed: true, Running: true, IP: ptrS("100.64.1.2"), DNSName: ptrS("h"), URLs: []string{"http://100.64.1.2:7777"}}, Port: 7777, Platform: "linux"}), nil
}

func (h *evilHub) NewPairCode(context.Context) (*client.PairCode, error) {
	if h.fail {
		return nil, errEvil
	}
	return &client.PairCode{Code: evil, Expires: hubNow.Add(10 * time.Minute).UnixMilli()}, nil
}

func (h *evilHub) Info(context.Context) (*client.Info, error) {
	return poisoned(&client.Info{Hostname: "hub", Port: 7777, Addresses: []hub.Address{{Name: "eth0", Address: "10.0.0.5"}}}), nil
}

func (h *evilHub) InstallURL(*client.Info) string { return "http://10.0.0.5:7777" + evil }

func (h *evilHub) InstallCommands(u string) (string, string) {
	return "curl " + u + evil, "irm " + u + evil
}

func (h *evilHub) Alerts(ctx context.Context) (*client.Alerts, error) {
	if h.fail {
		return nil, errEvil
	}
	a, err := h.fakeAlertsHub.Alerts(ctx)
	if err != nil {
		return nil, err
	}
	return poisoned(a), nil
}

func (h *evilHub) TestAlerts(context.Context) (alerts.Delivery, error) {
	return alerts.Delivery{Ntfy: &alerts.Result{Error: evil}, Webhook: &alerts.Result{Error: evil}}, errEvil
}

func (h *evilHub) Energy(_ context.Context, bounds []float64) (*client.Energy, error) {
	if h.fail {
		return nil, errEvil
	}
	cost := 0.615
	d := client.Energy{Bounds: bounds, Settings: &power.Settings{Region: "US", Rate: 0.165, Currency: "x" + evil}}
	for _, dev := range threeDevices() {
		d.Devices = append(d.Devices, hub.Usage{ID: dev.ID, Usage: power.Usage{Wh: 3727, Cost: &cost, Minutes: 600}})
	}
	return &d, nil
}

// leak reports what a frame holds that could act on a terminal. The theme's own colours
// (SGR) are expected; everything else with an ESC, a C1 control or a bell is not.
var sgr = regexp.MustCompile("\x1b\\[[0-9;:]*m")

func leak(frame string) string {
	rest := sgr.ReplaceAllString(frame, "")
	for _, r := range rest {
		if r == 0x1b || r == 0x07 || r >= 0x80 && r <= 0x9f || r == 0x202e || r == 0x200b {
			return rest
		}
	}
	return ""
}

// step sends msg, runs the commands it starts and feeds their answers back, as a program
// would; whatever is still waiting after a moment (stream, ticks) is left alone.
func step(m model, msg tea.Msg) model {
	next, cmd := m.Update(msg)
	m = next.(model)
	out := make(chan tea.Msg, 64)
	pending := 0
	start := func(c tea.Cmd) {
		if c != nil {
			pending++
			go func() { out <- c() }()
		}
	}
	start(cmd)
	deadline := time.After(250 * time.Millisecond)
	for pending > 0 {
		select {
		case res := <-out:
			pending--
			switch r := res.(type) {
			case tea.BatchMsg:
				for _, c := range r {
					start(c)
				}
			case tea.RawMsg, nil:
			default:
				next, c := m.Update(r)
				m = next.(model)
				start(c)
			}
		case <-deadline:
			return m
		}
	}
	return m
}

// TestNoEscapeInAnyFrame: every string a hub or a report supplies carries OSC 8, C1 CSI,
// DCS and bidi characters, and no view draws any of them (skeptic pass 3: N4–N7 and the
// audit of the rest).
func TestNoEscapeInAnyFrame(t *testing.T) {
	for _, fail := range []bool{false, true} {
		devs := poisoned(threeDevices())
		devs[0].Latest.Actions = []string{"restart", "shutdown"}
		devs[0].Latest.Features = []string{"procs", "actions"}
		f := &evilHub{fakeAlertsHub: alertsFixture(), fail: fail}
		f.events = []client.Event{{Type: client.EventConnected}, {Type: client.EventSnapshot, Devices: devs}}
		raw := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
		act := alerts.Active{RuleID: "bbbbbbbbbbbb", Type: alerts.TypeCPUTemp, Device: devs[0].ID, DeviceName: evil, Title: evil, Message: evil}
		ev := alerts.Event{ID: "e9", State: alerts.Firing, Title: evil, Message: evil}
		f.events = append(f.events,
			client.Event{Type: client.EventAlert, Raw: raw(alerts.Message{Active: []alerts.Active{act}, Event: &ev})},
			client.Event{Type: client.EventEnergySettings, Raw: raw(map[string]any{"settings": power.Settings{Region: "US", Rate: 0.1, Currency: evil}})},
			client.Event{Type: client.EventActionResult, ID: devs[0].ID, Raw: raw(map[string]any{"name": evil,
				"result": map[string]any{"ok": true, "message": evil}})},
		)
		m := New(Options{Hub: f, Interval: time.Hour, Now: func() time.Time { return hubNow }, Location: time.UTC}).(model)
		m = step(m, tea.WindowSizeMsg{Width: 140, Height: 50})
		for _, e := range f.events {
			m = step(m, hubMsg{e})
		}
		check := func(what string) {
			t.Helper()
			if s := leak(m.render()); s != "" {
				t.Errorf("fail=%v after %s: frame holds terminal control characters:\n%q", fail, what, s)
			}
		}
		check("the stream")
		for _, k := range []string{"2", "3", "4", "5", "6", "enter", "esc", ",", "j", "enter", "esc", "e", "esc", "a", "s", "esc",
			"1", "R", "esc", "P", "esc", "?", "?", "w", "tab", "tab", "tab", "tab"} {
			m = step(m, keyMsg(k))
			check("key " + k)
		}
		for _, v := range []string{"1", "2", "3", "4", "5", "6", ",", "a"} {
			m = step(m, keyMsg(v))
			for _, sub := range []string{"r", "t", "x", "T", "enter", "esc"} {
				m = step(m, keyMsg(sub))
				check("view " + v + " key " + sub)
			}
		}
		if !strings.Contains(frameText(m), "PWN") && !fail {
			t.Log("note: no hostile text reached the last frame; the cleaned text is drawn elsewhere")
		}
	}
}
