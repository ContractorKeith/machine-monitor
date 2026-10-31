package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/golden"
	"github.com/charmbracelet/x/exp/teatest/v2"

	"github.com/ContractorKeith/machine-monitor/internal/client"
	"github.com/ContractorKeith/machine-monitor/internal/history"
	"github.com/ContractorKeith/machine-monitor/internal/hub"
	"github.com/ContractorKeith/machine-monitor/internal/power"
	"github.com/ContractorKeith/machine-monitor/internal/report"
)

// hubNow is the fake clock of every hub-mode test.
var hubNow = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

// fakeHub serves a fixed stream and records requests.
type fakeHub struct {
	mu     sync.Mutex
	devs   map[string]client.Device
	events []client.Event
	calls  []string
	fail   error // every request fails with it when set
}

func (f *fakeHub) Stream(ctx context.Context) <-chan client.Event {
	ch := make(chan client.Event, len(f.events))
	for _, e := range f.events {
		ch <- e
	}
	go func() { // like the client: the stream ends with ctx
		<-ctx.Done()
		close(ch)
	}()
	return ch
}

func (f *fakeHub) record(s string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, s)
	return f.fail
}

func (f *fakeHub) Rename(ctx context.Context, id, name string) (*client.Device, error) {
	if err := f.record("rename " + id + " " + name); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.devs[id]
	d.Name, d.Renamed = name, true
	return &d, nil
}

func (f *fakeHub) Remove(ctx context.Context, id string) error { return f.record("remove " + id) }

func (f *fakeHub) SetOrder(ctx context.Context, ids []string) error {
	return f.record("order " + strings.Join(ids, ","))
}

func (f *fakeHub) Host() string { return "hub.lan:7777" }

func (f *fakeHub) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// hubDevice is a hub view of r with 40 history samples.
func hubDevice(r *report.Report, order int, online bool, lastSeen time.Time) client.Device {
	var pts []history.Point
	for i := range 40 {
		pts = append(pts, history.Point{
			T:   float64(lastSeen.Add(time.Duration(i-40) * 2 * time.Second).UnixMilli()),
			CPU: p(float64(50 + 40*((i*13+order)%7-3)/3)),
			GPU: p(float64((i*23 + order*31) % 100)),
			Mem: pctOf(r.Memory.Used, r.Memory.Total),
		})
	}
	return client.Device{
		Device: hub.Device{ID: r.ID, Name: *r.Hostname, Hostname: r.Hostname, Platform: r.Platform, OS: r.OS,
			FirstSeen: hubNow.Add(-72 * time.Hour).UnixMilli(), LastSeen: lastSeen.UnixMilli(), Order: order},
		Online:  online,
		Latest:  &hub.Latest{Report: *r, ReceivedAt: lastSeen.UnixMilli(), PowerEst: power.Estimate(r)},
		History: pts,
	}
}

// threeDevices is a workstation and a Mac online and a NAS offline for three hours.
func threeDevices() []client.Device {
	ws := workstation(16)

	mac := workstation(12)
	mac.ID = "mac-0001"
	mac.Hostname = report.Ptr("studio")
	mac.Platform = report.PlatformMacOS
	mac.OS = report.Ptr("macOS 15.1 · Mac Studio")
	mac.Uptime = p(26 * 3600)
	mac.CPU.Name, mac.CPU.Usage, mac.CPU.Temp, mac.CPU.ClockMHz = report.Ptr("Apple M2 Ultra"), p(18), nil, nil
	mac.Memory = report.Memory{Total: p(64 * gib), Used: p(41 * gib)}
	mac.GPUs = []report.GPU{{Name: report.Ptr("Apple M2 Ultra"), Vendor: report.Ptr("apple"), Usage: p(12),
		MemUsed: p(8 * gib), MemTotal: p(48 * gib)}}
	mac.Power = nil
	mac.Net = &report.Net{RX: p(220 * kib), TX: p(40 * kib)}
	mac.DiskIO = &report.DiskIO{Read: p(1.5 * mib), Write: p(600 * kib)}

	nas := workstation(4)
	nas.ID = "nas-0002"
	nas.Hostname = report.Ptr("nas-02")
	nas.GPUs, nas.AI = nil, nil

	return []client.Device{
		hubDevice(ws, 0, true, hubNow.Add(-time.Second)),
		hubDevice(mac, 1, true, hubNow.Add(-2*time.Second)),
		hubDevice(nas, 2, false, hubNow.Add(-3*time.Hour)),
	}
}

