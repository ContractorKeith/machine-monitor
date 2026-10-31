package agent

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/collect"
	"github.com/ContractorKeith/machine-monitor/internal/report"
)

// Defaults for Options.
const (
	DefaultInterval   = 2 * time.Second
	DefaultMaxBackoff = 30 * time.Second
	MaxProcesses      = 120 // the hub keeps no more than this many rows (normalizeProcesses)
)

// Options configures Run. HubURL, Key and Collector are required.
type Options struct {
	HubURL    string // origin, as NormalizeURL returns
	Key       string
	Collector collect.Collector
	// Procs lists this machine's processes, sent only while the hub asks (reply procs:true).
	// nil never sends them.
	Procs func(ctx context.Context) ([]report.Process, error)
	// AllowPowerActions lets the hub sleep, restart or shut down this machine with signed
	// commands (actions.go). Off by default.
	AllowPowerActions bool
	// Exec runs a verified power action; SystemExecutor when nil.
	Exec Executor
	// Now is the clock commands are checked against; time.Now when nil.
	Now func() time.Time
	// Version goes into each report as agent: "machine-monitor/<version>".
	Version string
	// Interval between reports; DefaultInterval when zero.
	Interval time.Duration
	// MaxBackoff caps the wait between tries while the hub is down; DefaultMaxBackoff when zero.
	MaxBackoff time.Duration
	// HTTP is the client for hub calls; NewHTTPClient() when nil.
	HTTP *http.Client
	// Logf logs state changes (connected, hub down, key refused); dropped when nil.
	Logf func(format string, args ...any)
}

// reply is the body of a successful POST /api/report.
type reply struct {
	OK       bool      `json:"ok"`
	Procs    bool      `json:"procs"`
	Commands []Command `json:"commands"`
}

// Run reports to the hub every Interval until ctx is done. While the hub is down or
// refuses the key it backs off, doubling the wait up to MaxBackoff, and proves the hub
// again before the next report. It logs only when the state changes. It returns nil when
// ctx is done.
func Run(ctx context.Context, opt Options) error {
	if opt.HubURL == "" || opt.Collector == nil {
		return errors.New("agent: no hub or collector")
	}
	if !ValidKey(opt.Key) {
		return errors.New("agent: no valid hub key")
	}
	if opt.Interval <= 0 {
		opt.Interval = DefaultInterval
	}
	if opt.MaxBackoff <= 0 {
		opt.MaxBackoff = DefaultMaxBackoff
	}
	if opt.HTTP == nil {
		opt.HTTP = NewHTTPClient()
	}
	if opt.Exec == nil {
		opt.Exec = SystemExecutor
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	a := &runner{opt: opt, guard: guard{key: opt.Key, allow: opt.AllowPowerActions}}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
		err := a.step(ctx)
		if ctx.Err() != nil {
			return nil
		}
		wait := opt.Interval
		if err != nil {
			a.fails++
			wait = jittered(backoff(opt.Interval, opt.MaxBackoff, a.fails))
			a.logState(fmt.Sprintf("%v (retrying in %s)", err, wait.Round(time.Second/10)), err.Error())
		} else {
			a.fails = 0
			a.logState("reporting to "+opt.HubURL, "ok")
		}
		timer.Reset(wait)
	}
}

type runner struct {
	opt       Options
	proven    bool // the hub proved it holds the key since the last failure
	wantProcs bool // the hub's last reply asked for processes
	fails     int
	state     string // last logged state, so repeats aren't logged

	guard   guard                 // checks power commands (actions.go)
	results []report.ActionResult // outcomes of commands run, sent one per report
}

