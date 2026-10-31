package hub

import "testing"

// Stats avg/peak are upstream's toFixed(1), which rounds the exact binary value (B15).
func TestRound1MatchesToFixed(t *testing.T) {
	for in, want := range map[float64]float64{
		0.15: 0.1, 0.35: 0.3, 1.45: 1.4, -0.15: -0.1, 0.25: 0.3, -0.25: -0.3, 0.05: 0.1,
		-0.04: 0, 2.5: 2.5, 99.95: 100, 12345.678: 12345.7,
	} {
		if got := round1(in); got != want {
			t.Errorf("round1(%v) = %v, want %v", in, got, want)
		}
	}
}
