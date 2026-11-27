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

	collect.Register(collect.Probe{Name: "sensors/windows", Every: 5, Timeout: 10 * time.Second, Run: func(ctx context.Context) (collect.Apply, error) {
		return readWindows(ctx, queryLHM, queryThermalZones).apply(), nil
	}})
}

func readWindows(ctx context.Context, lhm func(context.Context) []lhmSensor, zones func(context.Context) []thermalZone) reading {
	s := fromLHM(lhm(ctx))
	if s.cpuTemp == nil {
		if t := fromACPI(zones(ctx)); t != nil {
			s.cpuTemp, s.tempSource = t, report.Ptr("acpi")
		}
	}
	return s
}

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

func queryThermalZones(ctx context.Context) []thermalZone {
	var rows []thermalZone
	_ = wmiq.Query(ctx, "SELECT Name, HighPrecisionTemperature FROM Win32_PerfFormattedData_Counters_ThermalZoneInformation", `root\cimv2`, &rows)
	return rows
}
