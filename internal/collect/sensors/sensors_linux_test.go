//go:build linux

package sensors

import (
	"testing"

	"github.com/ContractorKeith/machine-monitor/internal/collect"
)

func TestLinuxProbeRegistered(t *testing.T) {
	for _, p := range collect.Probes() {
		if p.Name == "sensors/linux" {
			return
		}
	}
	t.Error("sensors/linux not registered")
}
