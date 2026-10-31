//go:build windows

package gpu

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/report"
)

func TestRunWindowsGenericFallback(t *testing.T) {
	recorded := func(context.Context) ([]gpuEngine, []gpuAdapterMemory, []videoController, error) {
		return recEngines, recMems, recControllers, nil
	}
	apply, err := runWindows(context.Background(), "", recorded)
	if err != nil {
		t.Fatal(err)
	}
	r := &report.Report{}
	apply(r)
	if len(r.GPUs) != 2 || *r.GPUs[0].Name != "AMD Radeon RX 7800 XT" || *r.GPUs[0].Vendor != "generic" {
		t.Errorf("GPUs = %s", asJSON(t, r.GPUs))
	}

	failing := func(context.Context) ([]gpuEngine, []gpuAdapterMemory, []videoController, error) {
		return nil, nil, nil, errors.New("wmi down")
	}
	if apply, err := runWindows(context.Background(), "", failing); apply != nil || err == nil {
		t.Errorf("WMI failure = %v, %v; want nil, error", apply != nil, err)
	}
}

// TestQueryGenericLive runs the real WMI queries; any machine with a GPU driver answers.
func TestQueryGenericLive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, _, _, err := queryGeneric(ctx); err != nil {
		t.Skipf("GPU perf counters unavailable: %v", err)
	}
}
