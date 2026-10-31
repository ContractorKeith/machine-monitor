package tui

// History view (PLAN §3 row 15, §7 view 4): one device's history as full-width braille line
// charts for utilisation, temperatures, power, network and disk, over 1 h to 30 d, with
// zoom, pan and a cursor readout. Hub mode reads GET /api/history/:id; local mode reads the
// session's history.Store, fed by the collector.
//
// Chart set, ranges and axis scaling follow upstream's history page (public/index.html,
// Forward-Future/device-monitor @ c3a4c41, MIT, © 2026 Matt).

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/ContractorKeith/machine-monitor/internal/client"
	"github.com/ContractorKeith/machine-monitor/internal/history"
	"github.com/ContractorKeith/machine-monitor/internal/power"
)

// viewSource is where the History and Energy views read from. *client.Client is one (hub
// mode); localSource wraps the session store (local mode). Hub values that don't implement
// it (older test fakes) leave the views empty.
type viewSource interface {
	History(ctx context.Context, id string, from, to float64, points int) (*history.Result, error)
	Energy(ctx context.Context, bounds []float64) (*client.Energy, error)
}

// histRanges are the History view's ranges, as upstream's.
var histRanges = []struct {
	name string
	span float64 // ms
}{{"1h", 3600e3}, {"6h", 6 * 3600e3}, {"24h", 24 * 3600e3}, {"7d", 7 * 86400e3}, {"30d", 30 * 86400e3}}

const (
	histDefaultRange = 1                // 6 h, upstream's default
	histMinSpan      = 5 * 60e3         // deepest zoom
	histKeep         = 30 * 86400e3     // oldest data the hub keeps
	histRefresh      = 30 * time.Second // re-read while following now, as upstream
	viewTimeout      = 15 * time.Second // one history or energy read
	histAxisW        = 7                // y-axis label column
	histMaxPoints    = 3000             // the hub's cap on ?points=
	histMinPoints    = 10               // and its floor
	histGapBuckets   = 3                // points further apart than this many buckets aren't joined,
	histGapMin       = 10e3             // nor than 10 s, as upstream
	histUnknownRes   = 60e3             // bucket width assumed when the hub sends none
	histPanFraction  = 4                // ← and → move the window by a quarter
	histZoomFactor   = 2                // + and - halve and double the span
	histNoDataText   = "No history for this time range yet. Data is recorded while the device is online."
	histNoSourceText = "History is not available here."
)

// History view keys.
var (
	keyRangePrev = key.NewBinding(key.WithKeys("["), key.WithHelp("[", "shorter range"))
	keyRangeNext = key.NewBinding(key.WithKeys("]"), key.WithHelp("]", "longer range"))
	keyZoomIn    = key.NewBinding(key.WithKeys("+", "="), key.WithHelp("+", "zoom in"))
	keyZoomOut   = key.NewBinding(key.WithKeys("-", "_"), key.WithHelp("-", "zoom out"))
	keyPanLeft   = key.NewBinding(key.WithKeys("left"), key.WithHelp("←", "pan back"))
	keyPanRight  = key.NewBinding(key.WithKeys("right"), key.WithHelp("→", "pan forward"))
	keyCurLeft   = key.NewBinding(key.WithKeys("h", "<"), key.WithHelp("h", "cursor left"))
	keyCurRight  = key.NewBinding(key.WithKeys("l", ">"), key.WithHelp("l", "cursor right"))
	keyZoomReset = key.NewBinding(key.WithKeys("0"), key.WithHelp("0", "reset zoom"))
	keyDevPrev   = key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "previous device"))
	keyDevNext   = key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "next device"))
)

// histState is the History view's window, cursor and last result.
type histState struct {
	rng      int     // index into histRanges
	zoomed   bool    // the window is from..to; otherwise it is the range ending now
	from, to float64 // Unix ms, while zoomed
	cursor   float64 // 0 (left edge) to 1 (right edge)
	res      *history.Result
	resID    string // device res belongs to
	err      error
	seq      int // counts reads; a reply for an older one is dropped
}

// History-view messages.
type (
	histMsg struct {
		seq int
		id  string
		res *history.Result
		err error
	}
	histRefreshMsg struct{ seq int }
	histFetchMsg   struct{ seq int } // a debounced fetch is due
)

