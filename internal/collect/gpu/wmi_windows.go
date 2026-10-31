//go:build windows

package gpu

import (
	"context"
	"errors"

	"github.com/yusufpapurcu/wmi"
)

// queryGeneric reads the GPU perf counter and video controller classes the generic GPU
// probe needs.
func queryGeneric(ctx context.Context) ([]gpuEngine, []gpuAdapterMemory, []videoController, error) {
	var engines []gpuEngine
	var mems []gpuAdapterMemory
	var vcs []videoController
	if err := query(ctx, "SELECT Name, UtilizationPercentage FROM Win32_PerfFormattedData_GPUPerformanceCounters_GPUEngine", &engines); err != nil {
		return nil, nil, nil, err
	}
	if err := query(ctx, "SELECT Name, DedicatedUsage FROM Win32_PerfFormattedData_GPUPerformanceCounters_GPUAdapterMemory", &mems); err != nil {
		return nil, nil, nil, err
	}
	// Names are optional: without them GPUs are still reported, as "GPU".
	_ = query(ctx, "SELECT Name, AdapterRAM FROM Win32_VideoController", &vcs)
	return engines, mems, vcs, nil
}

// query runs a WMI query in root\cimv2, giving up when ctx is done. wmi has no
// cancellation, so a hung query finishes in the background into a dst nobody reads.
func query[T any](ctx context.Context, q string, dst *[]T) error {
	var rows []T
	done := make(chan error, 1)
	go func() { done <- wmi.Query(q, &rows) }()
	select {
	case err := <-done:
		var mismatch *wmi.ErrFieldMismatch
		if err != nil && !errors.As(err, &mismatch) {
			return err
		}
		*dst = rows
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
