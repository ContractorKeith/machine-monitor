package collect

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/net"

	"github.com/ContractorKeith/machine-monitor/internal/report"
)

// twoSamples is a fakePS with two samples 2 s apart's worth of counters.
func twoSamples() *fakePS {
	return &fakePS{
		times: [][]cpu.TimesStat{
			{{User: 100, Idle: 300}, {User: 50, Idle: 150}, {User: 50, Idle: 150}},
			// total: +60 busy, +140 idle (+20 of it iowait) = 30 %; core0 50 %, core1 10 %
			{{User: 160, Idle: 420, Iowait: 20}, {User: 100, Idle: 200}, {User: 60, Idle: 240}},
		},
		count:  2,
		uptime: 5120,
		vm:     &mem.VirtualMemoryStat{Total: 32 << 30, Available: 20 << 30},
		diskIO: []map[string]disk.IOCountersStat{
			{"nvme0n1": {ReadBytes: 1000, WriteBytes: 50000}},
			{"nvme0n1": {ReadBytes: 3000, WriteBytes: 40000}}, // write went backwards: reset
		},
		netIO: [][]net.IOCountersStat{
			{{Name: "lo", BytesRecv: 5, BytesSent: 5}, {Name: "wlan0", BytesRecv: 1000, BytesSent: 100}},
			{{Name: "lo", BytesRecv: 9, BytesSent: 9}, {Name: "wlan0", BytesRecv: 5000, BytesSent: 300}, {Name: "new0", BytesRecv: 7, BytesSent: 7}},
		},
		ifaces: net.InterfaceStatList{
			{Name: "lo", HardwareAddr: "", Flags: []string{"up", "loopback"}},
			{Name: "wlan0", HardwareAddr: "A4:BB:6D:12:34:56", Flags: []string{"up"}},
			{Name: "eth0", HardwareAddr: "a4:bb:6d:12:34:56", Flags: []string{"up"}},        // duplicate
			{Name: "docker0", HardwareAddr: "02:42:ac:11:00:02", Flags: []string{"up"}},     // locally administered
			{Name: "eth1", HardwareAddr: "00:00:00:00:00:00", Flags: []string{"up"}},        // zero
			{Name: "eth2", HardwareAddr: "00:1b:21:aa:bb:cc", Flags: []string{"broadcast"}}, // real, down is fine
			{Name: "ib0", HardwareAddr: "80:00:02:08:fe:80:00:00:00:00", Flags: []string{}}, // not 6 bytes
		},
	}
}

func TestCoreCollect(t *testing.T) {
	p := twoSamples()
	src := linuxSource{fsys: os.DirFS("testdata/linux"), ps: p, hostname: func() (string, error) { return "atlas", nil }}
	c := &core{src: src, ps: p, probes: newProbeRunner(nil)}
	start := time.Unix(1_700_000_000, 0)
	c.now = func() time.Time { return start }
	ctx := context.Background()

	r1, err := c.Collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r1.CPU.Usage != nil || r1.CPU.Cores != nil || r1.Net != nil || r1.DiskIO != nil || r1.NetIfaces != nil {
		t.Error("first sample should leave rates nil")
	}
	if r1.ID != "4c4c4544004e3510804ab4c04f4d4e32" || r1.Platform != "linux" {
		t.Errorf("id, platform = %q, %q", r1.ID, r1.Platform)
	}
	if val(t, "uptime", r1.Uptime) != 5120 || val(t, "threads", r1.CPU.Threads) != 2 || val(t, "clock", r1.CPU.ClockMHz) != 2100 {
		t.Errorf("uptime, threads, clock = %v, %v, %v", *r1.Uptime, *r1.CPU.Threads, *r1.CPU.ClockMHz)
	}
	if val(t, "memory.used", r1.Memory.Used) != 12<<30 {
		t.Errorf("memory used = %v", *r1.Memory.Used)
	}
	if len(r1.MACs) != 2 || r1.MACs[0] != "00:1b:21:aa:bb:cc" || r1.MACs[1] != "a4:bb:6d:12:34:56" {
		t.Errorf("macs = %v", r1.MACs)
	}

	p.sample++
	c.now = func() time.Time { return start.Add(2 * time.Second) }
	r2, err := c.Collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := val(t, "usage", r2.CPU.Usage); got != 30 {
		t.Errorf("usage = %v, want 30", got)
	}
	if len(r2.CPU.Cores) != 2 || val(t, "core0", r2.CPU.Cores[0]) != 50 || val(t, "core1", r2.CPU.Cores[1]) != 10 {
		t.Errorf("cores = %v", r2.CPU.Cores)
	}
	if val(t, "read", r2.DiskIO.Read) != 1000 || val(t, "write", r2.DiskIO.Write) != 0 {
		t.Errorf("diskIO = %v/%v, want 1000 and a clamped 0", *r2.DiskIO.Read, *r2.DiskIO.Write)
	}
	if r2.Net == nil || val(t, "rx", r2.Net.RX) != 2000 || val(t, "tx", r2.Net.TX) != 100 {
		t.Errorf("net = %+v", r2.Net)
	}
	if len(r2.NetIfaces) != 1 || *r2.NetIfaces[0].Name != "wlan0" {
		t.Errorf("netIfaces = %+v, want wlan0 only (lo dropped, new0 has no previous sample)", r2.NetIfaces)
	}
}

