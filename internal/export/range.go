package export

import (
	"strings"
	"time"
)

var Ranges = map[string]time.Duration{
	"1h":  time.Hour,
	"6h":  6 * time.Hour,
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
	"30d": 30 * 24 * time.Hour,
}

const rangeNames = "1h, 6h, 24h, 7d or 30d"

func ParseRange(rng, from, to string, now time.Time, loc *time.Location) (time.Time, time.Time, error) {
	if rng != "" && (from != "" || to != "") {
		return time.Time{}, time.Time{}, sentence("Use either --range or --from/--to, not both.")
	}
	span := Ranges["24h"]
	if rng != "" {
		d, ok := Ranges[strings.ToLower(rng)]
		if !ok {
			return time.Time{}, time.Time{}, sentence("%q is not a range; use %s.", rng, rangeNames)
		}
		span = d
	}
	end := now
	if to != "" {
		t, err := parseTime(to, loc)
		if err != nil {
			return time.Time{}, time.Time{}, sentence("--to %v", err)
		}
		end = t
	}
	start := end.Add(-span)
	if from != "" {
		t, err := parseTime(from, loc)
		if err != nil {
			return time.Time{}, time.Time{}, sentence("--from %v", err)
		}
		start = t
	}
	if !start.Before(end) {
		return time.Time{}, time.Time{}, sentence("The start of the range (%s) must be before its end (%s).",
			start.In(loc).Format(time.RFC3339), end.In(loc).Format(time.RFC3339))
	}
	return start, end, nil
}

func parseTime(s string, loc *time.Location) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation(time.DateOnly, s, loc); err == nil {
		return t, nil
	}
	return time.Time{}, sentence("%q is not a time; use RFC 3339 (2026-10-31T14:00:00Z) or a date (2026-10-31).", s)
}
