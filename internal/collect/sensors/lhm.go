package sensors

import (
	"math"
	"regexp"

	"github.com/ContractorKeith/machine-monitor/internal/report"
)

// WMI rows for the Windows sensors probe. Field names are WMI property names.

// lhmSensor is a Sensor row from root\LibreHardwareMonitor or root\OpenHardwareMonitor,
// which those apps publish while running.
type lhmSensor struct {
	Name       string  // "CPU Package", "Core (Tctl/Tdie)", "GPU Core", "Fan #1"
	SensorType string  // "Temperature", "Fan", "Power", "Load", ...
	Parent     string  // hardware identifier, e.g. "/amdcpu/0", "/gpu-nvidia/0"
	Value      float32 // °C, RPM, W
}

// thermalZone is a Win32_PerfFormattedData_Counters_ThermalZoneInformation row.
type thermalZone struct {
	Name                     string
	HighPrecisionTemperature uint32 // tenths of a kelvin
}

// PowerShell's -match in agent.ps1 is case-insensitive; these are too.
var (
	lhmCPU        = regexp.MustCompile(`(?i)cpu`)
	lhmGPU        = regexp.MustCompile(`(?i)gpu`)
	lhmPackage    = regexp.MustCompile(`(?i)Package`)
	lhmCPUTemp    = regexp.MustCompile(`(?i)Package|Tctl|Tdie|CPU Total|Core Average`)
	lhmGPUTempKey = regexp.MustCompile(`(?i)GPU Core|GPU Temperature`)
)

// fromLHM turns hardware-monitor sensors into a reading, a port of agent.ps1: every
// temperature, every spinning fan, the CPU package power and temperature, and a GPU
// temperature for GPUs that have none.
func fromLHM(sensors []lhmSensor) reading {
	var s reading
	for _, x := range sensors {
		v := float64(x.Value)
		switch x.SensorType {
		case "Temperature":
			t := temp(&v)
			if t == nil {
				continue
			}
			s.temps = append(s.temps, report.Temp{Name: report.Ptr(x.Name), Parent: report.Ptr(x.Parent), Value: t})
			if s.cpuTemp == nil && lhmCPU.MatchString(x.Parent) && lhmCPUTemp.MatchString(x.Name) {
				s.cpuTemp = t
			}
			if s.gpuTemp == nil && lhmGPU.MatchString(x.Parent) && lhmGPUTempKey.MatchString(x.Name) {
				s.gpuTemp = t
			}
		case "Fan":
			if v > 0 {
				s.fans = append(s.fans, report.Fan{Name: report.Ptr(x.Name), RPM: report.Ptr(math.Round(v))})
			}
		case "Power":
			if s.cpuPower == nil && v > 0 && lhmCPU.MatchString(x.Parent) && lhmPackage.MatchString(x.Name) {
				s.cpuPower = report.Ptr(round1(v))
			}
		}
	}
	return s
}

// fromACPI is agent.ps1's CPU temperature fallback without a hardware monitor: the first
// thermal zone with a reading, converted from tenths of a kelvin.
func fromACPI(zones []thermalZone) *float64 {
	for _, z := range zones {
		if z.HighPrecisionTemperature > 0 {
			return temp(report.Ptr(float64(z.HighPrecisionTemperature)/10 - 273.15))
		}
	}
	return nil
}
