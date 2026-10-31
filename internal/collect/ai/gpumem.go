package ai

// GPU process memory on Windows, ported from agent/agent.ps1 (Get-GpuProcessMemory) at
// c3a4c41. MIT License, Copyright (c) 2026 Matt.

import (
	"context"
	"regexp"
	"strconv"
	"strings"
)

// Windows only; nil elsewhere (win_windows.go sets them).
var (
	// winGPUMem maps pid to dedicated GPU memory in bytes from the GPUProcessMemory perf
	// counters; nil when the query fails.
	winGPUMem func(ctx context.Context) map[int]float64
	// winListener reports whether a local TCP port has a listener, and its pid (0 when
	// Windows won't say).
	winListener func(ctx context.Context, port string) (listening bool, pid int)
	// winCmdline is a process's command line, "" when unknown.
	winCmdline func(ctx context.Context, pid int) string
)

// mainPyRe finds the directory before main.py in a command line, as agent.ps1's
// '"?([^"]*?)main\.py' (PowerShell's -match ignores case).
var mainPyRe = regexp.MustCompile(`(?i)"?([^"]*?)main\.py`)

// comfyHome is ComfyUI's directory from its server's command line, "" when it doesn't run
// main.py. Its models live in comfyHome/models.
func comfyHome(cmdline string) string {
	m := mainPyRe.FindStringSubmatch(cmdline)
	if m == nil {
		return ""
	}
	// A quoted path starts right after its quote. Otherwise (no quote, or the match began at
	// the closing quote of python.exe) the path has no spaces: it is the last word.
	dir := m[1]
	if !strings.HasPrefix(m[0], `"`) || strings.HasPrefix(dir, " ") {
		dir = dir[strings.LastIndex(dir, " ")+1:]
	}
	return dir
}

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
