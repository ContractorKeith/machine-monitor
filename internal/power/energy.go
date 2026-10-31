package power

import (
	"strconv"
	"time"
)

// Minute is one 1-minute history point, as far as energy is concerned.
type Minute struct {
	T        time.Time
	Power    *float64 // average watts at the wall over the minute, nil if not recorded
	CPU      *float64 // average CPU load, for estimating when Power is nil
	GPUPower *float64 // average measured GPU watts
}

// Usage is the energy a device used between two bounds, split into buckets.
type Usage struct {
	Wh               float64   `json:"wh"`               // rounded to whole Wh
	Buckets          []float64 `json:"buckets"`          // Wh per bucket, 1 decimal
	Cost             *float64  `json:"cost"`             // nil without a price; 4 decimals
	CostBuckets      []float64 `json:"costBuckets"`      // nil without a price
	Minutes          int       `json:"minutes"`          // points in range, with or without power
	EstimatedMinutes int       `json:"estimatedMinutes"` // points whose power came from FromSample
	FirstData        *int64    `json:"firstData"`        // unix ms of the first point in range
}

// Accumulate totals energy and cost over points, which must be sorted by time. bounds are
// bucket edges (e.g. local midnights) and must ascend; points outside [first, last) are
// skipped. Each point counts as exactly one minute at its average power, so a gap in the
// history (device off or unreachable) adds nothing rather than being interpolated. Points
// with no Power are estimated from CPU load with hw. Cost uses the rate in force at each
// point's time in loc; price may be nil. Ports usageSummary's energy part from server.js.
func Accumulate(points []Minute, bounds []time.Time, hw HW, price *Settings, loc *time.Location) Usage {
	var u Usage
	if len(bounds) < 2 {
		return u
	}
	buckets := make([]float64, len(bounds)-1)
	costBuckets := make([]float64, len(bounds)-1)
	from, to := bounds[0], bounds[len(bounds)-1]
	var wh, cost float64
	b := 0
	for _, p := range points {
		if p.T.Before(from) || !p.T.Before(to) {
			continue
		}
		for !p.T.Before(bounds[b+1]) {
			b++
		}
		u.Minutes++
		if u.FirstData == nil {
			ms := p.T.UnixMilli()
			u.FirstData = &ms
		}
		w := p.Power
		if w == nil {
			if w = FromSample(p.CPU, p.GPUPower, hw); w != nil {
				u.EstimatedMinutes++
			}
		}
		if w == nil {
			continue
		}
		buckets[b] += *w / 60
		wh += *w / 60
		if price != nil {
			c := *w / 60 / 1000 * price.RateAt(p.T.In(loc))
			costBuckets[b] += c
			cost += c
		}
	}
	u.Wh = jsRound(wh)
	u.Buckets = buckets
	for i := range buckets {
		buckets[i] = round1(buckets[i])
	}
	if price != nil {
		c := fixed4(cost)
		u.Cost = &c
		u.CostBuckets = costBuckets
		for i := range costBuckets {
			costBuckets[i] = fixed4(costBuckets[i])
		}
	}
	return u
}

// fixed4 is upstream's +v.toFixed(4).
func fixed4(v float64) float64 {
	f, _ := strconv.ParseFloat(strconv.FormatFloat(v, 'f', 4, 64), 64)
	return f
}
