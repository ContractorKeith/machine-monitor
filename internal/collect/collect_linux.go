//go:build linux

package collect

import "github.com/ContractorKeith/machine-monitor/internal/report"

func newOS() Collector { return noop{platform: report.PlatformLinux} }
