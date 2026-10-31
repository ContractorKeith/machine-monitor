package tui

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/ContractorKeith/machine-monitor/internal/alerts"
	"github.com/ContractorKeith/machine-monitor/internal/client"
	"github.com/ContractorKeith/machine-monitor/internal/history"
	"github.com/ContractorKeith/machine-monitor/internal/hub"
	"github.com/ContractorKeith/machine-monitor/internal/power"
	"github.com/ContractorKeith/machine-monitor/internal/tailscale"
)

// feed sends msgs and feeds back what their commands answer within a moment, through
// batches: hub requests and reads. Commands that wait (ticks, the stream) are dropped.
func feed(t *testing.T, m model, msgs ...tea.Msg) model {
	t.Helper()
	var runCmd func(cmd tea.Cmd)
	runCmd = func(cmd tea.Cmd) {
		if cmd == nil {
			return
		}
		out := make(chan tea.Msg, 1)
		go func() { out <- cmd() }()
		select {
		case res := <-out:
			switch r := res.(type) {
			case tea.BatchMsg:
				for _, c := range r {
					runCmd(c)
				}
			case priceLoadedMsg, priceSavedMsg, remoteMsg, pairMsg, todayMsg, alertsLoadedMsg, energyMsg:
				next, c := m.Update(r)
				m = next.(model)
				runCmd(c)
			}
		case <-time.After(300 * time.Millisecond):
		}
	}
	for _, msg := range msgs {
		next, cmd := m.Update(msg)
		m = next.(model)
		runCmd(cmd)
	}
	return m
}

// frameText is the frame without styling.
func frameText(m model) string { return ansi.Strip(m.render()) }

// clockModel is hub mode on the three fixture devices with a clock the test moves.
func clockModel(t *testing.T, hub Hub, w, h int) (model, *time.Time) {
	t.Helper()
	now := hubNow
	o := Options{Hub: hub, Interval: time.Hour, Now: func() time.Time { return now }, Location: time.UTC}
	m := New(o).(model)
	m = feed(t, m, tea.WindowSizeMsg{Width: w, Height: h})
	for _, e := range newFakeHub(threeDevices()).events {
		next, _ := m.Update(hubMsg{e})
		m = next.(model)
	}
	return m, &now
}

// TestStaleStream (L7): when the stream says nothing for 10 s, the dashboard stops
// showing devices as online; the next event brings them back.
func TestStaleStream(t *testing.T) {
	m, now := clockModel(t, newFakeHub(nil), 120, 40)
	if got := len(m.online()); got != 2 {
		t.Fatalf("%d online before", got)
	}
	*now = now.Add(5 * time.Second)
	m = feed(t, m, hubTickMsg{})
	if m.conn.stale || len(m.online()) != 2 {
		t.Fatal("stale after 5 s")
	}
	*now = now.Add(6 * time.Second) // 11 s of silence
	m = feed(t, m, hubTickMsg{})
	if !m.conn.stale || len(m.online()) != 0 || len(m.offline()) != 3 {
		t.Fatalf("not stale after 11 s: stale=%v online=%d", m.conn.stale, len(m.online()))
	}
	f := frameText(m)
	for _, want := range []string{"0 online · 3 offline", "no word from the hub for 11s", "Offline (3)", "the stream has gone quiet"} {
		if !strings.Contains(f, want) {
			t.Errorf("frame lacks %q:\n%s", want, f)
		}
	}
	m = feed(t, m, hubMsg{client.Event{Type: client.EventStatus, Online: map[string]bool{"mac-0001": true}}})
	if m.conn.stale || len(m.online()) != 2 {
		t.Errorf("still stale after an event: online=%d", len(m.online()))
	}
}

