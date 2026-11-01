package collect

// macOS core metrics. Logic ported from Forward-Future/device-monitor agent/agent-macos.sh
// (MIT, © 2026 Matt): https://github.com/Forward-Future/device-monitor

import (
	"context"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/ContractorKeith/machine-monitor/internal/report"
)

// runFunc runs a command and returns its stdout. Tests replace it with recorded output.
type runFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

// macSource reads macOS: gopsutil for CPU, totals, volumes and disk I/O (IOKit, no exec),
// and the same system commands upstream runs for the rest.
type macSource struct {
	ps       ps
	run      runFunc
	hostname func() (string, error)
}

func (macSource) platform() string { return report.PlatformMacOS }

func (s macSource) deviceID(ctx context.Context) string {
	if id := ioregUUID(s.out(ctx, "/usr/sbin/ioreg", "-rd1", "-c", "IOPlatformExpertDevice")); id != "" {
		return id
	}
	return s.hostName(ctx)
}

func (s macSource) hostName(ctx context.Context) string {
	host := firstLine(s.out(ctx, "/usr/sbin/scutil", "--get", "ComputerName"))
	if host == "" {
		h, _ := s.hostname()
		host, _, _ = strings.Cut(h, ".") // hostname -s
	}
	return host
}

func (s macSource) static(ctx context.Context) staticInfo {
	id := s.deviceID(ctx)
	host := s.hostName(ctx)
	model := profilerField(s.out(ctx, "/usr/sbin/system_profiler", "SPHardwareDataType"), "Model Name")
	osName := "macOS"
	if v := firstLine(s.out(ctx, "/usr/bin/sw_vers", "-productVersion")); v != "" {
		osName += " " + v
	}
	info := staticInfo{id: id, hostname: strPtr(host)}
	if model != "" {
		osName += " · " + model
		ff := report.FormFactorDesktop
		if strings.Contains(model, "Book") {
			ff = report.FormFactorLaptop
		}
		info.formFactor = &ff
	}
	info.os = &osName
	if ci, err := s.ps.CPUInfo(ctx); err == nil && len(ci) > 0 {
		info.cpuName = strPtr(ci[0].ModelName)
	}
	return info
}

// clockMHz: macOS doesn't expose a current clock to a normal user (upstream sends null).
func (macSource) clockMHz(context.Context) *float64 { return nil }

// memory is Activity Monitor's "Memory Used": app (active) + wired + compressed pages.
func (s macSource) memory(ctx context.Context) report.Memory {
	var m report.Memory
	if vm, err := s.ps.VirtualMemory(ctx); err == nil && vm.Total > 0 {
		m.Total = report.Ptr(float64(vm.Total))
	}
	m.Used = vmStatUsed(s.out(ctx, "/usr/bin/vm_stat"))
	return m
}

// disks lists the Data volume as "Macintosh HD" and everything under /Volumes, local only.
// APFS volumes share their container's space, so used = total − available, as Finder shows.
func (s macSource) disks(ctx context.Context) []report.Disk {
	parts, err := s.ps.Partitions(ctx)
	if err != nil {
		return nil
	}
	out := []report.Disk{}
	for _, p := range parts {
		if !slices.Contains(p.Opts, "local") {
			continue
		}
		var name string
		switch {
		case p.Mountpoint == "/System/Volumes/Data":
			name = "Macintosh HD"
		case strings.HasPrefix(p.Mountpoint, "/Volumes/"):
			name = strings.TrimPrefix(p.Mountpoint, "/Volumes/")
		default:
			continue
		}
		u, err := s.ps.Usage(ctx, p.Mountpoint)
		if err != nil || u.Total == 0 {
			continue
		}
		used := u.Total - min(u.Free, u.Total)
		out = append(out, report.Disk{Name: report.Ptr(name), Total: report.Ptr(float64(u.Total)), Used: report.Ptr(float64(used))})
	}
	return out
}

