package sensors

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/report"
)

var cpuChips = map[string]bool{"coretemp": true, "k10temp": true, "zenpower": true, "cpu_thermal": true, "soc_thermal": true}

var (
	hwmonDir  = regexp.MustCompile(`^hwmon(\d+)$`)
	zoneDir   = regexp.MustCompile(`^thermal_zone(\d+)$`)
	inputFile = regexp.MustCompile(`^(temp|fan)(\d+)_input$`)
	raplDir   = regexp.MustCompile(`^intel-rapl:(\d+)$`)
)

func numbered(dir string, re *regexp.Regexp) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	type item struct {
		n    int
		name string
	}
	var items []item
	for _, e := range entries {
		if m := re.FindStringSubmatch(e.Name()); m != nil {
			n, _ := strconv.Atoi(m[1])
			items = append(items, item{n, e.Name()})
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].n < items[j].n })
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = filepath.Join(dir, it.name)
	}
	return out
}

func readHwmon(sys string) (temps []report.Temp, fans []report.Fan, cpuTemp *float64) {
	seen := map[string]int{}
	for _, h := range numbered(filepath.Join(sys, "class", "hwmon"), hwmonDir) {
		chip := readString(filepath.Join(h, "name"))
		if chip == "" {
			continue
		}
		if cpuTemp == nil && cpuChips[chip] {
			cpuTemp = temp(scale(readNum(filepath.Join(h, "temp1_input")), 1e-3))
		}
		seen[chip]++
		parent := chip
		if seen[chip] > 1 {
			parent += " #" + strconv.Itoa(seen[chip])
		}

		entries, _ := os.ReadDir(h)
		type sensor struct {
			kind string
			n    int
		}
		var sensors []sensor
		for _, e := range entries {
			if m := inputFile.FindStringSubmatch(e.Name()); m != nil {
				n, _ := strconv.Atoi(m[2])
				sensors = append(sensors, sensor{m[1], n})
			}
		}
		sort.Slice(sensors, func(i, j int) bool {
			if sensors[i].kind != sensors[j].kind {
				return sensors[i].kind > sensors[j].kind
			}
			return sensors[i].n < sensors[j].n
		})
		for _, s := range sensors {
			base := filepath.Join(h, s.kind+strconv.Itoa(s.n))
			label := readString(base + "_label")
			v := readNum(base + "_input")
			switch s.kind {
			case "temp":
				if label == "" {
					label = "Temp " + strconv.Itoa(s.n)
				}
				if t := temp(scale(v, 1e-3)); t != nil {
					temps = append(temps, report.Temp{Name: report.Ptr(label), Parent: report.Ptr(parent), Value: t})
				}
			case "fan":
				if label == "" {
					label = "Fan " + strconv.Itoa(s.n)
				}
				if v != nil && *v > 0 {
					fans = append(fans, report.Fan{Name: report.Ptr(label), RPM: report.Ptr(*v)})
				}
			}
		}
	}
	return temps, fans, cpuTemp
}

func thermalZoneTemp(sys string) (t *float64, source *string) {
	var best *float64
	var bestType string
	for _, z := range numbered(filepath.Join(sys, "class", "thermal"), zoneDir) {
		typ := readString(filepath.Join(z, "type"))
		if typ != "x86_pkg_temp" && typ != "acpitz" && !strings.HasPrefix(typ, "cpu") && !strings.HasPrefix(typ, "soc") {
			continue
		}
		v := temp(scale(readNum(filepath.Join(z, "temp")), 1e-3))
		if v != nil && (best == nil || *v > *best) {
			best, bestType = v, typ
		}
	}
	if best != nil && bestType == "acpitz" {
		source = report.Ptr("acpi")
	}
	return best, source
}

func gracePower(sys string) *float64 {
	var sum float64
	var n int
	for _, h := range numbered(filepath.Join(sys, "class", "hwmon"), hwmonDir) {
		if readString(filepath.Join(h, "name")) != "power_meter" ||
			!strings.HasPrefix(readString(filepath.Join(h, "power1_oem_info")), "Grace Power Socket") {
			continue
		}
		if v := readNum(filepath.Join(h, "power1_average")); v != nil {
			sum += *v
			n++
		}
	}
	if n == 0 {
		return nil
	}
	return report.Ptr(round1(sum / 1e6))
}

func apuPower(sys string) *float64 {
	for _, h := range numbered(filepath.Join(sys, "class", "hwmon"), hwmonDir) {
		if readString(filepath.Join(h, "name")) != "amdgpu" || readString(filepath.Join(h, "power1_label")) != "PPT" {
			continue
		}
		v := readNum(filepath.Join(h, "power1_average"))
		if v == nil {
			v = readNum(filepath.Join(h, "power1_input"))
		}
		if v != nil && *v > 0 {
			return report.Ptr(round1(*v / 1e6))
		}
	}
	return nil
}

type raplMeter struct {
	mu   sync.Mutex
	prev map[string]raplCounter
	at   time.Time
}

type raplCounter struct{ energy, max float64 }

func (m *raplMeter) watts(sys string, now time.Time) *float64 {
	cur := map[string]raplCounter{}
	for _, z := range numbered(filepath.Join(sys, "class", "powercap"), raplDir) {

		if name := readString(filepath.Join(z, "name")); name != "" && !strings.HasPrefix(name, "package") {
			continue
		}
		e := readNum(filepath.Join(z, "energy_uj"))
		if e == nil {
			continue
		}
		c := raplCounter{energy: *e}
		if mx := readNum(filepath.Join(z, "max_energy_range_uj")); mx != nil {
			c.max = *mx
		}
		cur[filepath.Base(z)] = c
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	prev, at := m.prev, m.at
	m.prev, m.at = cur, now
	sec := now.Sub(at).Seconds()
	if len(prev) == 0 || len(cur) == 0 || sec <= 0 {
		return nil
	}
	var joules float64
	var n int
	for zone, c := range cur {
		p, ok := prev[zone]
		if !ok {
			continue
		}
		d := c.energy - p.energy
		if d < 0 {
			if c.max <= 0 {
				continue
			}
			d += c.max
		}
		joules += d / 1e6
		n++
	}
	if n == 0 {
		return nil
	}
	return report.Ptr(round1(joules / sec))
}

func readLinux(sys string, rapl *raplMeter, now time.Time) reading {
	var s reading
	s.temps, s.fans, s.cpuTemp = readHwmon(sys)
	if s.cpuTemp == nil {
		s.cpuTemp, s.tempSource = thermalZoneTemp(sys)
	}

	raplW := rapl.watts(sys, now)
	if s.cpuPower = gracePower(sys); s.cpuPower == nil {
		s.cpuPower = raplW
	}
	if s.cpuPower == nil {
		s.cpuPower = apuPower(sys)
	}
	return s
}

func scale(v *float64, f float64) *float64 {
	if v == nil {
		return nil
	}
	return report.Ptr(*v * f)
}
