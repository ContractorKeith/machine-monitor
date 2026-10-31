// Bucket aggregation and same-minute merging.
//
// Ported from Forward-Future/device-monitor history.js @ c3a4c41
// (MIT, © 2026 Matt): https://github.com/Forward-Future/device-monitor

package history

import (
	"math"
	"strconv"
)

// aggregate averages points into buckets width ms wide, aligned to start. Each bucket keeps
// the peak of the peaked fields, taken from a point's *Max field when it has one (so minutes
// re-aggregate correctly) and its value otherwise. points must be sorted by T.
func aggregate(points []Point, width, start float64) []Point {
	var out []Point
	var cur Point
	var sum [11]float64
	var n [11]int
	open := false
	flush := func() {
		if !open {
			return
		}
		f := cur.fields()
		for i := range f {
			*f[i] = nil
			if n[i] > 0 {
				*f[i] = ptr(round2(sum[i] / float64(n[i])))
			}
		}
		out = append(out, cur)
	}
	for _, s := range points {
		b := start + math.Floor((s.T-start)/width)*width
		if !open || cur.T != b {
			flush()
			cur, sum, n, open = Point{T: b}, [11]float64{}, [11]int{}, true
		}
		for i, v := range s.fields() {
			if *v != nil {
				sum[i] += **v
				n[i]++
			}
		}
		for i, pk := range s.peaks() {
			v := *pk[0]
			if v == nil {
				v = *pk[1]
			}
			if m := cur.peaks()[i][0]; v != nil && (*m == nil || *v > **m) {
				*m = v
			}
		}
	}
	flush()
	return out
}

// merge combines two points for the same minute (e.g. one flushed at hub shutdown, one after
// restart), weighting averages by their sample counts. Rows written before counts were stored
// count as 1.
func merge(a, b Point) Point {
	na, nb := max(a.N, 1), max(b.N, 1)
	p := Point{T: a.T, N: na + nb}
	pf, af, bf := p.fields(), a.fields(), b.fields()
	for i := range pf {
		x, y := *af[i], *bf[i]
		switch {
		case x == nil:
			*pf[i] = y
		case y == nil:
			*pf[i] = x
		default:
			*pf[i] = ptr(round2((*x*float64(na) + *y*float64(nb)) / float64(na+nb)))
		}
	}
	pp, ap, bp := p.peaks(), a.peaks(), b.peaks()
	for i := range pp {
		x, y := *ap[i][0], *bp[i][0]
		switch {
		case x == nil:
			*pp[i][0] = y
		case y == nil:
			*pp[i][0] = x
		default:
			*pp[i][0] = ptr(math.Max(*x, *y))
		}
	}
	return p
}

// round2 is JavaScript's +x.toFixed(2): round to 2 decimals, exact ties away from zero.
// strconv rounds the exact binary value correctly but breaks exact ties to even, and a tie
// at 2 decimals is exactly an odd multiple of 1/8 (0.125, 2.375, ...), so those are handled
// first; x*100 and x*100+0.5 are exact for them.
func round2(x float64) float64 {
	a := math.Abs(x)
	var r float64
	if e := a * 8; e == math.Trunc(e) && math.Mod(e, 2) == 1 && a < 1<<50 {
		r = math.Floor(a*100+0.5) / 100
	} else {
		r, _ = strconv.ParseFloat(strconv.FormatFloat(a, 'f', 2, 64), 64)
	}
	if r == 0 {
		return 0 // toFixed never yields -0
	}
	return math.Copysign(r, x)
}

func ptr(v float64) *float64 { return &v }
