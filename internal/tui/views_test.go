package tui

import (
	"context"
	"fmt"
	"math"
	"strings"
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
)

// Tests don't wait out the key debounce; TestFetchDebounce checks it still coalesces.
func init() { fetchDebounce = time.Millisecond }

// testPrice is the saved price every view test uses, so no golden depends on the time zone.
var testPrice = power.Settings{Region: "US", Rate: 0.165, Currency: "USD"}

// History and Energy make fakeHub a viewSource with fixed, synthetic data.
func (f *fakeHub) History(ctx context.Context, id string, from, to float64, points int) (*history.Result, error) {
	if err := f.record(fmt.Sprintf("history %s %.0f %.0f %d", id, from, to, points)); err != nil {
		return nil, err
	}
	r := synthHistory(id, from, to, points)
	return &r, nil
}

func (f *fakeHub) Energy(ctx context.Context, bounds []float64) (*client.Energy, error) {
	if err := f.record(fmt.Sprintf("energy %d", len(bounds))); err != nil {
		return nil, err
	}
	s := testPrice
	e := &client.Energy{Bounds: bounds, Settings: &s}
	for k, d := range threeDevices() {
		e.Devices = append(e.Devices, synthUsage(d.ID, k, bounds, s))
	}
	return e, nil
}

// synthHistory is deterministic history between from and to: buckets aligned to from, at
// least a minute wide, with an offline stretch in the middle and wave-like values.
func synthHistory(id string, from, to float64, points int) history.Result {
	res := math.Max(60e3, math.Ceil((to-from)/float64(points)/60e3)*60e3)
	seed := len(id)
	var pts []history.Point
	n := int((to - from) / res)
	for i := 0; i <= n; i++ {
		if i > n*45/100 && i < n*52/100 { // offline
			continue
		}
		k := i*7 + seed
		wave := float64((k*13)%61) + float64(i%40)
		pt := history.Point{T: from + float64(i)*res,
			CPU: p(15 + wave*0.8), CPUMax: p(math.Min(100, 25+wave)),
			GPU: p(float64((k*29)%90) + 5), GPUMax: p(float64((k*29)%90) + 10),
			Mem:     p(40 + float64(i%30)),
			CPUTemp: p(45 + wave*0.4), CPUTempMax: p(50 + wave*0.45),
			GPUTemp: p(40 + float64((k*29)%90)*0.5), GPUTempMax: p(44 + float64((k*29)%90)*0.5),
			GPUPower: p(30 + float64((k*29)%90)*2.5),
			RX:       p(float64((k*97)%40) * mib), TX: p(float64((k*31)%20) * 64 * kib),
			DR: p(float64((k*53)%30) * mib), DW: p(float64((k*17)%10) * mib),
		}
		pt.Power = p(120 + *pt.CPU*1.5 + *pt.GPUPower)
		pt.PowerMax = p(*pt.Power + 40)
		pts = append(pts, pt)
	}
	return history.Result{From: from, To: to, Resolution: res, Points: pts}
}

// synthUsage is deterministic energy totals for device k over bounds.
func synthUsage(id string, k int, bounds []float64, price power.Settings) hub.Usage {
	n := len(bounds) - 1
	u := hub.Usage{ID: id, Stats: map[string]hub.Stat{}, Bytes: map[string]int64{}}
	scale := []float64{1, 0.35, 0.12}[k%3]
	for i := range n {
		wh := math.Round((bounds[i+1]-bounds[i])/3600e3*(300+float64((i*37+k*11)%9)*40)*scale*10) / 10
		u.Buckets = append(u.Buckets, wh)
		u.CostBuckets = append(u.CostBuckets, math.Round(wh/1000*price.Rate*1e4)/1e4)
		u.Wh += wh
	}
	u.Wh = math.Round(u.Wh)
	c := math.Round(u.Wh/1000*price.Rate*1e4) / 1e4
	u.Cost = &c
	span := bounds[n] - bounds[0]
	u.Minutes = int(span / 60e3 * []float64{0.98, 0.6, 0.4}[k%3])
	first := int64(bounds[0])
	u.FirstData = &first
	u.Stats["power"] = hub.Stat{Avg: 420 * scale, Peak: p(610 * scale)}
	u.Stats["cpu"] = hub.Stat{Avg: 41.2 - float64(k)*10, Peak: p(98 - float64(k)*20)}
	u.Stats["mem"] = hub.Stat{Avg: 37 + float64(k)*12}
	u.Stats["cpuTemp"] = hub.Stat{Avg: 60, Peak: p(88 - float64(k)*9)}
	if k != 2 {
		u.Stats["gpu"] = hub.Stat{Avg: 55 - float64(k)*40, Peak: p(99 - float64(k)*50)}
		u.Stats["gpuTemp"] = hub.Stat{Avg: 60, Peak: p(91 - float64(k)*30)}
	}
	u.Bytes = map[string]int64{"rx": int64(span / 1000 * 2.1e6 * scale), "tx": int64(span / 1000 * 1.4e5 * scale),
		"dr": int64(span / 1000 * 5.5e6 * scale), "dw": int64(span / 1000 * 3.3e5 * scale)}
	return u
}

