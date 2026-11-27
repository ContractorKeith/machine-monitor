package gpu

import (
	"math"
	"regexp"
	"sort"
	"strings"

	"github.com/ContractorKeith/machine-monitor/internal/report"
)

type gpuEngine struct {
	Name                  string
	UtilizationPercentage uint64
}

type gpuAdapterMemory struct {
	Name           string
	DedicatedUsage uint64
}

type videoController struct {
	Name       string
	AdapterRAM uint32
}

var (
	luidRe       = regexp.MustCompile(`(?i)luid_(0x[0-9a-f]+_0x[0-9a-f]+)`)
	virtualVideo = regexp.MustCompile(`(?i)Basic|Virtual|Remote|Mirage|Parsec`)
)

func genericGPUs(engines []gpuEngine, mems []gpuAdapterMemory, vcs []videoController) []report.GPU {
	var real []videoController
	for _, vc := range vcs {
		if !virtualVideo.MatchString(vc.Name) {
			real = append(real, vc)
		}
	}

	byLUID := map[string]float64{}
	for _, e := range engines {
		if !strings.Contains(strings.ToLower(e.Name), "engtype_3d") {
			continue
		}
		if m := luidRe.FindStringSubmatch(e.Name); m != nil {
			byLUID[strings.ToLower(m[1])] += float64(e.UtilizationPercentage)
		}
	}
	luids := make([]string, 0, len(byLUID))
	for l := range byLUID {
		luids = append(luids, l)
	}
	sort.Strings(luids)

	var gpus []report.GPU
	for _, luid := range luids {
		var mem *gpuAdapterMemory
		for i := range mems {
			if strings.Contains(strings.ToLower(mems[i].Name), luid) {
				mem = &mems[i]
				break
			}
		}

		if mem == nil || (byLUID[luid] == 0 && mem.DedicatedUsage < 1<<20) {
			continue
		}
		g := report.GPU{
			Name:    report.Ptr("GPU"),
			Vendor:  report.Ptr("generic"),
			Usage:   report.Ptr(math.Min(100, math.Round(byLUID[luid]*10)/10)),
			MemUsed: report.Ptr(float64(mem.DedicatedUsage)),
		}
		if len(real) > 0 {
			vc := real[min(len(gpus), len(real)-1)]
			g.Name = report.Ptr(vc.Name)
			if vc.AdapterRAM > 0 {
				g.MemTotal = report.Ptr(float64(vc.AdapterRAM))
			}
		}
		gpus = append(gpus, g)
	}
	return gpus
}
