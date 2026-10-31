//go:build windows

package gpu

import (
	"context"
	"os"
	"path/filepath"

	"github.com/ContractorKeith/machine-monitor/internal/collect"
)

func init() {
	collect.Register(collect.Probe{Name: "gpu/windows", Run: func(ctx context.Context) (collect.Apply, error) {
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