// viewOptions is hub mode on the fake with the views' clock, zone and price fixed.
func viewOptions(f *fakeHub) Options {
	o := hubOptions(f)
	o.Location = time.UTC
	o.Price = &testPrice
	return o
}

// histLoaded and energyLoaded hold once the data on screen is for the current window or
// range, not one a debounced key has since replaced.
func histLoaded(m model) bool {
	from, to := m.histWindow()
	r := m.hv.res
	return r != nil && m.hv.resID == m.cur().id && r.From == from && r.To == to
}

func energyLoaded(m model) bool {
	b := energyBounds(m.now().In(m.loc()), m.ev.rng)
	d := m.ev.data
	return d != nil && len(d.Bounds) == len(b) && d.Bounds[0] == b[0]
}

func TestViewsGolden(t *testing.T) {
	cases := []struct {
		name, keys string
		w, h       int
		done       func(model) bool
		then       string // keys typed once done holds, e.g. scrolling the loaded view
	}{
		{"history_80x24", "4", 80, 24, histLoaded, ""},
		{"history_160x48", "4", 160, 48, histLoaded, ""},
		{"energy_80x24", "5", 80, 24, energyLoaded, ""},
		{"energy_160x48", "5", 160, 48, energyLoaded, ""},
		{"history_30d_160x48", "4]]]", 160, 48, histLoaded, ""},
		{"history_zoom_80x24", "4[++hhhhhhhhhh", 80, 24, histLoaded, ""},
		{"history_second_80x24", "4j", 80, 24, histLoaded, ""},
		{"energy_24h_160x48", "5[", 160, 48, energyLoaded, ""},
		{"energy_30d_80x24", "5]", 80, 24, energyLoaded, "jjjjjjjjjj"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeHub(threeDevices())
			ch := make(chan model)
			tm := teatest.NewTestModel(t, probe{New(viewOptions(f)), ch}, teatest.WithInitialTermSize(c.w, c.h))
			waitUntil(t, tm, ch, synced)
			typeKeys(tm, c.keys)
			waitUntil(t, tm, ch, c.done)
			typeKeys(tm, c.then)
			waitUntil(t, tm, ch, c.done)
			golden.RequireEqual(t, plain(finish(t, tm)))
		})
	}
}

// localViewOptions is local mode with a session store holding two hours of samples.
func localViewOptions(t *testing.T) Options {
	t.Helper()
	s, err := history.Open(t.TempDir(), func() time.Time { return hubNow })
	if err != nil {
		t.Fatal(err)
	}
	r := workstation(16)
	start := hubNow.Add(-2 * time.Hour)
	for i := range 3600 {
		at := start.Add(time.Duration(i) * 2 * time.Second)
		if i/300 == 7 { // ten minutes off
			continue
		}
		c := *r
		c.CPU.Usage = p(float64(20 + (i/15*7)%60))
		pt := localSample(at.UnixMilli(), &c)
		if err := s.Add(localID, pt); err != nil {
			t.Fatal(err)
		}
	}
	o := testOptions()
	o.History, o.Location, o.Price = s, time.UTC, &testPrice
	o.Now = func() time.Time { return hubNow }
	return o
}

