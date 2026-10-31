//go:build windows

package sensors

import (
	"context"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/collect"
	"github.com/ContractorKeith/machine-monitor/internal/collect/wmiq"
	"github.com/ContractorKeith/machine-monitor/internal/report"
)

func init() {
	// WMI is slow, so check every 5th sample with room to finish; the collector repeats the
	// last result in between.
	collect.Register(collect.Probe{Name: "sensors/windows", Every: 5, Timeout: 10 * time.Second, Run: func(ctx context.Context) (collect.Apply, error) {
		return readWindows(ctx, queryLHM, queryThermalZones).apply(), nil
	}})
}

// readWindows reads LibreHardwareMonitor (or OpenHardwareMonitor) sensors and falls back to
// the ACPI thermal zone for the CPU temperature, as agent.ps1 does. The two queries are
// parameters so tests can feed recorded rows.
func readWindows(ctx context.Context, lhm func(context.Context) []lhmSensor, zones func(context.Context) []thermalZone) reading {
	s := fromLHM(lhm(ctx))
	if s.cpuTemp == nil {
		if t := fromACPI(zones(ctx)); t != nil {
			s.cpuTemp, s.tempSource = t, report.Ptr("acpi")
		}
	}
	return s
}

// queryLHM returns the sensors of the first hardware monitor that is running, or nil.
func queryLHM(ctx context.Context) []lhmSensor {
	for _, ns := range []string{`root\LibreHardwareMonitor`, `root\OpenHardwareMonitor`} {
		if ctx.Err() != nil {
			return nil
		}
		var rows []lhmSensor
		if err := wmiq.Query(ctx, "SELECT Name, SensorType, Parent, Value FROM Sensor", ns, &rows); err == nil && len(rows) > 0 {
			return rows
		}
	}
	return nil
}

// queryThermalZones returns the ACPI thermal zones, or nil.
func queryThermalZones(ctx context.Context) []thermalZone {
	var rows []thermalZone
	_ = wmiq.Query(ctx, "SELECT Name, HighPrecisionTemperature FROM Win32_PerfFormattedData_Counters_ThermalZoneInformation", `root\cimv2`, &rows)
	return rows
}