// TestTemperatureUnit: °F applies to cards, the Device view and History, and is saved on
// this machine for the next start.
func TestTemperatureUnit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tui.json")
	o := hubOptions(newFakeHub(nil))
	o.PrefsPath = path
	m := New(o).(model)
	m = feed(t, m, tea.WindowSizeMsg{Width: 160, Height: 48})
	for _, e := range newFakeHub(threeDevices()).events {
		next, _ := m.Update(hubMsg{e})
		m = next.(model)
	}
	if !strings.Contains(frameText(m), "72°C") {
		t.Fatal("no °C on the card")
	}
	m = feed(t, m, keyMsg(","))
	if m.view != viewSettings || !strings.Contains(frameText(m), "°C  Celsius") {
		t.Fatalf("Settings not open:\n%s", frameText(m))
	}
	m = feed(t, m, keyMsg("u"))
	if !strings.Contains(frameText(m), "°F  Fahrenheit") {
		t.Errorf("unit didn't switch:\n%s", frameText(m))
	}
	m = feed(t, m, keyMsg("esc"))
	if m.view != viewDashboard {
		t.Fatalf("esc went to %v", m.view)
	}
	f := frameText(m)
	if !strings.Contains(f, "162°F") || strings.Contains(f, "72°C") { // 72.4 °C
		t.Errorf("card not in °F:\n%s", f)
	}
	m = feed(t, m, keyMsg("2"))
	if f := frameText(m); !strings.Contains(f, "°F") || strings.Contains(f, "°C") {
		t.Errorf("Device view not in °F:\n%s", f)
	}
	if got := m.theme.tempV(100); got != "212°F" {
		t.Errorf("tempV(100) = %q", got)
	}
	tl := m.histTiles([]history.Point{{T: 1, CPUTemp: p(50)}}, 0, 2, 200)
	if !strings.Contains(ansi.Strip(tl), "122°F") {
		t.Errorf("History tile = %q", ansi.Strip(tl))
	}

	again := New(o).(model)
	if !again.theme.fahrenheit {
		t.Error("°F was not saved")
	}
}

// settingsFake is a hub with the Settings and Add device APIs.
type settingsFake struct {
	*fakeHub
	mu     sync.Mutex
	price  *power.Settings
	put    []power.Settings
	remote error
	codes  int
}

func (f *settingsFake) EnergySettings(context.Context) (*power.Settings, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.price, nil
}

func (f *settingsFake) SetEnergySettings(_ context.Context, s power.Settings) (power.Settings, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.put = append(f.put, s)
	f.price = &s
	return s, nil
}

func (f *settingsFake) Remote(context.Context) (*client.Remote, error) {
	if f.remote != nil {
		return nil, f.remote
	}
	return &client.Remote{Status: tailscale.Status{Installed: true, Running: true, IP: ptrS("100.64.1.2"),
		DNSName: ptrS("hub.tail1234.ts.net"), URLs: []string{"http://100.64.1.2:7777"}}, Port: 7777}, nil
}

func (f *settingsFake) NewPairCode(context.Context) (*client.PairCode, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.codes++
	return &client.PairCode{Code: "042917", Expires: hubNow.Add(10 * time.Minute).UnixMilli()}, nil
}

func (f *settingsFake) Info(context.Context) (*client.Info, error) { return &client.Info{}, nil }

func (f *settingsFake) InstallURL(*client.Info) string { return "http://192.168.1.5:7777" }

func (f *settingsFake) InstallCommands(u string) (string, string) {
	return "curl -sSL --fail-with-body -H \"Authorization: Bearer KEY\" " + u + "/install.sh | sh",
		"irm -Headers @{Authorization='Bearer KEY'} " + u + "/install.ps1 | iex"
}

func ptrS(s string) *string { return &s }

