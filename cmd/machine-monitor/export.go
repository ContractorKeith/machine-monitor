package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/agent"
	"github.com/ContractorKeith/machine-monitor/internal/config"
	"github.com/ContractorKeith/machine-monitor/internal/export"
)

const exportUsage = `usage: machine-monitor export ID|NAME [--range 1h|6h|24h|7d|30d | --from TIME [--to TIME]]
                             [--format csv|json] [--hub URL] [--key KEY]

Writes a device's 1-minute history to stdout. The device is its id or its name as the TUI
shows it. Without --hub it reads this machine's history (kept by machine-monitor hub); with
--hub it asks that hub, using the key in client.json (or --key).
--range ends now. --from and --to take RFC 3339 (2026-10-31T14:00:00Z) or a date
(2026-10-31); --to defaults to now and --from to 24h before --to. Without either, the range
is the last 24h.
`

// exportNow is the clock for export; tests replace it.
var exportNow = time.Now

// runExport is `machine-monitor export ID --range 7d --format csv`.
func runExport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, exportUsage) }
	rng := fs.String("range", "", "1h, 6h, 24h, 7d or 30d")
	from := fs.String("from", "", "start time")
	to := fs.String("to", "", "end time")
	format := fs.String("format", "csv", "csv or json")
	hubURL := fs.String("hub", "", "hub to read from")
	keyFlag := fs.String("key", "", "hub key (default: the key in client.json for that hub)")

	// flag stops at the first positional, so take the ID wherever it sits among the flags.
	var ids []string
	for {
		if err := fs.Parse(args); err != nil {
			return 2
		}
		if fs.NArg() == 0 {
			break
		}
		ids = append(ids, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(ids) != 1 {
		fmt.Fprint(stderr, exportUsage)
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "machine-monitor export: %v\n", err)
		return 1
	}

	start, end, err := export.ParseRange(*rng, *from, *to, exportNow(), time.Local)
	if err != nil {
		return fail(err)
	}
	paths, err := config.Default()
	if err != nil {
		return fail(err)
	}
	var src export.Source = export.Local{Dir: paths.History, DevicesFile: paths.Devices, Now: exportNow}
	key := ""
	if *hubURL != "" {
		if _, linkKey, err := agent.NormalizeURL(*hubURL); err == nil && linkKey != "" {
			fmt.Fprintf(stderr, "machine-monitor export: %v\n", agent.ErrKeyInURL)
			return 2
		}
		u, err := export.HubURL(*hubURL)
		if err != nil {
			return fail(err)
		}
		if key = keyArg(*keyFlag, stderr); key == "" {
			if key, err = savedKey(paths.Client, u); err != nil {
				return fail(err)
			}
		}
		src = export.Hub{URL: u, Key: key}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	w := bufio.NewWriter(stdout)
	id, err := export.ResolveDevice(ctx, src, ids[0])
	if err != nil {
		return fail(lockedWithKey(err, *hubURL, key))
	}
	opts := export.Options{ID: id, From: start, To: end, Format: *format, Loc: time.Local}
	if err := export.Run(ctx, src, opts, w); err != nil {
		return fail(lockedWithKey(err, *hubURL, key))
	}
	if err := w.Flush(); err != nil {
		return fail(err)
	}
	return 0
}

// savedKey is the key client.json holds for hub u. "localhost" and the loopback addresses
// are the same hub, so a client.json that says 127.0.0.1 serves --hub localhost too.
func savedKey(clientPath, u string) (string, error) {
	key, err := export.KeyFor(clientPath, u)
	if key != "" || err != nil || !strings.Contains(u, "localhost") {
		return key, err
	}
	for _, loop := range []string{"127.0.0.1", "[::1]"} {
		if key, err = export.KeyFor(clientPath, strings.Replace(u, "localhost", loop, 1)); key != "" || err != nil {
			return key, err
		}
	}
	return "", nil
}

// lockedWithKey rewrites export's "locked and this machine has no key" when a key was sent
// and the hub still refused it: the key is wrong, not missing.
func lockedWithKey(err error, hub, key string) error {
	if key != "" && strings.Contains(err.Error(), "is locked and this machine has no key") {
		return fmt.Errorf("the hub at %s refused the key. Check it with the one the hub prints (machine-monitor hub --show-key), or pair again: machine-monitor connect %s", hub, hub)
	}
	return err
}
