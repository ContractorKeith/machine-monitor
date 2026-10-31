package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/ContractorKeith/machine-monitor/internal/agent"
	"github.com/ContractorKeith/machine-monitor/internal/collect"
	_ "github.com/ContractorKeith/machine-monitor/internal/collect/all" // register probes
	"github.com/ContractorKeith/machine-monitor/internal/config"
	"github.com/ContractorKeith/machine-monitor/internal/tui"
)

// runAgent is `machine-monitor agent [--hub URL] [--key KEY]`: report this machine to a hub.
// The hub and key default to client.json, written by connect.
func runAgent(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	fs.SetOutput(stderr)
	hubFlag := fs.String("hub", "", "hub URL (default: the hub in client.json)")
	keyFlag := fs.String("key", "", "hub key (default: the key in client.json)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "machine-monitor agent: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	paths, err := config.Default()
	if err != nil {
		fmt.Fprintf(stderr, "machine-monitor agent: %v\n", err)
		return 1
	}
	cl, err := agent.LoadClient(paths.Client)
	if err != nil && !(errors.Is(err, agent.ErrNotPaired) && *keyFlag != "") {
		fmt.Fprintf(stderr, "machine-monitor agent: %v\n", err)
		return 1
	}
	hubURL, key := cl.HubURL, cl.Key
	if *hubFlag != "" {
		u, linkKey, err := agent.NormalizeURL(*hubFlag)
		if err != nil {
			fmt.Fprintf(stderr, "machine-monitor agent: %v\n", err)
			return 2
		}
		hubURL = u
		if linkKey != "" {
			key = linkKey
		}
	}
	if *keyFlag != "" {
		key = *keyFlag
	}
	if hubURL == "" || !agent.ValidKey(key) {
		fmt.Fprintln(stderr, "machine-monitor agent: no hub or no valid key: run machine-monitor connect URL")
		return 1
	}
	return startAgent(hubURL, key, stderr)
}

// startAgent runs the report loop until SIGINT or SIGTERM.
func startAgent(hubURL, key string, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger := log.New(stderr, "", log.LstdFlags)
	procs := tui.SystemProcesses()
	err := agent.Run(ctx, agent.Options{HubURL: hubURL, Key: key, Collector: collect.New(),
		Procs: procs.Processes, Version: version, Logf: logger.Printf})
	if err != nil {
		fmt.Fprintf(stderr, "machine-monitor agent: %v\n", err)
		return 1
	}
	return 0
}
