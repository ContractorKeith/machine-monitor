package main

import (
	"fmt"
	"io"

	"github.com/ContractorKeith/machine-monitor/internal/client"
	"github.com/ContractorKeith/machine-monitor/internal/config"
	"github.com/ContractorKeith/machine-monitor/internal/tui"
)

func runConnected(hubURL, key string, o tui.Options, stderr io.Writer) int {
	if _, err := client.ParseURL(hubURL); err != nil {
		fmt.Fprintf(stderr, "machine-monitor --hub: %v\n", err)
		return 2
	}
	if key == "" {
		paths, err := config.Default()
		if err != nil {
			fmt.Fprintf(stderr, "machine-monitor --hub: %v\n", err)
			return 1
		}
		cfg, err := client.LoadConfig(paths.Client)
		if err != nil {
			fmt.Fprintf(stderr, "machine-monitor --hub: %v\n", err)
			return 1
		}
		if cfg.Key != "" && client.SameHub(cfg.Hub, hubURL) {
			key = cfg.Key
		}
	}
	if key == "" {
		fmt.Fprintf(stderr, "machine-monitor --hub: no key for %s. Pass --key KEY (the hub prints it at start), "+
			"or pair this machine with `machine-monitor connect %s`.\n", hubURL, hubURL)
		return 2
	}
	c, err := client.New(hubURL, key)
	if err != nil {
		fmt.Fprintf(stderr, "machine-monitor --hub: %v\n", err)
		return 2
	}
	o.Hub = c
	return runHubTUI(o, stderr)
}

var runHubTUI = func(o tui.Options, stderr io.Writer) int {
	if err := tui.Run(o); err != nil {
		fmt.Fprintf(stderr, "machine-monitor: %v\n", err)
		return 1
	}
	return 0
}
