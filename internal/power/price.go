package power

import (
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"sort"
	"time"
)

// Settings is the electricity price the hub charges energy at (config.json "energy").
type Settings struct {
	Region   string   `json:"region"`   // a Region code or "custom"
	Rate     float64  `json:"rate"`     // per kWh; with TOU, the rate for every other hour
	Currency string   `json:"currency"` // ISO 4217
	TOU      []Period `json:"tou,omitempty"`
}

// Period is one time-of-use window in local hours, From inclusive, To exclusive.
type Period struct {
	From float64 `json:"from"` // 0–24, quarter hours
	To   float64 `json:"to"`
	Rate float64 `json:"rate"`
}

var (
	regionRE   = regexp.MustCompile(`^([A-Z]{2}(-[A-Z]{2})?|custom)$`)
	currencyRE = regexp.MustCompile(`^[A-Z]{3}$`)
)

// ErrInvalidSettings is returned by ParseSettings for anything upstream would reject.
var ErrInvalidSettings = errors.New("invalid energy settings")

// ParseSettings validates untrusted settings JSON (PUT /api/energy/settings), rounding rates
// to 4 decimals and hours to quarter hours and sorting periods by start, as upstream's
// validEnergySettings does. At most 12 periods; each needs From < To.
func ParseSettings(data []byte) (Settings, error) {
	type rawPeriod struct{ From, To, Rate *float64 }
	var raw struct {
		Region   *string
		Rate     *float64
		Currency *string
		TOU      []*rawPeriod
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return Settings{}, ErrInvalidSettings
	}
	rate, ok := validRate(raw.Rate)
	if raw.Region == nil || !regionRE.MatchString(*raw.Region) || !ok ||
		raw.Currency == nil || !currencyRE.MatchString(*raw.Currency) || len(raw.TOU) > 12 {
		return Settings{}, ErrInvalidSettings
	}
	s := Settings{Region: *raw.Region, Rate: rate, Currency: *raw.Currency}
	for _, p := range raw.TOU {
		if p == nil {
			return Settings{}, ErrInvalidSettings
		}
		from, okFrom := validHour(p.From)
		to, okTo := validHour(p.To)
		r, okRate := validRate(p.Rate)
		if !okFrom || !okTo || !okRate || from >= to {
			return Settings{}, ErrInvalidSettings
		}
		s.TOU = append(s.TOU, Period{From: from, To: to, Rate: r})
	}
	sort.SliceStable(s.TOU, func(i, j int) bool { return s.TOU[i].From < s.TOU[j].From })
	return s, nil
}

// validRate accepts a price in (0, 1000], rounded to 4 decimals. Like upstream, a price
// that rounds to 0 is refused.
func validRate(v *float64) (float64, bool) {
	if v == nil || !(*v > 0 && *v <= 1000) {
		return 0, false
	}
	r := jsRound(*v*1e4) / 1e4
	return r, r != 0
}

// validHour accepts an hour in [0, 24], rounded to the quarter hour.
func validHour(v *float64) (float64, bool) {
	if v == nil || !(*v >= 0 && *v <= 24) {
		return 0, false
	}
	return jsRound(*v*4) / 4, true
}

// RateAt is the price per kWh in force at t, read in t's location (upstream uses the hub's
// local time). The first matching period wins; hours outside every period pay Rate.
func (s Settings) RateAt(t time.Time) float64 {
	if len(s.TOU) == 0 {
		return s.Rate
	}
	h := float64(t.Hour()) + float64(t.Minute())/60
	for _, p := range s.TOU {
		if h >= p.From && h < p.To {
			return p.Rate
		}
	}
	return s.Rate
}

// DayRate is the average price over a day, weighting each period by its hours.
func (s Settings) DayRate() float64 {
	var extra float64
	for _, p := range s.TOU {
		extra += (p.To - p.From) * (p.Rate - s.Rate)
	}
	return s.Rate + extra/24
}

// RateRange is the cheapest and dearest rate, for "$0.120–$0.450/kWh by time of day".
func (s Settings) RateRange() (lo, hi float64) {
	lo, hi = s.Rate, s.Rate
	for _, p := range s.TOU {
		lo, hi = math.Min(lo, p.Rate), math.Max(hi, p.Rate)
	}
	return lo, hi
}
