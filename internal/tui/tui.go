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

type ProcessLister interface {
	Processes(ctx context.Context) ([]report.Process, error)
}

type Options struct {
	Collector collect.Collector
	Procs     ProcessLister

	Hub Hub

	History *history.Store

	Price *power.Settings

	Location *time.Location

	Now func() time.Time

	Version string

	PrefsPath string

	Tailscale func(ctx context.Context) (*tailscale.Status, error)
	Interval  time.Duration
	ASCII     bool
	NoColor   bool
}

const DefaultInterval = 2 * time.Second

const collectTimeout = 5 * time.Second

const EnvASCII = "MACHINE_MONITOR_ASCII"

func OptionsFromEnv(o Options) Options {
	if os.Getenv("NO_COLOR") != "" {
		o.NoColor = true
	}
	if v := os.Getenv(EnvASCII); v != "" && v != "0" {
		o.ASCII = true
	}
	return o
}

func Run(o Options) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if o.Hub == nil && o.History == nil {

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
