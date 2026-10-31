//go:build windows

package gpu

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/collect"
)

func init() {
	// WMI is slow (GPUEngine alone can take seconds), so check every 5th sample with room
	// to finish; the collector repeats the last result in between.
	collect.Register(collect.Probe{Name: "gpu/windows", Every: 5, Timeout: 10 * time.Second, Run: func(ctx context.Context) (collect.Apply, error) {
		smi := findTool("nvidia-smi", filepath.Join(os.Getenv("SystemRoot"), "System32", "nvidia-smi.exe"))
		return runWindows(ctx, smi, queryGeneric)
	}})
}

// runWindows uses nvidia-smi when it finds GPUs, else the generic GPU perf counters
// (AMD, Intel), as agent.ps1 does. generic is the WMI query, swapped out in tests.
func runWindows(ctx context.Context, nvidiaSMI string, generic func(context.Context) ([]gpuEngine, []gpuAdapterMemory, []videoController, error)) (collect.Apply, error) {
	gpus, nvErr := queryNvidia(ctx, nvidiaSMI)
	if len(gpus) > 0 {
		return appendGPUs(gpus), nil
	}
	engines, mems, vcs, err := generic(ctx)
	if err != nil {
		if nvErr != nil {
			return nil, nvErr
		}
		return nil, err
	}
	return appendGPUs(genericGPUs(engines, mems, vcs)), nil
}
