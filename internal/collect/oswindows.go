package collect

// Windows core metrics. Logic ported from Forward-Future/device-monitor agent/agent.ps1
// (MIT, © 2026 Matt): https://github.com/Forward-Future/device-monitor

import (
	"context"
	"log/slog"
	"math"
	"slices"
	"strings"

	"github.com/ContractorKeith/machine-monitor/internal/report"
)

// wmiQuerier runs a WQL query into dst, a pointer to a slice of structs whose field names
// match the WMI properties. Tests replace it with recorded results.
type wmiQuerier interface {
	Query(ctx context.Context, query string, dst any) error
}

// windowsSource reads Windows: gopsutil for CPU times, memory, network and uptime; WMI
// classes (not localized PDH counter paths) for the rest, as upstream does.
type windowsSource struct {
	ps          ps
	wmi         wmiQuerier
	machineGUID func() (string, error) // HKLM\SOFTWARE\Microsoft\Cryptography MachineGuid
	hostname    func() string          // %COMPUTERNAME%

	maxClock float64 // Win32_Processor.MaxClockSpeed, MHz; read by static
}

// WMI classes, trimmed to the properties we read.
type (
	win32OperatingSystem struct{ Caption string }
	win32Processor       struct {
		Name          string
		MaxClockSpeed uint32
	}
	win32SystemEnclosure struct{ ChassisTypes []int32 } // WMI returns the array as int32
	win32ProcessorInfo   struct{ PercentProcessorPerformance uint64 }
	win32LogicalDisk     struct {
		DeviceID   string
		VolumeName string
		Size       uint64
		FreeSpace  uint64
	}
	win32PhysicalDisk struct {
		DiskReadBytesPersec  uint64
		DiskWriteBytesPersec uint64
	}
)

const (
	wqlOS        = "SELECT Caption FROM Win32_OperatingSystem"
	wqlProcessor = "SELECT Name, MaxClockSpeed FROM Win32_Processor"
	wqlEnclosure = "SELECT ChassisTypes FROM Win32_SystemEnclosure"
	wqlPerf      = "SELECT PercentProcessorPerformance FROM Win32_PerfFormattedData_Counters_ProcessorInformation WHERE Name = '_Total'"
	wqlDisks     = "SELECT DeviceID, VolumeName, Size, FreeSpace FROM Win32_LogicalDisk WHERE DriveType = 3"
	wqlDiskIO    = "SELECT DiskReadBytesPersec, DiskWriteBytesPersec FROM Win32_PerfRawData_PerfDisk_PhysicalDisk WHERE Name = '_Total'"
)

func (*windowsSource) platform() string { return report.PlatformWindows }

func (s *windowsSource) static(ctx context.Context) staticInfo {
	host := s.hostname()
	id, _ := s.machineGUID()
	if id = strings.TrimSpace(id); id == "" {
		id = host
	}
	info := staticInfo{id: id, hostname: strPtr(host)}

	var osi []win32OperatingSystem
	if s.query(ctx, wqlOS, &osi) && len(osi) > 0 {
		info.os = strPtr(strings.ReplaceAll(osi[0].Caption, "Microsoft ", ""))
	}
	var cpu []win32Processor
	if s.query(ctx, wqlProcessor, &cpu) && len(cpu) > 0 {
		info.cpuName = strPtr(cpu[0].Name)
		s.maxClock = float64(cpu[0].MaxClockSpeed)
	}
	var enc []win32SystemEnclosure
	if s.query(ctx, wqlEnclosure, &enc) {
		ff := report.FormFactorDesktop
		for _, e := range enc {
			for _, t := range e.ChassisTypes {
				// portable, laptop, notebook, hand held, sub notebook, docking station
				// (attached), tablet, convertible, detachable
				if slices.Contains([]int32{8, 9, 10, 11, 12, 14, 18, 21, 30, 31, 32}, t) {
					ff = report.FormFactorLaptop
				}
			}
		}
		info.formFactor = &ff
	}
	return info
}

// clockMHz is the max clock scaled by "% Processor Performance" (which tops 100 under
// turbo), as Task Manager shows it.
func (s *windowsSource) clockMHz(ctx context.Context) *float64 {
	var perf []win32ProcessorInfo
	if s.maxClock <= 0 || !s.query(ctx, wqlPerf, &perf) || len(perf) == 0 || perf[0].PercentProcessorPerformance == 0 {
		return nil
	}
	return report.Ptr(math.Round(s.maxClock * float64(perf[0].PercentProcessorPerformance) / 100))
}

// memory is ullTotalPhys − ullAvailPhys from GlobalMemoryStatusEx (via gopsutil). This is a
// deliberate difference from upstream's TotalVisibleMemorySize − FreePhysicalMemory: the
// totals differ by the memory firmware reserves.
func (s *windowsSource) memory(ctx context.Context) report.Memory {
	vm, err := s.ps.VirtualMemory(ctx)
	if err != nil || vm.Total == 0 {
		return report.Memory{}
	}
	return report.Memory{Total: report.Ptr(float64(vm.Total)), Used: report.Ptr(float64(vm.Total - vm.Available))}
}

// disks lists local fixed drives (DriveType 3) with their labels.
func (s *windowsSource) disks(ctx context.Context) []report.Disk {
	var ld []win32LogicalDisk
	if !s.query(ctx, wqlDisks, &ld) {
		return nil
	}
	out := []report.Disk{}
	for _, d := range ld {
		if d.Size == 0 {
			continue
		}
		out = append(out, report.Disk{
			Name:  strPtr(d.DeviceID),
			Label: strPtr(d.VolumeName),
			Total: report.Ptr(float64(d.Size)),
			Used:  report.Ptr(float64(d.Size - min(d.FreeSpace, d.Size))),
		})
	}
	return out
}

// diskIO reads the cumulative byte counters of all physical disks.
func (s *windowsSource) diskIO(ctx context.Context) (read, write uint64, ok bool) {
	var pd []win32PhysicalDisk
	if !s.query(ctx, wqlDiskIO, &pd) || len(pd) == 0 {
		return 0, 0, false
	}
	return pd[0].DiskReadBytesPersec, pd[0].DiskWriteBytesPersec, true
}

func (s *windowsSource) netIO(ctx context.Context) []ifaceBytes { return psNetIO(ctx, s.ps) }

func (*windowsSource) inNetTotal(string) bool { return true }

func (s *windowsSource) query(ctx context.Context, q string, dst any) bool {
	if err := s.wmi.Query(ctx, q, dst); err != nil {
		slog.Debug("collect: wmi", "query", q, "err", err)
		return false
	}
	return true
}
