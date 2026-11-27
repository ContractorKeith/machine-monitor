package power

import (
	"math"
	"math/big"
	"time"
)

type Minute struct {
	T        time.Time
	Power    *float64
	CPU      *float64
	GPUPower *float64
}

type Usage struct {
	Wh               float64   `json:"wh"`
	Buckets          []float64 `json:"buckets"`
	Cost             *float64  `json:"cost"`
	CostBuckets      []float64 `json:"costBuckets"`
	Minutes          int       `json:"minutes"`
	EstimatedMinutes int       `json:"estimatedMinutes"`
	FirstData        *int64    `json:"firstData"`
}

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

func fixed4(v float64) float64 { return ToFixed(v, 4) }

func ToFixed(v float64, digits int) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return v
	}
	scale := new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(digits)), nil))
	x := new(big.Rat).SetFloat64(math.Abs(v))
	x.Mul(x, scale)
	x.Add(x, big.NewRat(1, 2))
	n := new(big.Int).Div(x.Num(), x.Denom())
	r, _ := new(big.Rat).SetFrac(n, scale.Num()).Float64()
	if r == 0 {
		return 0
	}
	return math.Copysign(r, v)
}
