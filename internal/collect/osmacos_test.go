package collect

import (
	"context"
	"testing"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"

	"github.com/ContractorKeith/machine-monitor/internal/report"
)

var macFiles = map[string]string{
	"ioreg":           "ioreg.txt",
	"scutil":          "scutil.txt",
	"system_profiler": "system_profiler.txt",
	"sw_vers":         "sw_vers.txt",
	"vm_stat":         "vm_stat.txt",
	"netstat":         "netstat.txt",
}

func macFixture(t *testing.T, p *fakePS, files map[string]string) macSource {
	return macSource{ps: p, run: recorded(t, files), hostname: func() (string, error) { return "kmbp.local", nil }}
}

func TestMacStatic(t *testing.T) {
	p := &fakePS{info: []cpu.InfoStat{{ModelName: "Apple M1 Pro"}}}
	info := macFixture(t, p, macFiles).static(context.Background())
	if info.id != "1B2C3D4E-5F60-7182-93A4-B5C6D7E8F901" {
		t.Errorf("id = %q", info.id)
	}
	if got := val(t, "hostname", info.hostname); got != "Keith’s MacBook Pro" {
		t.Errorf("hostname = %q", got)
	}
	if got := val(t, "os", info.os); got != "macOS 15.1 · MacBook Pro" {
		t.Errorf("os = %q", got)
	}
	if got := val(t, "formFactor", info.formFactor); got != report.FormFactorLaptop {
		t.Errorf("formFactor = %q", got)
	}
	if got := val(t, "cpuName", info.cpuName); got != "Apple M1 Pro" {
		t.Errorf("cpuName = %q", got)
	}
}

func TestMacStaticWithoutCommands(t *testing.T) {
	info := macFixture(t, &fakePS{}, nil).static(context.Background())
	if info.id != "kmbp" || val(t, "hostname", info.hostname) != "kmbp" {
		t.Errorf("id, hostname = %q, %v; want short hostname for both", info.id, *info.hostname)
	}
	if got := val(t, "os", info.os); got != "macOS" {
		t.Errorf("os = %q", got)
	}
	if info.formFactor != nil || info.cpuName != nil {
		t.Error("formFactor and cpuName should be nil when unknown")
	}
}

func TestMacMemory(t *testing.T) {
	s := macFixture(t, &fakePS{vm: &mem.VirtualMemoryStat{Total: 16 << 30}}, macFiles)
	m := s.memory(context.Background())
	if val(t, "total", m.Total) != 16<<30 {
		t.Errorf("total = %v", *m.Total)
	}
	// Activity Monitor: (active 293043 + wired 118464 + compressed 216452) × 16 KiB pages.
	if got, want := val(t, "used", m.Used), float64(293043+118464+216452)*16384; got != want {
		t.Errorf("used = %v, want %v", got, want)
	}
	if got := vmStatUsed("Mach Virtual Memory Statistics: (page size of 4096 bytes)\nPages active: 10.\n"); got != nil {
		t.Errorf("partial vm_stat = %v, want nil", *got)
	}
}

func TestMacDisks(t *testing.T) {
	gb := uint64(1 << 30)
	p := &fakePS{
		parts: []disk.PartitionStat{
			{Mountpoint: "/", Opts: []string{"ro", "local"}},
			{Mountpoint: "/System/Volumes/Data", Opts: []string{"rw", "local"}},
			{Mountpoint: "/System/Volumes/VM", Opts: []string{"rw", "local"}},
			{Mountpoint: "/Volumes/Backup", Opts: []string{"rw", "local"}},
			{Mountpoint: "/Volumes/share", Opts: []string{"rw"}}, // SMB: not local
			{Mountpoint: "/dev", Opts: []string{"rw", "local"}},
		},
		usage: map[string]*disk.UsageStat{
			"/":                    {Total: 994 * gb, Free: 600 * gb, Used: 10 * gb},
			"/System/Volumes/Data": {Total: 994 * gb, Free: 600 * gb, Used: 380 * gb},
			"/Volumes/Backup":      {Total: 2000 * gb, Free: 500 * gb, Used: 1500 * gb},
			"/Volumes/share":       {Total: 8000 * gb, Free: 1 * gb},
		},
	}
	got := macFixture(t, p, macFiles).disks(context.Background())
	if len(got) != 2 || *got[0].Name != "Macintosh HD" || *got[1].Name != "Backup" {
		t.Fatalf("disks = %v, want [Macintosh HD Backup]", names(got))
	}
	// APFS container math: used = total − available, not the volume's own usage.
	if *got[0].Used != float64(394*gb) || *got[0].Total != float64(994*gb) {
		t.Errorf("Macintosh HD = %v/%v", *got[0].Used, *got[0].Total)
	}
}

func TestMacNet(t *testing.T) {
	s := macFixture(t, &fakePS{}, macFiles)
	got := map[string]ifaceBytes{}
	for _, i := range s.netIO(context.Background()) {
		got[i.name] = i
	}
	if len(got) != 3 {
		t.Errorf("interfaces = %v, want en0, awdl0, utun0", got)
	}
	if got["en0"].rx != 12345678901 || got["en0"].tx != 987654321 {
		t.Errorf("en0 = %+v", got["en0"])
	}
	if got["utun0"].rx != 14400 || got["utun0"].tx != 15600 {
		t.Errorf("utun0 (no address column) = %+v", got["utun0"])
	}
	if !s.inNetTotal("en0") || s.inNetTotal("utun0") || s.inNetTotal("awdl0") {
		t.Error("only en* interfaces count toward the total")
	}
}

func TestMacDiskIO(t *testing.T) {
	p := &fakePS{diskIO: []map[string]disk.IOCountersStat{{
		"disk0": {ReadBytes: 100, WriteBytes: 200},
		"disk4": {ReadBytes: 1, WriteBytes: 2},
	}}}
	r, w, ok := macFixture(t, p, macFiles).diskIO(context.Background())
	if !ok || r != 101 || w != 202 {
		t.Errorf("diskIO = %d, %d, %v", r, w, ok)
	}
	if c := (macSource{}).clockMHz(context.Background()); c != nil {
		t.Error("macOS clock should be nil")
	}
}
