package hub

// Energy and usage totals (GET /api/energy) and the power/cost source for alerts.
//
// Ported from Forward-Future/device-monitor server.js (usageSummary, costToday) @ c3a4c41
// (MIT, © 2026 Matt): https://github.com/Forward-Future/device-monitor

import (
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/alerts"
	"github.com/ContractorKeith/machine-monitor/internal/power"
	"github.com/ContractorKeith/machine-monitor/internal/report"
)

// Bounds limits for GET /api/energy?bounds=, as upstream.
const (
	maxBounds = 800
	maxSpanMS = 32 * 86400e3
	maxAbsMS  = 9e12 // keeps ms → ns (×1e6) inside int64, which ends at about 9.22e18
)

// Usage is one device's energy and usage totals between bounds.
type Usage struct {
	ID string `json:"id"`
	power.Usage
	Stats map[string]Stat  `json:"stats"` // avg and peak per metric, only metrics with data
	Bytes map[string]int64 `json:"bytes"` // rx, tx, dr, dw totals
}

// Stat is an average and peak, both to 1 decimal.
type Stat struct {
	Avg  float64  `json:"avg"`
	Peak *float64 `json:"peak"`
}

// parseBounds reads ?bounds=: 2–800 ascending finite ms timestamps spanning at most 32 days.
func parseBounds(s string) ([]float64, bool) {
	parts := strings.Split(s, ",")
	if len(parts) < 2 || len(parts) > maxBounds {
		return nil, false
	}
	out := make([]float64, len(parts))
	for i, p := range parts {
		p = strings.TrimSpace(p)
		v := 0.0 // JavaScript: Number("") === 0
		if p != "" {
			var err error
			if v, err = strconv.ParseFloat(p, 64); err != nil {
				return nil, false
			}
		}
		if math.IsNaN(v) || math.Abs(v) >= maxAbsMS || (i > 0 && v <= out[i-1]) {
			return nil, false
		}
		out[i] = v
	}
	return out, out[len(out)-1]-out[0] <= maxSpanMS
}

// msTime converts Unix ms (maybe fractional) to a time. Whole ms and the fraction convert
// separately: ms*1e6 in float64 would lose the last millisecond near maxAbsMS.
func msTime(ms float64) time.Time {
	whole := math.Floor(ms)
	return time.UnixMilli(int64(whole)).Add(time.Duration((ms - whole) * 1e6))
}

// usage totals one device over bounds. hw is its last reported hardware; price may be nil.
func (h *Hub) usage(id string, bounds []float64, hw power.HW, price *power.Settings) Usage {
	from, to := bounds[0], bounds[len(bounds)-1]
	edges := make([]time.Time, len(bounds))
	for i, b := range bounds {
		edges[i] = msTime(b)
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
		if peak == nil { // rows from before upstream stored peaks
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
	for _, p := range h.history.MinutePoints(id, from, to) {
		if p.T < from || p.T >= to {
			continue
		}
		minutes = append(minutes, power.Minute{T: msTime(p.T), Power: p.Power, CPU: p.CPU, GPUPower: p.GPUPower})
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
		bytes["rx"] += val(p.RX) * 60
		bytes["tx"] += val(p.TX) * 60
		bytes["dr"] += val(p.DR) * 60
		bytes["dw"] += val(p.DW) * 60
	}
	u := Usage{ID: id, Usage: power.Accumulate(minutes, edges, hw, price, h.opt.Location),
		Stats: map[string]Stat{}, Bytes: map[string]int64{}}
	for k, a := range accs {
		s := Stat{Avg: round1(a.sum / float64(a.n))}
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

// round1 is upstream's +v.toFixed(1): v's exact binary value rounded half up (0.15 is
// 0.1499999… so it gives 0.1, and 0.25 gives 0.3), with the sign applied afterwards.
func round1(v float64) float64 {
	s := strconv.FormatFloat(math.Abs(v), 'f', 30, 64)
	dot := strings.IndexByte(s, '.')
	tenths, err := strconv.ParseInt(s[:dot]+s[dot+1:dot+2], 10, 64)
	if err != nil { // beyond int64 tenths: no fraction left to round
		return v
	}
	if s[dot+2] >= '5' {
		tenths++
	}
	if tenths == 0 {
		return 0 // JSON would print -0
	}
	r := float64(tenths) / 10
	if v < 0 {
		r = -r
	}
	return r
}

// usageAll totals the devices only selects (a device id, or alerts.AllDevices) over bounds.
func (h *Hub) usageAll(bounds []float64, only string) (*power.Settings, []Usage) {
	type dev struct {
		id string
		hw power.HW
	}
	h.mu.Lock()
	price := h.cfg.Energy
	var devs []dev
	for _, id := range h.sortedIDs() {
		if only == alerts.AllDevices || only == id {
			devs = append(devs, dev{id, h.registry[id].powerHW()})
		}
	}
	h.mu.Unlock()
	out := []Usage{}
	for _, d := range devs {
		out = append(out, h.usage(d.id, bounds, d.hw, price))
	}
	return price, out
}

// energySource gives the alerts engine power estimates and today's cost.
type energySource struct{ h *Hub }

func (e energySource) Watts(_ string, r *report.Report) *float64 {
	if b := power.Estimate(r); b != nil {
		return &b.Watts
	}
	return nil
}

func (e energySource) CostToday(device string) (alerts.Cost, bool) {
	now := e.h.now().In(e.h.opt.Location)
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	bounds := []float64{float64(start.UnixMilli()), float64(now.UnixMilli() + 1)}
	price, list := e.h.usageAll(bounds, device)
	if price == nil {
		return alerts.Cost{}, false
	}
	var cost float64
	for _, u := range list {
		if u.Cost != nil {
			cost += *u.Cost
		}
	}
	return alerts.Cost{Amount: cost, Currency: price.Currency}, true
}

// settingsRefusal is the sentence PUT /api/energy/settings answers a refused price with.
func settingsRefusal(err error) string {
	if errors.Is(err, power.ErrOverlappingPeriods) {
		return "The time-of-use periods overlap. Give each hour of the day to one period."
	}
	return "invalid settings"
}
