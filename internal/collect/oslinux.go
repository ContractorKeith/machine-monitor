package collect

import (
	"bufio"
	"bytes"
	"context"
	"io/fs"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/shirou/gopsutil/v4/disk"

	"github.com/ContractorKeith/machine-monitor/internal/report"
)

type linuxSource struct {
	fsys     fs.FS
	ps       ps
	hostname func() (string, error)
}

func (linuxSource) platform() string { return report.PlatformLinux }

func (s linuxSource) deviceID(context.Context) string {
	host, _ := s.hostname()
	id := s.firstLine("etc/machine-id")
	if id == "" {
		id = s.firstLine("var/lib/dbus/machine-id")
	}
	if id == "" {
		id = host
	}

	return id
}

func (s linuxSource) static(ctx context.Context) staticInfo {
	host, _ := s.hostname()
	id := s.deviceID(ctx)
	osName := osRelease(s.read("etc/os-release"))
	model := s.firstLine("sys/class/dmi/id/product_name")
	if model != "" && !strings.Contains(model, "To be filled") && model != "System Product Name" {
		osName += " · " + model
	}

	info := staticInfo{id: id, hostname: strPtr(host), os: strPtr(osName), cpuName: s.cpuName(ctx)}
	if ct := s.firstLine("sys/class/dmi/id/chassis_type"); ct != "" {
		switch ct {
		case "8", "9", "10", "11", "14", "30", "31", "32":
			info.formFactor = report.Ptr(report.FormFactorLaptop)
		default:
			info.formFactor = report.Ptr(report.FormFactorDesktop)
		}
	}
	return info
}

func (s linuxSource) cpuName(ctx context.Context) *string {
	name := cpuinfoField(s.read("proc/cpuinfo"), "model name")
	if name == "" {
		if info, err := s.ps.CPUInfo(ctx); err == nil && len(info) > 0 {
			name = info[0].ModelName
		}
	}
	if name == "" {
		name = strings.TrimRight(s.firstLine("proc/device-tree/model"), "\x00")
	}
	if name == "Neoverse-V2" {
		name = "NVIDIA Grace (Neoverse-V2)"
	}
	return strPtr(name)
}

func (s linuxSource) clockMHz(context.Context) *float64 {
	var sum float64
	n := 0
	for line := range strings.Lines(string(s.read("proc/cpuinfo"))) {
		k, v, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(k) != "cpu MHz" {
			continue
		}
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			sum += f
			n++
		}
	}
	if n > 0 {
		return report.Ptr(math.Round(sum / float64(n)))
	}
	if khz, err := strconv.ParseFloat(s.firstLine("sys/devices/system/cpu/cpu0/cpufreq/scaling_cur_freq"), 64); err == nil && khz > 0 {
		return report.Ptr(math.Round(khz / 1000))
	}
	return nil
}

func (s linuxSource) memory(ctx context.Context) report.Memory {
	vm, err := s.ps.VirtualMemory(ctx)
	if err != nil || vm.Total == 0 {
		return report.Memory{}
	}
	return report.Memory{Total: report.Ptr(float64(vm.Total)), Used: report.Ptr(float64(vm.Total - vm.Available))}
}

var linuxSkipFS = []string{"tmpfs", "devtmpfs", "squashfs", "overlay", "efivarfs", "9p", "drvfs"}

var linuxSkipMount = regexp.MustCompile(`^/(boot|snap|init$|usr/lib/wsl|mnt/wsl)`)

func (s linuxSource) disks(ctx context.Context) []report.Disk {
	parts, err := s.ps.Partitions(ctx)
	if err != nil {
		return nil
	}
	slices.SortStableFunc(parts, func(a, b disk.PartitionStat) int { return len(a.Mountpoint) - len(b.Mountpoint) })
	seen := map[string]bool{}
	out := []report.Disk{}
	for _, p := range parts {
		if slices.Contains(linuxSkipFS, p.Fstype) || linuxSkipMount.MatchString(p.Mountpoint) || seen[p.Device] {
			continue
		}
		u, err := s.ps.Usage(ctx, p.Mountpoint)
		if err != nil || u.Total == 0 {
			continue
		}
		if strings.HasPrefix(p.Device, "/dev/") {
			seen[p.Device] = true
		}
		out = append(out, report.Disk{
			Name:  report.Ptr(p.Mountpoint),
			Total: report.Ptr(float64(u.Total)),
			Used:  report.Ptr(float64(u.Used)),
		})
	}
	slices.SortStableFunc(out, func(a, b report.Disk) int { return strings.Compare(*a.Name, *b.Name) })
	return out
}

var linuxWholeDisk = regexp.MustCompile(`^(sd[a-z]+|nvme[0-9]+n[0-9]+|vd[a-z]+|mmcblk[0-9]+)$`)

func (s linuxSource) diskIO(ctx context.Context) (read, write uint64, ok bool) {
	io, err := s.ps.DiskIO(ctx)
	if err != nil {
		return 0, 0, false
	}
	for name, d := range io {
		if linuxWholeDisk.MatchString(name) {
			read += d.ReadBytes
			write += d.WriteBytes
			ok = true
		}
	}
	return read, write, ok
}

func (s linuxSource) netIO(ctx context.Context) []ifaceBytes { return psNetIO(ctx, s.ps) }

func (linuxSource) inNetTotal(string) bool { return true }

func (s linuxSource) read(name string) []byte {
	b, _ := fs.ReadFile(s.fsys, name)
	return b
}

func (s linuxSource) firstLine(name string) string {
	line, _, _ := strings.Cut(string(s.read(name)), "\n")
	return strings.TrimSpace(line)
}

func osRelease(b []byte) string {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "PRETTY_NAME="); ok {
			if v = strings.Trim(v, `"'`); v != "" {
				return v
			}
		}
	}
	return "Linux"
}

func cpuinfoField(b []byte, key string) string {
	for line := range strings.Lines(string(b)) {
		k, v, ok := strings.Cut(line, ":")
		if ok && strings.TrimSpace(k) == key {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
