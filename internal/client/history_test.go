package client

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/config"
)

// TestHistoryAndEnergy reads a real hub's history and energy for a device that reported.
func TestHistoryAndEnergy(t *testing.T) {
	th := newTestHub(t, config.At(t.TempDir()), "")
	th.report("dev-a", nil)
	c := th.client(th.h.Token())
	ctx := context.Background()

	now := float64(time.Now().UnixMilli())
	r, err := c.History(ctx, "dev-a", now-3600e3, now+1000, 300)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(r.Points) != 1 || r.Points[0].CPU == nil || r.Resolution < 2000 || r.From != now-3600e3 {
		t.Errorf("History = %+v", r)
	}

	e, err := c.Energy(ctx, []float64{now - 3600e3, now - 1800e3, now + 60e3})
	if err != nil {
		t.Fatalf("Energy: %v", err)
	}
	if len(e.Bounds) != 3 || len(e.Devices) != 1 || e.Devices[0].ID != "dev-a" || len(e.Devices[0].Buckets) != 2 ||
		e.Devices[0].Minutes != 1 || e.Settings != nil {
		t.Errorf("Energy = %+v", e)
	}

	var se *StatusError
	if _, err := c.Energy(ctx, []float64{now}); !errors.As(err, &se) || se.Code != 400 {
		t.Errorf("one bound: %v", err)
	}
}
