// Package tui is the terminal UI (PLAN §7): 1 Dashboard, 2 Device, 3 Processes, 4 History,
// 5 Energy and 6 Alerts, plus , Settings, Add device (hub mode), / filter, ? help and q quit.
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
	"github.com/ContractorKeith/machine-monitor/internal/history"
	"github.com/ContractorKeith/machine-monitor/internal/power"
	"github.com/ContractorKeith/machine-monitor/internal/report"
	"github.com/ContractorKeith/machine-monitor/internal/tailscale"
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
	// History is local mode's session history, fed by the collector and read by the History
	// and Energy views. Run makes one when nil; hub mode reads the hub instead.
	History *history.Store
	// Price is local mode's electricity price, and the fallback in hub mode while the hub has
	// none saved. nil guesses a typical rate from the time zone and locale, as upstream.
	Price *power.Settings
	// Location is the time zone the History and Energy views label and bucket in; time.Local
	// when nil.
	Location *time.Location
	// Now is the clock for "last seen" times and history windows; time.Now when nil.
	Now func() time.Time
	// Version is shown in Settings; "" reads the version Go stamped into the binary.
	Version string
	// PrefsPath is where Settings saves the temperature unit and local mode's price; Run
	// uses tui.json in the data directory when empty. New with an empty path saves nothing.
	PrefsPath string
	// Tailscale reads this machine's Tailscale status for Settings in local mode (hub mode
	// asks the hub). Run sets it when nil; nil in tests shows "not available".
	Tailscale func(ctx context.Context) (*tailscale.Status, error)
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // ends the hub stream and any request still running
	if o.Hub == nil && o.History == nil {
		// history.Store keeps minutes on disk as well as in memory, so the session's go to a
		// temporary directory that goes when the TUI does.
		if dir, err := os.MkdirTemp("", "machine-monitor-session-"); err == nil {
			defer os.RemoveAll(dir)
			if s, err := history.Open(dir, o.Now); err == nil {
				o.History = s
			}
		}
	}
	if o.PrefsPath == "" {
		o.PrefsPath = defaultPrefsPath()
	}
	if o.Hub == nil && o.Tailscale == nil {
		o.Tailscale = func(ctx context.Context) (*tailscale.Status, error) {
			st := tailscale.Read(ctx, 0)
			return &st, nil
		}
	}
	m := New(o).(model)
	m.ctx = ctx
	_, err := tea.NewProgram(m).Run()
	return err
}
