package tui

import (
	"context"
	"encoding/json"
	"net"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/exp/golden"
	"github.com/charmbracelet/x/exp/teatest/v2"

	"github.com/ContractorKeith/machine-monitor/internal/alerts"
	"github.com/ContractorKeith/machine-monitor/internal/client"
	"github.com/ContractorKeith/machine-monitor/internal/config"
	"github.com/ContractorKeith/machine-monitor/internal/hub"
)

// fakeAlertsHub is a fakeHub with the alerts API.
type fakeAlertsHub struct {
	*fakeHub
	amu   sync.Mutex
	state client.Alerts
	saved []alerts.Settings
}

func (f *fakeAlertsHub) Alerts(context.Context) (*client.Alerts, error) {
	if err := f.record("alerts"); err != nil {
		return nil, err
	}
	f.amu.Lock()
	defer f.amu.Unlock()
	a := f.state
	return &a, nil
}

func (f *fakeAlertsHub) SetAlerts(_ context.Context, s alerts.Settings) (alerts.Settings, []alerts.Active, error) {
	if err := f.record("set alerts"); err != nil {
		return alerts.Settings{}, nil, err
	}
	f.amu.Lock()
	defer f.amu.Unlock()
	f.saved = append(f.saved, s)
	f.state.Settings = s
	return s, f.state.Active, nil
}

func (f *fakeAlertsHub) TestAlerts(context.Context) (alerts.Delivery, error) {
	if err := f.record("test"); err != nil {
		return alerts.Delivery{}, err
	}
	return alerts.Delivery{Ntfy: &alerts.Result{OK: true}, Webhook: &alerts.Result{Error: "HTTP 404 not found"}}, nil
}

func (f *fakeAlertsHub) ClearAlertLog(context.Context) error { return f.record("clear log") }

func ptrF(v float64) *float64 { return &v }

// alertsFixture is a hub with the default rules plus a power and a cost rule, ntfy on, one
// alert firing and three log entries.
func alertsFixture() *fakeAlertsHub {
	f := &fakeAlertsHub{fakeHub: newFakeHub(threeDevices())}
	s := alerts.DefaultSettings()
	for i := range s.Rules {
		s.Rules[i].ID = strings.Repeat(string(rune('a'+i)), 12)
	}
	ws := threeDevices()[0].ID
	s.Rules = append(s.Rules,
		alerts.Rule{ID: "eeeeeeeeeeee", Type: alerts.TypePower, Device: ws, Threshold: ptrF(450), Minutes: 5},
		alerts.Rule{ID: "ffffffffffff", Type: alerts.TypeDailyCost, Device: alerts.AllDevices, Threshold: ptrF(2.5), Enabled: true})
	s.Channels.Ntfy = alerts.Ntfy{Enabled: true, Server: alerts.DefaultNtfyServer, Topic: "my-lab"}
	ms := func(d time.Duration) int64 { return hubNow.Add(-d).UnixMilli() }
	f.state = client.Alerts{Settings: s,
		Active: []alerts.Active{{RuleID: "bbbbbbbbbbbb", Type: alerts.TypeCPUTemp, Device: ws, DeviceName: "ws-01",
			Since: ms(3 * time.Minute), Value: ptrF(93), Threshold: ptrF(90), Title: "ws-01: CPU temperature 93°C",
			Message: "Above 90°C for 2 min."}},
		Events: []alerts.Event{
			{ID: "e3", T: ms(3 * time.Minute), State: alerts.Firing, Title: "ws-01: CPU temperature 93°C", Message: "Above 90°C for 2 min."},
			{ID: "e2", T: ms(2 * time.Hour), State: alerts.Resolved, Title: "nas-02 is back online", Message: "It was offline since 9:12 AM."},
			{ID: "e1", T: ms(26 * time.Hour), State: alerts.Firing, Title: "nas-02 is offline", Message: "No reports since 9:12 AM."},
		}}
	return f
}

