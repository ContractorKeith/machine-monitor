package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"runtime"
	"strings"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/hub"
	"github.com/ContractorKeith/machine-monitor/internal/service"
	"github.com/ContractorKeith/machine-monitor/internal/update"
)

// Seams for tests.
var (
	updateSource     = update.GitHub
	updateExecutable = service.Executable
)

// The hub's installers download the release this binary is.
func init() { hub.InstallVersion = version }

// maxNotes caps how many lines of release notes update prints.
const maxNotes = 40

// runUpdate is `machine-monitor update [--check]`: install the latest GitHub release over
// this binary after checking checksums.txt's signature and the archive's SHA-256.
func runUpdate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: machine-monitor update [--check]")
		fs.PrintDefaults()
	}
	check := fs.Bool("check", false, "only report whether an update is available")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "machine-monitor update: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "machine-monitor update: "+format+"\n", a...)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	src := updateSource()
	rel, err := src.Latest(ctx)
	if err != nil {
		return fail("can't check for updates: %v", err)
	}
	fmt.Fprintf(stdout, "This is machine-monitor %s. The latest release is %s", version, rel.Version())
	if !rel.Published.IsZero() {
		fmt.Fprintf(stdout, " (%s)", rel.Published.Format("2006-01-02"))
	}
	fmt.Fprintln(stdout, ".")
	if !update.Newer(rel.Tag, version) {
		fmt.Fprintln(stdout, "You're up to date.")
		return 0
	}
	printNotes(stdout, rel)
	if *check {
		fmt.Fprintln(stdout, "Run `machine-monitor update` to install it.")
		return 0
	}

	exe, err := updateExecutable()
	if err != nil {
		return fail("can't find this binary: %v", err)
	}
	if pm := update.ManagedBy(exe); pm != "" {
		return fail("%s manages %s; update it with %s instead", pm, exe, map[string]string{
			"Homebrew": "brew upgrade machine-monitor", "Scoop": "scoop update machine-monitor"}[pm])
	}
	update.CleanOld(exe)
	fmt.Fprintf(stdout, "Downloading %s for %s/%s...\n", rel.Tag, runtime.GOOS, runtime.GOARCH)
	bin, err := src.Binary(ctx, rel.Tag, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return fail("%v; nothing was changed", err)
	}
	fmt.Fprintln(stdout, "Signature and checksum verified.")
	if err := update.Replace(exe, bin); err != nil {
		return fail("can't replace %s: %v", exe, err)
	}
	fmt.Fprintf(stdout, "Updated %s to %s.\n", exe, rel.Version())
	fmt.Fprintln(stdout, "A hub or agent already running keeps the old version until it restarts (machine-monitor service install restarts it).")
	return 0
}

// printNotes prints what changed: the release notes, trimmed to maxNotes lines.
func printNotes(w io.Writer, rel update.Release) {
	notes := strings.TrimSpace(strings.ReplaceAll(rel.Notes, "\r\n", "\n"))
	if notes == "" {
		if rel.URL != "" {
			fmt.Fprintf(w, "Release notes: %s\n", rel.URL)
		}
		return
	}
	fmt.Fprintf(w, "\nWhat's new in %s:\n", rel.Version())
	lines := strings.Split(notes, "\n")
	for i, l := range lines {
		if i == maxNotes {
			fmt.Fprintf(w, "  ... %d more lines: %s\n", len(lines)-maxNotes, rel.URL)
			break
		}
		fmt.Fprintf(w, "  %s\n", printable(l))
	}
	fmt.Fprintln(w)
}

// printable drops control characters (terminal escapes) from text fetched over the network.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\t' || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return -1
		}
		return r
	}, s)
}