// step sends one report: prove the hub if needed, collect, POST.
func (a *runner) step(ctx context.Context) error {
	if !a.proven {
		if err := Prove(ctx, a.opt.HTTP, a.opt.HubURL, a.opt.Key); err != nil {
			if errors.Is(err, ErrBadProof) {
				return fmt.Errorf("%s does not hold this machine's hub key, so the key was not sent: %w", a.opt.HubURL, err)
			}
			return fmt.Errorf("hub %s unreachable: %w", a.opt.HubURL, bare(err))
		}
		a.proven = true
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	r, err := a.opt.Collector.Collect(cctx)
	if err == nil && r != nil && a.wantProcs && a.opt.Procs != nil {
		r.Processes = topProcesses(cctx, a.opt.Procs)
	}
	cancel()
	if err == nil && r == nil {
		err = errors.New("no report")
	}
	if err != nil {
		return fmt.Errorf("collect: %w", err) // the hub is fine: keep it proven
	}
	r.Token = a.opt.Key
	r.Agent = "machine-monitor/" + a.opt.Version
	r.Features, r.Actions = a.features(), nil
	if a.opt.AllowPowerActions {
		r.Actions = PowerActions
	}
	if len(a.results) > 0 {
		r.ActionResult = &a.results[0]
	}
	body, err := json.Marshal(r)
	if err != nil {
		return err
	}
	rep, err := a.post(ctx, body)
	if err != nil {
		a.proven, a.wantProcs = false, false
		return err
	}
	if r.ActionResult != nil { // delivered
		a.results = a.results[1:]
	}
	a.wantProcs = rep.Procs
	a.runCommands(ctx, r.ID, rep.Commands)
	return nil
}

// features is what this agent can do on request, for the report's "features".
func (a *runner) features() []string {
	f := []string{}
	if a.opt.Procs != nil {
		f = append(f, "procs")
	}
	if a.opt.AllowPowerActions {
		f = append(f, "actions")
	}
	return f
}

func (a *runner) logf(format string, args ...any) {
	if a.opt.Logf != nil {
		a.opt.Logf(format, args...)
	}
}

// post sends one report and decodes the reply.
func (a *runner) post(ctx context.Context, body []byte) (reply, error) {
	var rep reply
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.opt.HubURL+"/api/report", bytes.NewReader(body))
	if err != nil {
		return rep, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.opt.HTTP.Do(req)
	if err != nil {
		return rep, fmt.Errorf("hub %s unreachable: %w", a.opt.HubURL, bare(err))
	}
	defer resp.Body.Close()
	var msg struct {
		Error string `json:"error"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxReply))
	switch {
	case resp.StatusCode == http.StatusOK:
		if json.Unmarshal(raw, &rep) != nil {
			return rep, errors.New("hub sent an unreadable reply")
		}
		return rep, nil
	case resp.StatusCode == http.StatusUnauthorized:
		return rep, errors.New("the hub refused this machine's key: pair again with machine-monitor connect")
	default:
		json.Unmarshal(raw, &msg)
		if m := cleanMsg(msg.Error); m != "" {
			return rep, fmt.Errorf("hub refused the report: HTTP %d: %s", resp.StatusCode, m)
		}
		return rep, fmt.Errorf("hub refused the report: HTTP %d", resp.StatusCode)
	}
}

// bare drops the method and URL net/http wraps around a transport error, so the same
// outage reads the same for a proof (whose URL has a fresh nonce) and a report.
func bare(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// logState logs msg when the state key changes.
func (a *runner) logState(msg, key string) {
	if key == a.state {
		return
	}
	a.state = key
	if a.opt.Logf != nil {
		a.opt.Logf("agent: %s", msg)
	}
}

// jittered spreads a wait by ×(1 + rand·0.2), so agents that lose the hub together don't
// retry in lockstep.
func jittered(d time.Duration) time.Duration {
	return d + time.Duration(rand.Float64()*0.2*float64(d))
}

// backoff is the wait after the nth failure in a row: interval × 2^(n-1), capped at max.
func backoff(interval, max time.Duration, n int) time.Duration {
	d := interval
	for range n - 1 {
		if d >= max/2 {
			return max
		}
		d *= 2
	}
	return min(d, max)
}

// topProcesses lists processes, busiest first, at most MaxProcesses. A failure sends none
// this time rather than holding up the report.
func topProcesses(ctx context.Context, procs func(context.Context) ([]report.Process, error)) []report.Process {
	list, err := procs(ctx)
	if err != nil {
		return nil
	}
	val := func(v *float64) float64 {
		if v == nil {
			return -1
		}
		return *v
	}
	slices.SortStableFunc(list, func(x, y report.Process) int {
		if c := cmp.Compare(val(y.CPU), val(x.CPU)); c != 0 {
			return c
		}
		return cmp.Compare(val(y.Mem), val(x.Mem))
	})
	if len(list) > MaxProcesses {
		list = list[:MaxProcesses]
	}
	return list
}
