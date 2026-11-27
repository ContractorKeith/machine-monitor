package collect

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/ContractorKeith/machine-monitor/internal/report"
)

type probeRunner struct {
	states []*probeState
}

type probeState struct {
	Probe
	n    int
	last Apply
	busy atomic.Bool
}

type probeResult struct {
	apply Apply
	err   error
}

func newProbeRunner(probes []Probe) *probeRunner {
	pr := &probeRunner{}
	for _, p := range probes {
		pr.states = append(pr.states, &probeState{Probe: p})
	}
	return pr
}

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
			s.busy.Store(false)
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

func wait(ctx context.Context, ch chan probeResult) probeResult {
	select {
	case res := <-ch:
		return res
	case <-ctx.Done():
		select {
		case res := <-ch:
			return res
		default:
			return probeResult{err: ctx.Err()}
		}
	}
}

func runProbe(ctx context.Context, run func(context.Context) (Apply, error)) (res probeResult) {
	defer func() {
		if v := recover(); v != nil {
			res = probeResult{err: fmt.Errorf("panic: %v", v)}
		}
	}()
	apply, err := run(ctx)
	return probeResult{apply: apply, err: err}
}

func applyProbe(apply Apply, r *report.Report) (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("panic: %v", v)
		}
	}()
	apply(r)
	return nil
}