// settle runs cmd and feeds every message it answers at once back into the model, through
// batches; commands that wait (the stream, the cursor blink) are left alone. It returns the
// model and how many bells were printed.
func settle(t *testing.T, m model, cmd tea.Cmd) (model, int) {
	t.Helper()
	bells := 0
	queue := []tea.Cmd{cmd}
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]
		if c == nil {
			continue
		}
		out := make(chan tea.Msg, 1)
		go func() { out <- c() }()
		var res tea.Msg
		select {
		case res = <-out:
		case <-time.After(50 * time.Millisecond):
			continue
		}
		switch r := res.(type) {
		case tea.BatchMsg:
			queue = append(queue, r...)
		case tea.RawMsg:
			if r.Msg == "\a" {
				bells++
			}
		case alertsLoadedMsg, alertsSavedMsg, alertsTestMsg, alertsClearedMsg, renameMsg, removeMsg, orderMsg:
			next, c := m.Update(r)
			m = next.(model)
			queue = append(queue, c)
		}
	}
	return m, bells
}

// send sends msgs one at a time, settling each; it returns the bells rung.
func send(t *testing.T, m model, msgs ...tea.Msg) (model, int) {
	t.Helper()
	total := 0
	for _, msg := range msgs {
		next, cmd := m.Update(msg)
		var n int
		m, n = settle(t, next.(model), cmd)
		total += n
	}
	return m, total
}

// alertsModel is hub mode at w×h on the fixture, with the Alerts view open and loaded.
func alertsModel(t *testing.T, f *fakeAlertsHub, w, h int) model {
	t.Helper()
	m := New(Options{Hub: f, Interval: time.Hour, Now: func() time.Time { return hubNow }}).(model)
	m, _ = send(t, m, tea.WindowSizeMsg{Width: w, Height: h})
	for _, e := range f.events {
		m, _ = send(t, m, hubMsg{e})
	}
	m, _ = send(t, m, keyMsg("6"))
	if !m.al.loaded {
		t.Fatal("alerts did not load")
	}
	return m
}

// alertEvent is the stream event the hub sends when an alert fires.
func alertEvent(t *testing.T, id string, notify bool) hubMsg {
	t.Helper()
	ev := alerts.Event{ID: id, T: hubNow.UnixMilli(), State: alerts.Firing, Device: "mac-0001",
		Title: "studio: GPU load 99%", Message: "Above 95% for 5 min."}
	b, err := json.Marshal(alerts.Message{Type: "alert", Event: &ev, Notify: notify,
		Active: []alerts.Active{{RuleID: "x", Title: ev.Title, Message: ev.Message, Since: ev.T}}})
	if err != nil {
		t.Fatal(err)
	}
	return hubMsg{client.Event{Type: client.EventAlert, Raw: b}}
}

func TestAlertsGolden(t *testing.T) {
	right := tea.KeyPressMsg{Code: tea.KeyRight}
	down := tea.KeyPressMsg{Code: tea.KeyDown}
	cases := []struct {
		name string
		w, h int
		msgs []tea.Msg
	}{
		{"alerts_80x24", 80, 24, nil},
		{"alerts_160x48", 160, 48, nil},
		{"alerts_selected_80x24", 80, 24, keys("j", "j", "j", "j")}, // the power rule, scrolled into view
		{"edit_rule_80x24", 80, 24, keys("j", "enter")},
		{"new_rule_offline_80x24", 80, 24, []tea.Msg{keyMsg("a"), tea.KeyPressMsg{Code: tea.KeyLeft}}},
		{"channels_80x24", 80, 24, []tea.Msg{keyMsg("c"), down, down, right}},
		{"confirm_delete_80x24", 80, 24, keys("D")},
		{"test_result_80x24", 80, 24, keys("t")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := alertsModel(t, alertsFixture(), c.w, c.h)
			m, _ = send(t, m, c.msgs...)
			golden.RequireEqual(t, plain(m))
		})
	}
}

// Local mode has no alerts engine: the view points at a hub.
func TestAlertsLocalGolden(t *testing.T) {
	m := drive(t, testOptions(), 80, 24, "6")
	golden.RequireEqual(t, plain(m))
}

