package tui

import (
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/ContractorKeith/machine-monitor/internal/client"
	"github.com/ContractorKeith/machine-monitor/internal/history"
	"github.com/ContractorKeith/machine-monitor/internal/report"
)

// TestSparkSeed (B1): our hub's compact snapshot has no history, so the selected device's
// sparklines are filled on demand from /api/history, once, in front of the live samples
// that arrived meanwhile and without repeating them.
func TestSparkSeed(t *testing.T) {
	devs := threeDevices()
	for i := range devs {
		devs[i].History = nil // a compact snapshot
	}
	f := newFakeHub(devs)
	m := hubModel(t, f, 120, 40)
	if m.seeding == "" {
		t.Fatal("the snapshot started no history request")
	}
	id := m.cur().id
	now := float64(hubNow.UnixMilli())
	live := now - 90e3 // the first live report since the snapshot
	next, _ := m.Update(hubMsg{client.Event{Type: client.EventReport, ID: id, Device: &devs[0],
		Sample: &history.Point{T: live, CPU: report.Ptr(77.0)}, Samples: []history.Point{{T: live, CPU: report.Ptr(77.0)}}}})
	m = next.(model)
	if d := m.cur(); d.seeded || len(d.cpu.v) != 1 {
		t.Fatalf("before seeding: seeded %v, %d samples", d.seeded, len(d.cpu.v))
	}

	m.seeding = "" // the test drops the commands updates return: start the request again
	cmd := m.sparkSeed()
	if cmd == nil {
		t.Fatal("no history request for the selected device")
	}
	if again := m.sparkSeed(); again != nil {
		t.Error("a second request while the first is in flight")
	}
	msg := cmd().(sparkMsg)
	next, _ = m.Update(msg)
	m = next.(model)

	calls := strings.Join(f.Calls(), "\n")
	if !strings.Contains(calls, "history "+id+" ") {
		t.Errorf("calls = %q", calls)
	}
	var want []float64
	for _, p := range msg.pts {
		if p.T < live {
			want = append(want, *p.CPU)
		}
	}
	want = append(want, 77)
	d := m.cur()
	if len(msg.pts) == 0 || !d.seeded || !slices.EqualFunc(d.cpu.v, want, func(a, b float64) bool {
		return a == b || math.IsNaN(a) && math.IsNaN(b)
	}) {
		t.Errorf("cpu series = %v, want %v", d.cpu.v, want)
	}
	if m.sparkSeed() != nil {
		t.Error("seeded twice")
	}
}
