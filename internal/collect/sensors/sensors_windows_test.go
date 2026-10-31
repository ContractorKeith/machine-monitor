//go:build windows

package sensors

import (
	"context"
	"testing"
	"time"
)

func TestReadWindows(t *testing.T) {
	lhm := func(context.Context) []lhmSensor { return recLHM }
	none := func(context.Context) []lhmSensor { return nil }
	zones := func(context.Context) []thermalZone { return []thermalZone{{HighPrecisionTemperature: 3132}} }

	if s := readWindows(context.Background(), lhm, zones); *s.cpuTemp != 71.6 || s.tempSource != nil {
		t.Errorf("with LHM: temp %v source %v", *s.cpuTemp, s.tempSource)
	}
	if s := readWindows(context.Background(), none, zones); *s.cpuTemp != 40.1 || *s.tempSource != "acpi" {
		t.Errorf("ACPI fallback: temp %v source %v", s.cpuTemp, s.tempSource)
	}
}

// TestQueriesLive runs the real WMI queries; neither may fail loudly or hang.
func TestQueriesLive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = queryLHM(ctx)
	_ = queryThermalZones(ctx)
}

func TestQueryLHMStopsWhenDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if rows := queryLHM(ctx); rows != nil {
		t.Errorf("cancelled ctx returned %d rows", len(rows))
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("queryLHM took %v with a cancelled ctx", d)
	}
}
