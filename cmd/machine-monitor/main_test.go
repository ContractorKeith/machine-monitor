package main

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/ContractorKeith/machine-monitor/internal/tui"
)

// built lists the commands that are no longer stubs; each has its own test.
var built = map[string]bool{"status": true, "hub": true, "agent": true, "connect": true}

type runCase struct {
	args        []string
	code        int
	out, errOut string // substrings expected on stdout and stderr
}

func TestRun(t *testing.T) {
	var tuiOpts *tui.Options
	runTUI = func(o tui.Options, _ io.Writer) int { tuiOpts = &o; return 0 }
	tests := []runCase{
		{[]string{"--version"}, 0, "machine-monitor dev\n", ""},
		{[]string{"-version"}, 0, "machine-monitor dev\n", ""},
		{[]string{"help"}, 0, "Usage:", ""},
		{[]string{"-h"}, 0, "", "Usage:"},
		{nil, 0, "", ""},
		{[]string{"nope"}, 2, "", `unknown command "nope"`},
		{[]string{"--bogus"}, 2, "", "flag provided but not defined"},
		{[]string{"hub", "x"}, 2, "", `hub: unexpected argument "x"`},
		{[]string{"hub", "--port", "0"}, 2, "", "hub: bad port 0"},
	}
	for name := range commands {
		if built[name] {
			continue
		}
		tests = append(tests, runCase{[]string{name, "x"}, 2, "", name + ": not built yet"})
	}
	for _, tt := range tests {
		var out, errOut bytes.Buffer
		code := run(tt.args, &out, &errOut)
		if code != tt.code || !strings.Contains(out.String(), tt.out) || !strings.Contains(errOut.String(), tt.errOut) {
			t.Errorf("run(%q) = %d, stdout %q, stderr %q; want %d, %q, %q",
				tt.args, code, out.String(), errOut.String(), tt.code, tt.out, tt.errOut)
		}
	}
	if tuiOpts == nil {
		t.Fatal("run(nil) did not open the TUI")
	}
	tuiOpts = nil
	t.Setenv(tui.EnvASCII, "")
	if run([]string{"--ascii"}, io.Discard, io.Discard); tuiOpts == nil || !tuiOpts.ASCII {
		t.Errorf("--ascii: TUI options %+v, want ASCII", tuiOpts)
	}
}
