//go:build linux

package sensors

import (
	"context"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/collect"
)

func init() {
	rapl := &raplMeter{}
	collect.Register(collect.Probe{Name: "sensors/linux", Run: func(ctx context.Context) (collect.Apply, error) {
		return readLinux("/sys", rapl, time.Now()).apply(), nil
	}})
}
