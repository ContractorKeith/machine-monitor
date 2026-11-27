package main

import (
	"fmt"
	"io"
	"os"

	"github.com/ContractorKeith/machine-monitor/internal/agent"
	"github.com/ContractorKeith/machine-monitor/internal/client"
	"github.com/ContractorKeith/machine-monitor/internal/tui"
)

const envKey = "MACHINE_MONITOR_KEY"

func keyArg(flagValue string, stderr io.Writer) string {
	if flagValue != "" {
		fmt.Fprintf(stderr, "warning: --key puts the hub key in the process list. Set %s, or pair with `machine-monitor connect`, instead.\n", envKey)
		return flagValue
	}
	return os.Getenv(envKey)
}

func loadPairing(path string) (agent.Client, error) {
	cfg, err := client.LoadConfig(path)
	if err != nil {
		return agent.Client{}, err
	}
	if cfg.Hub == "" && cfg.Key == "" {
		return agent.Client{}, agent.ErrNotPaired
	}
	return agent.Client{HubURL: cfg.Hub, Key: cfg.Key, DeviceKey: cfg.DeviceKey}, nil
}

func tuiOptions(o tui.Options) tui.Options {
	o = tui.OptionsFromEnv(o)
	o.Version = version
	return o
}
