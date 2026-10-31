//go:build darwin

package gpu

import (
	"context"

	"github.com/ContractorKeith/machine-monitor/internal/collect"
	"github.com/ContractorKeith/machine-monitor/internal/report"
)

// ioregArgs lists every IOAccelerator with its PerformanceStatistics.
var ioregArgs = []string{"-r", "-d", "1", "-w", "0", "-c", "IOAccelerator"}

func init() {
	collect.Register(collect.Probe{Name: "gpu/macos", Run: func(ctx context.Context) (collect.Apply, error) {
		return runMacOS(ctx, findTool("ioreg", "/usr/sbin/ioreg"))
	}})
}

// runMacOS reads every IOAccelerator from ioreg. GPU temperature comes from the sensors
// probe (smctemp -g), which fills it after this Apply.
func runMacOS(ctx context.Context, ioreg string) (collect.Apply, error) {
	if ioreg == "" {
		return nil, nil
	}
	out, err := command(ctx, ioreg, ioregArgs...)
	if err != nil {
		return nil, err
	}
	gpus := parseIoreg(out)
	if len(gpus) == 0 {
		return nil, nil
	}
	return func(r *report.Report) {
		named := append([]report.GPU(nil), gpus...)
		appleNames(named, r.CPU.Name)
		r.GPUs = append(r.GPUs, named...)
	}, nil
}
