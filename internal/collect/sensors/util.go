package sensors

import (
	"context"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/collect"
	"github.com/ContractorKeith/machine-monitor/internal/report"
)

type reading struct {
	cpuTemp    *float64
	tempSource *string
	cpuPower   *float64
	gpuTemp    *float64
	temps      []report.Temp
	fans       []report.Fan
}

func (s reading) apply() collect.Apply {
	if s.cpuTemp == nil && s.cpuPower == nil && s.gpuTemp == nil && len(s.temps) == 0 && len(s.fans) == 0 {
		return nil
	}
	return func(r *report.Report) {
		r.Temps = append(r.Temps, s.temps...)
		r.Fans = append(r.Fans, s.fans...)
		if s.cpuTemp != nil && r.CPU.Temp == nil {
			r.CPU.Temp = report.Ptr(*s.cpuTemp)
			r.CPU.TempSource = s.tempSource
		}
		if s.cpuPower != nil {
			if r.Power == nil {
				r.Power = &report.Power{}
			}
			if r.Power.CPU == nil {
				r.Power.CPU = report.Ptr(*s.cpuPower)
			}
		}
		if s.gpuTemp != nil {
			for i := range r.GPUs {
				if r.GPUs[i].Temp == nil {
					r.GPUs[i].Temp = report.Ptr(*s.gpuTemp)
				}
			}
		}
	}
}

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

func readString(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

func temp(v *float64) *float64 {
	if v == nil || *v <= 0 || *v > 150 {
		return nil
	}
	return report.Ptr(round1(*v))
}
