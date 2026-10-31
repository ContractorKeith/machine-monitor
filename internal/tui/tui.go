// Package tui is the terminal UI (PLAN §7). This milestone (M2) has the local views:
// 1 Dashboard (a single device card), 2 Device and 3 Processes, plus ? help and q quit.
//
// The model reads metrics through collect.Collector and processes through ProcessLister, so
// tests drive it with fakes. Run starts the real program; New builds the model for tests.
package tui

import (
	"context"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/ContractorKeith/machine-monitor/internal/collect"
	"github.com/ContractorKeith/machine-monitor/internal/report"
)

// ProcessLister returns this machine's processes. The model calls it about every Interval,
// and only while the Processes view is open, from one goroutine at a time.
type ProcessLister interface {
	Processes(ctx context.Context) ([]report.Process, error)
}

// Options configures the TUI.
type Options struct {
	Collector collect.Collector
	Procs     ProcessLister // nil: the Processes view says the list is unavailable
	Interval  time.Duration // sample period; 0 = DefaultInterval
	ASCII     bool          // ASCII bars, sparklines and borders instead of block and braille glyphs
	NoColor   bool          // monochrome theme (attributes only)
}

// DefaultInterval is the sample period, matching upstream's agents.
const DefaultInterval = 2 * time.Second

// collectTimeout bounds one Collect or Processes call.
const collectTimeout = 5 * time.Second

// EnvASCII turns on the ASCII fallback when set to anything but "" or "0".
const EnvASCII = "MACHINE_MONITOR_ASCII"

// OptionsFromEnv fills the display options from the environment: NO_COLOR (any non-empty
// value, per no-color.org) and MACHINE_MONITOR_ASCII.
func OptionsFromEnv(o Options) Options {
	if os.Getenv("NO_COLOR") != "" {
		o.NoColor = true
	}
	if v := os.Getenv(EnvASCII); v != "" && v != "0" {
		o.ASCII = true
	}
	return o
}

// Run opens the TUI full screen and blocks until the user quits.
func Run(o Options) error {
	_, err := tea.NewProgram(New(o)).Run()
	return err
}
