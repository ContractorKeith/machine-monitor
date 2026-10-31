//go:build windows

package gpu

import (
	"context"

	"github.com/ContractorKeith/machine-monitor/internal/collect/wmiq"
)

// queryGeneric reads the GPU perf counter and video controller classes the generic GPU
// probe needs, through the shared WMI worker.
func queryGeneric(ctx context.Context) ([]gpuEngine, []gpuAdapterMemory, []videoController, error) {
	var engines []gpuEngine
	var mems []gpuAdapterMemory
	var vcs []videoController
	if err := wmiq.Query(ctx, "SELECT Name, UtilizationPercentage FROM Win32_PerfFormattedData_GPUPerformanceCounters_GPUEngine", `root\cimv2`, &engines); err != nil {
		return nil, nil, nil, err
	}
	if err := wmiq.Query(ctx, "SELECT Name, DedicatedUsage FROM Win32_PerfFormattedData_GPUPerformanceCounters_GPUAdapterMemory", `root\cimv2`, &mems); err != nil {
		return nil, nil, nil, err
	}
	// Names are optional: without them GPUs are still reported, as "GPU".
	_ = queryVideoControllers(ctx, &vcs)
	return engines, mems, vcs, nil
}

func queryVideoControllers(ctx context.Context, dst *[]videoController) error {
	return wmiq.Query(ctx, "SELECT Name, AdapterRAM FROM Win32_VideoController", `root\cimv2`, dst)
}