// TestSettingsHub: the hub's price is read and saved through the price editor (TOU too),
// a broadcast price change reaches the view, and Tailscale, role, hub link and version
// show.
func TestSettingsHub(t *testing.T) {
	f := &settingsFake{fakeHub: newFakeHub(nil)}
	m, _ := clockModel(t, f, 120, 40)
	m = feed(t, m, keyMsg(","))
	frame := frameText(m)
	for _, want := range []string{"Typical home rate", "connected to a hub", "Hub link", "running · 100.64.1.2 · hub.tail1234.ts.net", "Version", "machine-monitor "} {
		if !strings.Contains(frame, want) {
			t.Errorf("Settings lacks %q:\n%s", want, frame)
		}
	}
	m = feed(t, m, keyMsg("e"))
	if !m.set.form.open {
		t.Fatal("editor not open")
	}
	// Rate: replace with 0.21; currency: EUR; TOU: 16-21 at 0.4.
	m = feed(t, m, keyMsg("down"))
	for range 10 {
		m = feed(t, m, keyMsg("backspace"))
	}
	m = feed(t, m, keys("0", ".", "2", "1")...)
	m = feed(t, m, keyMsg("down"))
	for range 3 {
		m = feed(t, m, keyMsg("backspace"))
	}
	m = feed(t, m, keys("e", "u", "r")...)
	m = feed(t, m, keyMsg("down"))
	for _, k := range "16-21 0.4" {
		m = feed(t, m, keyMsg(string(k)))
	}
	m = feed(t, m, keyMsg("enter"))
	if len(f.put) != 1 {
		t.Fatalf("PUTs: %+v (form error %q)", f.put, m.set.form.err)
	}
	got := f.put[0]
	if got.Region != "custom" || got.Rate != 0.21 || got.Currency != "EUR" || len(got.TOU) != 1 || got.TOU[0] != (power.Period{From: 16, To: 21, Rate: 0.4}) {
		t.Errorf("saved %+v", got)
	}
	if m.set.form.open || !strings.Contains(frameText(m), "€0.210–€0.400/kWh by time of day") {
		t.Errorf("after save:\n%s", frameText(m))
	}

	// Another TUI changed the price: the event updates this one.
	ev := client.Event{Type: client.EventEnergySettings, Raw: []byte(`{"type":"energySettings","settings":{"region":"GB","rate":0.245,"currency":"GBP"}}`)}
	m = feed(t, m, hubMsg{ev})
	if !strings.Contains(frameText(m), "£0.245/kWh") {
		t.Errorf("event not applied:\n%s", frameText(m))
	}

	// A bad rate is refused in the form, before any request.
	m = feed(t, m, keyMsg("e"), keyMsg("down"), keyMsg("x"), keyMsg("enter"))
	if !strings.Contains(m.set.form.err, "rate") || len(f.put) != 1 {
		t.Errorf("bad rate: err %q, puts %d", m.set.form.err, len(f.put))
	}

	// An older hub without /api/remote.
	f.remote = client.ErrNotAvailable
	m = feed(t, m, keyMsg("esc"), keyMsg("esc"), keyMsg(","))
	if !strings.Contains(frameText(m), "not available on this hub") {
		t.Errorf("no /api/remote:\n%s", frameText(m))
	}
}

// TestSettingsLocalPrice: local mode saves the price on this machine and uses it.
func TestSettingsLocalPrice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tui.json")
	o := testOptions()
	o.PrefsPath = path
	m := New(o).(model)
	m = feed(t, m, tea.WindowSizeMsg{Width: 120, Height: 40}, keyMsg(","))
	if !strings.Contains(frameText(m), "local: this machine only") {
		t.Fatalf("role:\n%s", frameText(m))
	}
	m = feed(t, m, keyMsg("e"), keyMsg("down"))
	for range 10 {
		m = feed(t, m, keyMsg("backspace"))
	}
	m = feed(t, m, keys("0", ".", "3")...)
	m = feed(t, m, keyMsg("enter"))
	if m.opts.Price == nil || m.opts.Price.Rate != 0.3 {
		t.Fatalf("price = %+v (%q)", m.opts.Price, m.set.form.err)
	}
	if p := loadPrefs(path); p.Price == nil || p.Price.Rate != 0.3 {
		t.Errorf("saved prefs = %+v", p)
	}
	again := New(o).(model)
	if again.opts.Price == nil || again.opts.Price.Rate != 0.3 {
		t.Errorf("price not loaded at start: %+v", again.opts.Price)
	}
}

