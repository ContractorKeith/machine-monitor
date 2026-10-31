package tui

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// frameBudget is the acceptance bound for one full frame (PLAN §8, M2).
const frameBudget = 16 * time.Millisecond

// bigModel is a model at w×h holding a 64-core report with a full sparkline history and
// 600 processes, showing view v.
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

// BenchmarkFrame renders a full frame of each view with a 64-core report at 160×48.
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

// TestFrameBudget fails if the average frame of any view takes 16 ms or more, so the
// benchmark's acceptance bound is checked on every CI run, not only when benchmarked.
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
