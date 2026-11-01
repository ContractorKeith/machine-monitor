package collect

// Linux core metrics. Logic ported from Forward-Future/device-monitor agent/agent.sh
// (MIT, © 2026 Matt): https://github.com/Forward-Future/device-monitor

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

// linuxSource reads Linux. fsys is rooted at "/" (os.DirFS in production, a fixture tree in
// tests); counters come from gopsutil.
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
		case "8", "9", "10", "11", "14", "30", "31", "32": // portable, laptop, notebook, ...
			info.formFactor = report.Ptr(report.FormFactorLaptop)
		default:
			info.formFactor = report.Ptr(report.FormFactorDesktop)
		}
	}
	return info
}

// cpuName is the first "model name" in /proc/cpuinfo; ARM has none, so fall back to
// gopsutil (which maps the CPU part number) and then the device-tree model.
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

// clockMHz is the average "cpu MHz" in /proc/cpuinfo, else cpu0's cpufreq.
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

// memory is total − available, as /proc/meminfo reports them.
func (s linuxSource) memory(ctx context.Context) report.Memory {
	vm, err := s.ps.VirtualMemory(ctx)
	if err != nil || vm.Total == 0 {
		return report.Memory{}
	}
	return report.Memory{Total: report.Ptr(float64(vm.Total)), Used: report.Ptr(float64(vm.Total - vm.Available))}
}

// Filesystems upstream passes to df -x, plus the zero-size pseudo filesystems df hides.
var linuxSkipFS = []string{"tmpfs", "devtmpfs", "squashfs", "overlay", "efivarfs", "9p", "drvfs"}

// Mount points upstream drops: /boot*, /snap*, /init, WSL's internal mounts.
var linuxSkipMount = regexp.MustCompile(`^/(boot|snap|init$|usr/lib/wsl|mnt/wsl)`)

// disks follows upstream's df filter. Unlike df it also drops repeat mounts of one block
// device (btrfs subvolumes, bind mounts), keeping the shortest mount point, so a 1 TB disk
// mounted four times is shown once. Only /dev/ sources are deduplicated: "none", fuse and
// ZFS dataset names can be shared by unrelated filesystems.
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
		if err != nil || u.Total == 0 { // df hides "dummy" filesystems with no blocks
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

// Whole disks only, so partitions aren't counted twice.
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

// inNetTotal: upstream sums every interface but lo. That includes tailscale0, docker0 and
// other virtual links, so traffic through a VPN or bridge is counted on both the virtual
// and the physical interface. Kept for parity with upstream; the per-interface rates in
// the report show each link on its own.
func (linuxSource) inNetTotal(string) bool { return true }

func (s linuxSource) read(name string) []byte {
	b, _ := fs.ReadFile(s.fsys, name)
	return b
}

func (s linuxSource) firstLine(name string) string {
	line, _, _ := strings.Cut(string(s.read(name)), "\n")
	return strings.TrimSpace(line)
}

// osRelease returns PRETTY_NAME from /etc/os-release, or "Linux".
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

// cpuinfoField returns the first value of key in /proc/cpuinfo.
func cpuinfoField(b []byte, key string) string {
	for line := range strings.Lines(string(b)) {
		k, v, ok := strings.Cut(line, ":")
		if ok && strings.TrimSpace(k) == key {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