// TestBannerGolden: an alert raises the banner over whatever view is open.
func TestBannerGolden(t *testing.T) {
	for _, c := range []struct {
		name string
		keys []string
		w, h int
	}{
		{"dashboard_80x24", nil, 80, 24},
		{"dashboard_160x48", nil, 160, 48},
		{"device_80x24", []string{"2"}, 80, 24},
		{"alerts_80x24", []string{"6"}, 80, 24},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := alertsFixture()
			m := hubModel(t, f.fakeHub, c.w, c.h)
			m.opts.Hub = f
			m, _ = send(t, m, keys(c.keys...)...)
			m, _ = send(t, m, alertEvent(t, "n1", true), alertEvent(t, "n2", true))
			golden.RequireEqual(t, plain(m))
			checkFrame(t, m, c.w, c.h)
		})
	}
}

// Acceptance: the bell rings exactly once per new alert, and only when the hub asks.
func TestBellOncePerAlert(t *testing.T) {
	f := alertsFixture()
	m := hubModel(t, f.fakeHub, 80, 24)
	m.opts.Hub = f
	m, n := send(t, m, alertEvent(t, "a1", true))
	if n != 1 || m.al.bells != 1 || m.al.banner == nil {
		t.Fatalf("first alert: %d bells, banner %v", n, m.al.banner)
	}
	// The same alert again (a replay) and one the hub says not to show: no bell.
	m, n = send(t, m, alertEvent(t, "a1", true), alertEvent(t, "quiet", false))
	if n != 0 || m.al.bells != 1 {
		t.Fatalf("repeat: %d bells", n)
	}
	// Dismissed, a new alert rings and shows again.
	m, _ = send(t, m, keyMsg("esc"))
	if m.al.banner != nil {
		t.Fatal("esc did not dismiss the banner")
	}
	m, n = send(t, m, alertEvent(t, "a2", true))
	if n != 1 || m.al.bells != 2 || m.al.banner == nil || m.al.banner.ID != "a2" {
		t.Fatalf("second alert: %d bells, banner %+v", n, m.al.banner)
	}
	// Every alert went into the log once, newest first.
	var got []string
	for _, e := range m.al.events {
		got = append(got, e.ID)
	}
	if !slices.Equal(got, []string{"a2", "quiet", "a1"}) {
		t.Errorf("log = %v", got)
	}
}

// The banner shows in every view and the frame keeps its size.
func TestBannerEveryView(t *testing.T) {
	f := alertsFixture()
	m := hubModel(t, f.fakeHub, 80, 24)
	m.opts.Hub = f
	m, _ = send(t, m, alertEvent(t, "b1", true))
	for v := range viewCount {
		m.view = v
		lines := strings.Split(plain(m), "\n")
		if !strings.Contains(lines[1], "studio: GPU load 99%") {
			t.Errorf("view %s: banner missing:\n%s", viewNames[v], plain(m))
		}
		checkFrame(t, m, 80, 24)
	}
	m.height = 3 // no room: the banner yields
	checkFrame(t, m, 80, 3)
}

