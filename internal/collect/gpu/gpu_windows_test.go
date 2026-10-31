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

// TestQueryVideoControllersLive checks the WMI class and property names against a real
// machine: every Windows install, CI's included, has at least one video controller.
func TestQueryVideoControllersLive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var vcs []videoController
	if err := queryVideoControllers(ctx, &vcs); err != nil {
		t.Fatal(err)
	}
	if len(vcs) == 0 {
		t.Fatal("Win32_VideoController returned no rows")
	}
	named := false
	for _, vc := range vcs {
		t.Logf("video controller %q, AdapterRAM %d", vc.Name, vc.AdapterRAM)
		named = named || vc.Name != ""
	}
	if !named {
		t.Errorf("every row decoded with an empty Name: %+v", vcs)
	}
}

// TestQueryGenericLive runs the real GPU perf counter queries. CI VMs may lack the
// counters, so an error skips, but it must come back within the deadline.
func TestQueryGenericLive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	engines, mems, _, err := queryGeneric(ctx)
	if err != nil {
		t.Skipf("GPU perf counters unavailable: %v", err)
	}
	t.Logf("%d GPU engines, %d adapter memory rows", len(engines), len(mems))
}