func newFakeHub(devs []client.Device) *fakeHub {
	f := &fakeHub{devs: map[string]client.Device{}}
	for _, d := range devs {
		f.devs[d.ID] = d
	}
	f.events = []client.Event{{Type: client.EventConnected}, {Type: client.EventSnapshot, Devices: devs}}
	return f
}

func hubOptions(f *fakeHub) Options {
	return Options{Hub: f, Interval: time.Hour, Now: func() time.Time { return hubNow }, Location: time.UTC} // "today" is a local day
}

// synced is true once the hub's snapshot is in, and the header's cost today when the hub
// has energy totals.
func synced(m model) bool { return m.conn.snapshot && (m.source() == nil || m.today.cost != nil) }

// driveHub runs hub mode at w×h, types keys, waits for done and returns the final model.
func driveHub(t *testing.T, o Options, w, h int, keys string, done func(model) bool) model {
	t.Helper()
	ch := make(chan model)
	tm := teatest.NewTestModel(t, probe{New(o), ch}, teatest.WithInitialTermSize(w, h))
	waitUntil(t, tm, ch, synced)
	typeKeys(tm, keys)
	if done == nil {
		done = synced
	}
	waitUntil(t, tm, ch, done)
	return finish(t, tm)
}

func TestHubGolden(t *testing.T) {
	cases := []struct {
		name, keys string
		w, h       int
	}{
		{"grid_80x24", "", 80, 24},
		{"grid_160x48", "", 160, 48},
		{"grid_second_80x24", "j", 80, 24}, // scrolled to the second card
		{"offline_selected_160x48", "jj", 160, 48},
		{"device_second_80x24", "l2", 80, 24},
		{"processes_80x24", "3", 80, 24},
		{"help_80x24", "?", 80, 24},
		{"rename_80x24", "R", 80, 24},
		{"remove_80x24", "D", 80, 24},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := driveHub(t, hubOptions(newFakeHub(threeDevices())), c.w, c.h, c.keys, nil)
			golden.RequireEqual(t, plain(m))
		})
	}
}

// TestGridColumns: columns follow the width, and one column under 80.
func TestGridColumns(t *testing.T) {
	for _, c := range []struct{ w, want int }{{40, 1}, {79, 1}, {80, 1}, {112, 2}, {160, 2}, {200, 3}, {240, 4}} {
		if got := gridColumns(c.w); got != c.want {
			t.Errorf("gridColumns(%d) = %d, want %d", c.w, got, c.want)
		}
	}
	devs := threeDevices()
	devs[2].Online = true
	m := hubModel(t, newFakeHub(devs), 200, 60)
	if top := strings.Split(ansi.Strip(m.render()), "\n")[2]; strings.Count(top, "╮")+strings.Count(top, "┓") != 3 {
		t.Errorf("200 columns: want 3 cards in the first row, got %q", top)
	}
}

// hubModel is a hub-mode model at w×h with the fake's events applied, without a program.
func hubModel(t *testing.T, f *fakeHub, w, h int) model {
	t.Helper()
	m := New(hubOptions(f)).(model)
	next, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	m = next.(model)
	for _, e := range f.events {
		next, _ = m.Update(hubMsg{e})
		m = next.(model)
	}
	return m
}

// run sends msgs and feeds back what their commands answer at once: the hub requests. The
// commands that wait (the stream, the cursor blink) are left alone.
func run(t *testing.T, m model, msgs ...tea.Msg) model {
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
			case renameMsg, removeMsg, orderMsg:
				next, _ = m.Update(res)
				m = next.(model)
			}
		case <-time.After(100 * time.Millisecond):
		}
	}
	return m
}

func keyMsg(k string) tea.Msg {
	switch k {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "backspace":
		return tea.KeyPressMsg{Code: tea.KeyBackspace}
	}
	return tea.KeyPressMsg{Code: []rune(k)[0], Text: k}
}

func keys(ks ...string) []tea.Msg {
	out := make([]tea.Msg, len(ks))
	for i, k := range ks {
		out[i] = keyMsg(k)
	}
	return out
}

func ids(m model) string {
	var s []string
	for _, d := range m.navOrder() {
		s = append(s, d.id)
	}
	return strings.Join(s, ",")
}

