package tui

import (
	"context"
	"math"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/client"
	"github.com/ContractorKeith/machine-monitor/internal/history"
	"github.com/ContractorKeith/machine-monitor/internal/hub"
	"github.com/ContractorKeith/machine-monitor/internal/power"
	"github.com/ContractorKeith/machine-monitor/internal/report"
)

type localSource struct {
	store *history.Store
	hw    power.HW
	price *power.Settings
	loc   *time.Location
}

func (s localSource) History(_ context.Context, id string, from, to float64, points int) (*history.Result, error) {
	r := s.store.Query(id, from, to, float64(points))
	return &r, nil
}

func (s localSource) Energy(_ context.Context, bounds []float64) (*client.Energy, error) {
	from, to := bounds[0], bounds[len(bounds)-1]
	u := usageOf(s.store.MinutePoints(localID, from, to), bounds, s.hw, s.price, s.loc)
	u.ID = localID
	return &client.Energy{Bounds: bounds, Settings: s.price, Devices: []hub.Usage{u}}, nil
}

func (m model) recordLocal(r *report.Report) {
	if m.multi() || m.opts.History == nil {
		return
	}
	_ = m.opts.History.Add(localID, localSample(m.now().UnixMilli(), r))
}

func localSample(now int64, r *report.Report) history.Point {
	var g report.GPU
	if len(r.GPUs) > 0 {
		g = r.GPUs[0]
	}
	p := history.Point{T: float64(now), CPU: r.CPU.Usage, GPU: g.Usage, CPUTemp: r.CPU.Temp,
		GPUTemp: g.Temp, GPUPower: g.Power, Mem: pctOf(r.Memory.Used, r.Memory.Total)}
	if est := power.Estimate(r); est != nil {
		p.Power = report.Ptr(est.Watts)
	}
	if r.Net != nil {
		p.RX, p.TX = r.Net.RX, r.Net.TX
	}
	if r.DiskIO != nil {
		p.DR, p.DW = r.DiskIO.Read, r.DiskIO.Write
	}
	return p
}

func usageOf(points []history.Point, bounds []float64, hw power.HW, price *power.Settings, loc *time.Location) hub.Usage {
	from, to := bounds[0], bounds[len(bounds)-1]
	edges := make([]time.Time, len(bounds))
	for i, b := range bounds {
		edges[i] = time.UnixMilli(int64(b))
	}
	type acc struct {
		sum  float64
		n    int
		peak float64
	}
	accs := map[string]*acc{}
	add := func(k string, v, peak *float64) {
		if v == nil {
			return
		}
		if peak == nil {
			peak = v
		}
		a := accs[k]
		if a == nil {
			a = &acc{peak: math.Inf(-1)}
			accs[k] = a
		}
		a.sum += *v
		a.n++
		a.peak = math.Max(a.peak, *peak)
	}
	bytes := map[string]float64{"rx": 0, "tx": 0, "dr": 0, "dw": 0}
	var minutes []power.Minute
	for _, p := range points {
		if p.T < from || p.T >= to {
			continue
		}
		minutes = append(minutes, power.Minute{T: time.UnixMilli(int64(p.T)), Power: p.Power, CPU: p.CPU, GPUPower: p.GPUPower})
		w := p.Power
		if w == nil {
			w = power.FromSample(p.CPU, p.GPUPower, hw)
		}
		add("power", w, p.PowerMax)
		add("cpu", p.CPU, p.CPUMax)
		add("gpu", p.GPU, p.GPUMax)
		add("mem", p.Mem, nil)
		add("cpuTemp", p.CPUTemp, p.CPUTempMax)
		add("gpuTemp", p.GPUTemp, p.GPUTempMax)
		bytes["rx"] += num(p.RX) * 60
		bytes["tx"] += num(p.TX) * 60
		bytes["dr"] += num(p.DR) * 60
		bytes["dw"] += num(p.DW) * 60
	}
	if loc == nil {
		loc = time.Local
	}
	u := hub.Usage{Usage: power.Accumulate(minutes, edges, hw, price, loc),
		Stats: map[string]hub.Stat{}, Bytes: map[string]int64{}}
	for k, a := range accs {
		s := hub.Stat{Avg: round1(a.sum / float64(a.n))}
		if !math.IsInf(a.peak, -1) {
			s.Peak = report.Ptr(round1(a.peak))
		}
		u.Stats[k] = s
	}
	for k, v := range bytes {
		u.Bytes[k] = int64(math.Floor(v + 0.5))
	}
	return u
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

func num(v *float64) float64 {
	if v == nil || math.IsNaN(*v) {
		return 0
	}
	return *v
}
