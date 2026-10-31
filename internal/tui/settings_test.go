package tui

import (
	"testing"

	"github.com/ContractorKeith/machine-monitor/internal/power"
)

// The price form names an overlap in a sentence (N12).
func TestPriceProblemNamesOverlap(t *testing.T) {
	s := power.Settings{Region: "custom", Rate: 0.2, Currency: "USD",
		TOU: []power.Period{{From: 18, To: 23, Rate: 0.5}, {From: 16, To: 21, Rate: 0.4}}}
	want := "The periods 16-21 and 18-23 overlap. Give each hour of the day to one period."
	if got := priceProblem(s); got != want {
		t.Fatalf("priceProblem = %q", got)
	}
	s.TOU[0].From = 21
	if got := priceProblem(s); got != "" {
		t.Fatalf("touching periods: %q", got)
	}
}