func TestLocalViewsGolden(t *testing.T) {
	for _, c := range []struct {
		name, keys string
		done       func(model) bool
		w, h       int
	}{
		{"history_local_80x24", "4", histLoaded, 80, 24},
		{"energy_local_80x24", "5[", energyLoaded, 80, 24},
		{"history_local_160x48", "4", histLoaded, 160, 48},
		{"energy_local_160x48", "5[", energyLoaded, 160, 48},
	} {
		t.Run(c.name, func(t *testing.T) {
			ch := make(chan model)
			tm := teatest.NewTestModel(t, probe{New(localViewOptions(t)), ch}, teatest.WithInitialTermSize(c.w, c.h))
			waitUntil(t, tm, ch, loaded)
			typeKeys(tm, c.keys)
			waitUntil(t, tm, ch, c.done)
			golden.RequireEqual(t, plain(finish(t, tm)))
		})
	}
}

// settleViews sends msgs and applies the History and Energy replies their commands produce.
// Commands that wait (ticks, the stream) are left alone.
func settleViews(t *testing.T, m model, msgs ...tea.Msg) model {
	t.Helper()
	var exec func(cmd tea.Cmd)
	exec = func(cmd tea.Cmd) {
		if cmd == nil {
			return
		}
		out := make(chan tea.Msg, 1)
		go func() { out <- cmd() }()
		select {
		case res := <-out:
			switch res := res.(type) {
			case tea.BatchMsg:
				for _, c := range res {
					exec(c)
				}
			case histMsg, energyMsg:
				next, _ := m.Update(res)
				m = next.(model)
			case histFetchMsg, energyFetchMsg: // a debounced read is due: send it
				next, cmd := m.Update(res)
				m = next.(model)
				exec(cmd)
			}
		case <-time.After(100 * time.Millisecond):
		}
	}
	for _, msg := range msgs {
		next, cmd := m.Update(msg)
		m = next.(model)
		exec(cmd)
	}
	return m
}

// lastCall is the fake hub's last request.
func lastCall(f *fakeHub) string {
	c := f.Calls()
	if len(c) == 0 {
		return ""
	}
	return c[len(c)-1]
}

