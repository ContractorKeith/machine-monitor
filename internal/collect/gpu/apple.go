package gpu

import (
	"regexp"
	"strings"

	"github.com/ContractorKeith/machine-monitor/internal/report"
)

var (
	ioregModel = regexp.MustCompile(`"model" = "([^"]*)"`)
	// Device Utilization % on Apple Silicon, GPU Activity(%) on older Intel/AMD Macs.
	ioregUtil    = []*regexp.Regexp{regexp.MustCompile(`"Device Utilization %"=([0-9]+)`), regexp.MustCompile(`"GPU Activity\(%\)"=([0-9]+)`)}
	ioregMemUsed = regexp.MustCompile(`"In use system memory"=([0-9]+)`)
)

// parseIoreg turns `ioreg -r -d 1 -w 0 -c IOAccelerator` output into GPUs, one per "+-o"
// entry that has PerformanceStatistics. Name is nil when ioreg has no model; the caller
// falls back to the CPU name. Memory total is unknown (unified memory).
func parseIoreg(out []byte) []report.GPU {
	var gpus []report.GPU
	var cur *report.GPU
	var name string
	flush := func() {
		if cur != nil {
			if name != "" {
				cur.Name = report.Ptr(name)
			}
			gpus = append(gpus, *cur)
		}
		cur, name = nil, ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.Contains(line, "+-o"):
			flush()
		case ioregModel.MatchString(line):
			name = strings.ReplaceAll(ioregModel.FindStringSubmatch(line)[1], `\`, "")
		case strings.Contains(line, `"PerformanceStatistics"`):
			g := report.GPU{Vendor: report.Ptr("apple")}
			for _, re := range ioregUtil {
				if m := re.FindStringSubmatch(line); m != nil {
					g.Usage = num(m[1])
					break
				}
			}
			if m := ioregMemUsed.FindStringSubmatch(line); m != nil {
				g.MemUsed = num(m[1])
			}
			cur = &g
		}
	}
	flush()
	return gpus
}

// appleNames fills missing GPU names with "<CPU name> GPU", as upstream does, or "Apple GPU".
func appleNames(gpus []report.GPU, cpuName *string) {
	fallback := "Apple GPU"
	if cpuName != nil && *cpuName != "" {
		fallback = *cpuName + " GPU"
	}
	for i := range gpus {
		if gpus[i].Name == nil {
			gpus[i].Name = report.Ptr(fallback)
		}
	}
}