func TestAlertsKeys(t *testing.T) {
	f := alertsFixture()
	m := alertsModel(t, f, 80, 24)

	// space toggles the selected rule and saves every rule.
	m, _ = send(t, m, keyMsg(" "))
	if len(f.saved) != 1 || f.saved[0].Rules[0].Enabled || m.al.cfg.Rules[0].Enabled {
		t.Fatalf("toggle: saved %+v", f.saved)
	}
	// D then n keeps the rule; D then y deletes it.
	m, _ = send(t, m, keyMsg("D"), keyMsg("n"))
	if len(f.saved) != 1 {
		t.Fatal("cancelled delete saved")
	}
	m, _ = send(t, m, keyMsg("D"), keyMsg("y"))
	if len(f.saved) != 2 || len(f.saved[1].Rules) != 5 || f.saved[1].Rules[0].Type != alerts.TypeCPUTemp {
		t.Fatalf("delete: %+v", f.saved[1].Rules)
	}
	// C then y clears the log.
	m, _ = send(t, m, keyMsg("C"), keyMsg("y"))
	if len(m.al.events) != 0 || !slices.Contains(f.Calls(), "clear log") {
		t.Fatalf("clear: %d events, calls %q", len(m.al.events), f.Calls())
	}
	// t reports each channel.
	m, _ = send(t, m, keyMsg("t"))
	if s := m.alertsFooter(); s != "test alert sent · ntfy ok · webhook failed: HTTP 404 not found" {
		t.Errorf("test footer = %q", s)
	}
	// An invalid threshold stays in the editor with the hub's reason, and nothing is sent.
	m, _ = send(t, m, keyMsg("enter"), tea.KeyPressMsg{Code: tea.KeyDown}, tea.KeyPressMsg{Code: tea.KeyDown})
	m, _ = send(t, m, keys("backspace", "backspace", "9", "9", "9", "enter")...)
	if !m.al.form.open || !strings.Contains(m.al.form.err, "threshold must be 20–150") || len(f.saved) != 2 {
		t.Fatalf("invalid save: open %v, err %q, saves %d", m.al.form.open, m.al.form.err, len(f.saved))
	}
	m, _ = send(t, m, keyMsg("esc"))
	if m.al.form.open || len(f.saved) != 2 {
		t.Fatal("esc did not cancel")
	}
	// Keys typed in the editor are text, not view switches.
	m, _ = send(t, m, keyMsg("enter"), tea.KeyPressMsg{Code: tea.KeyDown}, tea.KeyPressMsg{Code: tea.KeyDown}, keyMsg("1"), keyMsg("q"))
	if m.view != viewAlerts || !m.al.form.open {
		t.Fatal("1 or q left the editor")
	}
	// An alerts event from another client replaces the settings.
	s := cloneSettings(m.al.cfg)
	s.Rules = s.Rules[:1]
	b, _ := json.Marshal(alerts.Message{Type: "alerts", Settings: &s, Active: []alerts.Active{}})
	m, _ = send(t, m, keyMsg("esc"), hubMsg{client.Event{Type: client.EventAlertSettings, Raw: b}})
	if len(m.al.cfg.Rules) != 1 || len(m.al.active) != 0 || m.al.cursor != 0 {
		t.Fatalf("settings event: %d rules, %d active, cursor %d", len(m.al.cfg.Rules), len(m.al.active), m.al.cursor)
	}
}

// A channel switched on without its address is caught before the hub would quietly drop it.
func TestChannelsNeedAddress(t *testing.T) {
	f := alertsFixture()
	m := alertsModel(t, f, 80, 24)
	down := tea.KeyPressMsg{Code: tea.KeyDown}
	m, _ = send(t, m, keyMsg("c"), down, down, down, down, down, keyMsg(" "), keyMsg("enter"))
	if !m.al.form.open || m.al.form.err != "the webhook needs a URL" || len(f.saved) != 0 {
		t.Fatalf("form open %v, err %q, saves %d", m.al.form.open, m.al.form.err, len(f.saved))
	}
}

// ---- against a real hub ----------------------------------------------------------------

