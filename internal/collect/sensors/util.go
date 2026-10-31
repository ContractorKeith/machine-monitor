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

// reading is what one sensors Run found. Unknown values are nil.
type reading struct {
	cpuTemp    *float64
	tempSource *string // "acpi" when cpuTemp is an ACPI thermal-zone fallback
	cpuPower   *float64
	gpuTemp    *float64 // fills GPUs whose temp is still nil
	temps      []report.Temp
	fans       []report.Fan
}

// apply returns the Apply for r, or nil when it found nothing. Scalars are filled only
// when still nil, per the probe contract.
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

// command runs a vendor tool under ctx's deadline with a stripped environment.
func command(ctx context.Context, path string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = strippedEnv()
	cmd.WaitDelay = 250 * time.Millisecond // don't hang on pipes a killed child left open
	return cmd.Output()
}

// strippedEnv keeps only what a tool needs to start: PATH, SystemRoot on Windows, and a
// fixed locale so number formats don't change.
func strippedEnv() []string {
	env := []string{"LC_ALL=C"}
	for _, k := range []string{"PATH", "SystemRoot"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// findTool returns the path of name on PATH, else the first extra path that exists, else "".
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

// num parses a number; nil when s isn't one.
func num(s string) *float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return &v
}

// readNum reads a sysfs file holding one number; nil when missing or unreadable.
func readNum(path string) *float64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return num(string(b))
}

// readString reads a short sysfs text file, trimmed; "" when missing.
func readString(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

// temp returns v rounded to 0.1 °C, or nil when v is missing or not a plausible reading.
// Disconnected sensors read 0 or junk like -273 and 255; unknown must be nil, not 0.
func temp(v *float64) *float64 {
	if v == nil || *v <= 0 || *v > 150 {
		return nil
	}
	return report.Ptr(round1(*v))
}
