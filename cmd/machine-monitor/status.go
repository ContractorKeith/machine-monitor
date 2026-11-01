package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/collect"
	_ "github.com/ContractorKeith/machine-monitor/internal/collect/all" // register probes
	"github.com/ContractorKeith/machine-monitor/internal/report"
)

// statusGap is the time between the two samples status takes, so usage and rates are real.
const statusGap = time.Second

var statusCollector = collect.New

// runStatus prints one report of this machine: machine-monitor status --json.
func runStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print the report as JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !*asJSON || fs.NArg() > 0 {
		fmt.Fprintln(stderr, "usage: machine-monitor status --json")
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	c := statusCollector()
	if _, err := c.Collect(ctx); err != nil {
		fmt.Fprintf(stderr, "machine-monitor status: %v\n", err)
		return 1
	}
	select {
	case <-time.After(statusGap):
	case <-ctx.Done():
		fmt.Fprintf(stderr, "machine-monitor status: %v\n", ctx.Err())
		return 1
	}
	r, err := c.Collect(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "machine-monitor status: %v\n", err)
		return 1
	}
	r.Agent = "machine-monitor/" + version
	raw, err := json.Marshal(r)
	if err == nil {
		var normalized report.Report
		normalized, err = report.Normalize(raw)
		r = &normalized
	}
	if err != nil {
		fmt.Fprintf(stderr, "machine-monitor status: %v\n", err)
		return 1
	}

	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
		fmt.Fprintf(stderr, "machine-monitor status: %v\n", err)
		return 1
	}
	return 0
}
