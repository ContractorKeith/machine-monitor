package export

import (
	"strings"
	"time"
)

// Ranges are the named spans --range accepts (PLAN §7, History view).
var Ranges = map[string]time.Duration{
	"1h":  time.Hour,
	"6h":  6 * time.Hour,
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
	"30d": 30 * 24 * time.Hour,
}

// rangeNames is Ranges' keys in order, for error messages.
const rangeNames = "1h, 6h, 24h, 7d or 30d"

// ParseRange turns --range, --from and --to into a time span, the way the hub reads
// ?range=&from=&to=: to defaults to now, and from defaults to to minus the range (24h when
// --range is empty). --from and --to take RFC 3339 times or YYYY-MM-DD dates (midnight in
// loc).
func ParseRange(rng, from, to string, now time.Time, loc *time.Location) (time.Time, time.Time, error) {
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

// parseTime reads an RFC 3339 time or a YYYY-MM-DD date.
func parseTime(s string, loc *time.Location) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation(time.DateOnly, s, loc); err == nil {
		return t, nil
	}
	return time.Time{}, sentence("%q is not a time; use RFC 3339 (2026-10-31T14:00:00Z) or a date (2026-10-31).", s)
}
