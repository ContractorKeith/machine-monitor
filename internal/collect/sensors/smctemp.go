package sensors

import (
	"context"
	"strings"

	"github.com/ContractorKeith/machine-monitor/internal/report"
)

// readSmctemp asks smctemp (optional, `brew install smctemp`) for the CPU (-c) and GPU (-g)
// temperatures, as agent-macos.sh does. An empty path means smctemp isn't installed.
func readSmctemp(ctx context.Context, path string) (reading, error) {
	var s reading
	if path == "" {
		return s, nil
	}
	cpu, err := command(ctx, path, "-c")
	if err != nil {
		return s, err
	}
	s.cpuTemp = parseSmctemp(cpu)
	if gpu, err := command(ctx, path, "-g"); err == nil {
		s.gpuTemp = parseSmctemp(gpu)
	}
	for _, t := range []struct {
		name string
		v    *float64
	}{{"CPU", s.cpuTemp}, {"GPU", s.gpuTemp}} {
		if t.v != nil {
			s.temps = append(s.temps, report.Temp{Name: report.Ptr(t.name), Parent: report.Ptr("smctemp"), Value: report.Ptr(*t.v)})
		}
	}
	return s, nil
}

// parseSmctemp reads the temperature on smctemp's first line. smctemp prints 0 or nothing
// when the sensor is unsupported, which is unknown, not 0 °C.
func parseSmctemp(out []byte) *float64 {
	line, _, _ := strings.Cut(string(out), "\n")
	return temp(num(line))
}
