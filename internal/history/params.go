// Query-string parsing for GET /api/history/:id.
//
// Ported from Forward-Future/device-monitor server.js @ c3a4c41
// (MIT, © 2026 Matt): https://github.com/Forward-Future/device-monitor

package history

import (
	"errors"
	"math"
	"net/url"
	"strconv"
	"strings"
)

// Ranges are the named spans accepted by ?range=, in ms.
var Ranges = map[string]float64{
	"10m": 10 * 60e3,
	"1h":  60 * 60e3,
	"6h":  6 * 3600e3,
	"24h": 24 * 3600e3,
	"7d":  7 * 86400e3,
	"30d": 30 * 86400e3,
}

// ErrBadRange means from is not before to.
var ErrBadRange = errors.New("bad range")

// Params reads ?range=&from=&to=&points= the way upstream's hub does: to defaults to now;
// from defaults to to minus the range (1h when range is missing or unknown); points defaults
// to 1200 and is clamped to [10, 3000]. Only finite numbers count; anything else is treated
// as missing. The result goes straight to Store.Query.
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

// num parses q[k] as a finite number. "Infinity" or "1e999" would otherwise turn into NaN
// bucket times.
func num(q url.Values, k string) (float64, bool) {
	v := q.Get(k)
	if v == "" {
		return 0, false
	}
	if v = strings.TrimSpace(v); v == "" {
		return 0, true // JavaScript: Number("  ") === 0
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsInf(n, 0) || math.IsNaN(n) {
		return 0, false
	}
	return n, true
}
