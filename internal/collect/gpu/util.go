package gpu

import (
	"context"
	"math"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/collect"
	"github.com/ContractorKeith/machine-monitor/internal/report"
)

func command(ctx context.Context, path string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = strippedEnv()
	cmd.WaitDelay = 250 * time.Millisecond
	return cmd.Output()
}

func strippedEnv() []string {
	env := []string{"LC_ALL=C"}
	for _, k := range []string{"PATH", "SystemRoot"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

func findTool(name string, extra ...string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	for _, p := range extra {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

func num(s string) *float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return &v
}

func readNum(path string) *float64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return num(string(b))
}

func readText(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func scale(v *float64, f float64) *float64 {
	if v == nil {
		return nil
	}
	return report.Ptr(math.Round(*v*f*10) / 10)
}

func sortByVRAM(gpus []report.GPU) {
	vram := func(g report.GPU) float64 {
		if g.MemTotal == nil {
			return 0
		}
		return *g.MemTotal
	}
	sort.SliceStable(gpus, func(i, j int) bool { return vram(gpus[i]) > vram(gpus[j]) })
}

func appendGPUs(gpus []report.GPU) collect.Apply {
	if len(gpus) == 0 {
		return nil
	}
	sortByVRAM(gpus)
	return func(r *report.Report) { r.GPUs = append(r.GPUs, gpus...) }
}