func TestParseTOU(t *testing.T) {
	ps, err := parseTOU(" 16-21 0.35, 0-6.5 0.08 ,")
	if err != nil || len(ps) != 2 || ps[1] != (power.Period{From: 0, To: 6.5, Rate: 0.08}) {
		t.Errorf("parseTOU = %+v, %v", ps, err)
	}
	if ps, err := parseTOU(""); err != nil || ps != nil {
		t.Errorf("empty = %+v, %v", ps, err)
	}
	for _, bad := range []string{"16 0.3", "16-21", "a-b c"} {
		if _, err := parseTOU(bad); err == nil {
			t.Errorf("parseTOU(%q) accepted", bad)
		}
	}
	if s := fmtTOU(ps); s != "16-21 0.35, 0-6.5 0.08" {
		t.Errorf("fmtTOU = %q", s)
	}
}

// TestAddDevice: `a` asks the hub for a code and shows it with a countdown that runs out,
// the one-line installs, and copies them.
func TestAddDevice(t *testing.T) {
	f := &settingsFake{fakeHub: newFakeHub(nil)}
	m, now := clockModel(t, f, 160, 40)
	m = feed(t, m, keyMsg("a"))
	if m.view != viewAddDevice || f.codes != 1 {
		t.Fatalf("view %v, %d codes", m.view, f.codes)
	}
	frame := frameText(m)
	for _, want := range []string{"042 917", "expires in 10:00", "machine-monitor connect http://192.168.1.5:7777",
		`curl -sSL --fail-with-body -H "Authorization: Bearer KEY" http://192.168.1.5:7777/install.sh | sh`,
		`irm -Headers @{Authorization='Bearer KEY'} http://192.168.1.5:7777/install.ps1 | iex`} {
		if !strings.Contains(frame, want) {
			t.Errorf("Add device lacks %q:\n%s", want, frame)
		}
	}
	*now = now.Add(4*time.Minute + 30*time.Second)
	m = feed(t, m, hubTickMsg{})
	if !strings.Contains(frameText(m), "expires in 5:30") {
		t.Errorf("countdown:\n%s", frameText(m))
	}
	*now = now.Add(6 * time.Minute)
	m = feed(t, m, hubTickMsg{})
	if !strings.Contains(frameText(m), "expired · n for a new code") {
		t.Errorf("expiry:\n%s", frameText(m))
	}
	m = feed(t, m, keyMsg("n"))
	if f.codes != 2 {
		t.Errorf("n asked for %d codes", f.codes)
	}
	_, cmd := m.Update(keyMsg("c"))
	if cmd == nil {
		t.Error("c copies nothing")
	}
	m = feed(t, m, keyMsg("esc"))
	if m.view != viewDashboard {
		t.Errorf("esc went to %v", m.view)
	}
	// A narrow window wraps the install lines instead of cutting them.
	m = feed(t, m, tea.WindowSizeMsg{Width: 60, Height: 30}, keyMsg("a"))
	if f := strings.ReplaceAll(strings.ReplaceAll(frameText(m), "\n", ""), " ", ""); !strings.Contains(f, "/install.sh|sh") {
		t.Errorf("install line cut at 60 columns:\n%s", frameText(m))
	}
}

// TestCardFilter: `/` on the dashboard narrows the cards and the offline row by name;
// esc clears it.
func TestCardFilter(t *testing.T) {
	m, _ := clockModel(t, newFakeHub(nil), 160, 48)
	m = feed(t, m, keyMsg("/"))
	m = feed(t, m, keys("S", "t", "U")...)
	f := frameText(m)
	if !strings.Contains(f, "studio") || strings.Contains(f, "ws-01") || strings.Contains(f, "Offline (") {
		t.Errorf("filter \"stu\":\n%s", f)
	}
	if m.sel != "mac-0001" {
		t.Errorf("selection %q, want the one shown", m.sel)
	}
	m = feed(t, m, keyMsg("enter"))
	if !strings.Contains(frameText(m), `filter "StU": 1 shown`) {
		t.Errorf("filter not kept:\n%s", frameText(m))
	}
	m = feed(t, m, keyMsg("K"))
	if !strings.Contains(m.notice, "clear the filter") {
		t.Errorf("reorder under a filter: %q", m.notice)
	}
	m = feed(t, m, keyMsg("esc"))
	if f := frameText(m); !strings.Contains(f, "ws-01") || !strings.Contains(f, "Offline (1)") {
		t.Errorf("esc didn't clear:\n%s", f)
	}
	m = feed(t, m, keyMsg("/"), keyMsg("z"), keyMsg("z"))
	if !strings.Contains(frameText(m), "No device name matches the filter.") {
		t.Errorf("no match:\n%s", frameText(m))
	}
}