func TestHistoryKeys(t *testing.T) {
	f := newFakeHub(threeDevices())
	m := hubModel(t, f, 160, 48)
	m.opts.Location = time.UTC
	now := float64(hubNow.UnixMilli())
	const minute = 60e3
	window := func() (float64, float64) { return m.histWindow() }
	want := func(step string, from, to float64, zoomed bool) {
		t.Helper()
		gf, gt := window()
		if gf != from || gt != to || m.hv.zoomed != zoomed {
			t.Fatalf("%s: window %.0f..%.0f zoomed %v, want %.0f..%.0f zoomed %v (min from now: %.2f..%.2f)",
				step, gf, gt, m.hv.zoomed, from, to, zoomed, (gf-now)/minute, (gt-now)/minute)
		}
		if c := lastCall(f); c != fmt.Sprintf("history %s %.0f %.0f %d", m.cur().id, from, to, m.histPlotW()*2) {
			t.Fatalf("%s: last request %q", step, c)
		}
		if !histLoaded(m) {
			t.Fatalf("%s: no result", step)
		}
	}

	m = settleViews(t, m, keyMsg("4"))
	want("open", now-360*minute, now, false)
	m = settleViews(t, m, keyMsg("]"))
	want("]", now-24*60*minute, now, false)
	m = settleViews(t, m, keyMsg("["), keyMsg("["))
	want("[[", now-60*minute, now, false)
	n := len(f.Calls())
	m = settleViews(t, m, keyMsg("[")) // already the shortest: no request
	if len(f.Calls()) != n || m.hv.rng != 0 {
		t.Fatalf("[ at 1h: rng %d, calls %d -> %d", m.hv.rng, n, len(f.Calls()))
	}

	// Zoom in at the cursor (the right edge): the window keeps its right end at now.
	m = settleViews(t, m, keyMsg("+"))
	want("+", now-30*minute, now, true)
	m = settleViews(t, m, keyMsg("-")) // back to the whole hour, which follows now again
	want("-", now-60*minute, now, false)
	m = settleViews(t, m, keyMsg("-")) // not zoomed: nothing to zoom out of
	if m.hv.zoomed {
		t.Fatal("- past the range zoomed")
	}
	for range 6 {
		m = settleViews(t, m, keyMsg("+"))
	}
	if from, to := window(); to-from != histMinSpan {
		t.Fatalf("deepest zoom spans %.1f min", (to-from)/minute)
	}

	// Pan: a quarter of the span each way, never past now.
	m = settleViews(t, m, keyMsg("0"))
	want("0", now-60*minute, now, false)
	m = settleViews(t, m, keyMsg("left"))
	want("←", now-75*minute, now-15*minute, true)
	m = settleViews(t, m, keyMsg("left"))
	want("←←", now-90*minute, now-30*minute, true)
	m = settleViews(t, m, keyMsg("right"), keyMsg("right"), keyMsg("right"))
	want("→→→ (stops at now)", now-60*minute, now, false)

	// The 30-day range can't pan before the hub's 30 days.
	m = settleViews(t, m, keyMsg("]"), keyMsg("]"), keyMsg("]"), keyMsg("]"))
	want("30d", now-histKeep, now, false)
	n = len(f.Calls())
	m = settleViews(t, m, keyMsg("left"))
	if len(f.Calls()) != n || m.hv.zoomed {
		t.Fatal("panned before 30 days")
	}
	m = settleViews(t, m, keyMsg("+"), keyMsg("left"), keyMsg("left"), keyMsg("left"), keyMsg("left"), keyMsg("left"))
	if from, _ := window(); from != now-histKeep {
		t.Fatalf("zoomed pan went %.1f days back", (now-from)/86400e3)
	}

	// Cursor: h moves it a column left, l back, and not past the edge.
	w := m.histPlotW()
	m = settleViews(t, m, keyMsg("h"), keyMsg("h"))
	if c := m.cursorCol(w); c != w-3 {
		t.Fatalf("cursor at %d after hh, want %d", c, w-3)
	}
	m = settleViews(t, m, keyMsg("l"), keyMsg("l"), keyMsg("l"))
	if c := m.cursorCol(w); c != w-1 {
		t.Fatalf("cursor at %d after lll, want %d", c, w-1)
	}

	// j moves to the next device and reads its history.
	m = settleViews(t, m, keyMsg("j"))
	if m.sel != "mac-0001" || !strings.HasPrefix(lastCall(f), "history mac-0001 ") || !histLoaded(m) {
		t.Fatalf("j: sel %s, last %q", m.sel, lastCall(f))
	}
}

// TestHistoryStale: a reply for an older request never replaces a newer one.
func TestHistoryStale(t *testing.T) {
	m := hubModel(t, newFakeHub(threeDevices()), 80, 24)
	m.view = viewHistory
	m.fetchHistory()
	old := m.hv.seq
	m.fetchHistory()
	next, _ := m.Update(histMsg{seq: old, id: m.cur().id, res: &history.Result{}})
	if m = next.(model); m.hv.res != nil {
		t.Fatal("stale reply applied")
	}
}

func TestEnergyKeys(t *testing.T) {
	f := newFakeHub(threeDevices())
	m := hubModel(t, f, 80, 24)
	m.opts.Location = time.UTC
	steps := []struct {
		key    string
		rng    int
		bounds int
	}{{"5", 1, 8}, {"]", 2, 31}, {"]", 2, 31}, {"[", 1, 8}, {"[", 0, 25}}
	for _, s := range steps {
		m = settleViews(t, m, keyMsg(s.key))
		if m.ev.rng != s.rng || lastCall(f) != fmt.Sprintf("energy %d", s.bounds) || !energyLoaded(m) {
			t.Fatalf("%s: rng %d, last %q", s.key, m.ev.rng, lastCall(f))
		}
	}
	m = settleViews(t, m, keyMsg("G"))
	if max := len(m.energyLines()) - m.bodyHeight(); m.ev.scroll != max || max <= 0 {
		t.Fatalf("G: scroll %d, want %d", m.ev.scroll, max)
	}
	m = settleViews(t, m, keyMsg("g"))
	if m.ev.scroll != 0 {
		t.Fatalf("g: scroll %d", m.ev.scroll)
	}
}

