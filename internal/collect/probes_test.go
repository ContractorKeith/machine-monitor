package collect

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/report"
)

// sampleProbes runs one sample through pr and returns the report.
func sampleProbes(pr *probeRunner) *report.Report {
	r := &report.Report{}
	pr.start(context.Background())(r)
	return r
}

// gpuNamed appends a GPU with the given name.
func gpuNamed(name string) Apply {
	return func(r *report.Report) { r.GPUs = append(r.GPUs, report.GPU{Name: report.Ptr(name)}) }
}

func gpuNames(r *report.Report) []string {
	var out []string
	for _, g := range r.GPUs {
		out = append(out, *g.Name)
	}
	return out
}

func TestProbesRunConcurrentlyAndApplyInNameOrder(t *testing.T) {
	// Each Run waits for all three to start, so this deadlocks unless they run concurrently.
	var started sync.WaitGroup
	started.Add(3)
	probe := func(name string, delay time.Duration) Probe {
		return Probe{Name: name, Run: func(ctx context.Context) (Apply, error) {
			started.Done()
			started.Wait()
			time.Sleep(delay)
			return gpuNamed(name), nil
		}}
	}
	pr := newProbeRunner([]Probe{probe("a", 30*time.Millisecond), probe("b", 0), probe("c", 10*time.Millisecond)})
	got := gpuNames(sampleProbes(pr))
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Errorf("GPUs = %v, want [a b c]", got)
	}
}

func TestProbeEveryReappliesLastResult(t *testing.T) {
	var runs atomic.Int32
	pr := newProbeRunner([]Probe{{Name: "ai/slow", Every: 3, Run: func(context.Context) (Apply, error) {
		n := runs.Add(1)
		return gpuNamed(string(rune('0' + n))), nil
	}}})
	var got []string
	for range 7 {
		got = append(got, gpuNames(sampleProbes(pr))...)
	}
	// Runs on samples 1, 4 and 7; samples between reapply the last result.
	want := []string{"1", "1", "1", "2", "2", "2", "3"}
	if runs.Load() != 3 || len(got) != len(want) {
		t.Fatalf("runs = %d, GPUs = %v; want 3 runs and %v", runs.Load(), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sample %d: GPU %q, want %q", i+1, got[i], want[i])
		}
	}
}

func TestProbeFailuresAreSoft(t *testing.T) {
	var fail atomic.Bool
	var calls atomic.Int32
	pr := newProbeRunner([]Probe{
		{Name: "a/panics", Run: func(context.Context) (Apply, error) { panic("boom") }},
		{Name: "b/flaky", Every: 2, Run: func(context.Context) (Apply, error) {
			calls.Add(1)
			if fail.Load() {
				return nil, errors.New("nvidia-smi: exit 9")
			}
			return gpuNamed("flaky"), nil
		}},
		{Name: "c/apply-panics", Run: func(context.Context) (Apply, error) {
			return func(*report.Report) { panic("bad apply") }, nil
		}},
		{Name: "d/ok", Run: func(context.Context) (Apply, error) { return gpuNamed("ok"), nil }},
		{Name: "e/nothing", Run: func(context.Context) (Apply, error) { return nil, nil }},
	})

	if got := gpuNames(sampleProbes(pr)); len(got) != 2 || got[0] != "flaky" || got[1] != "ok" {
		t.Errorf("sample 1 GPUs = %v, want [flaky ok]", got)
	}
	if got := gpuNames(sampleProbes(pr)); len(got) != 2 {
		t.Errorf("sample 2 GPUs = %v, want flaky reapplied", got)
	}
	// A failed run drops the probe's last result until it next succeeds.
	fail.Store(true)
	if got := gpuNames(sampleProbes(pr)); len(got) != 1 || got[0] != "ok" {
		t.Errorf("sample 3 GPUs = %v, want [ok]", got)
	}
	if got := gpuNames(sampleProbes(pr)); len(got) != 1 {
		t.Errorf("sample 4 GPUs = %v, want [ok] (nothing to reapply)", got)
	}
	fail.Store(false)
	if got := gpuNames(sampleProbes(pr)); len(got) != 2 {
		t.Errorf("sample 5 GPUs = %v, want flaky back", got)
	}
	if calls.Load() != 3 {
		t.Errorf("flaky ran %d times, want 3 (samples 1, 3, 5)", calls.Load())
	}
}

func TestProbeDeadline(t *testing.T) {
	var sawDeadline atomic.Bool
	release := make(chan struct{})
	var stubbornRuns atomic.Int32
	pr := newProbeRunner([]Probe{
		{Name: "polite", Timeout: 20 * time.Millisecond, Run: func(ctx context.Context) (Apply, error) {
			_, ok := ctx.Deadline()
			sawDeadline.Store(ok)
			<-ctx.Done()
			return gpuNamed("late"), ctx.Err()
		}},
		// Ignores ctx: the collector must stop waiting at the deadline, and not start a
		// second Run while the first is still going.
		{Name: "stubborn", Timeout: 20 * time.Millisecond, Run: func(context.Context) (Apply, error) {
			stubbornRuns.Add(1)
			<-release
			return gpuNamed("stubborn"), nil
		}},
	})

	start := time.Now()
	if got := gpuNames(sampleProbes(pr)); len(got) != 0 {
		t.Errorf("GPUs = %v, want none from timed-out probes", got)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("sample took %v, want about the 20 ms deadline", d)
	}
	if !sawDeadline.Load() {
		t.Error("Run's ctx should carry a deadline")
	}
	sampleProbes(pr)
	if n := stubbornRuns.Load(); n != 1 {
		t.Errorf("stubborn ran %d times while still busy, want 1", n)
	}

	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for stubbornRuns.Load() < 2 && time.Now().Before(deadline) {
		sampleProbes(pr)
	}
	if stubbornRuns.Load() < 2 {
		t.Error("stubborn never ran again after it returned")
	}
}

func TestProbeDefaultTimeout(t *testing.T) {
	pr := newProbeRunner([]Probe{{Name: "x", Run: func(ctx context.Context) (Apply, error) {
		dl, ok := ctx.Deadline()
		if !ok || time.Until(dl) > DefaultTimeout || time.Until(dl) < DefaultTimeout-time.Second {
			return nil, errors.New("want DefaultTimeout")
		}
		return gpuNamed("x"), nil
	}}})
	if got := gpuNames(sampleProbes(pr)); len(got) != 1 {
		t.Error("probe without a Timeout should get DefaultTimeout")
	}
}

func TestCollectRunsRegisteredProbes(t *testing.T) {
	Register(Probe{Name: "test/collect", Run: func(context.Context) (Apply, error) {
		return func(r *report.Report) {
			if r.CPU.Temp == nil {
				r.CPU.Temp = report.Ptr(55.5)
			}
		}, nil
	}})
	t.Cleanup(func() {
		mu.Lock()
		delete(probes, "test/collect")
		mu.Unlock()
	})
	r, err := New().Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.CPU.Temp == nil || *r.CPU.Temp != 55.5 {
		t.Errorf("CPU.Temp = %v, want the probe's 55.5", r.CPU.Temp)
	}
}