func TestHubReorder(t *testing.T) {
	f := newFakeHub(threeDevices())
	m := hubModel(t, f, 160, 48)
	ws := threeDevices()[0].ID
	m = run(t, m, keyMsg("J"))
	if got := ids(m); got != "mac-0001,"+ws+",nas-0002" {
		t.Errorf("after J: %s", got)
	}
	if m.sel != ws {
		t.Errorf("selection moved off the card: %s", m.sel)
	}
	m = run(t, m, keyMsg("J")) // already last online card: no change, no request
	m = run(t, m, keyMsg("K"))
	want := []string{"order mac-0001," + ws + ",nas-0002", "order " + ws + ",mac-0001,nas-0002"}
	if got := f.Calls(); !slices.Equal(got, want) {
		t.Errorf("calls = %q, want %q", got, want)
	}

	// The hub refuses: the old order comes back.
	f.fail = errors.New("boom")
	m = run(t, m, keyMsg("J"))
	if got := ids(m); got != ws+",mac-0001,nas-0002" {
		t.Errorf("after a refused move: %s", got)
	}
	if !strings.Contains(m.hubStatus(), "reorder failed: boom") {
		t.Errorf("footer = %q", m.hubStatus())
	}

	// The hub's order event wins over the local order.
	m = run(t, m, hubMsg{client.Event{Type: client.EventOrder, Order: map[string]int{ws: 2, "mac-0001": 0, "nas-0002": 1}}})
	if got := ids(m); got != "mac-0001,"+ws+",nas-0002" {
		t.Errorf("after an order event: %s", got)
	}
}

func TestHubRename(t *testing.T) {
	f := newFakeHub(threeDevices())
	m := hubModel(t, f, 80, 24)
	msgs := append(keys("l", "R"), keyMsg("backspace"))
	msgs = append(msgs, keys("q", "!")...) // q is text in the box, not quit
	m = run(t, m, msgs...)
	if !m.rename.Focused() || m.rename.Value() != "studiq!" {
		t.Fatalf("rename box: focused %v, %q", m.rename.Focused(), m.rename.Value())
	}
	m = run(t, m, keyMsg("enter"))
	if got := f.Calls(); !slices.Equal(got, []string{"rename mac-0001 studiq!"}) {
		t.Errorf("calls = %q", got)
	}
	if d := m.cur(); d.name != "studiq!" || !d.online || d.order != 1 {
		t.Errorf("renamed device = %+v", d)
	}
	m = run(t, m, keys("R", "x", "esc")...)
	if len(f.Calls()) != 1 || m.cur().name != "studiq!" {
		t.Error("esc still renamed")
	}
}

func TestHubRemove(t *testing.T) {
	f := newFakeHub(threeDevices())
	m := hubModel(t, f, 80, 24)
	m = run(t, m, keys("j", "j", "D", "n")...) // down to the NAS in the offline row; n cancels
	if len(f.Calls()) != 0 || len(m.devs) != 3 {
		t.Fatalf("cancelled remove went through: %q", f.Calls())
	}
	m = run(t, m, keys("D", "y")...)
	if got := f.Calls(); !slices.Equal(got, []string{"remove nas-0002"}) {
		t.Errorf("calls = %q", got)
	}
	if len(m.devs) != 2 || m.sel != "mac-0001" {
		t.Errorf("after remove: %s, selected %s", ids(m), m.sel)
	}
	// The hub's own removed event for a device already gone changes nothing.
	m = run(t, m, hubMsg{client.Event{Type: client.EventRemoved, ID: "nas-0002"}})
	if len(m.devs) != 2 {
		t.Error("double remove")
	}
}

