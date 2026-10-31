package power

import (
	"math"
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

// Expected values come from upstream's runtime (node): [v, +v.toFixed(1), +v.toFixed(4)].
func TestToFixedMatchesNode(t *testing.T) {
	for _, c := range [][3]float64{
		{0.15, 0.1, 0.15}, {0.25, 0.3, 0.25}, {0.35, 0.3, 0.35}, {0.45, 0.5, 0.45},
		{1.005, 1, 1.005}, {2.5, 2.5, 2.5}, {0.00005, 0, 0.0001}, {0.00015, 0, 0.0001},
		{0.00025, 0, 0.0003}, {0.00035, 0, 0.0003}, {0.0005, 0, 0.0005}, {0.12345, 0.1, 0.1235},
		{0.00045, 0, 0.0004}, {1.45, 1.4, 1.45}, {8.345, 8.3, 8.345}, {1.0000500000000001, 1, 1.0001},
		{0.0625, 0.1, 0.0625}, {0.03125, 0, 0.0313}, {0.5, 0.5, 0.5}, {1.5, 1.5, 1.5},
		{-0.25, -0.3, -0.25}, {-0.15, -0.1, -0.15}, {1e21, 1e21, 1e21},
		{123456.78905, 123456.8, 123456.7891}, {0.00075, 0, 0.0008}, {0.0004999999999999999, 0, 0.0005},
	} {
		if got := ToFixed(c[0], 1); got != c[1] {
			t.Errorf("ToFixed(%v, 1) = %v, want %v", c[0], got, c[1])
		}
		if got := ToFixed(c[0], 4); got != c[2] {
			t.Errorf("ToFixed(%v, 4) = %v, want %v", c[0], got, c[2])
		}
	}
	if z := ToFixed(math.Copysign(0, -1), 2); math.Signbit(z) {
		t.Error("ToFixed(-0) is negative zero")
	}
	if !math.IsNaN(ToFixed(math.NaN(), 1)) || !math.IsInf(ToFixed(math.Inf(1), 1), 1) {
		t.Error("non-finite values must pass through")
	}
}

// One minute at 27 W and a flat 1.0 rate is 0.00045: upstream's toFixed(4) gives 0.0004.
func TestAccumulateCostTie(t *testing.T) {
	price := &Settings{Region: "US-FL", Rate: 1, Currency: "USD"}
	u := Accumulate(minutes(at(10, 0), 1, 27), []time.Time{at(0, 0), at(24, 0)}, HW{}, price, est)
	if *u.Cost != 0.0004 || u.CostBuckets[0] != 0.0004 {
		t.Errorf("cost = %v %v, want 0.0004 (node: +(27/60/1000).toFixed(4))", *u.Cost, u.CostBuckets)
	}
}
