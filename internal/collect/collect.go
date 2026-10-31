// Package collect samples this machine into a report.Report.
//
// # Collector
//
// A Collector returns one fresh Report per call. The caller (TUI, agent, hub) calls it about
// every 2 s from a single goroutine. Rates (CPU usage, net, disk I/O) are deltas since the
// previous call, so the first call leaves them nil. New returns the collector for this OS.
// The core metrics (id, host, CPU, memory, disks, I/O, network, form factor, MACs) come from
// a per-OS source (oslinux.go, osmacos.go, oswindows.go) that reads through small seams:
// gopsutil, an fs.FS, recorded commands, WMI. Those files build on every OS so their tests
// run anywhere; collect_<os>.go only wires the real seams.
//
// # Probes
//
// GPU, sensor and AI metrics come from probes in the gpu, sensors and ai subpackages. Each
// one registers itself from an init function with Register, behind build tags for the OSes it
// supports, so adding a probe never edits this package. Package collect/all imports every
// probe package; binaries import collect/all for its side effects.
//
// The contract between a probe and the collector:
//
//   - Run does the slow work (exec, HTTP, sysfs reads) and returns an Apply func. The
//     collector runs probes concurrently, then calls each Apply in turn on the report being
//     built, sorted by probe name. Apply must be quick and only touch the fields the probe
//     owns; it never runs concurrently with another Apply.
//   - Every N means Run is called on every Nth sample (the 1st, N+1th, ...). On the samples
//     between, the collector calls the last Apply again, so slow-path values persist the
//     way upstream's agents keep $AI between checks. 0 and 1 both mean every sample.
//   - Run must return when ctx is done. The collector gives it a deadline (Timeout, or
//     DefaultTimeout when zero).
//   - Probes fail soft. An error, a timeout or a panic in Run means that probe contributes
//     nothing (its fields stay nil) until its next successful Run; the rest of the report
//     is unaffected. The collector may log the error, never return it.
//     A nil Apply with a nil error means "checked, nothing found".
//   - Missing tools and hardware are not errors: return (nil, nil) when nvidia-smi is not on
//     PATH or there is no hwmon device.
//   - Every exec has a timeout (from ctx) and a stripped environment.
//   - Apply appends to slices (GPUs, Temps, Fans) rather than replacing them, and fills a
//     scalar (CPU.Temp, Power) only if it is still nil, so two probes can share a field.
//     AI probes also append; each makes r.AI non-nil first, since nil AI means "not checked".
package collect

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/report"
)

// Collector samples this machine.
type Collector interface {
	Collect(ctx context.Context) (*report.Report, error)
}

// New returns the collector for this OS.
func New() Collector { return newOS() }

// DefaultTimeout bounds a probe's Run when Probe.Timeout is zero.
const DefaultTimeout = 1500 * time.Millisecond

// Apply writes a probe's last result into the report being built.
type Apply func(r *report.Report)

// Probe is one optional source of metrics. See the package doc for the contract.
type Probe struct {
	Name    string        // unique, e.g. "gpu/nvidia", "sensors/hwmon", "ai/ollama"
	Every   int           // run on every Nth sample; 0 or 1 = every sample
	Timeout time.Duration // deadline for Run; 0 = DefaultTimeout
	Run     func(ctx context.Context) (Apply, error)
}

var (
	mu     sync.Mutex
	probes = map[string]Probe{}
)

// Register adds a probe. Call it from an init function. It panics on a duplicate or
// incomplete probe, since that is a programming error.
func Register(p Probe) {
	if p.Name == "" || p.Run == nil {
		panic("collect: probe needs a Name and a Run func")
	}
	mu.Lock()
	defer mu.Unlock()
	if _, dup := probes[p.Name]; dup {
		panic(fmt.Sprintf("collect: probe %q registered twice", p.Name))
	}
	probes[p.Name] = p
}

// Probes returns every registered probe, sorted by name.
func Probes() []Probe {
	mu.Lock()
	defer mu.Unlock()
	out := make([]Probe, 0, len(probes))
	for _, p := range probes {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