// TestHubEvents: live events update the cards; a device going quiet moves to the offline row.
func TestHubEvents(t *testing.T) {
	devs := threeDevices()
	m := hubModel(t, newFakeHub(devs), 160, 48)
	ws := devs[0].ID

	upd := devs[1]
	upd.Latest = &hub.Latest{Report: upd.Latest.Report}
	upd.Latest.CPU.Usage = p(99)
	upd.History = nil
	m = run(t, m, hubMsg{client.Event{Type: client.EventReport, ID: upd.ID, Device: &upd, Sample: &history.Point{CPU: p(99)}}})
	mac := m.devs[slices.IndexFunc(m.devs, func(d device) bool { return d.id == "mac-0001" })]
	if *mac.last.CPU.Usage != 99 || mac.cpu.v[len(mac.cpu.v)-1] != 99 || len(mac.cpu.v) != 41 {
		t.Errorf("report not applied: %v", mac.cpu.v)
	}

	m = run(t, m, hubMsg{client.Event{Type: client.EventStatus, Online: map[string]bool{ws: false}}})
	if got := ids(m); got != "mac-0001,"+ws+",nas-0002" {
		t.Errorf("offline devices not last: %s", got)
	}
	frame := ansi.Strip(m.render())
	if !strings.Contains(frame, "Offline (2)") || !strings.Contains(frame, "1 online · 2 offline") {
		t.Errorf("frame:\n%s", frame)
	}

	// A brand-new device appears with its first report.
	nd := hubDevice(workstation(2), 3, true, hubNow)
	nd.ID, nd.Name = "new-0003", "pi"
	m = run(t, m, hubMsg{client.Event{Type: client.EventReport, ID: nd.ID, Device: &nd}})
	if len(m.devs) != 4 {
		t.Errorf("new device not added: %s", ids(m))
	}

	// Disconnected: the footer says so, and the cards stay until a fresh snapshot.
	m = run(t, m, hubMsg{client.Event{Type: client.EventDisconnected, Err: fmt.Errorf("dial: %w", syscall.ECONNREFUSED), Retry: 4 * time.Second}})
	if s := m.hubStatus(); !strings.Contains(s, "unreachable: the hub isn't accepting connections (is it running?) · retrying in 4s") {
		t.Errorf("footer = %q", s)
	}
	m = run(t, m, hubMsg{client.Event{Type: client.EventSnapshot, Devices: devs[:1]}})
	if len(m.devs) != 1 || m.sel != ws {
		t.Errorf("snapshot did not replace the list: %s (selected %s)", ids(m), m.sel)
	}
	m = run(t, m, hubMsg{client.Event{Type: client.EventDisconnected, Err: client.ErrBadProof}})
	if !m.conn.ended || !strings.Contains(m.hubStatus(), "could not prove") {
		t.Errorf("footer after a bad proof = %q", m.hubStatus())
	}
}

// TestHubResize checks every hub-mode frame fills the window exactly, at any size.
func TestHubResize(t *testing.T) {
	m := hubModel(t, newFakeHub(threeDevices()), 80, 24)
	for _, s := range [][2]int{{40, 12}, {80, 24}, {100, 30}, {160, 48}, {200, 60}, {20, 5}, {80, 2}} {
		m.width, m.height = s[0], s[1]
		for _, sel := range []string{threeDevices()[0].ID, "mac-0001", "nas-0002"} {
			m.sel = sel
			for v := range viewCount {
				m.view = v
				checkFrame(t, m, s[0], s[1])
			}
		}
	}
	empty := hubModel(t, newFakeHub(nil), 80, 24)
	connecting := New(hubOptions(newFakeHub(nil))).(model)
	for _, mm := range []model{empty, connecting} {
		for v := range viewCount {
			mm.view = v
			checkFrame(t, mm, 80, 24)
		}
	}
	if f := ansi.Strip(connecting.render()); !strings.Contains(f, "Connecting to the hub at hub.lan:7777") {
		t.Errorf("connecting frame:\n%s", f)
	}
}

// TestHubNoColor: the grid, selection and offline row set no colours under NO_COLOR.
func TestHubNoColor(t *testing.T) {
	o := hubOptions(newFakeHub(threeDevices()))
	o.NoColor = true
	m := New(o).(model)
	for _, e := range o.Hub.(*fakeHub).events {
		next, _ := m.Update(hubMsg{e})
		m = next.(model)
	}
	m.width, m.height = 160, 48
	for _, sel := range []string{"mac-0001", "nas-0002"} {
		m.sel = sel
		if hasColorSGR(m.render()) {
			t.Errorf("colour codes under NO_COLOR with %s selected", sel)
		}
	}
}

func TestAgo(t *testing.T) {
	m := New(hubOptions(newFakeHub(nil))).(model)
	for _, c := range []struct {
		ago  time.Duration
		want string
	}{{3 * time.Second, "just now"}, {11 * time.Second, "11s ago"}, {5 * time.Minute, "5m ago"}, {3 * time.Hour, "3h ago"}, {72 * time.Hour, "3d ago"}} {
		if got := m.ago(hubNow.Add(-c.ago).UnixMilli()); got != c.want {
			t.Errorf("ago(%v) = %q, want %q", c.ago, got, c.want)
		}
	}
	if got := m.ago(0); got != "never seen" {
		t.Errorf("ago(0) = %q", got)
	}
}

