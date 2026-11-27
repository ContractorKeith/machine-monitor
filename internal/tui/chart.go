package tui

import (
	"math"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/NimbleMarkets/ntcharts/v2/canvas"
	"github.com/NimbleMarkets/ntcharts/v2/canvas/graph"

	"github.com/ContractorKeith/machine-monitor/internal/history"
)

const brailleEmpty = '⠀'

var asciiMarks = []rune{'*', '+', 'o'}

type plotSeries struct {
	get   func(*history.Point) *float64
	style lipgloss.Style
}

type plot struct {
	pts      []history.Point
	series   []plotSeries
	from, to float64
	yMax     float64
	gap      float64
	w, h     int
	cursor   int
}

func (p plot) render(t theme) []string {
	if p.w <= 0 || p.h <= 0 {
		return nil
	}
	var cells [][]rune
	var owner [][]int8
	if t.ascii {
		cells, owner = p.ascii()
	} else {
		cells, owner = p.braille()
	}
	wraps := make([]wrap, len(p.series))
	for i, s := range p.series {
		wraps[i] = wrapOf(s.style)
	}
	lines := make([]string, p.h)
	for y := range p.h {
		var b strings.Builder
		run, cur := []rune{}, int8(-2)
		flush := func() {
			if len(run) == 0 {
				return
			}
			if cur >= 0 {
				b.WriteString(wraps[cur].render(string(run)))
			} else {
				b.WriteString(string(run))
			}
			run = run[:0]
		}
		for x := range p.w {
			r, o := cells[y][x], owner[y][x]
			if x == p.cursor {
				flush()
				cur = -2
				if o < 0 {
					b.WriteString(t.dim.Render(string(t.cursorRune())))
				} else {
					b.WriteString(t.selected.Render(string(r)))
				}
				continue
			}
			if o != cur {
				flush()
				cur = o
			}
			if o < 0 {
				r = ' '
			}
			run = append(run, r)
		}
		flush()
		lines[y] = b.String()
	}
	return lines
}

type wrap struct{ pre, post string }

func wrapOf(st lipgloss.Style) wrap {
	pre, post, _ := strings.Cut(st.Render("X"), "X")
	return wrap{pre, post}
}

func (w wrap) render(s string) string { return w.pre + s + w.post }

func (t theme) cursorRune() rune {
	if t.ascii {
		return '|'
	}
	return '│'
}

func (p plot) braille() ([][]rune, [][]int8) {
	cells := make([][]rune, p.h)
	owner := make([][]int8, p.h)
	for y := range cells {
		cells[y] = make([]rune, p.w)
		owner[y] = make([]int8, p.w)
		for x := range cells[y] {
			cells[y][x], owner[y][x] = brailleEmpty, -1
		}
	}
	for si := len(p.series) - 1; si >= 0; si-- {
		s := p.series[si]
		grid := graph.NewBrailleGrid(p.w, p.h, p.from, p.to, 0, p.yMax)
		gw := 2 * p.w
		lo, hi := make([]int, gw), make([]int, gw)
		for x := range lo {
			lo[x], hi[x] = math.MaxInt, -1
		}
		mark := func(q canvas.Point) {
			if q.X >= 0 && q.X < gw {
				lo[q.X], hi[q.X] = min(lo[q.X], q.Y), max(hi[q.X], q.Y)
			}
		}
		var prev canvas.Point
		prevT, have := 0.0, false
		for i := range p.pts {
			v := s.get(&p.pts[i])
			if v == nil || math.IsNaN(*v) {
				have = false
				continue
			}
			pt := grid.GridPoint(canvas.Float64Point{X: p.pts[i].T, Y: math.Max(0, math.Min(p.yMax, *v))})
			if have && p.pts[i].T-prevT <= p.gap && pt.X != prev.X {
				for _, q := range graph.GetLinePoints(prev, pt) {
					mark(q)
				}
			} else {
				mark(pt)
			}
			if have && p.pts[i].T-prevT <= p.gap && pt.X == prev.X {
				mark(prev)
			}
			prev, prevT, have = pt, p.pts[i].T, true
		}
		for x := range gw {
			for y := lo[x]; y <= hi[x]; y++ {
				grid.Set(canvas.Point{X: x, Y: y})
			}
		}
		for y, row := range grid.BraillePatterns() {
			if y >= p.h {
				break
			}
			for x, r := range row {
				if x >= p.w || r == brailleEmpty || r == 0 {
					continue
				}
				cells[y][x] |= r
				owner[y][x] = int8(si)
			}
		}
	}
	return cells, owner
}

func (p plot) ascii() ([][]rune, [][]int8) {
	cells := make([][]rune, p.h)
	owner := make([][]int8, p.h)
	for y := range cells {
		cells[y] = []rune(strings.Repeat(" ", p.w))
		owner[y] = make([]int8, p.w)
		for x := range owner[y] {
			owner[y][x] = -1
		}
	}
	span := p.to - p.from
	for si := len(p.series) - 1; si >= 0; si-- {
		sum := make([]float64, p.w)
		n := make([]int, p.w)
		for i := range p.pts {
			v := p.series[si].get(&p.pts[i])
			if v == nil || math.IsNaN(*v) || span <= 0 {
				continue
			}
			x := int((p.pts[i].T - p.from) / span * float64(p.w))
			if x < 0 || x >= p.w {
				continue
			}
			sum[x] += *v
			n[x]++
		}
		for x := range p.w {
			if n[x] == 0 {
				continue
			}
			f := math.Max(0, math.Min(1, sum[x]/float64(n[x])/p.yMax))
			y := p.h - 1 - int(math.Round(f*float64(p.h-1)))
			cells[y][x] = asciiMarks[si%len(asciiMarks)]
			owner[y][x] = int8(si)
		}
	}
	return cells, owner
}

func niceMax(v float64) float64 {
	if !(v > 0) || math.IsInf(v, 0) {
		return 1
	}
	pw := math.Pow(10, math.Floor(math.Log10(v)))
	m := v / pw
	switch {
	case m <= 1:
		m = 1
	case m <= 2:
		m = 2
	case m <= 2.5:
		m = 2.5
	case m <= 5:
		m = 5
	default:
		m = 10
	}
	return m * pw
}
