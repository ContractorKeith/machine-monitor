package sensors

import (
	"math"
	"regexp"

	"github.com/ContractorKeith/machine-monitor/internal/report"
)

type lhmSensor struct {
	Name       string
	SensorType string
	Parent     string
	Value      float32
}

type thermalZone struct {
	Name                     string
	HighPrecisionTemperature uint32
}

var (
	lhmCPU        = regexp.MustCompile(`(?i)cpu`)
	lhmGPU        = regexp.MustCompile(`(?i)gpu`)
	lhmPackage    = regexp.MustCompile(`(?i)Package`)
	lhmCPUTemp    = regexp.MustCompile(`(?i)Package|Tctl|Tdie|CPU Total|Core Average`)
	lhmGPUTempKey = regexp.MustCompile(`(?i)GPU Core|GPU Temperature`)
)

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

func fromACPI(zones []thermalZone) *float64 {
	for _, z := range zones {
		if z.HighPrecisionTemperature > 0 {
			return temp(report.Ptr(float64(z.HighPrecisionTemperature)/10 - 273.15))
		}
	}
	return nil
}
