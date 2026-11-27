//go:build linux

package gpu

import (
	"context"

	"github.com/ContractorKeith/machine-monitor/internal/collect"
)

func init() {
	collect.Register(collect.Probe{Name: "gpu/linux", Run: func(ctx context.Context) (collect.Apply, error) {
		return runLinux(ctx, findTool("nvidia-smi"), "/sys")
	}})
}

func runLinux(ctx context.Context, nvidiaSMI, sys string) (collect.Apply, error) {
	gpus, err := queryNvidia(ctx, nvidiaSMI)
	gpus = append(gpus, readAMD(sys)...)
	if len(gpus) == 0 {
		return nil, err
	}
	return appendGPUs(gpus), nil
}