// fetchDebounce is how long range, zoom, pan and device keys wait before reading, so key
// repeat doesn't send a request per keystroke. A var so tests can shorten it.
var fetchDebounce = 150 * time.Millisecond

// source is where the History and Energy views read, or nil when there is none.
func (m model) source() viewSource {
	if m.multi() {
		s, _ := m.opts.Hub.(viewSource)
		return s
	}
	if m.opts.History == nil {
		return nil
	}
	s := localSource{store: m.opts.History, price: m.opts.Price, loc: m.loc()}
	if d := m.cur(); d != nil && d.last != nil {
		s.hw = power.HWOf(d.last)
	}
	return s
}

// loc is the time zone the views label and bucket times in.
func (m model) loc() *time.Location {
	if m.opts.Location != nil {
		return m.opts.Location
	}
	return time.Local
}

// nowMS is the clock in Unix ms.
func (m model) nowMS() float64 { return float64(m.now().UnixMilli()) }

// histWindow is the time span the charts show, in Unix ms.
func (m model) histWindow() (from, to float64) {
	if m.hv.zoomed {
		return m.hv.from, m.hv.to
	}
	to = m.nowMS()
	return to - histRanges[m.hv.rng].span, to
}

// histPlotW is the chart width in cells.
func (m model) histPlotW() int { return max(4, m.width-histAxisW-1) }

// fetchHistory reads the selected device's history for the current window.
func (m *model) fetchHistory() tea.Cmd {
	m.hv.seq++
	m.hv.err = nil
	src, d := m.source(), m.cur()
	if src == nil || d == nil {
		return nil
	}
	seq, id, parent := m.hv.seq, d.id, m.ctx
	from, to := m.histWindow()
	points := max(histMinPoints, min(histMaxPoints, m.histPlotW()*2))
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, viewTimeout)
		defer cancel()
		res, err := src.History(ctx, id, from, to, points)
		return histMsg{seq: seq, id: id, res: res, err: err}
	}
}

// updateViews handles the History and Energy views' messages; ok is false for anything else.
func (m model) updateViews(msg tea.Msg) (model, tea.Cmd, bool) {
	switch msg := msg.(type) {
	case histMsg:
		if msg.seq != m.hv.seq {
			return m, nil, true
		}
		m.hv.err = msg.err
		if msg.err == nil {
			m.hv.res, m.hv.resID = msg.res, msg.id
		}
		seq := msg.seq
		return m, tea.Tick(histRefresh, func(time.Time) tea.Msg { return histRefreshMsg{seq} }), true
	case histRefreshMsg:
		if msg.seq != m.hv.seq || m.view != viewHistory || m.hv.zoomed {
			return m, nil, true
		}
		cmd := m.fetchHistory()
		return m, cmd, true
	case histFetchMsg:
		if msg.seq != m.hv.seq { // a later key replaced this fetch
			return m, nil, true
		}
		cmd := m.fetchHistory()
		return m, cmd, true
	case energyMsg:
		if msg.seq != m.ev.seq {
			return m, nil, true
		}
		m.ev.err = msg.err
		if msg.err == nil {
			m.ev.data = msg.data
		}
		m.clampEnergy()
		seq := msg.seq
		return m, tea.Tick(energyRefresh, func(time.Time) tea.Msg { return energyRefreshMsg{seq} }), true
	case energyRefreshMsg:
		if msg.seq != m.ev.seq || m.view != viewEnergy {
			return m, nil, true
		}
		cmd := m.fetchEnergy()
		return m, cmd, true
	case energyFetchMsg:
		if msg.seq != m.ev.seq {
			return m, nil, true
		}
		cmd := m.fetchEnergy()
		return m, cmd, true
	}
	return m, nil, false
}

// openView starts the read a view needs when it opens.
func (m *model) openView() tea.Cmd {
	switch m.view {
	case viewHistory:
		return m.fetchHistory()
	case viewEnergy:
		return m.fetchEnergy()
	}
	return nil
}

