package history

import (
	"errors"
	"math"
	"net/url"
	"strconv"
	"strings"
)

var Ranges = map[string]float64{
	"10m": 10 * 60e3,
	"1h":  60 * 60e3,
	"6h":  6 * 3600e3,
	"24h": 24 * 3600e3,
	"7d":  7 * 86400e3,
	"30d": 30 * 86400e3,
}

var ErrBadRange = errors.New("bad range")

func (s *Store) Params(q url.Values) (from, to, points float64, err error) {
	to, ok := num(q, "to")
	if !ok {
		to = s.nowMS()
	}
	span, ok := Ranges[q.Get("range")]
	if !ok {
		span = Ranges["1h"]
	}
	if from, ok = num(q, "from"); !ok {
		from = to - span
	}
	points, ok = num(q, "points")
	if !ok || points == 0 {
		points = 1200
	}
	points = math.Max(10, math.Min(3000, points))
	if from >= to {
		return 0, 0, 0, ErrBadRange
	}
	return from, to, points, nil
}

func num(q url.Values, k string) (float64, bool) {
	v := q.Get(k)
	if v == "" {
		return 0, false
	}
	if v = strings.TrimSpace(v); v == "" {
		return 0, true
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsInf(n, 0) || math.IsNaN(n) {
		return 0, false
	}
	return n, true
}
