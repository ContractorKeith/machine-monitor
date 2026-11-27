package power

import (
	"math"

	"github.com/ContractorKeith/machine-monitor/internal/report"
)

type HW struct {
	FormFactor *string
	Threads    *float64
}

func HWOf(r *report.Report) HW {
	return HW{FormFactor: r.FormFactor, Threads: r.CPU.Threads}
}

type Profile struct {
	Laptop     bool
	CPUIdle    float64
	CPUMax     float64
	Rest       float64
	Efficiency float64
}

func ProfileOf(hw HW) Profile {
	laptop := hw.FormFactor != nil && *hw.FormFactor == report.FormFactorLaptop
	threads := 8.0
	if hw.Threads != nil && *hw.Threads != 0 && !math.IsNaN(*hw.Threads) {
		threads = *hw.Threads
	}
	threads = math.Max(1, math.Min(512, threads))
	if laptop {
		return Profile{Laptop: true, CPUIdle: 3, CPUMax: math.Min(45, 8+threads*2), Rest: 6, Efficiency: 0.88}
	}
	return Profile{CPUIdle: 12, CPUMax: math.Min(250, 30+threads*5), Rest: 25, Efficiency: 0.9}
}

func cpuEstimate(usage float64, p Profile) float64 {
	return p.CPUIdle + (p.CPUMax-p.CPUIdle)*math.Max(0, math.Min(100, usage))/100
}

func gpuEstimate(g report.GPU) float64 {
	if val(g.MemTotal) < 2*(1<<30) || g.Integrated {
		return 0
	}
	return 10 + 1.5*val(g.Usage)
}

type Breakdown struct {
	Watts        float64 `json:"watts"`
	CPU          float64 `json:"cpu"`
	GPU          float64 `json:"gpu"`
	Rest         float64 `json:"rest"`
	Losses       float64 `json:"losses"`
	CPUMeasured  bool    `json:"cpuMeasured"`
	GPUMeasured  bool    `json:"gpuMeasured"`
	GPUEstimated bool    `json:"gpuEstimated"`
}

func Estimate(r *report.Report) *Breakdown {
	p := ProfileOf(HWOf(r))
	var measuredCPU *float64
	if r.Power != nil {
		measuredCPU = r.Power.CPU
	}
	var cpu float64
	switch {
	case measuredCPU != nil:
		cpu = *measuredCPU
	case r.CPU.Usage != nil:
		cpu = cpuEstimate(*r.CPU.Usage, p)
	default:
		return nil
	}
	b := Breakdown{Rest: p.Rest, CPUMeasured: measuredCPU != nil}
	var gpu float64
	for _, g := range r.GPUs {
		if g.Power != nil {
			gpu += *g.Power
			b.GPUMeasured = true
		} else if e := gpuEstimate(g); e != 0 {
			gpu += e
			b.GPUEstimated = true
		}
	}
	components := cpu + gpu + p.Rest
	watts := components / p.Efficiency
	b.Watts, b.CPU, b.GPU, b.Losses = round1(watts), round1(cpu), round1(gpu), round1(watts-components)
	return &b
}

func FromSample(cpu, gpuPower *float64, hw HW) *float64 {
	if cpu == nil {
		return nil
	}
	p := ProfileOf(hw)
	w := (cpuEstimate(*cpu, p) + val(gpuPower) + p.Rest) / p.Efficiency
	return &w
}

func val(v *float64) float64 {
	if v == nil || math.IsNaN(*v) {
		return 0
	}
	return *v
}

func jsRound(v float64) float64 {
	f := math.Floor(v)
	if v-f >= 0.5 {
		f++
	}
	return f
}

func round1(v float64) float64 { return jsRound(v*10) / 10 }