// historyKey handles the History view's keys.
func (m model) historyKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	from, to := m.histWindow()
	span := to - from
	switch {
	case key.Matches(msg, keyRangePrev), key.Matches(msg, keyRangeNext):
		r := m.hv.rng - 1
		if key.Matches(msg, keyRangeNext) {
			r = m.hv.rng + 1
		}
		if r < 0 || r >= len(histRanges) {
			return m, nil
		}
		m.hv.rng, m.hv.zoomed = r, false
	case key.Matches(msg, keyZoomIn):
		if span <= histMinSpan {
			return m, nil
		}
		m.zoomTo(math.Max(histMinSpan, span/histZoomFactor))
	case key.Matches(msg, keyZoomOut):
		if !m.hv.zoomed {
			return m, nil
		}
		m.zoomTo(math.Min(histRanges[m.hv.rng].span, span*histZoomFactor))
	case key.Matches(msg, keyPanLeft), key.Matches(msg, keyPanRight):
		d := span / histPanFraction
		if key.Matches(msg, keyPanLeft) {
			d = -d
		}
		if !m.setWindow(from+d, span) {
			return m, nil
		}
	case key.Matches(msg, keyZoomReset):
		if !m.hv.zoomed {
			return m, nil
		}
		m.hv.zoomed = false
	case key.Matches(msg, keyCurLeft), key.Matches(msg, keyCurRight):
		w := m.histPlotW()
		c := m.cursorCol(w)
		if key.Matches(msg, keyCurLeft) {
			c--
		} else {
			c++
		}
		m.hv.cursor = float64(max(0, min(w-1, c))) / float64(max(1, w-1))
		return m, nil
	case m.multi() && (key.Matches(msg, keyDevPrev) || key.Matches(msg, keyDevNext)):
		if !m.stepDevice(key.Matches(msg, keyDevNext)) {
			return m, nil
		}
	default:
		return m, nil
	}
	cmd := m.scheduleHistory()
	return m, cmd
}

// scheduleHistory reads the history after fetchDebounce, unless another key reschedules it
// first, so holding a key down sends one request rather than one per repeat.
func (m *model) scheduleHistory() tea.Cmd {
	m.hv.seq++
	m.hv.err = nil
	seq := m.hv.seq
	return tea.Tick(fetchDebounce, func(time.Time) tea.Msg { return histFetchMsg{seq} })
}

// stepDevice moves the selection to the next or previous device; false if it can't move.
func (m *model) stepDevice(next bool) bool {
	nav := m.navOrder()
	i := -1
	for j, d := range nav {
		if d.id == m.sel {
			i = j
		}
	}
	if next {
		i++
	} else {
		i--
	}
	if i < 0 || i >= len(nav) {
		return false
	}
	m.sel = nav[i].id
	return true
}

// cursorCol is the cursor's column in a plot w cells wide.
func (m model) cursorCol(w int) int {
	return max(0, min(w-1, int(math.Round(m.hv.cursor*float64(w-1)))))
}

// cursorTime is the time under the cursor column of a w-cell plot over from..to. It matches
// where ntcharts puts a point: 2w braille dots spread over the span.
func cursorTime(col, w int, from, to float64) float64 {
	return from + (float64(2*col)+0.5)/float64(max(1, 2*w-1))*(to-from)
}

// zoomTo changes the span, keeping the time under the cursor where it is on screen. With
// the cursor on the last column of a window that ends now, the window keeps ending now.
func (m *model) zoomTo(span float64) {
	from, to := m.histWindow()
	w := m.histPlotW()
	c := m.cursorCol(w)
	if c == w-1 && to >= m.nowMS() {
		m.setWindow(to-span, span)
		return
	}
	tc := cursorTime(c, w, from, to)
	frac := (tc - from) / (to - from)
	m.setWindow(tc-frac*span, span)
}

// setWindow moves the window to start at from, kept within the 30 days the hub keeps and
// not past now. A window as wide as the range that reaches now follows now again. It
// reports whether the window changed.
func (m *model) setWindow(from, span float64) bool {
	oldFrom, oldTo := m.histWindow()
	oldZoomed := m.hv.zoomed
	now := m.nowMS()
	span = math.Min(span, histKeep)
	from = math.Max(now-histKeep, math.Min(from, now-span))
	if span >= histRanges[m.hv.rng].span && from+span >= now {
		m.hv.zoomed = false
	} else {
		m.hv.zoomed, m.hv.from, m.hv.to = true, from, from+span
	}
	newFrom, newTo := m.histWindow()
	return oldZoomed != m.hv.zoomed || newFrom != oldFrom || newTo != oldTo
}

