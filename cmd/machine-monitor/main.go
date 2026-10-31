// Command machine-monitor monitors this machine, or every machine on the network, in a
// terminal. Each subcommand parses its own flags with the standard flag package.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/ContractorKeith/machine-monitor/internal/collect"
	_ "github.com/ContractorKeith/machine-monitor/internal/collect/all" // register probes
	"github.com/ContractorKeith/machine-monitor/internal/tui"
)

// version is set at build time: -ldflags "-X main.version=1.2.3".
var version = "dev"

const usage = `machine-monitor — monitor this machine or every machine on your network

Usage:
  machine-monitor                            open the TUI for this machine
  machine-monitor --hub URL                  open the TUI against a hub
  machine-monitor hub [--port 7777]          run a headless hub (also monitors itself)
  machine-monitor agent --hub URL            report this machine to a hub
  machine-monitor connect URL                pair this machine with a hub and open the TUI
  machine-monitor status --json              print one report
  machine-monitor export ID --range 7d       export a device's history
  machine-monitor service install|uninstall  start at login
  machine-monitor update                     update to the latest release
  machine-monitor help                       show this help

Flags:
  --version   print the version
  --hub URL   hub to connect the TUI to
  --ascii     ASCII bars and charts instead of braille (also MACHINE_MONITOR_ASCII=1)

The TUI respects NO_COLOR.
`

// commands lists every subcommand. Each later milestone replaces its stub.
var commands = map[string]func(args []string, stdout, stderr io.Writer) int{
	"hub":     runHub,
	"agent":   stub("agent"),
	"connect": stub("connect"),
	"status":  runStatus,
	"export":  runExport,
	"service": stub("service"),
	"update":  stub("update"),
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run parses args and dispatches; it returns the process exit code.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("machine-monitor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage) }
	showVersion := fs.Bool("version", false, "print the version")
	hub := fs.String("hub", "", "hub to connect the TUI to")
	ascii := fs.Bool("ascii", false, "ASCII bars and charts instead of braille")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *showVersion {
		fmt.Fprintf(stdout, "machine-monitor %s\n", version)
		return 0
	}

	rest := fs.Args()
	if len(rest) == 0 {
		if *hub != "" {
			return stub("--hub")(nil, stdout, stderr) // connected TUI is M4
		}
		return runTUI(tui.OptionsFromEnv(tui.Options{ASCII: *ascii}), stderr)
	}
	name, rest := rest[0], rest[1:]
	if name == "help" {
		fmt.Fprint(stdout, usage)
		return 0
	}
	cmd, ok := commands[name]
	if !ok {
		fmt.Fprintf(stderr, "machine-monitor: unknown command %q\n\n%s", name, usage)
		return 2
	}
	return cmd(rest, stdout, stderr)
}

// runTUI opens the local TUI; tests replace it.
var runTUI = func(o tui.Options, stderr io.Writer) int {
	o.Collector = collect.New()
	o.Procs = tui.SystemProcesses()
	if err := tui.Run(o); err != nil {
		fmt.Fprintf(stderr, "machine-monitor: %v\n", err)
		return 1
	}
	return 0
}

// stub is a placeholder for a command that is not built yet.
func stub(name string) func(args []string, stdout, stderr io.Writer) int {
	return func(_ []string, _, stderr io.Writer) int {
		fmt.Fprintf(stderr, "machine-monitor %s: not built yet\n", name)
		return 2
	}
}
