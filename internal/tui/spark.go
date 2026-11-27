package tui

import (
	"math"
	"slices"
	"strings"

	"github.com/NimbleMarkets/ntcharts/v2/canvas"
	"github.com/NimbleMarkets/ntcharts/v2/canvas/graph"
)

const historyLen = 120

type series struct{ v []float64 }

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

func (s series) then(later series) series {
	v := append(slices.Clone(s.v), later.v...)
	return series{v[max(0, len(v)-historyLen):]}
}

func (s series) known() bool {
	for _, v := range s.v {
		if !math.IsNaN(v) {
			return true
		}
	}
	return false
}

const sparkHeight = 2

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

func asciiSpark(d []float64, w int) string {
	const ramp = " .-=#"
	levels := len(ramp) - 1
	rows := make([][]byte, sparkHeight)
	for i := range rows {
		rows[i] = []byte(strings.Repeat(" ", w))
	}
	off := w - len(d)
	for i, v := range d {
		if math.IsNaN(v) {
			continue
		}
		n := int(v/100*float64(levels*sparkHeight) + 0.5)
		n = max(0, min(n, levels*sparkHeight))
		for r := range sparkHeight {
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