// histChart is one chart: its series and how its values read.
type histChart struct {
	title  string
	max    float64 // fixed y maximum; 0 scales to the data
	series []histSeries
	fmt    func(float64) string // readout
	axis   func(float64) string // y-axis label, at most histAxisW cells
}

// histSeries is one line of a chart; peak is nil for fields without a stored peak.
type histSeries struct {
	label     string
	get, peak func(*history.Point) *float64
}

// histCharts are upstream's history charts, with whole-machine power beside GPU power.
var histCharts = []histChart{
	{title: "Utilization", max: 100, fmt: fmtPctV, axis: func(v float64) string { return fmt.Sprintf("%.0f%%", v) },
		series: []histSeries{
			{"CPU", func(p *history.Point) *float64 { return p.CPU }, func(p *history.Point) *float64 { return p.CPUMax }},
			{"GPU", func(p *history.Point) *float64 { return p.GPU }, func(p *history.Point) *float64 { return p.GPUMax }},
			{"Memory", func(p *history.Point) *float64 { return p.Mem }, nil},
		}},
	{title: "Temperature", fmt: fmtTempV,
		axis: func(v float64) string { return fmt.Sprintf("%.0f°", v) },
		series: []histSeries{
			{"CPU", func(p *history.Point) *float64 { return p.CPUTemp }, func(p *history.Point) *float64 { return p.CPUTempMax }},
			{"GPU", func(p *history.Point) *float64 { return p.GPUTemp }, func(p *history.Point) *float64 { return p.GPUTempMax }},
		}},
	{title: "Power", fmt: fmtWattsV, axis: fmtWattsV,
		series: []histSeries{
			{"System", func(p *history.Point) *float64 { return p.Power }, func(p *history.Point) *float64 { return p.PowerMax }},
			{"GPU", func(p *history.Point) *float64 { return p.GPUPower }, nil},
		}},
	{title: "Network", fmt: func(v float64) string { return fmtBytes(v) + "/s" }, axis: shortRate,
		series: []histSeries{
			{"Download", func(p *history.Point) *float64 { return p.RX }, nil},
			{"Upload", func(p *history.Point) *float64 { return p.TX }, nil},
		}},
	{title: "Disk I/O", fmt: func(v float64) string { return fmtBytes(v) + "/s" }, axis: shortRate,
		series: []histSeries{
			{"Read", func(p *history.Point) *float64 { return p.DR }, nil},
			{"Write", func(p *history.Point) *float64 { return p.DW }, nil},
		}},
}

func fmtPctV(v float64) string   { return fmt.Sprintf("%.0f%%", v) }
func fmtWattsV(v float64) string { return fmt.Sprintf("%.0f W", v) }

// shortRate is a compact bytes-per-second label for an axis: "12M/s".
func shortRate(v float64) string {
	units := []string{"B", "K", "M", "G", "T"}
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if v < 10 && v != math.Trunc(v) {
		return fmt.Sprintf("%.1f%s/s", v, units[i])
	}
	return fmt.Sprintf("%.0f%s/s", v, units[i])
}

// seriesStyles colour a chart's lines, as upstream's --cpu, --gpu and --mem.
func (m model) seriesStyles() []lipgloss.Style {
	if m.opts.NoColor {
		return []lipgloss.Style{lipglossNone, lipglossNone, lipglossNone}
	}
	return []lipgloss.Style{
		lipgloss.NewStyle().Foreground(lipgloss.Cyan),
		lipgloss.NewStyle().Foreground(lipgloss.Magenta),
		lipgloss.NewStyle().Foreground(lipgloss.Yellow),
	}
}

