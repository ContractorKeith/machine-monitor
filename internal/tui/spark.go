package tui

import (
	"math"
	"slices"
	"strings"

	"github.com/NimbleMarkets/ntcharts/v2/canvas"
	"github.com/NimbleMarkets/ntcharts/v2/canvas/graph"
)

// historyLen is how many samples a series keeps: 4 minutes at the 2 s default, enough to
// fill the widest card.
const historyLen = 120

// series is a fixed-length history of one percentage metric, oldest first. An unknown
// sample is NaN, drawn as a gap.
type series struct{ v []float64 }

// push appends a sample; nil (or NaN) is recorded as unknown so the line keeps its timing.
func (s *series) push(p *float64) {
	v := math.NaN()
	if p != nil {
		v = *p
	}
	s.v = append(s.v, v)
	if len(s.v) > historyLen {
		s.v = s.v[len(s.v)-historyLen:]
	}
}

// then is s followed by later, cut to historyLen.
func (s series) then(later series) series {
	v := append(slices.Clone(s.v), later.v...)
	return series{v[max(0, len(v)-historyLen):]}
}

// known reports whether the series has any known sample.
func (s series) known() bool {
	for _, v := range s.v {
		if !math.IsNaN(v) {
			return true
		}
	}
	return false
}

// sparkHeight is the sparkline height in rows.
const sparkHeight = 2

// spark draws the last w samples of a 0–100 series right-aligned in a sparkHeight-row chart,
// with a gap for each unknown sample: braille through ntcharts' braille grid by default, or
// an ASCII ramp under the fallback.
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
	grid := graph.NewBrailleGrid(w, sparkHeight, 0, float64(w), 0, 100)
	off := w - len(d)
	point := func(i int) canvas.Point {
		return grid.GridPoint(canvas.Float64Point{X: float64(off + i), Y: math.Max(0, math.Min(100, d[i]))})
	}
	for i, v := range d {
		if math.IsNaN(v) {
			continue
		}
		if i+1 < len(d) && !math.IsNaN(d[i+1]) {
			for _, pt := range graph.GetLinePoints(point(i), point(i+1)) {
				grid.Set(pt)
			}
		} else {
			grid.Set(point(i))
		}
	}
	c := canvas.New(w, sparkHeight)
	graph.DrawBraillePatterns(&c, canvas.Point{}, grid.BraillePatterns(), t.sparkStyle)
	return c.View()
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
		if math.IsNaN(v) {
			continue // gap
		}
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
