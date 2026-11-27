package tui

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

const frameBudget = 16 * time.Millisecond

func bigModel(w, h int, v view) model {
	o := testOptions()
	o.Procs = procList(600)
	m := New(o).(model)
	next, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	m = next.(model)
	for i := range historyLen {
		r := workstation(64)
		r.CPU.Usage = p(float64(i % 100))
		next, _ = m.Update(reportMsg{r: r})
		m = next.(model)
	}
	next, _ = m.Update(procsMsg{p: procList(600)})
	m = next.(model)
	m.view = v
	return m
}

func BenchmarkFrame(b *testing.B) {
	for v := range viewCount {
		b.Run(viewNames[v], func(b *testing.B) {
			m := bigModel(160, 48, v)
			b.ReportAllocs()
			for b.Loop() {
				_ = m.render()
			}
		})
	}
}

func TestFrameBudget(t *testing.T) {
	for v := range viewCount {
		m := bigModel(160, 48, v)
		const n = 20
		start := time.Now()
		for range n {
			_ = m.render()
		}
		if avg := time.Since(start) / n; avg >= frameBudget {
			t.Errorf("%s frame takes %v, budget %v", viewNames[v], avg, frameBudget)
		} else {
			t.Logf("%s frame: %v", viewNames[v], avg)
		}
	}
}
