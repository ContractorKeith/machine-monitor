package main

import (
	"bytes"
	"encoding/json"
	"runtime"
	"strings"
	"testing"

	"github.com/ContractorKeith/machine-monitor/internal/report"
)

// TestStatusJSON runs status --json against this machine, so CI checks it on every OS.
func TestStatusJSON(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"status", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
	var r report.Report
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatalf("not a report: %v\n%s", err, out.String())
	}
	want := map[string]string{"linux": report.PlatformLinux, "darwin": report.PlatformMacOS, "windows": report.PlatformWindows}[runtime.GOOS]
	if r.Platform != want || r.ID == "" || r.Agent != "machine-monitor/dev" {
		t.Errorf("platform, id, agent = %q, %q, %q", r.Platform, r.ID, r.Agent)
	}
	// Two samples a second apart: every CI runner has CPU usage, memory, a disk and a
	// network interface, so an OS whose collector returns one of them empty fails here.
	if r.CPU.Usage == nil || r.Memory.Total == nil || r.Memory.Used == nil || r.Uptime == nil ||
		len(r.Disks) == 0 || len(r.NetIfaces) == 0 {
		t.Errorf("missing core metrics:\n%s", out.String())
	}
	t.Logf("status --json:\n%s", out.String())
}

func TestStatusUsage(t *testing.T) {
	for _, args := range [][]string{{"status"}, {"status", "--json", "extra"}, {"status", "--bogus"}} {
		var out, errOut bytes.Buffer
		if code := run(args, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "status") {
			t.Errorf("run(%q) = %d, stderr %q; want 2 and usage", args, code, errOut.String())
		}
	}
}
