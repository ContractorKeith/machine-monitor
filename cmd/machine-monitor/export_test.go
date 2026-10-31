package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/config"
)

func init() { built["export"] = true }

// TestExport runs export against internal/export's fixture history directory.
func TestExport(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(config.DirEnv, dir)
	paths := config.At(dir)
	if err := os.MkdirAll(paths.History, 0o700); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join("..", "..", "internal", "export", "testdata", "history", "box-1.ndjson")
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.History, "box-1.ndjson"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	exportNow = func() time.Time { return time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC) }
	defer func() { exportNow = time.Now }()
	local := time.Local
	time.Local = time.UTC
	defer func() { time.Local = local }()

	tests := []runCase{
		{[]string{"export", "box-1", "--range", "7d"}, 0, "2026-10-31T10:01:00Z,80,97.5,", ""},
		{[]string{"export", "--format", "json", "box-1", "--range", "30d"}, 0, `"time": "2026-10-20T08:00:00Z"`, ""},
		{[]string{"export", "box-1", "--from", "2026-10-31T10:01:00Z", "--to", "2026-10-31T10:01:30Z"}, 0, "\n2026-10-31T10:01:00Z,", ""},
		{[]string{"export"}, 2, "", "usage: machine-monitor export ID"},
		{[]string{"export", "a", "b"}, 2, "", "usage: machine-monitor export ID"},
		{[]string{"export", "ghost"}, 1, "", `machine-monitor export: There is no device "ghost" in this machine's history.`},
		{[]string{"export", "box-1", "--range", "1h"}, 1, "", `has no history between 2026-10-31T11:00:00Z and 2026-10-31T12:00:00Z.`},
		{[]string{"export", "box-1", "--range", "2w"}, 1, "", `"2w" is not a range; use 1h, 6h, 24h, 7d or 30d.`},
		{[]string{"export", "box-1", "--hub", "ftp://x"}, 1, "", `is not a hub address`},
	}
	for _, tt := range tests {
		var out, errOut bytes.Buffer
		code := run(tt.args, &out, &errOut)
		if code != tt.code || !strings.Contains(out.String(), tt.out) || !strings.Contains(errOut.String(), tt.errOut) {
			t.Errorf("run(%q) = %d, stdout %q, stderr %q; want %d, %q, %q",
				tt.args, code, out.String(), errOut.String(), tt.code, tt.out, tt.errOut)
		}
		if code != 0 && out.Len() > 0 {
			t.Errorf("run(%q) failed but wrote %q to stdout", tt.args, out.String())
		}
	}
}
