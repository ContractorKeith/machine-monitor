// Package power estimates a device's electricity use "at the wall" and what it costs.
//
// Agents report whatever the hardware measures: NVIDIA GPU power (nvidia-smi), CPU package
// power where a sensor exists (LibreHardwareMonitor on Windows, RAPL or NVIDIA Grace power
// meters on Linux), AMD GPU power on Linux. Everything else is estimated:
//   - an unmeasured CPU, from its load and a rough power range for its size
//   - an unmeasured discrete GPU, from its load
//   - the rest of the system (motherboard, RAM, drives, fans), a fixed allowance
//   - power-supply / charger losses
//
// so totals are estimates, usually within ~20% of a plug-in power meter.
//
// Ported from Forward-Future/device-monitor power.js, the energy part of server.js and the
// electricity price tables in public/index.html, pinned at c3a4c41
// (MIT, © 2026 Matt): https://github.com/Forward-Future/device-monitor
package power

import (
	"math"

	"github.com/ContractorKeith/machine-monitor/internal/report"
)

// HW is the hardware a device last reported, kept so history points recorded without a
// measured power can still be estimated while the device is offline.
type HW struct {
	FormFactor *string  // report.FormFactorLaptop or anything else (desktop)
	Threads    *float64 // logical processors
}

// HWOf returns the hardware fields of r that the power model uses.
func HWOf(r *report.Report) HW {
	return HW{FormFactor: r.FormFactor, Threads: r.CPU.Threads}
}

// Profile is the power model for one class of machine, in watts.
type Profile struct {
	Laptop     bool
	CPUIdle    float64
	CPUMax     float64
	Rest       float64 // motherboard, RAM, drives, fans
	Efficiency float64 // power supply or charger, 0–1
}

// ProfileOf picks the power model for hw. Unknown or zero threads count as 8, as upstream.
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

// cpuEstimate is CPU watts from load, linear between idle and max.
func cpuEstimate(usage float64, p Profile) float64 {
	return p.CPUIdle + (p.CPUMax-p.CPUIdle)*math.Max(0, math.Min(100, usage))/100
}

// gpuEstimate is watts for a discrete GPU without a power reading (AMD/Intel on Windows).
// Adapters under 2 GiB are assumed integrated, whose power is already in the CPU package;
// so are GPUs the agent flagged integrated (an APU whose carve-out can exceed 2 GiB). A name
// says nothing: only the agent's own sensors can tell.
func gpuEstimate(g report.GPU) float64 {
	if val(g.MemTotal) < 2*(1<<30) || g.Integrated {
		return 0
	}
	return 10 + 1.5*val(g.Usage)
}

// Breakdown is a device's estimated draw. The JSON matches upstream's report.powerEst.
type Breakdown struct {
	Watts        float64 `json:"watts"` // at the wall
	CPU          float64 `json:"cpu"`
	GPU          float64 `json:"gpu"`
	Rest         float64 `json:"rest"`
	Losses       float64 `json:"losses"` // power supply
	CPUMeasured  bool    `json:"cpuMeasured"`
	GPUMeasured  bool    `json:"gpuMeasured"`
	GPUEstimated bool    `json:"gpuEstimated"`
}

// Estimate returns total watts and the breakdown for one report, or nil when the CPU has
// neither a power reading nor a load to estimate from.
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

// FromSample estimates watts for a stored history point that has no measured power
// (recorded before power was tracked), from its CPU load and GPU power. Nil when cpu is.
func FromSample(cpu, gpuPower *float64, hw HW) *float64 {
	if cpu == nil {
		return nil
	}
	p := ProfileOf(hw)
	w := (cpuEstimate(*cpu, p) + val(gpuPower) + p.Rest) / p.Efficiency
	return &w
}

// val is *v, or 0 when v is nil (JavaScript's `v || 0`).
func val(v *float64) float64 {
	if v == nil || math.IsNaN(*v) {
		return 0
	}
	return *v
}

// jsRound is JavaScript's Math.round: halves round toward +Inf, unlike math.Round.
func jsRound(v float64) float64 {
	f := math.Floor(v)
	if v-f >= 0.5 {
		f++
	}
	return f
}

// round1 rounds to one decimal the way upstream does: Math.round(v * 10) / 10.
func round1(v float64) float64 { return jsRound(v*10) / 10 }