// energyFake is a hub whose energy read reports a fixed cost per device.
type energyFake struct {
	*fakeAlertsHub
}

func (energyFake) History(context.Context, string, float64, float64, int) (*history.Result, error) {
	return &history.Result{}, nil
}

func (energyFake) Energy(_ context.Context, bounds []float64) (*client.Energy, error) {
	cost := 0.615
	d := client.Energy{Bounds: bounds, Settings: &power.Settings{Region: "US", Rate: 0.165, Currency: "USD"}}
	for _, id := range []string{"4c4c4544-0042-3510-8051-b2c04f4e3732", "mac-0001"} {
		d.Devices = append(d.Devices, hub.Usage{ID: id, Usage: power.Usage{Wh: 3727, Cost: &cost, Minutes: 600}})
	}
	return &d, nil
}

// TestHeaderCostAndAlerts: the dashboard's summary carries the cost since midnight and the
// active-alerts count.
func TestHeaderCostAndAlerts(t *testing.T) {
	af := alertsFixture()
	af.state.Active = []alerts.Active{{RuleID: "r1", Device: "mac-0001"}, {RuleID: "r2", Device: "mac-0001"}}
	f := energyFake{af}
	m := New(Options{Hub: f, Interval: time.Hour, Now: func() time.Time { return hubNow }}).(model)
	m = feed(t, m, tea.WindowSizeMsg{Width: 160, Height: 48})
	for _, e := range newFakeHub(threeDevices()).events {
		next, _ := m.Update(hubMsg{e})
		m = next.(model)
	}
	m = feed(t, m, m.fetchToday()(), m.loadAlerts()())
	head := strings.Split(frameText(m), "\n")[1]
	for _, want := range []string{"$1.23 today", "2 active alerts", "2 online · 1 offline"} {
		if !strings.Contains(head, want) {
			t.Errorf("summary %q lacks %q", head, want)
		}
	}
}

// TestCosmetics: the skeptic report's TUI weaknesses.
func TestCosmetics(t *testing.T) {
	m, _ := clockModel(t, newFakeHub(nil), 200, 50)
	// At 200 columns three cards share the width; the disk-write rate must still show.
	if f := frameText(m); !strings.Contains(f, "w 3.2 MiB/s") {
		t.Errorf("NET/DISK cut at 200 columns:\n%s", f)
	}
	// At 40×10 the open view keeps its name in the tab bar.
	for _, v := range []string{"1", "4", "5"} {
		m2 := feed(t, m, tea.WindowSizeMsg{Width: 40, Height: 10}, keyMsg(v))
		head := strings.Split(frameText(m2), "\n")[0]
		if name := viewNames[m2.view]; !strings.Contains(head, name) {
			t.Errorf("40 columns, view %s: header %q", name, head)
		}
	}
	// History keeps naming the hub.
	m3 := feed(t, m, keyMsg("4"))
	if head := strings.Split(frameText(m3), "\n")[0]; !strings.Contains(head, "hub hub.lan:7777") {
		t.Errorf("History header %q", head)
	}
	// 24 h crosses midnight: both ends name their day.
	m3.hv.rng = 2
	if info := ansi.Strip(m3.histInfo()); !strings.Contains(info, "Fri 12:00 – Sat 12:00") {
		t.Errorf("24 h label %q", info)
	}
	// A device silent for 11 s is not "just now".
	if got := m.ago(hubNow.Add(-11 * time.Second).UnixMilli()); got != "11s ago" {
		t.Errorf("ago(11s) = %q", got)
	}
}
