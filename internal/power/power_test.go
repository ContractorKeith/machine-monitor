package power

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/ContractorKeith/machine-monitor/internal/report"
)

// Cases are in testdata/cases.json; the expected numbers in testdata/expected.json come from
// running upstream's power.js over them (testdata/upstream.js).
type fixture struct {
	Estimate []struct {
		Name   string        `json:"name"`
		Report report.Report `json:"report"`
	} `json:"estimate"`
	FromSample []struct {
		Name   string `json:"name"`
		Sample struct {
			CPU      *float64 `json:"cpu"`
			GPUPower *float64 `json:"gpuPower"`
		} `json:"sample"`
		HW struct {
			FormFactor *string  `json:"formFactor"`
			Threads    *float64 `json:"threads"`
		} `json:"hw"`
	} `json:"fromSample"`
}

type expected struct {
	Estimate []struct {
		Name string     `json:"name"`
		Want *Breakdown `json:"want"`
	} `json:"estimate"`
	FromSample []struct {
		Name string   `json:"name"`
		Want *float64 `json:"want"`
	} `json:"fromSample"`
	PriceRegions []Region          `json:"priceRegions"`
	TZRegions    map[string]string `json:"tzRegions"`
}

func load(t *testing.T) (fixture, expected) {
	t.Helper()
	var f fixture
	var e expected
	for path, v := range map[string]any{"testdata/cases.json": &f, "testdata/expected.json": &e} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, v); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
	if len(f.Estimate) != len(e.Estimate) || len(f.FromSample) != len(e.FromSample) {
		t.Fatal("cases.json and expected.json out of step; rerun testdata/upstream.js")
	}
	return f, e
}

func TestEstimateMatchesUpstream(t *testing.T) {
	f, e := load(t)
	for i, c := range f.Estimate {
		t.Run(c.Name, func(t *testing.T) {
			if e.Estimate[i].Name != c.Name {
				t.Fatalf("expected.json case %d is %q", i, e.Estimate[i].Name)
			}
			got, want := Estimate(&c.Report), e.Estimate[i].Want
			switch {
			case got == nil && want == nil:
			case got == nil || want == nil:
				t.Fatalf("Estimate = %+v, upstream %+v", got, want)
			case *got != *want:
				t.Fatalf("Estimate = %+v\n          upstream %+v", *got, *want)
			}
		})
	}
}

func TestFromSampleMatchesUpstream(t *testing.T) {
	f, e := load(t)
	for i, c := range f.FromSample {
		t.Run(c.Name, func(t *testing.T) {
			got := FromSample(c.Sample.CPU, c.Sample.GPUPower, HW{FormFactor: c.HW.FormFactor, Threads: c.HW.Threads})
			want := e.FromSample[i].Want
			if (got == nil) != (want == nil) || (got != nil && *got != *want) {
				t.Fatalf("FromSample = %v, upstream %v", deref(got), deref(want))
			}
		})
	}
}

func deref(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}

func TestJSRound(t *testing.T) {
	for in, want := range map[float64]float64{2.5: 3, -2.5: -2, -2.6: -3, 0.49999999999999994: 0, 122.5: 123} {
		if got := jsRound(in); got != want {
			t.Errorf("jsRound(%v) = %v, want %v", in, got, want)
		}
	}
}

// An APU's GPU (flagged integrated by the agent) is part of the package power, even with a
// large memory carve-out: no discrete-GPU estimate on top (skeptic pass 2, B5).
func TestEstimateIntegratedGPUAddsNothing(t *testing.T) {
	r := &report.Report{
		CPU:   report.CPU{Usage: report.Ptr(10.0)},
		Power: &report.Power{CPU: report.Ptr(15.0)},
		GPUs: []report.GPU{{Name: report.Ptr("AMD GPU"), Integrated: true, Usage: report.Ptr(100.0),
			MemTotal: report.Ptr(float64(2 << 30))}},
	}
	b := Estimate(r)
	if b == nil || b.GPU != 0 || b.GPUEstimated {
		t.Fatalf("integrated GPU estimate = %+v, want none", b)
	}
	// Not flagged (an upstream agent): the estimate stays, whatever the name says (N16).
	r.GPUs[0].Integrated = false
	if b := Estimate(r); b.GPU != 160 {
		t.Fatalf("discrete GPU = %v W, want 160", b.GPU)
	}
	r.GPUs[0].Name = report.Ptr("NVIDIA GeForce RTX 4090 Integrated Edition")
	r.GPUs[0].MemTotal = report.Ptr(24.0 * (1 << 30))
	if b := Estimate(r); b.GPU != 160 {
		t.Fatalf("a name containing \"integrated\" suppressed the estimate: %v W", b.GPU)
	}
}
