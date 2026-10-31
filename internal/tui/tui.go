// Package tui is the terminal UI (PLAN §7): 1 Dashboard, 2 Device and 3 Processes, plus
// ? help and q quit.
//
// It has two sources of devices. Local mode samples this machine through collect.Collector
// (and its processes through ProcessLister); hub mode (Options.Hub) follows every device on
// a hub through its event stream, and the dashboard becomes a grid of cards. Both feed the
// same device list, which is all the views read, so tests drive either with fakes. Run
// starts the real program; New builds the model for tests.
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
	// Hub switches to hub mode: every device on the hub instead of this machine. Collector
	// and Procs are then unused.
	Hub Hub
	// Now is the clock for "last seen" times; time.Now when nil.
	Now      func() time.Time
	Interval time.Duration // sample period; 0 = DefaultInterval
	ASCII    bool          // ASCII bars, sparklines and borders instead of block and braille glyphs
	NoColor  bool          // monochrome theme (attributes only)
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // ends the hub stream and any request still running
	m := New(o).(model)
	m.ctx = ctx
	_, err := tea.NewProgram(m).Run()
	return err
}
