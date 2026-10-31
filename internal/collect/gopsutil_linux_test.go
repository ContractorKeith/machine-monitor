//go:build linux

package collect

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/shirou/gopsutil/v4/common"
)

// TestGopsutilProcFixture checks the real ps against the procfs fixture tree, through
// gopsutil's HOST_PROC override, so the Linux wiring is tested end to end.
func TestGopsutilProcFixture(t *testing.T) {
	proc, err := filepath.Abs("testdata/linux/proc")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), common.EnvKey, common.EnvMap{common.HostProcEnvKey: proc})
	p := gopsutil{}

	times, err := p.CPUTimes(ctx, true)
	if err != nil || len(times) != 2 {
		t.Fatalf("per-cpu times = %v, %v; want 2 cores", times, err)
	}
	total, err := p.CPUTimes(ctx, false)
	if err != nil || len(total) != 1 || total[0].Idle != 36.99 {
		t.Errorf("total times = %+v, %v; want idle 36.99 s", total, err)
	}

	vm, err := p.VirtualMemory(ctx)
	if err != nil || vm.Total != 32505856*1024 || vm.Available != 20000000*1024 {
		t.Errorf("memory = %+v, %v", vm, err)
	}

	io, err := p.DiskIO(ctx)
	if err != nil || io["nvme0n1"].ReadBytes != 4000000*512 {
		t.Errorf("diskIO = %+v, %v; want nvme0n1 read in 512-byte sectors", io["nvme0n1"], err)
	}

	src := linuxSource{ps: p}
	r, w, ok := src.diskIO(ctx)
	if !ok || r != (4000000+20000)*512 || w != (6000000+40000)*512 {
		t.Errorf("whole-disk IO = %d, %d, %v", r, w, ok)
	}

	stats, err := p.NetIO(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]uint64{}
	for _, s := range stats {
		got[s.Name] = s.BytesRecv
	}
	if got["wlan0"] != 1234567890 || got["tailscale0"] != 55555 {
		t.Errorf("net = %v", got)
	}
}
