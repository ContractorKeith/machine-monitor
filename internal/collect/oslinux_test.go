package collect

import (
	"context"
	"os"
	"testing"
	"testing/fstest"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"

	"github.com/ContractorKeith/machine-monitor/internal/report"
)

func linuxFixture(p *fakePS) linuxSource {
	return linuxSource{fsys: os.DirFS("testdata/linux"), ps: p, hostname: func() (string, error) { return "atlas", nil }}
}

func TestLinuxStatic(t *testing.T) {
	info := linuxFixture(&fakePS{}).static(context.Background())
	if info.id != "4c4c4544004e3510804ab4c04f4d4e32" {
		t.Errorf("id = %q", info.id)
	}
	if got := val(t, "hostname", info.hostname); got != "atlas" {
		t.Errorf("hostname = %q", got)
	}
	if got := val(t, "os", info.os); got != "Ubuntu 24.04.1 LTS · ThinkPad X1 Carbon Gen 11" {
		t.Errorf("os = %q", got)
	}
	if got := val(t, "formFactor", info.formFactor); got != report.FormFactorLaptop {
		t.Errorf("formFactor = %q", got)
	}
	if got := val(t, "cpuName", info.cpuName); got != "13th Gen Intel(R) Core(TM) i7-1365U" {
		t.Errorf("cpuName = %q", got)
	}
}

func TestLinuxStaticFallbacks(t *testing.T) {
	fsys := fstest.MapFS{
		"var/lib/dbus/machine-id":       {Data: []byte("dbus-id\n")},
		"sys/class/dmi/id/product_name": {Data: []byte("System Product Name\n")},
		"proc/cpuinfo":                  {Data: []byte("processor\t: 0\nBogoMIPS\t: 2000.00\n")},
	}
	s := linuxSource{fsys: fsys, ps: &fakePS{info: []cpu.InfoStat{{ModelName: "Neoverse-V2"}}}, hostname: func() (string, error) { return "grace", nil }}
	info := s.static(context.Background())
	if info.id != "dbus-id" {
		t.Errorf("id = %q, want dbus machine-id", info.id)
	}
	if got := val(t, "os", info.os); got != "Linux" {
		t.Errorf("os = %q, want placeholder model dropped", got)
	}
	if info.formFactor != nil {
		t.Errorf("formFactor = %q, want nil without DMI", *info.formFactor)
	}
	if got := val(t, "cpuName", info.cpuName); got != "NVIDIA Grace (Neoverse-V2)" {
		t.Errorf("cpuName = %q", got)
	}

	// No machine-id at all: the hostname is the id, as upstream.
	s.fsys = fstest.MapFS{}
	if id := s.static(context.Background()).id; id != "grace" {
		t.Errorf("id = %q, want hostname", id)
	}
}

func TestLinuxClock(t *testing.T) {
	s := linuxFixture(&fakePS{})
	if got := val(t, "clock", s.clockMHz(context.Background())); got != 2100 {
		t.Errorf("clock = %v, want mean of 1800 and 2400.5 rounded", got)
	}

	s.fsys = fstest.MapFS{"sys/devices/system/cpu/cpu0/cpufreq/scaling_cur_freq": {Data: []byte("1500000\n")}}
	if got := val(t, "clock", s.clockMHz(context.Background())); got != 1500 {
		t.Errorf("cpufreq clock = %v, want 1500", got)
	}

	s.fsys = fstest.MapFS{}
	if got := s.clockMHz(context.Background()); got != nil {
		t.Errorf("clock = %v, want nil", *got)
	}
}

func TestLinuxMemory(t *testing.T) {
	s := linuxFixture(&fakePS{vm: &mem.VirtualMemoryStat{Total: 32 << 30, Available: 20 << 30, Used: 1}})
	m := s.memory(context.Background())
	if val(t, "total", m.Total) != 32<<30 || val(t, "used", m.Used) != 12<<30 {
		t.Errorf("memory = %v/%v, want total − available", *m.Total, *m.Used)
	}
	s.ps = &fakePS{}
	if m := s.memory(context.Background()); m.Total != nil || m.Used != nil {
		t.Error("memory should be nil when unknown")
	}
}