// historyView renders the History view's body.
func (m model) historyView() string {
	w, h := m.width, m.bodyHeight()
	lines := []string{m.histPicker(w), m.histInfo()}
	res := m.hv.res
	d := m.cur()
	switch {
	case m.source() == nil:
		return strings.Join(append(lines, "", m.theme.dim.Render(histNoSourceText)), "\n")
	case d == nil:
		return strings.Join(append(lines, "", m.theme.dim.Render("No devices yet.")), "\n")
	case m.hv.err != nil && (res == nil || m.hv.resID != d.id):
		return strings.Join(append(lines, "", m.theme.err.Render("history: "+m.hv.err.Error())), "\n")
	case res == nil || m.hv.resID != d.id:
		return strings.Join(append(lines, "", m.theme.dim.Render("loading…")), "\n")
	}

	from, to := m.histWindow()
	pts := clipPoints(res.Points, from, to)
	var present []histChart
	for _, c := range histCharts {
		var ss []histSeries
		for _, s := range c.series {
			if hasData(pts, s.get) {
				ss = append(ss, s)
			}
		}
		if len(ss) > 0 {
			c.series = ss
			present = append(present, c)
		}
	}
	if len(present) == 0 {
		return strings.Join(append(lines, "", m.theme.dim.Render(histNoDataText)), "\n")
	}
	lines = append(lines, m.histTiles(res.Points, from, to, w))

	// Rows: each chart is a title line and at least two plot rows; the time axis takes one.
	avail := h - len(lines) - 1
	n := min(len(present), max(1, avail/3))
	present = present[:n]
	each, extra := avail/n, avail%n

	resolution := res.Resolution
	if resolution <= 0 {
		resolution = histUnknownRes
	}
	pw := m.histPlotW()
	col := m.cursorCol(pw)
	tc := cursorTime(col, pw, from, to)
	at := pointAt(pts, tc, resolution)
	styles := m.seriesStyles()
	for i, c := range present {
		rows := each - 1
		if i < extra {
			rows++
		}
		lines = append(lines, m.chartTitle(c, at, styles, w))
		lines = append(lines, m.chartRows(c, pts, from, to, resolution, rows, pw, col, styles)...)
	}
	lines = append(lines, m.timeAxis(from, to, pw))
	return strings.Join(lines, "\n")
}

// histTile is one window-wide statistic, as upstream's history tiles.
type histTile struct {
	label     string
	get, peak func(*history.Point) *float64
	fmt       func(float64) string
}

// histTiles are upstream's tiles: utilisation, temperatures and GPU power.
var histTiles = []histTile{
	{"CPU", func(p *history.Point) *float64 { return p.CPU }, func(p *history.Point) *float64 { return p.CPUMax }, fmtPctV},
	{"GPU", func(p *history.Point) *float64 { return p.GPU }, func(p *history.Point) *float64 { return p.GPUMax }, fmtPctV},
	{"Memory", func(p *history.Point) *float64 { return p.Mem }, nil, fmtPctV},
	{"CPU temp", func(p *history.Point) *float64 { return p.CPUTemp }, func(p *history.Point) *float64 { return p.CPUTempMax }, fmtTempV},
	{"GPU temp", func(p *history.Point) *float64 { return p.GPUTemp }, func(p *history.Point) *float64 { return p.GPUTempMax }, fmtTempV},
	{"GPU power", func(p *history.Point) *float64 { return p.GPUPower }, nil, fmtWattsV},
}

func fmtTempV(v float64) string { return fmt.Sprintf("%.0f°C", v) }

// histTiles is the line of window-wide statistics: each metric's peak and average over the
// visible window (upstream's renderTiles), as many as fit in w.
func (m model) histTiles(pts []history.Point, from, to float64, w int) string {
	t := m.theme
	peakMark := "▲"
	if t.ascii {
		peakMark = "^"
	}
	i := sort.Search(len(pts), func(i int) bool { return pts[i].T >= from })
	j := sort.Search(len(pts), func(i int) bool { return pts[i].T > to })
	pts = pts[i:j]
	line := ""
	for _, tl := range histTiles {
		sum, n, peak := 0.0, 0, math.Inf(-1)
		for k := range pts {
			v := tl.get(&pts[k])
			if v == nil {
				continue
			}
			sum += *v
			n++
			pk := v
			if tl.peak != nil && tl.peak(&pts[k]) != nil {
				pk = tl.peak(&pts[k])
			}
			peak = math.Max(peak, *pk)
		}
		if n == 0 {
			continue
		}
		tile := t.label.Render(tl.label) + " " + peakMark + tl.fmt(peak) + t.dim.Render(" avg "+tl.fmt(sum/float64(n)))
		if line != "" {
			tile = "   " + tile
		}
		if lipgloss.Width(line+tile) > w {
			break
		}
		line += tile
	}
	return line
}

