package tui

// Energy view (PLAN §3 rows 17–19, §7 view 5): over 24 h, 7 d or 30 d, what every device
// draws now and used on average, its energy, cost and projected monthly cost, a bar chart of
// energy per hour or day, and the usage totals table (online time, average and peak loads
// and temperatures, bytes moved). Hub mode reads GET /api/energy; local mode totals the
// session store the same way.
//
// The tiles, price wording, projections and tables follow upstream's Advanced page
// (public/index.html renderAdv, advBounds, drawAdvChart), Forward-Future/device-monitor
// @ c3a4c41 (MIT, © 2026 Matt): https://github.com/Forward-Future/device-monitor

import (
	"context"
	"fmt"
	"image/color"
	"math"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/ContractorKeith/machine-monitor/internal/client"
	"github.com/ContractorKeith/machine-monitor/internal/hub"
	"github.com/ContractorKeith/machine-monitor/internal/power"
)

// energyRanges are the Energy view's ranges, as upstream's.
var energyRanges = []struct{ name, label string }{
	{"24 h", "last 24 hours"}, {"7 days", "last 7 days"}, {"30 days", "last 30 days"},
}

const (
	energyDefaultRange = 1                // 7 days, upstream's default
	energyRefresh      = 60 * time.Second // as upstream
	monthDays          = 30.44
	energyAxisW        = 9 // bar chart y-axis labels: "12.5 kWh"
)

// energyState is the Energy view's range, scroll and last result.
type energyState struct {
	rng    int
	data   *client.Energy
	err    error
	seq    int
	scroll int
}

// Energy-view messages.
type (
	energyMsg struct {
		seq  int
		data *client.Energy
		err  error
	}
	energyRefreshMsg struct{ seq int }
	energyFetchMsg   struct{ seq int } // a debounced fetch is due
)

// energyBounds are the bucket edges for range r ending around now, in Unix ms: whole local
// hours for 24 h (the current hour last), local midnights otherwise (today last).
func energyBounds(now time.Time, r int) []float64 {
	var out []float64
	y, mo, d := now.Date()
	if r == 0 {
		end := time.Date(y, mo, d, now.Hour()+1, 0, 0, 0, now.Location())
		for i := 24; i >= 0; i-- {
			out = append(out, float64(end.Add(-time.Duration(i)*time.Hour).UnixMilli()))
		}
		return out
	}
	n := 7
	if r == 2 {
		n = 30
	}
	for i := n; i >= 0; i-- {
		out = append(out, float64(time.Date(y, mo, d+1-i, 0, 0, 0, 0, now.Location()).UnixMilli()))
	}
	return out
}

// fetchEnergy reads energy totals for the current range.
func (m *model) fetchEnergy() tea.Cmd {
	m.ev.seq++
	m.ev.err = nil
	src := m.source()
	if src == nil {
		return nil
	}
	seq, parent := m.ev.seq, m.ctx
	bounds := energyBounds(m.now().In(m.loc()), m.ev.rng)
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, viewTimeout)
		defer cancel()
		data, err := src.Energy(ctx, bounds)
		return energyMsg{seq: seq, data: data, err: err}
	}
}

// energyKey handles the Energy view's keys: range and scrolling.
func (m model) energyKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	k := m.keys
	switch {
	case key.Matches(msg, keyRangePrev), key.Matches(msg, keyRangeNext):
		r := m.ev.rng - 1
		if key.Matches(msg, keyRangeNext) {
			r = m.ev.rng + 1
		}
		if r < 0 || r >= len(energyRanges) {
			return m, nil
		}
		m.ev.rng, m.ev.data, m.ev.scroll = r, nil, 0
		m.ev.seq++
		m.ev.err = nil
		seq := m.ev.seq
		return m, tea.Tick(fetchDebounce, func(time.Time) tea.Msg { return energyFetchMsg{seq} })
	case key.Matches(msg, k.Up):
		m.ev.scroll--
	case key.Matches(msg, k.Down):
		m.ev.scroll++
	case key.Matches(msg, k.PageUp):
		m.ev.scroll -= m.bodyHeight()
	case key.Matches(msg, k.PageDown):
		m.ev.scroll += m.bodyHeight()
	case key.Matches(msg, k.Top):
		m.ev.scroll = 0
	case key.Matches(msg, k.Bottom):
		m.ev.scroll = 1 << 30
	}
	m.clampEnergy()
	return m, nil
}

