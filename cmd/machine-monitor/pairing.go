package main

import (
	"fmt"
	"io"
	"os"

	"github.com/ContractorKeith/machine-monitor/internal/agent"
	"github.com/ContractorKeith/machine-monitor/internal/client"
	"github.com/ContractorKeith/machine-monitor/internal/tui"
)

// envKey names the environment variable that holds the hub key, so it needn't be in argv
// where `ps` shows it to every user on the machine.
const envKey = "MACHINE_MONITOR_KEY"

// keyArg is the key from --key (warning that argv is readable by other users), else from
// MACHINE_MONITOR_KEY, else "".
func keyArg(flagValue string, stderr io.Writer) string {
	if flagValue != "" {
		fmt.Fprintf(stderr, "warning: --key puts the hub key in the process list. Set %s, or pair with `machine-monitor connect`, instead.\n", envKey)
		return flagValue
	}
	return os.Getenv(envKey)
}

// loadPairing reads client.json through client.LoadConfig, so every spelling of its fields
// that the TUI and export accept ({"hub","key"} as well as {"hubUrl","token"}) works for the
// agent and the service too. A missing or empty file is agent.ErrNotPaired.
func loadPairing(path string) (agent.Client, error) {
	cfg, err := client.LoadConfig(path)
	if err != nil {
		return agent.Client{}, err
	}
	if cfg.Hub == "" && cfg.Key == "" {
		return agent.Client{}, agent.ErrNotPaired
	}
	return agent.Client{HubURL: cfg.Hub, Key: cfg.Key}, nil
}

// tuiOptions are the display options from the environment plus this build's version, which
// Settings shows as `--version` does.
func tuiOptions(o tui.Options) tui.Options {
	o = tui.OptionsFromEnv(o)
	o.Version = version
	return o
}