// histPicker is the first line: the devices (selected one highlighted) and the ranges.
func (m model) histPicker(w int) string {
	t := m.theme
	var ranges []string
	for i, r := range histRanges {
		label := " " + r.name + " "
		if i == m.hv.rng && !m.hv.zoomed {
			ranges = append(ranges, t.tabOn.Render(label))
		} else {
			ranges = append(ranges, t.tabOff.Render(label))
		}
	}
	right := strings.Join(ranges, "")
	if m.hv.zoomed {
		right = t.label.Render("zoomed") + " " + t.dim.Render("0 reset") + "  " + right
	}
	nav := m.navOrder()
	var names []string
	for _, d := range nav {
		dot := t.normal.Render("●")
		if !d.online {
			dot = t.dim.Render("○")
		}
		if t.ascii {
			dot = map[bool]string{true: "*", false: "o"}[d.online]
		}
		label := dot + " " + orNone(d.name)
		if c := m.cur(); c != nil && d.id == c.id {
			label = t.selected.Render(" " + ansi.Strip(label) + " ")
		} else {
			label = " " + label + " "
		}
		names = append(names, label)
	}
	left := strings.Join(names, "")
	if lipgloss.Width(left)+1+lipgloss.Width(right) > w { // too many devices: just the selected one
		left = ""
		if c := m.cur(); c != nil {
			pos := ""
			for i, d := range nav {
				if d.id == c.id {
					pos = fmt.Sprintf(" %d/%d", i+1, len(nav))
				}
			}
			left = t.selected.Render(" "+orNone(c.name)+" ") + t.dim.Render(pos)
		}
	}
	return spread(left, right, w)
}

// histInfo is the second line: the window, the bucket width and the cursor's time.
func (m model) histInfo() string {
	from, to := m.histWindow()
	span := to - from
	s := timeLabel(m.msTime(from), span) + " – " + timeLabel(m.msTime(to), span)
	if r := m.hv.res; r != nil && r.Resolution > 0 {
		s += " · " + fmtBucket(r.Resolution) + " buckets"
	}
	pw := m.histPlotW()
	tc := cursorTime(m.cursorCol(pw), pw, from, to)
	return m.theme.dim.Render(s+" · cursor ") + timeLabel(m.msTime(tc), span)
}

// msTime converts Unix ms to a time in the view's zone.
func (m model) msTime(ms float64) time.Time {
	return time.UnixMilli(int64(math.Round(ms))).In(m.loc())
}

// timeLabel formats t for a window span ms wide, as upstream: the clock up to 36 h, the day
// and clock up to 8 days, the day beyond.
func timeLabel(t time.Time, span float64) string {
	switch {
	case span <= 36*3600e3:
		return t.Format("15:04")
	case span <= 8*86400e3:
		return t.Format("Mon Jan 2 15:04")
	default:
		return t.Format("Mon Jan 2")
	}
}

// fmtBucket formats a bucket width in ms: "2 s", "5 min", "2.4 h".
func fmtBucket(ms float64) string {
	switch s := ms / 1000; {
	case s < 60:
		return fmt.Sprintf("%.0f s", s)
	case s < 3600:
		return trimZero(fmt.Sprintf("%.1f", s/60)) + " min"
	default:
		return trimZero(fmt.Sprintf("%.1f", s/3600)) + " h"
	}
}

// trimZero drops a trailing ".0".
func trimZero(s string) string { return strings.TrimSuffix(s, ".0") }

// chartTitle is a chart's title and, per series, the reading under the cursor: the bucket's
// average and, for fields with a stored peak, its peak.
func (m model) chartTitle(c histChart, at *history.Point, styles []lipgloss.Style, w int) string {
	t := m.theme
	peak := "▲"
	if t.ascii {
		peak = "^"
	}
	parts := []string{t.label.Render(c.title)}
	for i, s := range c.series {
		mark := "■"
		if t.ascii {
			mark = string(asciiMarks[i%len(asciiMarks)])
		}
		r := styles[i%len(styles)].Render(mark) + " " + s.label + " "
		v := (*float64)(nil)
		if at != nil {
			v = s.get(at)
		}
		if v == nil {
			r += none
		} else {
			r += c.fmt(*v)
			if s.peak != nil {
				if pk := s.peak(at); pk != nil {
					r += t.dim.Render(" " + peak + c.fmt(*pk))
				}
			}
		}
		parts = append(parts, r)
	}
	return pad(strings.Join(parts, "   "), w)
}