func TestEnergyBounds(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 34, 0, 0, time.UTC)
	ms := func(t time.Time) float64 { return float64(t.UnixMilli()) }
	for _, c := range []struct {
		rng         int
		n           int
		first, last time.Time
	}{
		{0, 25, time.Date(2026, 10, 30, 13, 0, 0, 0, time.UTC), time.Date(2026, 10, 31, 13, 0, 0, 0, time.UTC)},
		{1, 8, time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)},
		{2, 31, time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)},
	} {
		b := energyBounds(now, c.rng)
		if len(b) != c.n || b[0] != ms(c.first) || b[len(b)-1] != ms(c.last) {
			t.Errorf("range %d: %d bounds %v..%v", c.rng, len(b), time.UnixMilli(int64(b[0])).UTC(), time.UnixMilli(int64(b[len(b)-1])).UTC())
		}
	}
	// Local midnights across a DST change are 23 or 25 hours apart, not 24.
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip(err)
	}
	b := energyBounds(time.Date(2026, 11, 3, 9, 0, 0, 0, ny), 1)
	if d := b[5] - b[4]; d != 25*3600e3 { // Nov 1 2026 is 25 hours long in New York
		t.Errorf("DST day is %.0f h", d/3600e3)
	}
}

// TestLocalUsage checks local energy totals against a hand calculation.
func TestLocalUsage(t *testing.T) {
	base := float64(hubNow.UnixMilli())
	pts := []history.Point{
		{T: base, Power: p(600), PowerMax: p(700), CPU: p(50), RX: p(1000)},
		{T: base + 60e3, Power: p(300), CPU: p(10)},
		{T: base + 120e3, CPU: p(100)},   // no power: estimated from load
		{T: base + 180e3, Power: p(900)}, // past the last bound: not counted
	}
	hw := power.HW{Threads: p(8)}
	price := power.Settings{Rate: 0.2, Currency: "USD"}
	u := usageOf(pts, []float64{base, base + 120e3, base + 180e3}, hw, &price, time.UTC)
	est := *power.FromSample(p(100), nil, hw)
	wantWh := 600.0/60 + 300.0/60 + est/60
	if u.Wh != math.Round(wantWh) || u.Minutes != 3 || u.EstimatedMinutes != 1 || len(u.Buckets) != 2 ||
		u.Buckets[0] != 15 || u.Cost == nil || math.Abs(*u.Cost-wantWh/1000*0.2) > 1e-4 {
		t.Errorf("usage = %+v, cost %v; want Wh %.2f", u.Usage, *u.Cost, wantWh)
	}
	if s := u.Stats["power"]; s.Peak == nil || *s.Peak != 700 || s.Avg != round1((600+300+est)/3) {
		t.Errorf("power stat = %+v", s)
	}
	if u.Bytes["rx"] != 60000 || u.Stats["cpu"].Avg != 53.3 {
		t.Errorf("bytes %v, cpu %+v", u.Bytes, u.Stats["cpu"])
	}
}

func TestEnergyFormats(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{money(1234.5, "USD"), "$1,235"},
		{money(12.345, "EUR"), "€12.35"},
		{money(0.5, "SEK"), "0.50 SEK"},
		{fmtKwhPrice(0.165, "USD"), "$0.165"},
		{fmtKwh(3.456), "3.46 kWh"},
		{fmtKwh(45.67), "45.7 kWh"},
		{fmtKwh(1234.4), "1,234 kWh"},
		{fmtW(p(1500)), "1.50 kW"},
		{fmtW(p(99.5)), "100 W"},
		{fmtPrice(power.Settings{Rate: 0.2, Currency: "USD", TOU: []power.Period{{From: 16, To: 21, Rate: 0.45}}}),
			"$0.200–$0.450/kWh by time of day"},
		{priceSource(power.Settings{Region: "US", Rate: 0.165}, true), "Typical home rate for United States (average), guessed from this device's time zone."},
		{priceSource(power.Settings{Region: "US", Rate: 0.165}, false), "Typical home rate for United States (average)."},
		{priceSource(power.Settings{Region: "custom", Rate: 0.2}, false), "The rate you entered."},
		{fmtBucket(2000), "2 s"},
		{fmtBucket(300e3), "5 min"},
		{fmtBucket(8640e3), "2.4 h"},
		{shortRate(12.4 * mib), "12M/s"},
		{shortRate(2.5 * kib), "2.5K/s"},
		{shortRate(1000), "1000B/s"},
		{shortRate(1023), "1023B/s"},
		{shortRate(1024), "1K/s"},
	} {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
}

