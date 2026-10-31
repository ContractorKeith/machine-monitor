package collect

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/ContractorKeith/machine-monitor/internal/report"
)

// probeRunner runs probes once per sample, following the contract in the package doc.
// It is driven from the collector's single goroutine; only Run goroutines run concurrently.
type probeRunner struct {
	states []*probeState // sorted by name, so Applies run in name order
}

type probeState struct {
	Probe
	n    int         // samples seen
	last Apply       // result of the last successful Run, reapplied between runs
	busy atomic.Bool // a Run is still going after its deadline; don't start another
}

type probeResult struct {
	apply Apply
	err   error
}

// newProbeRunner takes probes sorted by name, as Probes returns them.
func newProbeRunner(probes []Probe) *probeRunner {
	pr := &probeRunner{}
	for _, p := range probes {
		pr.states = append(pr.states, &probeState{Probe: p})
	}
	return pr
}

// start launches every probe due this sample and returns finish, which waits for them
// (each up to its deadline) and applies every probe's latest result to r in name order.
func (pr *probeRunner) start(ctx context.Context) (finish func(r *report.Report)) {
	type pending struct {
		ch     chan probeResult
		ctx    context.Context
		cancel context.CancelFunc
	}
	runs := make([]*pending, len(pr.states))
	for i, s := range pr.states {
		every := max(s.Every, 1)
		due := s.n%every == 0
		s.n++
		if !due {
			continue
		}
		if s.busy.Load() {
			// The previous Run ignored its deadline and is still going.
			s.last = nil
			slog.Debug("collect: probe still running from an earlier sample", "probe", s.Name)
			continue
		}
		timeout := s.Timeout
		if timeout <= 0 {
			timeout = DefaultTimeout
		}
		pctx, cancel := context.WithTimeout(ctx, timeout)
		p := &pending{ch: make(chan probeResult, 1), ctx: pctx, cancel: cancel}
		runs[i] = p
		s.busy.Store(true)
		go func() {
			res := runProbe(pctx, s.Run)
			s.busy.Store(false) // before the send, so the next sample never sees a stale busy
			p.ch <- res
		}()
	}

	return func(r *report.Report) {
		for i, s := range pr.states {
			if p := runs[i]; p != nil {
				res := wait(p.ctx, p.ch)
				p.cancel()
				if res.err != nil {
					s.last = nil
					slog.Debug("collect: probe failed", "probe", s.Name, "err", res.err)
				} else {
					s.last = res.apply
				}
			}
			if s.last != nil {
				if err := applyProbe(s.last, r); err != nil {
					s.last = nil
					slog.Debug("collect: probe apply failed", "probe", s.Name, "err", err)
				}
			}
		}
	}
}

// wait returns a probe's result, or a timeout error once its deadline passes.
func wait(ctx context.Context, ch chan probeResult) probeResult {
	select {
	case res := <-ch:
		return res
	case <-ctx.Done():
		select {
		case res := <-ch: // finished at the deadline
			return res
		default:
			return probeResult{err: ctx.Err()}
		}
	}
}

// runProbe calls run, turning a panic into an error.
func runProbe(ctx context.Context, run func(context.Context) (Apply, error)) (res probeResult) {
	defer func() {
		if v := recover(); v != nil {
			res = probeResult{err: fmt.Errorf("panic: %v", v)}
		}
	}()
	apply, err := run(ctx)
	return probeResult{apply: apply, err: err}
}

// applyProbe calls apply, turning a panic into an error.
func applyProbe(apply Apply, r *report.Report) (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("panic: %v", v)
		}
	}()
	apply(r)
	return nil
}