// chartRows draws a chart's plot rows with the y-axis labels to their left.
func (m model) chartRows(c histChart, pts []history.Point, from, to, resolution float64, rows, pw, col int, styles []lipgloss.Style) []string {
	if rows <= 0 {
		return nil
	}
	yMax := c.max
	if yMax == 0 {
		hi := 0.0
		for i := range pts {
			for _, s := range c.series {
				v := s.get(&pts[i])
				if s.peak != nil {
					if pk := s.peak(&pts[i]); pk != nil {
						v = pk
					}
				}
				if v != nil {
					hi = math.Max(hi, *v)
				}
			}
		}
		yMax = niceMax(hi * 1.1)
	}
	ps := make([]plotSeries, len(c.series))
	for i, s := range c.series {
		ps[i] = plotSeries{get: s.get, style: styles[i%len(styles)]}
	}
	body := plot{pts: pts, series: ps, from: from, to: to, yMax: yMax, gap: math.Max(histGapMin, histGapBuckets*resolution),
		w: pw, h: rows, cursor: col}.render(m.theme)
	out := make([]string, rows)
	for y, line := range body {
		label := ""
		switch {
		case y == 0:
			label = c.axis(yMax)
		case y == rows-1:
			label = c.axis(0)
		case rows >= 5 && y == (rows-1)/2:
			label = c.axis(yMax * float64(rows-1-y) / float64(rows-1))
		}
		out[y] = m.theme.dim.Render(padLeft(label, histAxisW)) + " " + line
	}
	return out
}

// timeAxis labels the plots' left edge, middle and right edge.
func (m model) timeAxis(from, to float64, pw int) string {
	span := to - from
	l := timeLabel(m.msTime(from), span)
	mid := timeLabel(m.msTime(from+span/2), span)
	r := timeLabel(m.msTime(to), span)
	line := []rune(strings.Repeat(" ", pw))
	put := func(s string, at int) {
		rs := []rune(s)
		at = max(0, min(pw-len(rs), at))
		for i, c := range rs {
			if at+i < len(line) {
				line[at+i] = c
			}
		}
	}
	put(l, 0)
	if pw >= len(l)+len(mid)+len(r)+4 {
		put(mid, pw/2-len([]rune(mid))/2)
	}
	put(r, pw)
	return m.theme.dim.Render(strings.Repeat(" ", histAxisW+1) + string(line))
}

// historyFooter lists the view's keys.
func (m model) historyFooter() string {
	s := "[ ] range · +/- zoom · ←/→ pan · h/l cursor · 0 reset"
	if m.multi() {
		s += " · j/k device"
	}
	if m.hv.err != nil && m.hv.res != nil {
		s = "history: " + m.hv.err.Error() + " · " + s
	}
	return s
}

// clipPoints is the points within from..to, plus one on each side so lines reach the edges.
func clipPoints(pts []history.Point, from, to float64) []history.Point {
	i := sort.Search(len(pts), func(i int) bool { return pts[i].T >= from })
	j := sort.Search(len(pts), func(i int) bool { return pts[i].T > to })
	return pts[max(0, i-1):min(len(pts), j+1)]
}

// hasData reports whether any point has a value for get.
func hasData(pts []history.Point, get func(*history.Point) *float64) bool {
	for i := range pts {
		if get(&pts[i]) != nil {
			return true
		}
	}
	return false
}

// pointAt is the bucket holding time t, or the nearest one within a bucket width, or nil.
func pointAt(pts []history.Point, t, resolution float64) *history.Point {
	i := sort.Search(len(pts), func(i int) bool { return pts[i].T > t }) - 1
	best, dist := -1, math.Inf(1)
	for _, j := range []int{i, i + 1} {
		if j < 0 || j >= len(pts) {
			continue
		}
		d := math.Abs(pts[j].T - t)
		if j == i && t < pts[j].T+resolution {
			d = 0 // inside the bucket
		}
		if d < dist {
			best, dist = j, d
		}
	}
	if best < 0 || dist > resolution {
		return nil
	}
	return &pts[best]
}