// realHub serves a real hub over httptest and returns a client for it.
func realHub(t *testing.T) (*hub.Hub, *client.Client) {
	t.Helper()
	h, err := hub.New(hub.Options{Paths: config.At(t.TempDir()), Hostname: "testbox", Location: time.UTC, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(h.Handler())
	srv.Listener = ln
	srv.Start()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.Run(ctx); close(done) }()
	t.Cleanup(func() {
		srv.CloseClientConnections()
		cancel()
		<-done
		srv.Close()
		h.Close()
	})
	c, err := client.New(srv.URL, h.Token())
	if err != nil {
		t.Fatal(err)
	}
	return h, c
}

// Acceptance: the rule editor round-trips through a real hub. A rule added, edited, toggled
// and deleted in the TUI is what GET /api/alerts returns, and the hub's own events keep the
// TUI in step.
func TestAlertsEditorAgainstHub(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a hub")
	}
	_, c := realHub(t)
	ctx := context.Background()
	o := Options{Hub: c, Interval: time.Hour}
	ch := make(chan model)
	tm := teatest.NewTestModel(t, probe{New(o), ch}, teatest.WithInitialTermSize(100, 30))
	waitUntil(t, tm, ch, synced)
	tm.Type("6")
	m := waitUntil(t, tm, ch, func(m model) bool { return m.al.loaded })
	if len(m.al.cfg.Rules) != 4 {
		t.Fatalf("defaults: %d rules", len(m.al.cfg.Rules))
	}
	hubRules := func() []alerts.Rule {
		t.Helper()
		a, err := c.Alerts(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return a.Settings.Rules
	}
	key := func(code rune) { tm.Send(tea.KeyPressMsg{Code: code}) }
	saved := func(n int) func(model) bool {
		return func(m model) bool { return m.al.notice == "saved" && len(m.al.cfg.Rules) == n }
	}

	// Add: a = new rule (CPU temperature); → to CPU load; ↓↓ threshold 77; ↓ minutes 3.
	tm.Type("a")
	key(tea.KeyRight)
	key(tea.KeyRight)
	key(tea.KeyDown)
	key(tea.KeyDown)
	key(tea.KeyBackspace)
	key(tea.KeyBackspace)
	tm.Type("77")
	key(tea.KeyDown)
	key(tea.KeyBackspace)
	tm.Type("3")
	key(tea.KeyEnter)
	m = waitUntil(t, tm, ch, saved(5))
	rules := hubRules()
	if len(rules) != 5 {
		t.Fatalf("hub has %d rules", len(rules))
	}
	r := rules[4]
	if r.Type != alerts.TypeCPU || r.Device != alerts.AllDevices || *r.Threshold != 77 || r.Minutes != 3 || !r.Enabled || r.ID == "" {
		t.Fatalf("added rule = %+v", r)
	}
	if m.al.cursor != 4 || m.al.cfg.Rules[4].ID != r.ID {
		t.Fatalf("TUI: cursor %d, rule %+v", m.al.cursor, m.al.cfg.Rules[4])
	}

	// Edit it: threshold 80; the id stays.
	key(tea.KeyEnter)
	key(tea.KeyDown)
	key(tea.KeyDown)
	key(tea.KeyBackspace)
	key(tea.KeyBackspace)
	tm.Type("80")
	key(tea.KeyEnter)
	waitUntil(t, tm, ch, func(m model) bool {
		return m.al.notice == "saved" && *m.al.cfg.Rules[4].Threshold == 80
	})
	if r2 := hubRules()[4]; r2.ID != r.ID || *r2.Threshold != 80 {
		t.Fatalf("edited rule = %+v", r2)
	}

	// Toggle it off, then delete it.
	tm.Type(" ")
	waitUntil(t, tm, ch, func(m model) bool { return m.al.notice == "saved" && !m.al.cfg.Rules[4].Enabled })
	if hubRules()[4].Enabled {
		t.Fatal("hub rule still enabled")
	}
	tm.Type("Dy")
	waitUntil(t, tm, ch, saved(4))
	if n := len(hubRules()); n != 4 {
		t.Fatalf("after delete the hub has %d rules", n)
	}

	// A change made by another client arrives as the hub's alerts event.
	s, _, err := c.SetAlerts(ctx, alerts.Settings{Rules: hubRules()[:2], Channels: alerts.Channels{Desktop: true}})
	if err != nil || len(s.Rules) != 2 {
		t.Fatalf("SetAlerts: %v", err)
	}
	waitUntil(t, tm, ch, func(m model) bool { return len(m.al.cfg.Rules) == 2 })

	// The test button: the hub's test alert comes back as a banner and one bell.
	tm.Type("t")
	m = waitUntil(t, tm, ch, func(m model) bool { return m.al.banner != nil && strings.HasPrefix(m.al.notice, "test alert sent") })
	if !m.al.banner.Test || m.al.bells != 1 {
		t.Fatalf("banner %+v, bells %d", m.al.banner, m.al.bells)
	}
	finish(t, tm)
}