// diskIO sums IOBlockStorageDriver statistics, which gopsutil reads through IOKit.
func (s macSource) diskIO(ctx context.Context) (read, write uint64, ok bool) {
	io, err := s.ps.DiskIO(ctx)
	if err != nil || len(io) == 0 {
		return 0, 0, false
	}
	for _, d := range io {
		read += d.ReadBytes
		write += d.WriteBytes
	}
	return read, write, true
}

// netIO parses netstat -ibn, as upstream does.
func (s macSource) netIO(ctx context.Context) []ifaceBytes {
	return netstatBytes(s.out(ctx, "/usr/sbin/netstat", "-ibn"))
}

// inNetTotal: upstream counts en* only, so utun, bridge and awdl traffic isn't counted twice.
func (macSource) inNetTotal(name string) bool { return strings.HasPrefix(name, "en") }

func (s macSource) out(ctx context.Context, name string, args ...string) string {
	b, _ := s.run(ctx, name, args...)
	return string(b)
}

var ioregUUIDRe = regexp.MustCompile(`"IOPlatformUUID"\s*=\s*"([^"]+)"`)

// ioregUUID extracts IOPlatformUUID from ioreg -rd1 -c IOPlatformExpertDevice.
func ioregUUID(out string) string {
	if m := ioregUUIDRe.FindStringSubmatch(out); m != nil {
		return m[1]
	}
	return ""
}

// profilerField returns "key: value" from system_profiler output.
func profilerField(out, key string) string {
	for line := range strings.Lines(out) {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), key+":"); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// vmStatUsed computes (active + wired + compressed) × page size from vm_stat output.
func vmStatUsed(out string) *float64 {
	page := 4096.0
	var active, wired, compressed float64
	found := 0
	for line := range strings.Lines(out) {
		if _, after, ok := strings.Cut(line, "page size of "); ok {
			if f := strings.Fields(after); len(f) > 0 {
				if n, err := strconv.ParseFloat(f[0], 64); err == nil && n > 0 {
					page = n
				}
			}
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		n, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(v), "."), 64)
		if err != nil {
			continue
		}
		switch strings.TrimSpace(k) {
		case "Pages active":
			active, found = n, found+1
		case "Pages wired down":
			wired, found = n, found+1
		case "Pages occupied by compressor":
			compressed, found = n, found+1
		}
	}
	if found < 3 {
		return nil
	}
	return report.Ptr((active + wired + compressed) * page)
}

// netstatBytes reads per-interface byte counters from netstat -ibn: the <Link#n> row of
// each interface. The Ibytes and Obytes columns come from the header line. Address (before
// the counters) is blank for interfaces without a MAC (utun, gif), so a row can be one
// field short; the counters are found by their distance from the end of the line.
func netstatBytes(out string) []ifaceBytes {
	var res []ifaceBytes
	seen := map[string]bool{}
	inEnd, outEnd := 0, 0 // Ibytes and Obytes, counted back from the last column
	for line := range strings.Lines(out) {
		f := strings.Fields(line)
		if len(f) > 0 && f[0] == "Name" {
			in, out := slices.Index(f, "Ibytes"), slices.Index(f, "Obytes")
			if in < 0 || out < 0 {
				return nil
			}
			inEnd, outEnd = len(f)-in, len(f)-out
			continue
		}
		if inEnd == 0 || len(f) < 3 || !strings.HasPrefix(f[2], "<Link#") || len(f)-inEnd < 3 || len(f)-outEnd < 3 {
			continue
		}
		name := strings.TrimSuffix(f[0], "*") // * marks an interface that is down
		if strings.HasPrefix(name, "lo") || seen[name] {
			continue
		}
		rx, err1 := strconv.ParseUint(f[len(f)-inEnd], 10, 64)
		tx, err2 := strconv.ParseUint(f[len(f)-outEnd], 10, 64)
		if err1 != nil || err2 != nil || (rx == 0 && tx == 0) {
			continue
		}
		seen[name] = true
		res = append(res, ifaceBytes{name: name, rx: rx, tx: tx})
	}
	return res
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return strings.TrimSpace(line)
}
