package tui

import (
	"fmt"
	"math"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// none is shown for a metric that is unknown (nil).
const none = "—"

// fmtPct formats a percentage, or none.
func fmtPct(p *float64) string {
	if p == nil {
		return none
	}
	return fmt.Sprintf("%.0f%%", *p)
}

// fmtBytes formats a byte count with binary units: "12.3 GiB".
func fmtBytes(b float64) string {
	const unit = 1024.0
	if b < unit {
		return fmt.Sprintf("%.0f B", b)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	v, i := b/unit, 0
	for v >= unit && i < len(units)-1 {
		v /= unit
		i++
	}
	if v >= 100 {
		return fmt.Sprintf("%.0f %s", v, units[i])
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}

// fmtBytesP formats an optional byte count.
func fmtBytesP(b *float64) string {
	if b == nil {
		return none
	}
	return fmtBytes(*b)
}

// fmtRate formats an optional bytes-per-second rate.
func fmtRate(b *float64) string {
	if b == nil {
		return none
	}
	return fmtBytes(*b) + "/s"
}

// fmtUsedTotal formats "used / total" bytes, either side possibly unknown.
func fmtUsedTotal(used, total *float64) string {
	return fmtBytesP(used) + " / " + fmtBytesP(total)
}

// fmtClock formats a clock in MHz as GHz.
func fmtClock(mhz *float64) string {
	if mhz == nil {
		return none
	}
	return fmt.Sprintf("%.2f GHz", *mhz/1000)
}

// fmtWatts formats watts.
func fmtWatts(w *float64) string {
	if w == nil {
		return none
	}
	return fmt.Sprintf("%.0f W", *w)
}

// fmtUptime formats seconds as "3d 4h 12m".
func fmtUptime(s *float64) string {
	if s == nil {
		return none
	}
	t := int64(*s)
	d, h, m := t/86400, t%86400/3600, t%3600/60
	switch {
	case d > 0:
		return fmt.Sprintf("%dd %dh %dm", d, h, m)
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	default:
		return fmt.Sprintf("%dm", m)
	}
}

// str dereferences an optional string, or none.
func str(s *string) string {
	if s == nil || *s == "" {
		return none
	}
	return *s
}

// orNone is s, or none when it is empty.
func orNone(s string) string {
	if s == "" {
		return none
	}
	return s
}

// pctOf returns used/total as a percentage, or nil when either is unknown or total is 0.
func pctOf(used, total *float64) *float64 {
	if used == nil || total == nil || *total <= 0 {
		return nil
	}
	p := *used / *total * 100
	return &p
}

// fmtTemp formats a temperature in its band colour, or none.
func (t theme) fmtTemp(c *float64) string {
	if c == nil {
		return none
	}
	return t.tempStyle(*c).Render(fmt.Sprintf("%.0f°C", *c))
}

// bar draws a w-cell gauge for a percentage; an unknown value draws an empty track.
func (t theme) bar(p *float64, w int) string {
	if w <= 0 {
		return ""
	}
	n := 0
	style := t.barFill
	if p != nil && !math.IsNaN(*p) { // NaN is unknown, like nil
		v := math.Max(0, math.Min(100, *p))
		n = max(0, min(w, int(math.Round(v/100*float64(w)))))
		style = t.loadStyle(v)
	}
	return style.Render(strings.Repeat(string(t.fill), n)) +
		t.barEmpty.Render(strings.Repeat(string(t.empty), w-n))
}

// pad fits s to exactly w cells: truncated with "…" or padded with spaces.
func pad(s string, w int) string {
	if w <= 0 {
		return ""
	}
	sw := lipgloss.Width(s)
	if sw > w {
		return ansi.Truncate(s, w, "…")
	}
	return s + strings.Repeat(" ", w-sw)
}

// padLeft right-aligns s in w cells.
func padLeft(s string, w int) string {
	sw := lipgloss.Width(s)
	if sw >= w {
		return ansi.Truncate(s, w, "…")
	}
	return strings.Repeat(" ", w-sw) + s
}

// spread puts left and right on one line of width w, right-aligned.
func spread(left, right string, w int) string {
	gap := w - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		return pad(left+" "+right, w)
	}
	return left + strings.Repeat(" ", gap) + right
}

// fit clips or pads s to exactly w columns by h lines, so a frame never wraps or scrolls.
func fit(s string, w, h int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > h {
		lines = lines[:h]
	}
	for len(lines) < h {
		lines = append(lines, "")
	}
	for i, l := range lines {
		lines[i] = pad(l, w)
	}
	return strings.Join(lines, "\n")
}

// cols right-aligns styled values in fixed widths, separated by a space: cols(a, 5, b, 9).
// It is fmt's %5s for strings that may carry ANSI styling.
func cols(pairs ...any) string {
	var parts []string
	for i := 0; i+1 < len(pairs); i += 2 {
		parts = append(parts, padLeft(pairs[i].(string), pairs[i+1].(int)))
	}
	return strings.Join(parts, " ")
}
