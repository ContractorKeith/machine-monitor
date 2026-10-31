//go:build windows

package sensors

import (
	"context"
	"errors"

	"github.com/ContractorKeith/machine-monitor/internal/collect"
	"github.com/ContractorKeith/machine-monitor/internal/report"
	"github.com/yusufpapurcu/wmi"
)

func init() {
	collect.Register(collect.Probe{Name: "sensors/windows", Run: func(ctx context.Context) (collect.Apply, error) {
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
		var rows []lhmSensor
		if err := query(ctx, "SELECT Name, SensorType, Parent, Value FROM Sensor", ns, &rows); err == nil && len(rows) > 0 {
			return rows
		}
	}
	return nil
}

// queryThermalZones returns the ACPI thermal zones, or nil.
func queryThermalZones(ctx context.Context) []thermalZone {
	var rows []thermalZone
	_ = query(ctx, "SELECT Name, HighPrecisionTemperature FROM Win32_PerfFormattedData_Counters_ThermalZoneInformation", `root\cimv2`, &rows)
	return rows
}

// query runs a WMI query, giving up when ctx is done. wmi has no cancellation, so a hung
// query finishes in the background into a slice nobody reads.
func query[T any](ctx context.Context, q, namespace string, dst *[]T) error {
	var rows []T
	done := make(chan error, 1)
	go func() { done <- wmi.QueryNamespace(q, &rows, namespace) }()
	select {
	case err := <-done:
		var mismatch *wmi.ErrFieldMismatch
		if err != nil && !errors.As(err, &mismatch) {
			return err
		}
		*dst = rows
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
