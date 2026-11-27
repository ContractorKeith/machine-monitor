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

func TestSparkSeed(t *testing.T) {
	devs := threeDevices()
	for i := range devs {
		devs[i].History = nil
	}
	f := newFakeHub(devs)
	m := hubModel(t, f, 120, 40)
	if m.seeding == "" {
		t.Fatal("the snapshot started no history request")
	}
	id := m.cur().id
	now := float64(hubNow.UnixMilli())
	live := now - 90e3
	next, _ := m.Update(hubMsg{client.Event{Type: client.EventReport, ID: id, Device: &devs[0],
		Sample: &history.Point{T: live, CPU: report.Ptr(77.0)}, Samples: []history.Point{{T: live, CPU: report.Ptr(77.0)}}}})
	m = next.(model)
	if d := m.cur(); d.seeded || len(d.cpu.v) != 1 {
		t.Fatalf("before seeding: seeded %v, %d samples", d.seeded, len(d.cpu.v))
	}

	m.seeding = ""
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
