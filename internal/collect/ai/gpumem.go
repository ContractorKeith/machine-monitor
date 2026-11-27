package ai

import (
	"context"
	"regexp"
	"strconv"
	"strings"
)

var (
	winGPUMem func(ctx context.Context) map[int]float64

	winListener func(ctx context.Context, port string) (listening bool, pid int)

	winCmdline func(ctx context.Context, pid int) string
)

var mainPyRe = regexp.MustCompile(`(?i)"?([^"]*?)main\.py`)

func comfyHome(cmdline string) string {
	m := mainPyRe.FindStringSubmatch(cmdline)
	if m == nil {
		return ""
	}

	dir := m[1]
	if !strings.HasPrefix(m[0], `"`) || strings.HasPrefix(dir, " ") {
		dir = dir[strings.LastIndex(dir, " ")+1:]
	}
	return dir
}

type gpuProcMemRow struct {
	Name           string
	DedicatedUsage uint64
}

var pidNameRe = regexp.MustCompile(`^pid_(\d+)_`)

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