// TestHubStreamEnds: when the stream closes, the footer says updates have stopped.
func TestHubStreamEnds(t *testing.T) {
	m := New(hubOptions(newFakeHub(threeDevices()))).(model)
	ctx, cancel := context.WithCancel(context.Background())
	m.ctx = ctx
	next, cmd := m.Update(m.startStream()()) // streamMsg: start reading
	m = next.(model)
	next, cmd = m.Update(cmd()) // connected and snapshot, in one batch
	m = next.(model)
	if !m.conn.connected || !m.conn.snapshot {
		t.Fatalf("not synced: %+v", m.conn)
	}
	cancel()
	msg := cmd()
	if _, ok := msg.(hubDoneMsg); !ok {
		t.Fatalf("after the stream closed got %T", msg)
	}
	next, cmd = m.Update(msg)
	m = next.(model)
	if cmd != nil {
		t.Error("still waiting on a closed stream")
	}
	if s := m.hubStatus(); !strings.Contains(s, "hub hub.lan:7777: the stream ended · no longer updating · q quit") {
		t.Errorf("footer = %q", s)
	}
}

// TestHubLateFailures: a reorder refused after the hub sent its own order keeps the hub's
// order, and a rename answered after the device was removed doesn't bring it back.
func TestHubLateFailures(t *testing.T) {
	f := newFakeHub(threeDevices())
	m := hubModel(t, f, 160, 48)
	ws := threeDevices()[0].ID
	next, cmd := m.Update(keyMsg("J")) // the PUT is now "in flight"
	m = next.(model)
	m = run(t, m, hubMsg{client.Event{Type: client.EventOrder, Order: map[string]int{"nas-0002": 0, "mac-0001": 1, ws: 2}}})
	f.fail = errors.New("boom")
	next, _ = m.Update(cmd())
	m = next.(model)
	if got := ids(m); got != "mac-0001,"+ws+",nas-0002" || m.devs[0].id != "nas-0002" {
		t.Errorf("late rollback overwrote the hub's order: %s (devs[0] %s)", got, m.devs[0].id)
	}
	if !strings.Contains(m.hubStatus(), "reorder failed") {
		t.Errorf("footer = %q", m.hubStatus())
	}

	// Rename answered after removal.
	f.fail = nil
	next, cmd = m.Update(keyMsg("R"))
	m = next.(model)
	_ = cmd
	next, cmd = m.Update(keyMsg("enter"))
	m = next.(model)
	m = run(t, m, hubMsg{client.Event{Type: client.EventRemoved, ID: m.renameID}})
	gone := m.renameID
	next, _ = m.Update(cmd())
	m = next.(model)
	if m.hasDev(gone) {
		t.Errorf("rename brought back removed %s", gone)
	}

	// A fresh connection clears a stale error so the connection state shows.
	m.notice = "rename failed: boom"
	m = run(t, m, hubMsg{client.Event{Type: client.EventDisconnected, Err: errors.New("refused"), Retry: 0}})
	m.conn.ended = false
	m = run(t, m, hubMsg{client.Event{Type: client.EventConnected}})
	if m.notice != "" {
		t.Errorf("notice survived a reconnect: %q", m.notice)
	}
	m = run(t, m, hubMsg{client.Event{Type: client.EventDisconnected, Err: errors.New("refused"), Retry: 300 * time.Millisecond}})
	if s := m.hubStatus(); !strings.Contains(s, "retrying in 1s") {
		t.Errorf("footer = %q", s)
	}
}

// TestOfflineRowWindow: many offline devices show a window around the selection with a tail.
func TestOfflineRowWindow(t *testing.T) {
	var devs []client.Device
	for i := range 12 {
		r := workstation(2)
		r.ID, r.Hostname = fmt.Sprintf("dev-%02d", i), report.Ptr(fmt.Sprintf("machine-%02d", i))
		devs = append(devs, hubDevice(r, i, false, hubNow.Add(-time.Hour)))
	}
	m := hubModel(t, newFakeHub(devs), 80, 24)
	for _, sel := range []string{"dev-00", "dev-06", "dev-11"} {
		m.sel = sel
		row := ansi.Strip(m.offlineRow(m.offline()))
		if w := ansi.StringWidth(row); w > 80 {
			t.Errorf("%s: row is %d wide: %q", sel, w, row)
		}
		want := "▶ machine-" + sel[4:]
		if !strings.Contains(row, want) || !strings.Contains(row, "more") {
			t.Errorf("%s: row %q lacks the selection or the tail", sel, row)
		}
	}
	m.sel = "dev-11"
	row := ansi.Strip(m.offlineRow(m.offline()))
	if strings.Contains(row, "machine-00") {
		t.Errorf("window not around the selection: %q", row)
	}
}