// clampEnergy keeps the Energy view's scroll in range.
func (m *model) clampEnergy() {
	m.ev.scroll = max(0, min(m.ev.scroll, len(m.energyLines())-m.bodyHeight()))
}

// energyView renders the visible part of the Energy view.
func (m model) energyView() string {
	lines := m.energyLines()
	end := min(len(lines), m.ev.scroll+m.bodyHeight())
	return strings.Join(lines[min(m.ev.scroll, end):end], "\n")
}

// energyFooter lists the view's keys and where the scroll is.
func (m model) energyFooter() string {
	s := "[ ] range · ↑/↓ scroll"
	if n, h := len(m.energyLines()), m.bodyHeight(); n > h {
		s += fmt.Sprintf(" · %d–%d of %d", m.ev.scroll+1, min(n, m.ev.scroll+h), n)
	}
	if m.ev.err != nil && m.ev.data != nil {
		s = "energy: " + m.ev.err.Error() + " · " + s
	}
	return s
}

// energyRow is one device on the Energy view: its totals (nil before any) and what it draws
// now (nil when offline or unknown).
type energyRow struct {
	d     device
	u     *hub.Usage
	now   *power.Breakdown
	color int // index into deviceStyles
}

// energyRows pairs every device, in display order, with its totals.
func (m model) energyRows() []energyRow {
	byID := map[string]*hub.Usage{}
	if m.ev.data != nil {
		for i := range m.ev.data.Devices {
			byID[m.ev.data.Devices[i].ID] = &m.ev.data.Devices[i]
		}
	}
	var rows []energyRow
	for i, d := range m.devs {
		r := energyRow{d: d, u: byID[d.id], color: i}
		if m.isOnline(d) && d.last != nil {
			if b := power.Estimate(d.last); b != nil {
				if d.watts != nil { // the hub's own estimate wins
					b.Watts = *d.watts
				}
				r.now = b
			}
		}
		rows = append(rows, r)
	}
	return rows
}

// priceSettings is the price in force: the hub's (or the user's local) saved price, or a
// guess from this machine's time zone.
func (m model) priceSettings() (s power.Settings, guessed bool) {
	if m.ev.data != nil && m.ev.data.Settings != nil {
		return *m.ev.data.Settings, false
	}
	if m.opts.Price != nil {
		return *m.opts.Price, false
	}
	return m.guess, true
}

