package tui

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/golden"
	"github.com/charmbracelet/x/exp/teatest/v2"
)

// testOptions is a TUI on fakes that samples once: the interval is long enough that no
// tick fires during a test, so every frame is deterministic.
func testOptions() Options {
	return Options{
		Collector: fakeCollector{workstation(16)},
		Procs:     procList(40),
		Interval:  time.Hour,
	}
}

// probe wraps the model so a test can read its state while the program runs: on a probeMsg
// it hands over the current model. Messages are handled in order, so a probe answered after
// some keys sees their effect.
type probe struct {
	tea.Model
	ch chan model
}

type probeMsg struct{}

func (p probe) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if _, ok := msg.(probeMsg); ok {
		p.ch <- p.Model.(model)
		return p, nil
	}
	next, cmd := p.Model.Update(msg)
	p.Model = next
	return p, cmd
}

// waitUntil probes the running program until ready holds for its model.
func waitUntil(t *testing.T, tm *teatest.TestModel, ch chan model, ready func(model) bool) model {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		tm.Send(probeMsg{})
		select {
		case m := <-ch:
			if ready(m) {
				return m
			}
		case <-deadline:
			t.Fatal("timed out waiting for the model")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// loaded is true once the first sample is in and, on the Processes view, the list too.
func loaded(m model) bool {
	return m.report() != nil && (m.view != viewProcesses || m.procs != nil)
}

// start runs the TUI at w×h through teatest and waits for the first sample.
func start(t *testing.T, o Options, w, h int) (*teatest.TestModel, chan model) {
	t.Helper()
	ch := make(chan model)
	tm := teatest.NewTestModel(t, probe{New(o), ch}, teatest.WithInitialTermSize(w, h))
	waitUntil(t, tm, ch, loaded)
	return tm, ch
}

// typeKeys types keys; '\r' is Enter.
func typeKeys(tm *teatest.TestModel, keys string) {
	for _, k := range keys {
		if k == '\r' {
			tm.Send(tea.KeyPressMsg{Code: tea.KeyEnter})
		} else {
			tm.Type(string(k))
		}
	}
}

// finish quits the program and returns its final model.
func finish(t *testing.T, tm *teatest.TestModel) model {
	t.Helper()
	if err := tm.Quit(); err != nil {
		t.Fatal(err)
	}
	return tm.FinalModel(t, teatest.WithFinalTimeout(5*time.Second)).(probe).Model.(model)
}

// drive runs the program at w×h, feeds it a CPU/GPU history for the sparklines, types
// keys, waits for the view to load, quits and returns the final model.
func drive(t *testing.T, o Options, w, h int, keys string) model {
	t.Helper()
	tm, ch := start(t, o, w, h)
	base := o.Collector.(fakeCollector).r
	for i := range 40 {
		r := *base
		r.CPU.Usage = p(float64(50 + 40*((i*13)%7-3)/3))
		r.GPUs = slices.Clone(base.GPUs)
		if len(r.GPUs) > 0 {
			r.GPUs[0].Usage = p(float64((i * 23) % 100))
		}
		tm.Send(reportMsg{r: &r})
	}
	// The first key picks the view; let it load (the process list is async) before the rest.
	for i, k := range []rune(keys) {
		typeKeys(tm, string(k))
		if i == 0 {
			waitUntil(t, tm, ch, loaded)
		}
	}
	waitUntil(t, tm, ch, loaded)
	return finish(t, tm)
}

// plain is a frame without ANSI styling, for readable goldens.
func plain(m model) string { return ansi.Strip(m.render()) }

func TestGolden(t *testing.T) {
	sizes := [][2]int{{80, 24}, {160, 48}}
	views := []struct{ name, keys string }{
		{"dashboard", ""},
		{"device", "2"},
		{"processes", "3"},
		{"help", "?"},
	}
	for _, sz := range sizes {
		for _, v := range views {
			t.Run(fmt.Sprintf("%s_%dx%d", v.name, sz[0], sz[1]), func(t *testing.T) {
				m := drive(t, testOptions(), sz[0], sz[1], v.keys)
				golden.RequireEqual(t, plain(m))
			})
		}
	}
}

// TestGoldenInteractions covers the keys that change a view's contents.
func TestGoldenInteractions(t *testing.T) {
	cases := []struct {
		name, keys string
		opts       func(*Options)
	}{
		{"processes_sort_name", "3ssssss", nil},
		{"processes_sort_reverse", "3r", nil},
		{"processes_filter", "3/ollama\r", nil},
		{"processes_scroll", "3jjjjjjjjjjjjjjjjjjjjjjjjj", nil},
		{"device_scroll", "2jjjjjjjjjj", nil},
		{"dashboard_ascii", "", func(o *Options) { o.ASCII = true }},
		{"dashboard_no_gpu", "", func(o *Options) {
			r := workstation(4)
			r.GPUs, r.AI, r.Power, r.CPU.Temp = nil, nil, nil, nil
			o.Collector = fakeCollector{r}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := testOptions()
			if c.opts != nil {
				c.opts(&o)
			}
			m := drive(t, o, 80, 24, c.keys)
			golden.RequireEqual(t, plain(m))
		})
	}
}

// TestResize checks every frame fills the window exactly as it changes size.
func TestResize(t *testing.T) {
	tm, ch := start(t, testOptions(), 80, 24)
	sizes := [][2]int{{160, 48}, {40, 12}, {200, 60}, {100, 30}}
	for _, s := range sizes {
		tm.Send(tea.WindowSizeMsg{Width: s[0], Height: s[1]})
	}
	tm.Type("3")
	waitUntil(t, tm, ch, loaded)
	m := finish(t, tm)
	if m.width != 100 || m.height != 30 {
		t.Fatalf("size %dx%d, want 100x30", m.width, m.height)
	}
	for _, s := range append(sizes, [2]int{20, 5}) {
		m.width, m.height = s[0], s[1]
		for v := range viewCount {
			m.view = v
			checkFrame(t, m, s[0], s[1])
		}
	}
}

// checkFrame asserts a frame is exactly w columns by h lines.
func checkFrame(t *testing.T, m model, w, h int) {
	t.Helper()
	lines := strings.Split(m.render(), "\n")
	if len(lines) != h {
		t.Errorf("%s at %dx%d: %d lines", viewNames[m.view], w, h, len(lines))
	}
	for i, l := range lines {
		if got := ansi.StringWidth(l); got != w {
			t.Errorf("%s at %dx%d: line %d is %d wide: %q", viewNames[m.view], w, h, i, got, ansi.Strip(l))
			return
		}
	}
}

func TestTempBands(t *testing.T) {
	th := newTheme(false, false)
	for _, c := range []struct {
		c    float64
		want string
	}{
		{20, th.normal.Render("x")}, {69.9, th.normal.Render("x")},
		{70, th.warm.Render("x")}, {84.9, th.warm.Render("x")},
		{85, th.hot.Render("x")}, {110, th.hot.Render("x")},
	} {
		if got := th.tempStyle(c.c).Render("x"); got != c.want {
			t.Errorf("tempStyle(%v) renders %q, want %q", c.c, got, c.want)
		}
	}
	if th.normal.Render("x") == th.warm.Render("x") || th.warm.Render("x") == th.hot.Render("x") {
		t.Error("temperature bands are not distinct")
	}
}

// TestNoColor checks the monochrome theme sets no colours, only attributes.
func TestNoColor(t *testing.T) {
	o := testOptions()
	o.NoColor = true
	m := New(o).(model)
	m.Update(tea.WindowSizeMsg{Width: 160, Height: 48})
	next, _ := m.Update(reportMsg{r: workstation(16)})
	m = next.(model)
	for v := range viewCount {
		m.view = v
		if hasColorSGR(m.render()) {
			t.Errorf("%s view has colour codes under NO_COLOR", viewNames[v])
		}
	}
	t.Setenv("NO_COLOR", "1")
	t.Setenv(EnvASCII, "1")
	if e := OptionsFromEnv(Options{}); !e.NoColor || !e.ASCII {
		t.Errorf("OptionsFromEnv = %+v, want NoColor and ASCII", e)
	}
	t.Setenv("NO_COLOR", "")
	t.Setenv(EnvASCII, "0")
	if e := OptionsFromEnv(Options{}); e.NoColor || e.ASCII {
		t.Errorf("OptionsFromEnv = %+v, want neither", e)
	}
}

// hasColorSGR reports whether s sets a foreground or background colour.
func hasColorSGR(s string) bool {
	for _, seq := range strings.Split(s, "\x1b[")[1:] {
		end := strings.IndexByte(seq, 'm')
		if end < 0 {
			continue
		}
		for _, f := range strings.Split(seq[:end], ";") {
			n, err := strconv.Atoi(f)
			if err == nil && (n >= 30 && n <= 38 || n >= 40 && n <= 48 || n >= 90 && n <= 97 || n >= 100 && n <= 107) {
				return true
			}
		}
	}
	return false
}

// TestASCII checks the fallback draws no braille or block glyphs.
func TestASCII(t *testing.T) {
	o := testOptions()
	o.ASCII = true
	m := New(o).(model)
	next, _ := m.Update(reportMsg{r: workstation(16)})
	m = next.(model)
	frame := m.render()
	for _, r := range frame {
		if (r >= 0x2800 && r <= 0x28ff) || r == '█' || r == '░' || r == '╭' {
			t.Fatalf("ASCII frame contains %q", r)
		}
	}
	if !strings.Contains(frame, "#") {
		t.Error("ASCII frame has no bars")
	}
}

func TestFormat(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{fmtBytes(512), "512 B"},
		{fmtBytes(1536), "1.5 KiB"},
		{fmtBytes(47.3 * gib), "47.3 GiB"},
		{fmtBytes(912 * gib), "912 GiB"},
		{fmtUptime(p(59)), "0m"},
		{fmtUptime(p(3*3600 + 120)), "3h 2m"},
		{fmtUptime(p(86400 + 3600)), "1d 1h 0m"},
		{fmtUptime(nil), none},
		{fmtPct(nil), none},
		{fmtPct(p(45.6)), "46%"},
		{fmtClock(p(4350)), "4.35 GHz"},
		{asciiSpark([]float64{0, 50, 100}, 4), "   #\n  ##"},
	} {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
}

// loadedModel is a model at w×h holding one workstation sample and the process list.
func loadedModel(t *testing.T, o Options, w, h int) model {
	t.Helper()
	m := New(o).(model)
	for _, msg := range []tea.Msg{
		tea.WindowSizeMsg{Width: w, Height: h},
		reportMsg{r: workstation(16)},
		procsMsg{p: procList(40)},
	} {
		next, _ := m.Update(msg)
		m = next.(model)
	}
	return m
}

// press sends key presses to m; "ctrl+c" and "enter" are named keys.
func press(m model, keys ...string) (model, tea.Cmd) {
	var cmd tea.Cmd
	for _, k := range keys {
		var msg tea.KeyPressMsg
		switch k {
		case "ctrl+c":
			msg = tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
		case "enter":
			msg = tea.KeyPressMsg{Code: tea.KeyEnter}
		default:
			msg = tea.KeyPressMsg{Code: []rune(k)[0], Text: k}
		}
		var next tea.Model
		next, cmd = m.Update(msg)
		m = next.(model)
	}
	return m, cmd
}

func TestBarNaN(t *testing.T) {
	th := newTheme(true, true)
	for _, v := range []*float64{nil, p(math.NaN()), p(math.Inf(1)), p(math.Inf(-1)), p(-5), p(250)} {
		got := ansi.Strip(th.bar(v, 10))
		if len(got) != 10 {
			t.Errorf("bar(%v) = %q, want 10 cells", v, got)
		}
	}
	if got := ansi.Strip(th.bar(p(math.NaN()), 4)); got != "...." {
		t.Errorf("bar(NaN) = %q, want an empty track", got)
	}
}

// TestTinyTerminal checks a 1- or 2-row window gets only the body, never overflowing.
func TestTinyTerminal(t *testing.T) {
	for _, h := range []int{1, 2} {
		m := loadedModel(t, testOptions(), 80, h)
		for v := range viewCount {
			m.view = v
			checkFrame(t, m, 80, h)
			if strings.Contains(m.render(), "q quit") {
				t.Errorf("%s at h=%d still draws the header", viewNames[v], h)
			}
		}
	}
}

func TestCtrlCWhileFiltering(t *testing.T) {
	m := loadedModel(t, testOptions(), 80, 24)
	m, _ = press(m, "3", "/", "q")
	if !m.filter.Focused() || m.filter.Value() != "q" {
		t.Fatalf("filter focused %v, value %q; want focused with q typed", m.filter.Focused(), m.filter.Value())
	}
	if _, cmd := press(m, "ctrl+c"); cmd == nil || cmd() != tea.Quit() {
		t.Error("ctrl+c while filtering does not quit")
	}
}

func TestHelpSwallowsViewKeys(t *testing.T) {
	m := loadedModel(t, testOptions(), 80, 24)
	m, _ = press(m, "3", "?", "/", "s", "j")
	if m.filter.Focused() || m.procSort != sortCPU || m.procCursor != 0 {
		t.Errorf("keys under help reached the view: filter %v, sort %v, cursor %d",
			m.filter.Focused(), m.procSort, m.procCursor)
	}
}

// TestFilterSortCache checks the cached visible list follows every change.
func TestFilterSortCache(t *testing.T) {
	m := loadedModel(t, testOptions(), 80, 24)
	m, _ = press(m, "3", "/", "o", "l", "l", "a", "m", "a", "enter")
	if n := len(m.visibleProcs()); n != 5 {
		t.Fatalf("filter ollama: %d visible, want 5", n)
	}
	m, _ = press(m, "r")
	if v := m.visibleProcs(); *v[0].CPU > *v[len(v)-1].CPU {
		t.Error("reverse did not reorder the visible list")
	}
	next, _ := m.Update(procsMsg{p: procList(80)})
	if n := len(next.(model).visibleProcs()); n != 10 {
		t.Errorf("new list, same filter: %d visible, want 10", n)
	}
	m, _ = press(next.(model), "esc")
	if n := len(m.visibleProcs()); n != 80 {
		t.Errorf("esc: %d visible, want 80", n)
	}
}

// TestSparkGaps checks unknown samples leave gaps and an all-unknown row is dropped.
func TestSparkGaps(t *testing.T) {
	var s series
	for range 3 {
		s.push(p(100))
	}
	s.push(nil)
	s.push(p(100))
	if got := asciiSpark(s.v, 5); got != "### #\n### #" {
		t.Errorf("ASCII spark with a gap = %q", got)
	}
	full := series{v: []float64{100, 100, 100, 100, 100}}
	th := newTheme(true, false)
	if gap, solid := th.spark(s, 5), th.spark(full, 5); gap == solid {
		t.Errorf("braille spark draws the unknown sample as a line: %q", ansi.Strip(gap))
	}

	o := testOptions()
	r := workstation(16)
	r.GPUs[0].Usage = nil
	o.Collector = fakeCollector{r}
	m := New(o).(model)
	next, _ := m.Update(reportMsg{r: r})
	if frame := ansi.Strip(next.(model).render()); strings.Contains(frame, "gpu ") {
		t.Errorf("GPU sparkline drawn with no known sample:\n%s", frame)
	}
}

func TestDeviceEmptyStrings(t *testing.T) {
	o := testOptions()
	r := workstation(4)
	r.Platform, r.ID = "", ""
	m := New(o).(model)
	next, _ := m.Update(reportMsg{r: r})
	m = next.(model)
	for _, l := range m.deviceLines() {
		l = ansi.Strip(l)
		for _, k := range []string{"Platform", "ID"} {
			if strings.HasPrefix(strings.TrimSpace(l), k+" ") && !strings.HasSuffix(strings.TrimSpace(l), none) {
				t.Errorf("empty %s renders as %q", k, l)
			}
		}
	}
}

// TestCardHotTemp checks the band colour reaches the card: the goldens are ANSI-stripped.
func TestCardHotTemp(t *testing.T) {
	m := loadedModel(t, testOptions(), 80, 24)
	m.view = viewDashboard
	hot := m.theme.hot.Render("86°C")
	if !strings.Contains(hot, "\x1b[") {
		t.Fatalf("hot style emits no SGR: %q", hot)
	}
	if !strings.Contains(m.render(), hot) {
		t.Error("the card's 86 °C GPU temperature is not in the hot style")
	}
}
