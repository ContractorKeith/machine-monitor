package collect

import (
	"context"
	"runtime"
	"testing"

	"github.com/ContractorKeith/machine-monitor/internal/report"
)

func TestNewReportsPlatform(t *testing.T) {
	r, err := New().Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"linux": report.PlatformLinux, "darwin": report.PlatformMacOS, "windows": report.PlatformWindows}[runtime.GOOS]
	if r.Platform != want {
		t.Errorf("platform = %q, want %q", r.Platform, want)
	}
	if r.CPU.Usage != nil {
		t.Error("unknown metric should be nil")
	}
}

func TestRegister(t *testing.T) {
	run := func(context.Context) (Apply, error) { return nil, nil }
	Register(Probe{Name: "test/b", Run: run})
	Register(Probe{Name: "test/a", Every: 5, Run: run})
	t.Cleanup(func() {
		mu.Lock()
		delete(probes, "test/a")
		delete(probes, "test/b")
		mu.Unlock()
	})

	var names []string
	for _, p := range Probes() {
		names = append(names, p.Name)
	}
	if len(names) != 2 || names[0] != "test/a" || names[1] != "test/b" {
		t.Errorf("Probes() = %v, want sorted [test/a test/b]", names)
	}

	for name, p := range map[string]Probe{
		"duplicate":  {Name: "test/a", Run: run},
		"no name":    {Run: run},
		"no Run fun": {Name: "test/c"},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("want panic")
				}
			}()
			Register(p)
		})
	}
}