// energyLines builds the whole Energy view, before scrolling.
func (m model) energyLines() []string {
	t, w := m.theme, m.width
	var tabs []string
	for i, r := range energyRanges {
		if i == m.ev.rng {
			tabs = append(tabs, t.tabOn.Render(" "+r.name+" "))
		} else {
			tabs = append(tabs, t.tabOff.Render(" "+r.name+" "))
		}
	}
	lines := []string{spread(t.label.Render("Electricity"), strings.Join(tabs, ""), w)}
	if m.source() == nil {
		return append(lines, "", t.dim.Render("Energy is not available here."))
	}
	if m.ev.err != nil && m.ev.data == nil {
		return append(lines, "", t.err.Render("energy: "+m.ev.err.Error()))
	}

	price, guessed := m.priceSettings()
	rows, data := m.energyRows(), m.ev.data
	now := m.nowMS()
	bounds := energyBounds(m.now().In(m.loc()), m.ev.rng)
	if data != nil && len(data.Bounds) >= 2 {
		bounds = data.Bounds
	}
	end := math.Min(now, bounds[len(bounds)-1])
	var nowW, wh, cost float64
	first := math.Inf(1)
	for _, r := range rows {
		if r.now != nil {
			nowW += r.now.Watts
		}
		if r.u != nil {
			wh += r.u.Wh
			cost += devCost(r.u, price)
			if r.u.FirstData != nil {
				first = math.Min(first, float64(*r.u.FirstData))
			}
		}
	}
	kwh := wh / 1000
	covered := 0.0
	if !math.IsInf(first, 1) {
		covered = end - math.Max(bounds[0], first)
	}
	since := energyRanges[m.ev.rng].label
	if !math.IsInf(first, 1) && first > bounds[1] {
		since = "since " + m.msTime(first).Format("Jan 2 15:04")
	}
	monthKwh := nowW * 24 * monthDays / 1000
	monthRate := price.DayRate()
	monthNote := "at the current draw"
	if covered >= 3600e3 {
		monthKwh = kwh / covered * monthDays * 86400e3
		monthNote = "at the recent average"
		if kwh > 0 {
			monthRate = cost / kwh
		}
	}
	tou := len(price.TOU) > 0
	rateNote := "this rate"
	if tou {
		rateNote = "your rates"
	}
	costNote := "at " + fmtPrice(price)
	if tou {
		costNote = "with your time-of-use rates"
		if kwh > 0 {
			costNote = "avg " + fmtKwhPrice(cost/kwh, price.Currency) + "/kWh " + costNote
		}
	}
	loading := func(s string) string {
		if data == nil {
			return "…"
		}
		return s
	}
	tile := func(k, v, sub string) string {
		return pad(t.label.Render(k), 13) + pad(v, 11) + t.dim.Render(sub)
	}
	lines = append(lines,
		tile("Drawing now", fmtW(&nowW), money(nowW/1000*24*price.DayRate(), price.Currency)+" a day at "+rateNote),
		tile("Energy used", loading(fmtKwh(kwh)), since),
		tile("Cost", loading(money(cost, price.Currency)), costNote),
		tile("Per month", loading(money(monthKwh*monthRate, price.Currency)), "≈ "+fmtKwh(monthKwh)+" "+monthNote),
		t.dim.Render("Electricity price ")+fmtPrice(price)+t.dim.Render(" · "+priceSource(price, guessed)),
		"",
	)
	if data == nil {
		return append(lines, t.dim.Render("loading…"))
	}

	lines = append(lines, m.energyChart(rows, bounds)...)
	lines = append(lines, "")
	lines = append(lines, m.powerTable(rows, price, nowW, kwh, cost)...)
	lines = append(lines, "", t.label.Render("Usage"))
	lines = append(lines, m.usageTable(rows, math.Max(60e3, end-bounds[0]))...)
	return append(lines, "", t.dim.Render(`Power is estimated "at the wall"; expect totals within about 20% of a plug-in meter.`),
		t.dim.Render("Energy is only counted while the device is reporting."))
}

// devCost is a device's cost: the hub's, priced minute by minute, or its energy at the
// guessed flat rate when no price is saved.
func devCost(u *hub.Usage, price power.Settings) float64 {
	if u.Cost != nil {
		return *u.Cost
	}
	return u.Wh / 1000 * price.Rate
}

// deviceStyles colour devices in the chart and tables, cycling as upstream's DEVICE_COLORS.
func (m model) deviceStyles() []lipgloss.Style {
	if m.opts.NoColor {
		return []lipgloss.Style{lipglossNone}
	}
	colors := []color.Color{lipgloss.Cyan, lipgloss.Magenta, lipgloss.Yellow, lipgloss.Blue,
		lipgloss.Green, lipgloss.Red, lipgloss.BrightCyan, lipgloss.BrightMagenta}
	out := make([]lipgloss.Style, len(colors))
	for i, c := range colors {
		out[i] = lipgloss.NewStyle().Foreground(c)
	}
	return out
}

// mark is a device's colour swatch.
func (m model) mark(i int) string {
	st := m.deviceStyles()
	g := "■"
	if m.theme.ascii {
		g = "#"
	}
	return st[i%len(st)].Render(g)
}

