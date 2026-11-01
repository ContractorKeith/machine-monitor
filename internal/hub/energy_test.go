package hub

import (
	"encoding/json"
	"fmt"
	"math"
	"testing"
)

func TestClampedRatesDoNotOverflowUsage(t *testing.T) {
	th := newTestHub(t)
	th.postReport("rates", func(m map[string]any) {
		m["net"] = map[string]any{"rx": 1e100, "tx": 1e100}
		m["diskIO"] = map[string]any{"read": 1e100, "write": 1e100}
	})
	var body struct {
		Devices []Usage `json:"devices"`
	}
	ms := th.Hub.now().UnixMilli()
	from := ms / 60000 * 60000
	w := th.keyed("GET", fmt.Sprintf("/api/energy?bounds=%d,%d", from-60000, from+60000), "")
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, d := range body.Devices {
		if d.ID == "rates" {
			if len(d.Bytes) != 4 {
				t.Fatalf("missing byte totals: %v", d.Bytes)
			}
			for _, k := range []string{"rx", "tx", "dr", "dw"} {
				n := d.Bytes[k]
				if n != math.MaxInt64 {
					t.Errorf("%s bytes = %d; want saturation", k, n)
				}
			}
			return
		}
	}
	t.Fatal("missing rates device")
}

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
