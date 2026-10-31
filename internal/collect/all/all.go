// Package all imports every probe package so their init functions register with collect.
// Binaries that collect import it for side effects: import _ ".../internal/collect/all".
package all

import (
	_ "github.com/ContractorKeith/machine-monitor/internal/collect/ai"
	_ "github.com/ContractorKeith/machine-monitor/internal/collect/gpu"
	_ "github.com/ContractorKeith/machine-monitor/internal/collect/sensors"
)