// energyChart is the stacked bar chart of energy per hour or day, one colour per device.
func (m model) energyChart(rows []energyRow, bounds []float64) []string {
	t, w := m.theme, m.width
	n := len(bounds) - 1
	var with []energyRow
	for _, r := range rows {
		if r.u != nil && r.u.Wh > 0 && len(r.u.Buckets) == n {
			with = append(with, r)
		}
	}
	title := "Energy per day"
	if m.ev.rng == 0 {
		title = "Energy per hour"
	}
	var legend []string
	for _, r := range with {
		legend = append(legend, m.mark(r.color)+" "+r.d.name)
	}
	out := []string{spread(t.label.Render(title), strings.Join(legend, "  "), w)}
	if len(with) == 0 {
		return append(out, t.dim.Render("No energy recorded in this range yet."))
	}
	// At least one cell per bar: a window too narrow for every bucket shows the latest ones.
	pw := max(1, w-energyAxisW-1)
	first := max(0, n-pw)
	shown := n - first
	totals := make([]float64, n)
	hi := 0.001
	for i := first; i < n; i++ {
		for _, r := range with {
			totals[i] += r.u.Buckets[i] / 1000
		}
		hi = math.Max(hi, totals[i])
	}
	yMax := niceMax(hi * 1.1)
	h := max(3, min(12, m.bodyHeight()/4))
	slot := float64(pw) / float64(shown)
	bw := max(1, min(4, int(slot*0.7)))
	var wraps []wrap
	for _, st := range m.deviceStyles() {
		wraps = append(wraps, wrapOf(st))
	}

	// Each bar is a column of cells, bottom up; a cell takes the colour of the device whose
	// share covers its middle, and the top cell is a partial block.
	eighths := []rune(" ▁▂▃▄▅▆▇█")
	grid := make([][]string, h)
	for y := range grid {
		grid[y] = make([]string, pw)
		for x := range grid[y] {
			grid[y][x] = " "
		}
	}
	for i := first; i < n; i++ {
		x0 := int(slot*float64(i-first) + (slot-float64(bw))/2)
		cells := totals[i] / yMax * float64(h)
		for y := range h { // y = 0 is the bottom row
			fill := math.Min(1, cells-float64(y))
			if fill <= 0 {
				break
			}
			mid := (float64(y) + math.Min(0.5, fill/2)) / float64(h) * yMax
			owner, acc := len(with)-1, 0.0
			for j, r := range with {
				acc += r.u.Buckets[i] / 1000
				if mid < acc {
					owner = j
					break
				}
			}
			var g string
			if t.ascii {
				if fill < 0.5 {
					continue
				}
				g = "#"
			} else {
				g = string(eighths[max(1, int(math.Round(fill*8)))])
			}
			cell := wraps[with[owner].color%len(wraps)].render(g)
			for x := x0; x < x0+bw && x < pw; x++ {
				grid[h-1-y][x] = cell
			}
		}
	}
	for y := range h {
		out = append(out, t.dim.Render(padLeft(axisLabel(y, h, yMax), energyAxisW))+" "+strings.Join(grid[y], ""))
	}
	// Bucket labels, spaced so they don't touch; one that would run off the end is left out.
	labelW := 0
	for i := first; i < n; i++ {
		labelW = max(labelW, len([]rune(m.bucketLabel(bounds, i))))
	}
	every := max(1, int(math.Ceil(float64(labelW+1)/slot)))
	axis := []rune(strings.Repeat(" ", pw))
	for i := first; i < n; i += every {
		x := int(slot * float64(i-first))
		label := []rune(m.bucketLabel(bounds, i))
		if x+len(label) > pw {
			break
		}
		copy(axis[x:], label)
	}
	return append(out, t.dim.Render(strings.Repeat(" ", energyAxisW+1)+string(axis)))
}

// axisLabel is the bar chart's y label for row y of h (0 at the top): the maximum, half way
// and zero.
func axisLabel(y, h int, yMax float64) string {
	switch {
	case y == 0:
		return fmtKwhAxis(yMax)
	case y == h-1:
		return "0 kWh"
	case h >= 5 && y == (h-1)/2:
		return fmtKwhAxis(yMax * float64(h-1-y) / float64(h-1))
	}
	return ""
}

