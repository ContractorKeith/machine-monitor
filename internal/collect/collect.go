package collect

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/report"
)

type Collector interface {
	Collect(ctx context.Context) (*report.Report, error)
}

func New() Collector { return newOS() }

func DeviceID(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, staticTimeout)
	defer cancel()
	id := newSource().deviceID(ctx)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return id, nil
}

const DefaultTimeout = 1500 * time.Millisecond

type Apply func(r *report.Report)

type Probe struct {
	Name    string
	Every   int
	Timeout time.Duration
	Run     func(ctx context.Context) (Apply, error)
}

var (
	mu     sync.Mutex
	probes = map[string]Probe{}
)

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
