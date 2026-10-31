//go:build windows

package ai

// Windows GPU process memory and port owner, as agent.ps1 (Get-GpuProcessMemory and
// Get-NetTCPConnection) at c3a4c41. MIT License, Copyright (c) 2026 Matt.

import (
	"context"
	"strconv"

	"github.com/shirou/gopsutil/v4/net"

	"github.com/ContractorKeith/machine-monitor/internal/collect/wmiq"
)

func init() {
	winGPUMem = func(ctx context.Context) map[int]float64 {
		var rows []gpuProcMemRow
		if wmiq.Query(ctx, "SELECT Name, DedicatedUsage FROM Win32_PerfFormattedData_GPUPerformanceCounters_GPUProcessMemory", `root\cimv2`, &rows) != nil {
			return nil
		}
		return parseGPUProcMem(rows)
	}
	winListenPID = func(ctx context.Context, port string) int {
		p, err := strconv.ParseUint(port, 10, 16)
		if err != nil {
			return 0
		}
		conns, err := net.ConnectionsWithContext(ctx, "tcp")
		if err != nil {
			return 0
		}
		for _, c := range conns {
			if c.Status == "LISTEN" && c.Laddr.Port == uint32(p) && c.Pid > 0 {
				return int(c.Pid)
			}
		}
		return 0
	}
}
