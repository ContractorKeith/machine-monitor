package gpu

import (
	"context"
	"strings"

	"github.com/ContractorKeith/machine-monitor/internal/report"
)

// nvidiaArgs is the field list upstream's agents ask nvidia-smi for; parseNvidia reads it.
var nvidiaArgs = []string{
	"--query-gpu=name,utilization.gpu,temperature.gpu,memory.used,memory.total,power.draw,power.limit,fan.speed,clocks.gr",
	"--format=csv,noheader,nounits",
}

// queryNvidia runs nvidia-smi at path; an empty path means it isn't installed.
func queryNvidia(ctx context.Context, path string) ([]report.GPU, error) {
	if path == "" {
		return nil, nil
	}
	out, err := command(ctx, path, nvidiaArgs...)
	if err != nil {
		return nil, err
	}
	return parseNvidia(out), nil
}

// parseNvidia turns nvidia-smi CSV rows into GPUs. Memory is MiB on the wire; a zero or
// missing memory value is unknown, as in upstream.
func parseNvidia(out []byte) []report.GPU {
	var gpus []report.GPU
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Split(line, ",")
		if len(f) < 9 {
			continue
		}
		for i := range f {
			f[i] = strings.TrimSpace(f[i])
		}
		g := report.GPU{
			Vendor:     report.Ptr("nvidia"),
			Usage:      num(f[1]),
			Temp:       num(f[2]),
			MemUsed:    mib(f[3]),
			MemTotal:   mib(f[4]),
			Power:      num(f[5]),
			PowerLimit: num(f[6]),
			Fan:        num(f[7]),
			ClockMHz:   num(f[8]),
		}
		if f[0] != "" {
			g.Name = report.Ptr(f[0])
		}
		gpus = append(gpus, g)
	}
	return gpus
}

// mib converts a MiB string to bytes; nil when unknown or not positive.
func mib(s string) *float64 {
	v := num(s)
	if v == nil || *v <= 0 {
		return nil
	}
	return report.Ptr(*v * 1048576)
}
