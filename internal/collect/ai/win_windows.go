//go:build windows

package ai

// Windows GPU process memory, port listener and command line, as agent.ps1
// (Get-GpuProcessMemory, GetActiveTcpListeners, Get-NetTCPConnection and Win32_Process) at
// c3a4c41. MIT License, Copyright (c) 2026 Matt.

import (
	"context"
	"fmt"
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
	winListener = func(ctx context.Context, port string) (bool, int) {
		p, err := strconv.ParseUint(port, 10, 16)
		if err != nil {
			return false, 0
		}
		conns, err := net.ConnectionsWithContext(ctx, "tcp")
		if err != nil {
			return false, 0
		}
		listening := false
		for _, c := range conns {
			if c.Status == "LISTEN" && c.Laddr.Port == uint32(p) {
				if c.Pid > 0 {
					return true, int(c.Pid)
				}
				listening = true // a listener whose owner Windows won't name
			}
		}
		return listening, 0
	}
	winCmdline = func(ctx context.Context, pid int) string {
		var rows []struct{ CommandLine string }
		q := fmt.Sprintf("SELECT CommandLine FROM Win32_Process WHERE ProcessId = %d", pid)
		if wmiq.Query(ctx, q, `root\cimv2`, &rows) != nil || len(rows) == 0 {
			return ""
		}
		return rows[0].CommandLine
	}
}
