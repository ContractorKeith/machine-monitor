package ai

// GPU process memory on Windows, ported from agent/agent.ps1 (Get-GpuProcessMemory) at
// c3a4c41. MIT License, Copyright (c) 2026 Matt.

import (
	"context"
	"regexp"
	"strconv"
)

// Windows only; nil elsewhere (win_windows.go sets them).
var (
	// winGPUMem maps pid to dedicated GPU memory in bytes from the GPUProcessMemory perf
	// counters; nil when the query fails.
	winGPUMem func(ctx context.Context) map[int]float64
	// winListenPID is the pid listening on a local TCP port, 0 when none.
	winListenPID func(ctx context.Context, port string) int
)

// gpuProcMemRow is one Win32_PerfFormattedData_GPUPerformanceCounters_GPUProcessMemory row.
type gpuProcMemRow struct {
	Name           string // "pid_1234_luid_0x00000000_0x0000D1A4_phys_0"
	DedicatedUsage uint64
}

var pidNameRe = regexp.MustCompile(`^pid_(\d+)_`)

// parseGPUProcMem sums dedicated usage per pid across adapters.
func parseGPUProcMem(rows []gpuProcMemRow) map[int]float64 {
	out := map[int]float64{}
	for _, r := range rows {
		m := pidNameRe.FindStringSubmatch(r.Name)
		if m == nil {
			continue
		}
		if pid, err := strconv.Atoi(m[1]); err == nil && pid > 0 {
			out[pid] += float64(r.DedicatedUsage)
		}
	}
	return out
}

// memOf totals mem for pids, nil when that is 0 (agent.ps1 sends null then).
func memOf(mem map[int]float64, pids ...int) *float64 {
	var total float64
	for _, p := range pids {
		total += mem[p]
	}
	if total > 0 {
		return &total
	}
	return nil
}
