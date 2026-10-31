package collect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/net"
)

var errFake = errors.New("fake: not available")

// fakePS is a ps backed by fixtures. Fields left nil return errFake.
type fakePS struct {
	times  [][]cpu.TimesStat // per sample: [total, core0, core1, ...]
	sample int               // which entry of times/diskIO/netIO the next read returns

	info   []cpu.InfoStat
	count  int
	vm     *mem.VirtualMemoryStat
	parts  []disk.PartitionStat
	usage  map[string]*disk.UsageStat
	diskIO []map[string]disk.IOCountersStat
	netIO  [][]net.IOCountersStat
	ifaces net.InterfaceStatList
	uptime uint64
}

func (f *fakePS) CPUTimes(_ context.Context, percpu bool) ([]cpu.TimesStat, error) {
	if f.sample >= len(f.times) {
		return nil, errFake
	}
	t := f.times[f.sample]
	if percpu {
		return t[1:], nil
	}
	return t[:1], nil
}

func (f *fakePS) CPUInfo(context.Context) ([]cpu.InfoStat, error) {
	if f.info == nil {
		return nil, errFake
	}
	return f.info, nil
}

func (f *fakePS) CPUCount(context.Context) (int, error) {
	if f.count == 0 {
		return 0, errFake
	}
	return f.count, nil
}

func (f *fakePS) VirtualMemory(context.Context) (*mem.VirtualMemoryStat, error) {
	if f.vm == nil {
		return nil, errFake
	}
	return f.vm, nil
}

func (f *fakePS) Partitions(context.Context) ([]disk.PartitionStat, error) {
	if f.parts == nil {
		return nil, errFake
	}
	return append([]disk.PartitionStat(nil), f.parts...), nil
}

func (f *fakePS) Usage(_ context.Context, path string) (*disk.UsageStat, error) {
	if u, ok := f.usage[path]; ok {
		return u, nil
	}
	return nil, errFake
}

func (f *fakePS) DiskIO(context.Context) (map[string]disk.IOCountersStat, error) {
	if f.sample >= len(f.diskIO) {
		return nil, errFake
	}
	return f.diskIO[f.sample], nil
}

func (f *fakePS) NetIO(context.Context) ([]net.IOCountersStat, error) {
	if f.sample >= len(f.netIO) {
		return nil, errFake
	}
	return f.netIO[f.sample], nil
}

func (f *fakePS) Interfaces(context.Context) (net.InterfaceStatList, error) {
	if f.ifaces == nil {
		return nil, errFake
	}
	return f.ifaces, nil
}

func (f *fakePS) Uptime(context.Context) (uint64, error) {
	if f.uptime == 0 {
		return 0, errFake
	}
	return f.uptime, nil
}

// recorded is a runFunc that replays testdata/macos/<file> for known commands.
func recorded(t *testing.T, files map[string]string) runFunc {
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		key := filepath.Base(name)
		f, ok := files[key]
		if !ok {
			return nil, fmt.Errorf("fake: %s not recorded", key)
		}
		return os.ReadFile(filepath.Join("testdata", "macos", f))
	}
}

// fakeWMI replays testdata/windows/wmi.json, keyed by query. Queries not in it fail.
type fakeWMI map[string]json.RawMessage

func loadWMI(t *testing.T) fakeWMI {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "windows", "wmi.json"))
	if err != nil {
		t.Fatal(err)
	}
	var w fakeWMI
	if err := json.Unmarshal(b, &w); err != nil {
		t.Fatal(err)
	}
	return w
}

func (w fakeWMI) Query(_ context.Context, q string, dst any) error {
	raw, ok := w[q]
	if !ok {
		return fmt.Errorf("fake: no result for %q", q)
	}
	return json.Unmarshal(raw, dst)
}

// val dereferences a pointer for comparisons, failing the test on nil.
func val[T any](t *testing.T, name string, p *T) T {
	t.Helper()
	if p == nil {
		t.Fatalf("%s = nil, want a value", name)
	}
	return *p
}
