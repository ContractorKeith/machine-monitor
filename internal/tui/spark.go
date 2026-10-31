package tui

import (
	"strings"

	"github.com/NimbleMarkets/ntcharts/v2/sparkline"
)

// historyLen is how many samples a series keeps: 4 minutes at the 2 s default, enough to
// fill the widest card.
const historyLen = 120

// series is a fixed-length history of one percentage metric, oldest first.
type series struct{ v []float64 }

// push appends a sample; an unknown sample is recorded as 0 so the line keeps its timing.
func (s *series) push(p *float64) {
	v := 0.0
	if p != nil {
		v = *p
	}
	s.v = append(s.v, v)
	if len(s.v) > historyLen {
		s.v = s.v[len(s.v)-historyLen:]
	}
}

// sparkHeight is the sparkline height in rows.
const sparkHeight = 2

// spark draws the last w samples of a 0–100 series as a sparkHeight-row chart: braille by
// default (ntcharts), or an ASCII ramp under the fallback.
func (t theme) spark(s series, w int) string {
	if w <= 0 {
		return ""
	}
	d := s.v
	if len(d) > w {
		d = d[len(d)-w:]
	}
	if t.ascii {
		return t.sparkStyle.Render(asciiSpark(d, w))
	}
	sl := sparkline.New(w, sparkHeight,
		sparkline.WithNoAutoMaxValue(), sparkline.WithMaxValue(100), sparkline.WithStyle(t.sparkStyle))
	sl.PushAll(d)
	sl.DrawBraille()
	return sl.View()
}

// asciiSpark draws d right-aligned in w columns over sparkHeight rows using ASCII only.
func asciiSpark(d []float64, w int) string {
	const ramp = " .-=#" // a cell's fill level, 0–4
	levels := len(ramp) - 1
	rows := make([][]byte, sparkHeight)
	for i := range rows {
		rows[i] = []byte(strings.Repeat(" ", w))
	}
	off := w - len(d)
	for i, v := range d {
		n := int(v/100*float64(levels*sparkHeight) + 0.5) // filled levels across all rows
		n = max(0, min(n, levels*sparkHeight))
		for r := range sparkHeight { // r = 0 is the bottom row
			cell := max(0, min(n-r*levels, levels))
			rows[sparkHeight-1-r][off+i] = ramp[cell]
		}
	}
	out := make([]string, sparkHeight)
	for i, r := range rows {
		out[i] = string(r)
	}
	return strings.Join(out, "\n")
}
