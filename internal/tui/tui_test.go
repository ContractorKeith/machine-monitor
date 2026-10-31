package tui

import (
	"fmt"
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
	return m.last != nil && (m.view != viewProcesses || m.procs != nil)
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
