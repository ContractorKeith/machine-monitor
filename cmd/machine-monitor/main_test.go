package main

import (
	"bytes"
	"strings"
	"testing"
)

// built lists the commands that are no longer stubs; each has its own test.
var built = map[string]bool{"status": true}

type runCase struct {
	args        []string
	code        int
	out, errOut string // substrings expected on stdout and stderr
}

func TestRun(t *testing.T) {
	tests := []runCase{
		{[]string{"--version"}, 0, "machine-monitor dev\n", ""},
		{[]string{"-version"}, 0, "machine-monitor dev\n", ""},
		{[]string{"help"}, 0, "Usage:", ""},
		{[]string{"-h"}, 0, "", "Usage:"},
		{nil, 2, "", "tui: not built yet"},
		{[]string{"--hub", "http://h:7777"}, 2, "", "tui: not built yet"},
		{[]string{"nope"}, 2, "", `unknown command "nope"`},
		{[]string{"--bogus"}, 2, "", "flag provided but not defined"},
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
}
