package power

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"time"
)

type Settings struct {
	Region   string   `json:"region"`
	Rate     float64  `json:"rate"`
	Currency string   `json:"currency"`
	TOU      []Period `json:"tou,omitempty"`
}

type Period struct {
	From float64 `json:"from"`
	To   float64 `json:"to"`
	Rate float64 `json:"rate"`
}

var (
	regionRE   = regexp.MustCompile(`^([A-Z]{2}(-[A-Z]{2})?|custom)$`)
	currencyRE = regexp.MustCompile(`^[A-Z]{3}$`)
)

var ErrInvalidSettings = errors.New("invalid energy settings")

var ErrOverlappingPeriods = fmt.Errorf("%w: time-of-use periods must not overlap; give each hour of the day to one period", ErrInvalidSettings)

func ParseSettings(data []byte) (Settings, error) {
	s, err := LoadSettings(data)
	if err != nil {
		return Settings{}, err
	}
	return s, nil
}

func LoadSettings(data []byte) (Settings, error) {
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
	for i := 1; i < len(s.TOU); i++ {
		if s.TOU[i].From < s.TOU[i-1].To {
			s.TOU = nil
			return s, ErrOverlappingPeriods
		}
	}
	return s, nil
}

func validRate(v *float64) (float64, bool) {
	if v == nil || !(*v > 0 && *v <= 1000) {
		return 0, false
	}
	r := jsRound(*v*1e4) / 1e4
	return r, r != 0
}

func validHour(v *float64) (float64, bool) {
	if v == nil || !(*v >= 0 && *v <= 24) {
		return 0, false
	}
	return jsRound(*v*4) / 4, true
}

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

func (s Settings) DayRate() float64 {
	var extra float64
	for _, p := range s.TOU {
		extra += (p.To - p.From) * (p.Rate - s.Rate)
	}
	return s.Rate + extra/24
}

func (s Settings) RateRange() (lo, hi float64) {
	lo, hi = s.Rate, s.Rate
	for _, p := range s.TOU {
		lo, hi = math.Min(lo, p.Rate), math.Max(hi, p.Rate)
	}
	return lo, hi
}