// TestViewsResize checks the new views fill odd window sizes exactly, with and without data.
func TestViewsResize(t *testing.T) {
	f := newFakeHub(threeDevices())
	m := hubModel(t, f, 160, 48)
	m.opts.Location = time.UTC
	m = settleViews(t, m, keyMsg("4"), keyMsg("5"))
	for _, s := range [][2]int{{20, 5}, {40, 12}, {79, 23}, {200, 60}, {100, 3}} {
		m.width, m.height = s[0], s[1]
		for _, v := range []view{viewHistory, viewEnergy} {
			m.view = v
			checkFrame(t, m, s[0], s[1])
		}
	}
}

// bigHistory is a hub-mode model on the History view's 30-day range at w×h, holding the
// given number of points: 0 for what the view asks for (two per column), or up to the hub's
// cap of 3000.
func bigHistory(b testing.TB, w, h, points int) model {
	f := newFakeHub(threeDevices())
	m := New(viewOptions(f)).(model)
	next, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	m = next.(model)
	for _, e := range f.events {
		next, _ = m.Update(hubMsg{e})
		m = next.(model)
	}
	m.view, m.hv.rng = viewHistory, len(histRanges)-1
	from, to := m.histWindow()
	if points == 0 {
		points = m.histPlotW() * 2
	}
	r := synthHistory(m.cur().id, from, to, points)
	m.hv.res, m.hv.resID = &r, m.cur().id
	return m
}

// BenchmarkHistory30d renders the History view's 30-day range at 160×48, with the points
// the view asks for and with the hub's cap.
func BenchmarkHistory30d(b *testing.B) {
	for _, n := range []int{0, histMaxPoints} {
		name := "requested"
		if n > 0 {
			name = "hub_cap"
		}
		b.Run(name, func(b *testing.B) {
			m := bigHistory(b, 160, 48, n)
			b.ReportAllocs()
			for b.Loop() {
				_ = m.render()
			}
		})
	}
}

// TestHistoryFrameBudget holds the 30-day History frame and the 30-day Energy frame at
// 160 columns under the frame budget on every run.
func TestHistoryFrameBudget(t *testing.T) {
	if raceDetector {
		t.Skip("frame times under -race measure the instrumentation; the plain run checks the budget")
	}
	hist := bigHistory(t, 160, 48, 0)
	en := hist
	en.view, en.ev.rng = viewEnergy, 2
	bounds := energyBounds(hubNow, 2)
	en.ev.data = &client.Energy{Bounds: bounds, Settings: &testPrice}
	for k, d := range threeDevices() {
		en.ev.data.Devices = append(en.ev.data.Devices, synthUsage(d.ID, k, bounds, testPrice))
	}
	for name, m := range map[string]model{"History 30d": hist, "Energy 30d": en} {
		const n = 20
		start := time.Now()
		for range n {
			_ = m.render()
		}
		if avg := time.Since(start) / n; avg >= frameBudget {
			t.Errorf("%s frame takes %v, budget %v", name, avg, frameBudget)
		} else {
			t.Logf("%s frame: %v", name, avg)
		}
	}
}

// TestViewsNoColor checks the History and Energy views, with data, set no colours under
// NO_COLOR.
func TestViewsNoColor(t *testing.T) {
	m := bigHistory(t, 160, 48, 0)
	m.opts.NoColor = true
	m.theme = newTheme(true, false)
	if hasColorSGR(m.render()) {
		t.Error("History view has colour codes under NO_COLOR")
	}
	bounds := energyBounds(hubNow, 1)
	m.view, m.ev.data = viewEnergy, &client.Energy{Bounds: bounds, Settings: &testPrice}
	for k, d := range threeDevices() {
		m.ev.data.Devices = append(m.ev.data.Devices, synthUsage(d.ID, k, bounds, testPrice))
	}
	if hasColorSGR(m.render()) {
		t.Error("Energy view has colour codes under NO_COLOR")
	}
}

