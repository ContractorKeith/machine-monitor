package power

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseSettings(t *testing.T) {
	ok := []struct {
		name, in string
		want     Settings
	}{
		{"flat", `{"region":"US-FL","rate":0.15,"currency":"USD"}`,
			Settings{Region: "US-FL", Rate: 0.15, Currency: "USD"}},
		{"empty tou is flat", `{"region":"custom","rate":0.2,"currency":"EUR","tou":[]}`,
			Settings{Region: "custom", Rate: 0.2, Currency: "EUR"}},
		{"rounds and sorts", `{"region":"GB","rate":0.123456,"currency":"GBP","tou":[{"from":16.1,"to":21,"rate":0.4},{"from":0,"to":7.4,"rate":0.075}]}`,
			Settings{Region: "GB", Rate: 0.1235, Currency: "GBP", TOU: []Period{{0, 7.5, 0.075}, {16, 21, 0.4}}}},
	}
	for _, c := range ok {
		got, err := ParseSettings([]byte(c.in))
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: ParseSettings = %+v, %v; want %+v", c.name, got, err, c.want)
		}
	}
	bad := map[string]string{
		"not json":       `nope`,
		"null":           `null`,
		"lower region":   `{"region":"us","rate":0.1,"currency":"USD"}`,
		"long region":    `{"region":"USA","rate":0.1,"currency":"USD"}`,
		"zero rate":      `{"region":"US","rate":0,"currency":"USD"}`,
		"rounds to zero": `{"region":"US","rate":0.00004,"currency":"USD"}`,
		"huge rate":      `{"region":"US","rate":1001,"currency":"USD"}`,
		"string rate":    `{"region":"US","rate":"0.1","currency":"USD"}`,
		"no currency":    `{"region":"US","rate":0.1}`,
		"bad currency":   `{"region":"US","rate":0.1,"currency":"usd"}`,
		"tou not array":  `{"region":"US","rate":0.1,"currency":"USD","tou":{}}`,
		"tou null entry": `{"region":"US","rate":0.1,"currency":"USD","tou":[null]}`,
		"tou no from":    `{"region":"US","rate":0.1,"currency":"USD","tou":[{"to":5,"rate":0.2}]}`,
		"tou from >= to": `{"region":"US","rate":0.1,"currency":"USD","tou":[{"from":5,"to":5,"rate":0.2}]}`,
		"tou past 24":    `{"region":"US","rate":0.1,"currency":"USD","tou":[{"from":5,"to":25,"rate":0.2}]}`,
		"tou bad rate":   `{"region":"US","rate":0.1,"currency":"USD","tou":[{"from":5,"to":6,"rate":-1}]}`,
		"tou too many":   `{"region":"US","rate":0.1,"currency":"USD","tou":[` + repeat(`{"from":1,"to":2,"rate":1},`, 12) + `{"from":1,"to":2,"rate":1}]}`,
	}
	for name, in := range bad {
		if s, err := ParseSettings([]byte(in)); err == nil {
			t.Errorf("%s: ParseSettings accepted %+v", name, s)
		}
	}
}

func repeat(s string, n int) (out string) {
	for range n {
		out += s
	}
	return out
}

func TestRateAt(t *testing.T) {
	s := Settings{Rate: 0.10, TOU: []Period{{0, 7, 0.05}, {16, 21, 0.40}, {20, 22, 0.99}}}
	day := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		h, m int
		want float64
	}{
		{0, 0, 0.05}, {6, 59, 0.05}, {7, 0, 0.10}, {15, 59, 0.10}, {16, 0, 0.40},
		{20, 30, 0.40}, // overlapping periods: the first (sorted) wins
		{21, 0, 0.99}, {22, 0, 0.10}, {23, 59, 0.10},
	} {
		if got := s.RateAt(day.Add(time.Duration(c.h)*time.Hour + time.Duration(c.m)*time.Minute)); got != c.want {
			t.Errorf("RateAt(%02d:%02d) = %v, want %v", c.h, c.m, got, c.want)
		}
	}
	if got := (Settings{Rate: 0.2}).RateAt(day); got != 0.2 {
		t.Errorf("flat RateAt = %v", got)
	}
}

func TestDayRateAndRange(t *testing.T) {
	s := Settings{Rate: 0.10, TOU: []Period{{16, 21, 0.40}}}
	// 19 h × 0.10 + 5 h × 0.40 = 3.9 over 24 h.
	if got := s.DayRate(); !near(got, 3.9/24) {
		t.Errorf("DayRate = %v, want %v", got, 3.9/24)
	}
	if lo, hi := s.RateRange(); lo != 0.10 || hi != 0.40 {
		t.Errorf("RateRange = %v, %v", lo, hi)
	}
	if got := (Settings{Rate: 0.25}).DayRate(); got != 0.25 {
		t.Errorf("flat DayRate = %v", got)
	}
}

func near(a, b float64) bool { return a-b < 1e-12 && b-a < 1e-12 }

// Overlapping time-of-use periods are refused with a sentence; touching ones are fine (B9).
func TestParseSettingsRefusesOverlap(t *testing.T) {
	const head = `{"region":"US","rate":0.1,"currency":"USD","tou":`
	_, err := ParseSettings([]byte(head + `[{"from":16,"to":21,"rate":0.35},{"from":18,"to":23,"rate":0.5}]}`))
	if !errors.Is(err, ErrOverlappingPeriods) || !strings.Contains(err.Error(), "must not overlap") {
		t.Fatalf("overlap: %v", err)
	}
	if _, err := ParseSettings([]byte(head + `[{"from":16,"to":21,"rate":0.35},{"from":0,"to":16,"rate":0.08}]}`)); err != nil {
		t.Fatalf("touching periods refused: %v", err)
	}
}

// LoadSettings keeps the flat rate and currency of a price whose periods overlap.
func TestLoadSettingsDropsOverlappingPeriods(t *testing.T) {
	s, err := LoadSettings([]byte(`{"region":"US","rate":0.2,"currency":"USD","tou":[` +
		`{"from":16,"to":21,"rate":0.4},{"from":18,"to":23,"rate":0.5}]}`))
	if !errors.Is(err, ErrOverlappingPeriods) || s.Rate != 0.2 || s.Currency != "USD" || len(s.TOU) != 0 {
		t.Fatalf("got %+v, %v", s, err)
	}
	if _, err := LoadSettings([]byte(`{"region":"US","rate":-1,"currency":"USD"}`)); !errors.Is(err, ErrInvalidSettings) {
		t.Fatalf("invalid rate: %v", err)
	}
}
