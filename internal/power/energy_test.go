package power

import (
	"reflect"
	"testing"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/report"
)

// est is Eastern Standard Time without DST, so TOU hours are deterministic on any machine.
var est = time.FixedZone("EST", -5*3600)

// minutes returns one point a minute from start for n minutes, each drawing w watts.
func minutes(start time.Time, n int, w float64) []Minute {
	pts := make([]Minute, n)
	for i := range pts {
		pts[i] = Minute{T: start.Add(time.Duration(i) * time.Minute), Power: report.Ptr(w)}
	}
	return pts
}

func at(h, m int) time.Time { return time.Date(2026, 10, 31, h, m, 0, 0, est) }

func TestAccumulateFlat(t *testing.T) {
	// 100 W for 60 minutes = 100 Wh = 0.1 kWh; at 0.15/kWh that's 0.015.
	price := &Settings{Region: "US-FL", Rate: 0.15, Currency: "USD"}
	u := Accumulate(minutes(at(10, 0), 60, 100), []time.Time{at(0, 0), at(24, 0)}, HW{}, price, est)
	if u.Wh != 100 || u.Minutes != 60 || u.EstimatedMinutes != 0 || *u.Cost != 0.015 {
		t.Fatalf("got %+v cost %v", u, *u.Cost)
	}
	if !reflect.DeepEqual(u.Buckets, []float64{100}) || !reflect.DeepEqual(u.CostBuckets, []float64{0.015}) {
		t.Fatalf("buckets %v %v", u.Buckets, u.CostBuckets)
	}
	if *u.FirstData != at(10, 0).UnixMilli() {
		t.Fatalf("FirstData = %v", *u.FirstData)
	}
}

func TestAccumulateTOU(t *testing.T) {
	// 600 W from 15:30 to 16:30. Each minute is 10 Wh = 0.01 kWh.
	// Off-peak 15:30–16:00: 30 × 0.01 × 0.10 = 0.03. Peak 16:00–16:30: 30 × 0.01 × 0.40 = 0.12.
	price := &Settings{Region: "custom", Rate: 0.10, Currency: "USD", TOU: []Period{{16, 21, 0.40}}}
	bounds := []time.Time{at(15, 0), at(16, 0), at(17, 0)}
	u := Accumulate(minutes(at(15, 30), 60, 600), bounds, HW{}, price, est)
	if u.Wh != 600 || *u.Cost != 0.15 {
		t.Fatalf("Wh %v cost %v, want 600 and 0.15", u.Wh, *u.Cost)
	}
	if !reflect.DeepEqual(u.Buckets, []float64{300, 300}) || !reflect.DeepEqual(u.CostBuckets, []float64{0.03, 0.12}) {
		t.Fatalf("buckets %v %v", u.Buckets, u.CostBuckets)
	}
	// The same instants read in UTC are 20:30–21:30, so the peak half moves to the first bucket.
	u = Accumulate(minutes(at(15, 30), 60, 600), bounds, HW{}, price, time.UTC)
	if !reflect.DeepEqual(u.CostBuckets, []float64{0.12, 0.03}) {
		t.Fatalf("UTC cost buckets %v", u.CostBuckets)
	}
}

func TestAccumulateGaps(t *testing.T) {
	// 3 minutes, a 7-minute gap (device off), then 2 more: 5 minutes at 120 W = 10 Wh.
	// The gap adds nothing; nothing is interpolated across it.
	pts := append(minutes(at(9, 0), 3, 120), minutes(at(9, 10), 2, 120)...)
	u := Accumulate(pts, []time.Time{at(9, 0), at(9, 5), at(10, 0)}, HW{}, nil, est)
	if u.Wh != 10 || u.Minutes != 5 || u.Cost != nil || u.CostBuckets != nil {
		t.Fatalf("got %+v", u)
	}
	if !reflect.DeepEqual(u.Buckets, []float64{6, 4}) {
		t.Fatalf("buckets %v", u.Buckets)
	}
}

func TestAccumulateEstimatesAndSkips(t *testing.T) {
	hw := HW{FormFactor: report.Ptr("desktop"), Threads: report.Ptr(16.0)}
	pts := []Minute{
		{T: at(7, 59), Power: report.Ptr(1000.0)},                       // before range
		{T: at(8, 0), CPU: report.Ptr(50.0)},                            // estimated: (61 + 25) / 0.9 W
		{T: at(8, 1)},                                                   // no power and no load: counted, no energy
		{T: at(8, 2), CPU: report.Ptr(0.0), GPUPower: report.Ptr(45.0)}, // (12 + 45 + 25) / 0.9 W
		{T: at(9, 0), Power: report.Ptr(1000.0)},                        // at the end bound: excluded
	}
	price := &Settings{Rate: 0.30}
	u := Accumulate(pts, []time.Time{at(8, 0), at(9, 0)}, hw, price, est)
	// (86 + 82) / 0.9 / 60 = 3.111 Wh; × 0.30 / 1000 = 0.000933 → 0.0009.
	if u.Minutes != 3 || u.EstimatedMinutes != 2 || u.Wh != 3 || u.Buckets[0] != 3.1 || *u.Cost != 0.0009 {
		t.Fatalf("got %+v cost %v", u, *u.Cost)
	}
	if *u.FirstData != at(8, 0).UnixMilli() {
		t.Fatalf("FirstData = %v", *u.FirstData)
	}
}

func TestAccumulateNoBounds(t *testing.T) {
	u := Accumulate(minutes(at(1, 0), 5, 50), []time.Time{at(0, 0)}, HW{}, nil, est)
	if u.Minutes != 0 || u.Wh != 0 || u.FirstData != nil {
		t.Fatalf("got %+v", u)
	}
}
