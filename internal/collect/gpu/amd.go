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

			if readText(filepath.Join(h, "power1_label")) == "PPT" {
				g.Integrated = true
				continue
			}

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
