package tui

import (
	"fmt"
	"math"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

const none = "—"

func fmtPct(p *float64) string {
	if p == nil {
		return none
	}
	return fmt.Sprintf("%.0f%%", *p)
}

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

func fmtBytesP(b *float64) string {
	if b == nil {
		return none
	}
	return fmtBytes(*b)
}

func fmtRate(b *float64) string {
	if b == nil {
		return none
	}
	return fmtBytes(*b) + "/s"
}

func fmtUsedTotal(used, total *float64) string {
	return fmtBytesP(used) + " / " + fmtBytesP(total)
}

func fmtClock(mhz *float64) string {
	if mhz == nil {
		return none
	}
	return fmt.Sprintf("%.2f GHz", *mhz/1000)
}

func fmtWatts(w *float64) string {
	if w == nil {
		return none
	}
	return fmt.Sprintf("%.0f W", *w)
}

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

func str(s *string) string {
	if s == nil || *s == "" {
		return none
	}
	return *s
}

func orNone(s string) string {
	if s == "" {
		return none
	}
	return s
}

func pctOf(used, total *float64) *float64 {
	if used == nil || total == nil || *total <= 0 {
		return nil
	}
	p := *used / *total * 100
	return &p
}

func (t theme) fmtTemp(c *float64) string {
	if c == nil {
		return none
	}
	return t.tempStyle(*c).Render(t.tempV(*c))
}

func (t theme) bar(p *float64, w int) string {
	if w <= 0 {
		return ""
	}
	n := 0
	style := t.barFill
	if p != nil && !math.IsNaN(*p) {
		v := math.Max(0, math.Min(100, *p))
		n = max(0, min(w, int(math.Round(v/100*float64(w)))))
		style = t.loadStyle(v)
	}
	return style.Render(strings.Repeat(string(t.fill), n)) +
		t.barEmpty.Render(strings.Repeat(string(t.empty), w-n))
}

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

func padLeft(s string, w int) string {
	sw := lipgloss.Width(s)
	if sw >= w {
		return ansi.Truncate(s, w, "…")
	}
	return strings.Repeat(" ", w-sw) + s
}

func spread(left, right string, w int) string {
	gap := w - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		return pad(left+" "+right, w)
	}
	return left + strings.Repeat(" ", gap) + right
}

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

func cols(pairs ...any) string {
	var parts []string
	for i := 0; i+1 < len(pairs); i += 2 {
		parts = append(parts, padLeft(pairs[i].(string), pairs[i+1].(int)))
	}
	return strings.Join(parts, " ")
}