// fmtKwhAxis is an axis kWh label: "2.5 kWh".
func fmtKwhAxis(v float64) string {
	return trimZero(strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", v), "0"), ".")) + " kWh"
}

// bucketLabel names bucket i: the hour for 24 h, the weekday and day for 7 days, the month
// and day for 30 days.
func (m model) bucketLabel(bounds []float64, i int) string {
	d := m.msTime(bounds[i])
	switch m.ev.rng {
	case 0:
		return d.Format("15h")
	case 1:
		return d.Format("Mon 2")
	default:
		return d.Format("Jan 2")
	}
}

// powerTable is upstream's power table: now, average, peak, energy, cost and share per
// device, then the total.
func (m model) powerTable(rows []energyRow, price power.Settings, nowW, kwh, cost float64) []string {
	t := m.theme
	total := kwh * 1000
	if total == 0 {
		total = 1
	}
	cols := []tcol{{"Device", 0}, {"Now", 8}, {"Average", 8}, {"Peak", 8}, {"Energy", 10}, {"Cost", 9}, {"Share", 6}, {"Power reading", -28}}
	var body [][]string
	for _, r := range rows {
		if (r.u == nil || r.u.Minutes == 0) && r.now == nil {
			continue
		}
		nowS := t.dim.Render("offline")
		if r.now != nil {
			nowS = fmtW(&r.now.Watts)
		}
		avg, peak, energy, costS, share := none, none, none, none, ""
		if r.u != nil {
			if s, ok := r.u.Stats["power"]; ok {
				avg, peak = fmtW(&s.Avg), fmtW(s.Peak)
			}
			energy = fmtKwh(r.u.Wh / 1000)
			costS = money(devCost(r.u, price), price.Currency)
			share = fmt.Sprintf("%.0f%%", math.Round(r.u.Wh/total*100))
		}
		src := r.now
		if src == nil && r.d.last != nil {
			src = power.Estimate(r.d.last)
		}
		body = append(body, []string{m.mark(r.color) + " " + r.d.name, nowS, avg, peak, energy, costS, share, powerSource(src)})
	}
	if len(body) == 0 {
		return []string{t.dim.Render("No power data yet.")}
	}
	body = append(body, []string{t.label.Render("All devices"), fmtW(&nowW), "", "", fmtKwh(kwh), money(cost, price.Currency), "", ""})
	return m.table(cols, body)
}

// powerSource says which parts of a device's power were measured.
func powerSource(e *power.Breakdown) string {
	switch {
	case e == nil:
		return none
	case e.CPUMeasured && e.GPUMeasured:
		return "CPU and GPU measured"
	case e.GPUMeasured:
		return "GPU measured, CPU estimated"
	case e.CPUMeasured:
		return "CPU measured, rest estimated"
	}
	return "Estimated from load"
}

// usageTable is upstream's usage table: online time and share of the range, CPU and GPU
// average/peak, memory average, peak temperatures, and bytes moved.
func (m model) usageTable(rows []energyRow, span float64) []string {
	t := m.theme
	cols := []tcol{{"Device", 0}, {"Online", 11}, {"CPU avg/pk", 10}, {"GPU avg/pk", 10}, {"Mem", 4},
		{"Temp CPU/GPU", 12}, {"Down", 9}, {"Up", 9}, {"Read", 9}, {"Written", 9}}
	stat := func(u *hub.Usage, k string, peak bool, f func(float64) string) string {
		s, ok := u.Stats[k]
		switch {
		case !ok:
			return none
		case !peak:
			return f(s.Avg)
		case s.Peak == nil:
			return none
		}
		return f(*s.Peak)
	}
	pct := func(v float64) string { return fmt.Sprintf("%.0f%%", v) }
	temp := t.tempDeg
	var body [][]string
	for _, r := range rows {
		u := r.u
		if u == nil || u.Minutes == 0 {
			continue
		}
		hours := float64(u.Minutes) / 60
		on := fmt.Sprintf("%.1f h", hours)
		if u.Minutes >= 600 {
			on = fmt.Sprintf("%.0f h", hours)
		}
		on += t.dim.Render(fmt.Sprintf(" %.0f%%", math.Min(100, math.Round(float64(u.Minutes)*60e3/span*100))))
		body = append(body, []string{
			m.mark(r.color) + " " + r.d.name, on,
			stat(u, "cpu", false, pct) + t.dim.Render("/"+stat(u, "cpu", true, pct)),
			stat(u, "gpu", false, pct) + t.dim.Render("/"+stat(u, "gpu", true, pct)),
			stat(u, "mem", false, pct),
			stat(u, "cpuTemp", true, temp) + t.dim.Render(" / ") + stat(u, "gpuTemp", true, temp),
			fmtBytes(float64(u.Bytes["rx"])), fmtBytes(float64(u.Bytes["tx"])),
			fmtBytes(float64(u.Bytes["dr"])), fmtBytes(float64(u.Bytes["dw"])),
		})
	}
	if len(body) == 0 {
		return []string{t.dim.Render("No data for this time range yet.")}
	}
	return m.table(cols, body)
}

// tcol is a table column: its heading and width. Width 0 is the name column, which takes
// what is left (at least 10); a negative width is left-aligned. Columns that don't fit the
// window are dropped from the right.
type tcol struct {
	head string
	w    int
}

// table lays out a header and rows in the window's width.
func (m model) table(cols []tcol, rows [][]string) []string {
	const nameMin = 10
	used := nameMin
	n := 0
	for i, c := range cols {
		if c.w == 0 {
			n = i + 1
			continue
		}
		cw := c.w
		if cw < 0 {
			cw = -cw
		}
		if used+1+cw > m.width {
			break
		}
		used += 1 + cw
		n = i + 1
	}
	cols = cols[:n]
	nameW := nameMin + m.width - used
	nameW = min(nameW, 24)
	line := func(cells []string, style func(string) string) string {
		var parts []string
		for i, c := range cols {
			s := ""
			if i < len(cells) {
				s = cells[i]
			}
			switch {
			case c.w == 0:
				parts = append(parts, pad(s, nameW))
			case c.w < 0:
				parts = append(parts, pad(s, -c.w))
			default:
				parts = append(parts, padLeft(s, c.w))
			}
		}
		return style(strings.TrimRight(strings.Join(parts, " "), " "))
	}
	heads := make([]string, len(cols))
	for i, c := range cols {
		heads[i] = c.head
	}
	out := []string{line(heads, func(s string) string { return m.theme.label.Render(s) })}
	for _, r := range rows {
		out = append(out, line(r, func(s string) string { return s }))
	}
	return out
}

// fmtW formats watts as upstream: whole watts, or kW from 1000 W.
func fmtW(w *float64) string {
	switch {
	case w == nil:
		return none
	case *w >= 1000:
		return fmt.Sprintf("%.2f kW", *w/1000)
	}
	return fmt.Sprintf("%.0f W", math.Round(*w))
}

// fmtKwh formats kWh as upstream: 2 decimals under 10, 1 under 100, whole above.
func fmtKwh(k float64) string {
	switch {
	case k < 10:
		return fmt.Sprintf("%.2f kWh", k)
	case k < 100:
		return fmt.Sprintf("%.1f kWh", k)
	}
	return thousands(int64(math.Round(k))) + " kWh"
}

// thousands writes n with comma separators.
func thousands(n int64) string {
	s := fmt.Sprint(n)
	if n < 0 {
		return "-" + thousands(-n)
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// currencySymbols are the prefixes for common currencies; others print their code after.
var currencySymbols = map[string]string{"USD": "$", "CAD": "CA$", "AUD": "A$", "NZD": "NZ$", "EUR": "€",
	"GBP": "£", "JPY": "¥", "INR": "₹", "KRW": "₩", "CNY": "CN¥", "BRL": "R$", "MXN": "MX$"}

// money formats an amount like upstream: whole units from 100, cents below.
func money(v float64, cur string) string {
	digits := 2
	if math.Abs(v) >= 100 {
		digits = 0
	}
	return moneyN(v, cur, digits)
}

// moneyN formats an amount with digits decimals.
func moneyN(v float64, cur string, digits int) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return none
	}
	pow := math.Pow(10, float64(digits))
	num := fmt.Sprintf("%.*f", digits, math.Round(math.Abs(v)*pow)/pow) // half away from zero, as Intl
	if whole, frac, ok := strings.Cut(num, "."); ok {
		n := 0
		fmt.Sscan(whole, &n)
		num = thousands(int64(n)) + "." + frac
	} else {
		n := 0
		fmt.Sscan(num, &n)
		num = thousands(int64(n))
	}
	sign := ""
	if v < 0 {
		sign = "-"
	}
	if sym, ok := currencySymbols[cur]; ok {
		return sign + sym + num
	}
	return sign + num + " " + cur
}

// fmtKwhPrice formats a price per kWh: 3 decimals under 1.
func fmtKwhPrice(r float64, cur string) string {
	if r < 1 {
		return moneyN(r, cur, 3)
	}
	return moneyN(r, cur, 2)
}

// fmtPrice is the price line: "$0.165/kWh", or the time-of-use range.
func fmtPrice(s power.Settings) string {
	if len(s.TOU) == 0 {
		return fmtKwhPrice(s.Rate, s.Currency) + "/kWh"
	}
	lo, hi := s.RateRange()
	return fmtKwhPrice(lo, s.Currency) + "–" + fmtKwhPrice(hi, s.Currency) + "/kWh by time of day"
}

// priceSource says where the price came from, as upstream.
func priceSource(s power.Settings, guessed bool) string {
	name := "your area"
	r, known := power.LookupRegion(s.Region)
	if known {
		name = r.Name
	}
	switch {
	case len(s.TOU) > 0:
		return "Your time-of-use rates."
	case guessed:
		return "Typical home rate for " + name + ", guessed from this device's time zone."
	case s.Region != "custom" && known && r.Rate == s.Rate:
		return "Typical home rate for " + name + "."
	}
	return "The rate you entered."
}

// ---- cost today (dashboard header) -----------------------------------------------------

// todayState is what every device has cost since local midnight.
type todayState struct {
	seq  int
	cost *float64 // nil until the first read
	cur  string
}

// Cost-today messages.
type (
	todayMsg struct {
		seq  int
		data *client.Energy
		err  error
	}
	todayRefreshMsg struct{ seq int }
)

// fetchToday reads energy since local midnight, for the dashboard's cost today.
func (m *model) fetchToday() tea.Cmd {
	src := m.source()
	if src == nil {
		return nil
	}
	m.today.seq++
	seq, parent := m.today.seq, m.ctx
	now := m.now().In(m.loc())
	y, mo, d := now.Date()
	start := float64(time.Date(y, mo, d, 0, 0, 0, 0, now.Location()).UnixMilli())
	end := math.Max(float64(now.UnixMilli()), start+60e3) // the hub wants an ascending pair
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, viewTimeout)
		defer cancel()
		data, err := src.Energy(ctx, []float64{start, end})
		return todayMsg{seq: seq, data: data, err: err}
	}
}

// updateToday handles the cost-today messages; ok is false for anything else.
func (m model) updateToday(msg tea.Msg) (model, tea.Cmd, bool) {
	switch msg := msg.(type) {
	case todayMsg:
		if msg.seq != m.today.seq {
			return m, nil, true
		}
		if msg.err == nil && msg.data != nil {
			price, _ := m.priceSettings()
			if msg.data.Settings != nil {
				price = *msg.data.Settings
			}
			total := 0.0
			for i := range msg.data.Devices {
				total += devCost(&msg.data.Devices[i], price)
			}
			m.today.cost, m.today.cur = &total, price.Currency
		}
		seq := msg.seq
		return m, tea.Tick(energyRefresh, func(time.Time) tea.Msg { return todayRefreshMsg{seq} }), true
	case todayRefreshMsg:
		if msg.seq != m.today.seq {
			return m, nil, true
		}
		return m, m.fetchToday(), true
	}
	return m, nil, false
}

// todayText is the header's "$1.23 today", or "" before the first read.
func (m model) todayText() string {
	if m.today.cost == nil {
		return ""
	}
	return money(*m.today.cost, m.today.cur) + " today"
}
