//go:build darwin

package collect

import "github.com/ContractorKeith/machine-monitor/internal/report"

func newOS() Collector { return noop{platform: report.PlatformMacOS} }