func TestCoreMissingEverything(t *testing.T) {
	p := &fakePS{}
	c := &core{src: macSource{ps: p, run: recorded(t, nil), hostname: func() (string, error) { return "", os.ErrNotExist }}, ps: p, probes: newProbeRunner(nil), now: time.Now}
	for range 2 {
		r, err := c.Collect(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if r.Uptime != nil || r.CPU.Threads != nil || r.CPU.Usage != nil || r.Memory.Total != nil || r.Memory.Used != nil ||
			r.Net != nil || r.DiskIO != nil || r.Disks != nil || r.Hostname != nil || r.MACs != nil {
			t.Errorf("unknown metrics should be nil: %+v", r)
		}
	}
}

func TestCollectCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := New().Collect(ctx); err == nil {
		t.Error("want an error from a cancelled context")
	}
}

func TestCPUUsage(t *testing.T) {
	a := cpu.TimesStat{User: 10, Idle: 90}
	for name, tc := range map[string]struct {
		b    cpu.TimesStat
		want *float64
	}{
		"idle":        {cpu.TimesStat{User: 10, Idle: 190}, ptr(0.0)},
		"busy":        {cpu.TimesStat{User: 110, Idle: 90}, ptr(100.0)},
		"rounded":     {cpu.TimesStat{User: 11, Idle: 92}, ptr(33.3)},
		"no time":     {a, nil},
		"went back":   {cpu.TimesStat{User: 5, Idle: 80}, nil},
		"iowait idle": {cpu.TimesStat{User: 10, Idle: 140, Iowait: 50}, ptr(0.0)},
	} {
		got := cpuUsage(a, tc.b)
		if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
			t.Errorf("%s: usage = %v, want %v", name, deref(got), deref(tc.want))
		}
	}
}

func ptr(f float64) *float64 { return &f }

func deref(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

// stuckSource is a Linux source whose memory read waits for its ctx (a command that obeys
// its timeout) and whose network read ignores ctx entirely (a hung statfs or WMI call).
type stuckSource struct {
	linuxSource
	release chan struct{}
}

func (s stuckSource) memory(ctx context.Context) report.Memory {
	<-ctx.Done()
	return report.Memory{} // like vm_stat killed at the deadline
}

func (s stuckSource) netIO(context.Context) []ifaceBytes {
	<-s.release
	return nil
}

func TestCoreSampleDeadline(t *testing.T) {
	p := twoSamples()
	src := stuckSource{linuxSource: linuxFixture(p), release: make(chan struct{})}
	defer close(src.release)
	c := &core{src: src, ps: p, probes: newProbeRunner(nil), now: time.Now, timeout: 50 * time.Millisecond}

	for i := range 2 {
		p.sample = i
		start := time.Now()
		r, err := c.Collect(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if d := time.Since(start); d > time.Second {
			t.Fatalf("sample %d took %v, want about the 50 ms budget", i+1, d)
		}
		if r.Memory.Total != nil || r.Memory.Used != nil || r.NetIfaces != nil || r.Net != nil {
			t.Errorf("sample %d: stuck reads should leave memory and net nil: %+v %+v", i+1, r.Memory, r.Net)
		}
		if r.Uptime == nil || r.CPU.ClockMHz == nil {
			t.Errorf("sample %d: healthy reads should still be filled", i+1)
		}
		if i == 1 && (r.CPU.Usage == nil || r.DiskIO == nil) {
			t.Errorf("sample 2: usage and disk I/O should not wait on the stuck reads")
		}
	}
}
