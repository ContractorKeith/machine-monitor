package collect

import (
	"context"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/net"
)

type ps interface {
	CPUTimes(ctx context.Context, percpu bool) ([]cpu.TimesStat, error)
	CPUInfo(ctx context.Context) ([]cpu.InfoStat, error)
	CPUCount(ctx context.Context) (int, error)
	VirtualMemory(ctx context.Context) (*mem.VirtualMemoryStat, error)
	Partitions(ctx context.Context) ([]disk.PartitionStat, error)
	Usage(ctx context.Context, path string) (*disk.UsageStat, error)
	DiskIO(ctx context.Context) (map[string]disk.IOCountersStat, error)
	NetIO(ctx context.Context) ([]net.IOCountersStat, error)
	Interfaces(ctx context.Context) (net.InterfaceStatList, error)
	Uptime(ctx context.Context) (uint64, error)
}

type gopsutil struct{}

func (gopsutil) CPUTimes(ctx context.Context, percpu bool) ([]cpu.TimesStat, error) {
	return cpu.TimesWithContext(ctx, percpu)
}
func (gopsutil) CPUInfo(ctx context.Context) ([]cpu.InfoStat, error) {
	return cpu.InfoWithContext(ctx)
}
func (gopsutil) CPUCount(ctx context.Context) (int, error) {
	return cpu.CountsWithContext(ctx, true)
}
func (gopsutil) VirtualMemory(ctx context.Context) (*mem.VirtualMemoryStat, error) {
	return mem.VirtualMemoryWithContext(ctx)
}
func (gopsutil) Partitions(ctx context.Context) ([]disk.PartitionStat, error) {
	return disk.PartitionsWithContext(ctx, true)
}
func (gopsutil) Usage(ctx context.Context, path string) (*disk.UsageStat, error) {
	return disk.UsageWithContext(ctx, path)
}
func (gopsutil) DiskIO(ctx context.Context) (map[string]disk.IOCountersStat, error) {
	return disk.IOCountersWithContext(ctx)
}
func (gopsutil) NetIO(ctx context.Context) ([]net.IOCountersStat, error) {
	return net.IOCountersWithContext(ctx, true)
}
func (gopsutil) Interfaces(ctx context.Context) (net.InterfaceStatList, error) {
	return net.InterfacesWithContext(ctx)
}
func (gopsutil) Uptime(ctx context.Context) (uint64, error) {
	return host.UptimeWithContext(ctx)
}
