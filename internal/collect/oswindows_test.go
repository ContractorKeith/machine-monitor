package collect

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/shirou/gopsutil/v4/mem"

	"github.com/ContractorKeith/machine-monitor/internal/report"
)

func windowsFixture(t *testing.T, w wmiQuerier) *windowsSource {
	return &windowsSource{
		ps:          &fakePS{vm: &mem.VirtualMemoryStat{Total: 64 << 30, Available: 40 << 30}},
		wmi:         w,
		machineGUID: func() (string, error) { return "6f1c2b3a-1111-2222-3333-444455556666", nil },
		hostname:    func() string { return "DESKTOP-ABC" },
	}
}

func TestWindowsStatic(t *testing.T) {
	s := windowsFixture(t, loadWMI(t))
	info := s.static(context.Background())
	if info.id != "6f1c2b3a-1111-2222-3333-444455556666" || val(t, "hostname", info.hostname) != "DESKTOP-ABC" {
		t.Errorf("id, hostname = %q, %q", info.id, *info.hostname)
	}
	if got := val(t, "os", info.os); got != "Windows 11 Pro" {
		t.Errorf("os = %q", got)
	}
	if got := val(t, "cpuName", info.cpuName); got != "AMD Ryzen 9 7950X 16-Core Processor" {
		t.Errorf("cpuName = %q", got)
	}
	if got := val(t, "formFactor", info.formFactor); got != report.FormFactorDesktop {
		t.Errorf("formFactor = %q", got)
	}
	// 4501 MHz × 112 % performance.
	if got := val(t, "clock", s.clockMHz(context.Background())); got != 5041 {
		t.Errorf("clock = %v, want 5041", got)
	}
}

func TestWindowsLaptopAndFallbacks(t *testing.T) {
	w := loadWMI(t)
	w[wqlEnclosure] = json.RawMessage(`[{"ChassisTypes":[10]}]`)
	delete(w, wqlOS)
	delete(w, wqlProcessor)
	s := windowsFixture(t, w)
	s.machineGUID = func() (string, error) { return "", errors.New("access denied") }
	info := s.static(context.Background())
	if got := val(t, "formFactor", info.formFactor); got != report.FormFactorLaptop {
		t.Errorf("formFactor = %q", got)
	}
	if info.id != "DESKTOP-ABC" {
		t.Errorf("id = %q, want hostname fallback", info.id)
	}
	if info.os != nil || info.cpuName != nil {
		t.Error("os and cpuName should be nil when WMI fails")
	}
	if c := s.clockMHz(context.Background()); c != nil {
		t.Errorf("clock = %v, want nil without MaxClockSpeed", *c)
	}
}

func TestWindowsMetrics(t *testing.T) {
	s := windowsFixture(t, loadWMI(t))
	ctx := context.Background()

	m := s.memory(ctx)
	if val(t, "total", m.Total) != 64<<30 || val(t, "used", m.Used) != 24<<30 {
		t.Errorf("memory = %v/%v", *m.Used, *m.Total)
	}

	d := s.disks(ctx)
	if len(d) != 2 {
		t.Fatalf("disks = %v, want C: and D: (empty drive dropped)", names(d))
	}
	if *d[0].Name != "C:" || val(t, "label", d[0].Label) != "Windows" || *d[0].Used != 1999599939584-1250000000000 {
		t.Errorf("C: = %+v", d[0])
	}
	if d[1].Label != nil {
		t.Errorf("D: label = %q, want nil", *d[1].Label)
	}

	r, w, ok := s.diskIO(ctx)
	if !ok || r != 987654321000 || w != 123456789000 {
		t.Errorf("diskIO = %d, %d, %v", r, w, ok)
	}

	s.wmi = fakeWMI{}
	if d := s.disks(ctx); d != nil {
		t.Errorf("disks = %v, want nil when WMI fails", names(d))
	}
	if _, _, ok := s.diskIO(ctx); ok {
		t.Error("diskIO should be unknown when WMI fails")
	}
}