func TestLinuxDisks(t *testing.T) {
	gb := uint64(1 << 30)
	p := &fakePS{
		parts: []disk.PartitionStat{
			{Device: "/dev/mapper/root", Mountpoint: "/home", Fstype: "btrfs"},
			{Device: "/dev/mapper/root", Mountpoint: "/", Fstype: "btrfs"},
			{Device: "/dev/nvme0n1p1", Mountpoint: "/boot", Fstype: "vfat"},
			{Device: "/dev/sdb1", Mountpoint: "/mnt/data", Fstype: "ext4"},
			{Device: "tmpfs", Mountpoint: "/run", Fstype: "tmpfs"},
			{Device: "/dev/loop0", Mountpoint: "/snap/core/1", Fstype: "squashfs"},
			{Device: "overlay", Mountpoint: "/var/lib/docker/x", Fstype: "overlay"},
			{Device: "proc", Mountpoint: "/proc", Fstype: "proc"},
			{Device: "gvfsd-fuse", Mountpoint: "/run/user/1000/gvfs", Fstype: "fuse.gvfsd-fuse"},
			{Device: "/dev/sdc1", Mountpoint: "/mnt/wsl/x", Fstype: "ext4"},
			{Device: "none", Mountpoint: "/init", Fstype: "ext4"},
		},
		usage: map[string]*disk.UsageStat{
			"/":                   {Total: 500 * gb, Used: 120 * gb},
			"/home":               {Total: 500 * gb, Used: 120 * gb},
			"/boot":               {Total: 2 * gb, Used: 1 * gb},
			"/mnt/data":           {Total: 4000 * gb, Used: 10 * gb},
			"/proc":               {Total: 0},
			"/run/user/1000/gvfs": {Total: 0},
			"/mnt/wsl/x":          {Total: gb},
			"/init":               {Total: gb},
		},
	}
	got := linuxFixture(p).disks(context.Background())
	want := []string{"/", "/mnt/data"}
	if len(got) != len(want) {
		t.Fatalf("disks = %v, want %v", names(got), want)
	}
	for i, d := range got {
		if *d.Name != want[i] {
			t.Errorf("disk %d = %q, want %q", i, *d.Name, want[i])
		}
	}
	if *got[0].Total != float64(500*gb) || *got[0].Used != float64(120*gb) {
		t.Errorf("/ = %v/%v", *got[0].Used, *got[0].Total)
	}
}

func TestLinuxDiskIO(t *testing.T) {
	p := &fakePS{diskIO: []map[string]disk.IOCountersStat{{
		"nvme0n1":   {ReadBytes: 1000, WriteBytes: 2000},
		"nvme0n1p1": {ReadBytes: 900, WriteBytes: 1900}, // partition: counted in its disk
		"dm-0":      {ReadBytes: 1000, WriteBytes: 2000},
		"sda":       {ReadBytes: 10, WriteBytes: 20},
		"mmcblk0":   {ReadBytes: 1, WriteBytes: 2},
		"loop0":     {ReadBytes: 5, WriteBytes: 5},
	}}}
	r, w, ok := linuxFixture(p).diskIO(context.Background())
	if !ok || r != 1011 || w != 2022 {
		t.Errorf("diskIO = %d, %d, %v; want 1011, 2022, true", r, w, ok)
	}
	p.diskIO = nil
	if _, _, ok := linuxFixture(p).diskIO(context.Background()); ok {
		t.Error("diskIO should be unknown when gopsutil fails")
	}
}

func names(ds []report.Disk) []string {
	var out []string
	for _, d := range ds {
		out = append(out, *d.Name)
	}
	return out
}
