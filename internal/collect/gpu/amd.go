package gpu

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"

	"github.com/ContractorKeith/machine-monitor/internal/report"
)

var cardName = regexp.MustCompile(`^card(\d+)$`)

// readAMD reads AMD GPUs from sysfs under sys (normally "/sys"): every DRM card whose
// device exposes gpu_busy_percent, which only the amdgpu driver does.
func readAMD(sys string) []report.GPU {
	entries, err := os.ReadDir(filepath.Join(sys, "class", "drm"))
	if err != nil {
		return nil
	}
	var cards []int
	for _, e := range entries {
		if m := cardName.FindStringSubmatch(e.Name()); m != nil {
			n, _ := strconv.Atoi(m[1])
			cards = append(cards, n)
		}
	}
	sort.Ints(cards)

	var gpus []report.GPU
	for _, n := range cards {
		dev := filepath.Join(sys, "class", "drm", "card"+strconv.Itoa(n), "device")
		busy := readNum(filepath.Join(dev, "gpu_busy_percent"))
		if busy == nil {
			continue
		}
		g := report.GPU{
			Name:     report.Ptr("AMD GPU"),
			Vendor:   report.Ptr("amd"),
			Usage:    busy,
			MemUsed:  readNum(filepath.Join(dev, "mem_info_vram_used")),
			MemTotal: readNum(filepath.Join(dev, "mem_info_vram_total")),
		}
		hwmons, _ := filepath.Glob(filepath.Join(dev, "hwmon", "hwmon*"))
		for _, h := range hwmons {
			if t := scale(readNum(filepath.Join(h, "temp1_input")), 1e-3); t != nil {
				g.Temp = t
			}
			// An APU's amdgpu hwmon labels its power PPT: the whole package, CPU cores
			// included. That is CPU power (the sensors probe reports it), not GPU power, and
			// upstream's agent.sh reads only power1_average, so it reports null here too.
			// The name marks the GPU integrated so the hub adds no discrete-GPU estimate
			// on top of the package power (power.IntegratedName).
			if readText(filepath.Join(h, "power1_label")) == "PPT" {
				g.Name = report.Ptr("AMD Integrated GPU")
				continue
			}
			// power1_average on older kernels, power1_input on newer ones; microwatts.
			p := readNum(filepath.Join(h, "power1_average"))
			if p == nil {
				p = readNum(filepath.Join(h, "power1_input"))
			}
			if p := scale(p, 1e-6); p != nil {
				g.Power = p
			}
		}
		gpus = append(gpus, g)
	}
	return gpus
}