// TestFetchDebounce: holding a range key sends one request, for the last window.
func TestFetchDebounce(t *testing.T) {
	f := newFakeHub(threeDevices())
	m := settleViews(t, hubModel(t, f, 160, 48), keyMsg("4"))
	n := len(f.Calls())
	var due []tea.Cmd
	for _, k := range []string{"]", "]", "]"} { // three repeats before the debounce fires
		next, cmd := m.Update(keyMsg(k))
		m = next.(model)
		due = append(due, cmd)
	}
	for _, cmd := range due {
		next, fetch := m.Update(cmd())
		m = next.(model)
		if fetch != nil {
			next, _ = m.Update(fetch())
			m = next.(model)
		}
	}
	calls := f.Calls()[n:]
	from, to := m.histWindow()
	want := fmt.Sprintf("history %s %.0f %.0f %d", m.cur().id, from, to, m.histPlotW()*2)
	if len(calls) != 1 || calls[0] != want || m.hv.rng != 4 {
		t.Fatalf("calls %q, want one %q (rng %d)", calls, want, m.hv.rng)
	}
}

// TestHistoryErrorClears: an error from one device doesn't stay on screen for the next.
func TestHistoryErrorClears(t *testing.T) {
	f := newFakeHub(threeDevices())
	m := hubModel(t, f, 80, 24)
	f.fail = fmt.Errorf("boom")
	m = settleViews(t, m, keyMsg("4"))
	if m.hv.err == nil || !strings.Contains(plain(m), "history: boom") {
		t.Fatalf("err %v", m.hv.err)
	}
	f.fail = nil
	next, _ := m.Update(keyMsg("j"))
	if m = next.(model); m.hv.err != nil || strings.Contains(plain(m), "boom") {
		t.Fatalf("after j: err %v", m.hv.err)
	}
}

// TestEnergyNarrow: a window narrower than the buckets shows the latest ones whole.
func TestEnergyNarrow(t *testing.T) {
	m := hubModel(t, newFakeHub(threeDevices()), 30, 40)
	m.opts.Location = time.UTC
	m = settleViews(t, m, keyMsg("5"), keyMsg("]")) // 30 days in 20 plot columns
	if m.ev.rng != 2 || !energyLoaded(m) {
		t.Fatal("not loaded")
	}
	lines := m.energyChart(m.energyRows(), m.ev.data.Bounds)
	for _, l := range lines {
		if w := ansi.StringWidth(l); w > 30 {
			t.Errorf("chart line %d wide: %q", w, ansi.Strip(l))
		}
	}
	// 30 buckets in 20 columns: the chart starts at bucket 10 (Oct 12) and labels don't touch.
	axis := strings.Fields(ansi.Strip(lines[len(lines)-1]))
	if want := m.bucketLabel(m.ev.data.Bounds, 10); len(axis) < 2 || axis[0]+" "+axis[1] != want {
		t.Errorf("axis %q, want it to start at %q", axis, want)
	}
	checkFrame(t, m, 30, 40)
}

// TestGapThreshold: points join across at most max(10 s, 3 buckets), as upstream.
func TestGapThreshold(t *testing.T) {
	line := func(res, dt float64) string {
		pts := []history.Point{{T: 0, CPU: p(50)}, {T: dt, CPU: p(50)}}
		pl := plot{pts: pts, series: []plotSeries{{get: func(p *history.Point) *float64 { return p.CPU }}},
			from: 0, to: dt, yMax: 100, gap: math.Max(histGapMin, histGapBuckets*res), w: 20, h: 1, cursor: -1}
		return strings.Join(pl.render(newTheme(true, false)), "")
	}
	if got := line(2000, 10e3); strings.Contains(got, " ") { // 2 s buckets, 10 s apart: joined
		t.Errorf("10 s apart at 2 s buckets: %q", got)
	}
	if got := line(2000, 11e3); !strings.Contains(got, " ") {
		t.Errorf("11 s apart at 2 s buckets: %q", got)
	}
	if got := line(60e3, 180e3); strings.Contains(got, " ") {
		t.Errorf("3 buckets apart: %q", got)
	}
	if got := line(60e3, 181e3); !strings.Contains(got, " ") {
		t.Errorf("just over 3 buckets apart: %q", got)
	}
}
