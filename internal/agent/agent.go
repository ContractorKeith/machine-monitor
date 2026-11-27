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

const (
	DefaultInterval   = 2 * time.Second
	DefaultMaxBackoff = 30 * time.Second
	MaxProcesses      = 120
)

type Options struct {
	HubURL    string
	DeviceKey string
	Key       string
	Collector collect.Collector

	Procs func(ctx context.Context) ([]report.Process, error)

	AllowPowerActions bool

	SeenFile string

	Exec Executor

	Now func() time.Time

	Version string

	Interval time.Duration

	MaxBackoff time.Duration

	HTTP *http.Client

	Logf func(format string, args ...any)
}

type reply struct {
	OK       bool      `json:"ok"`
	Procs    bool      `json:"procs"`
	Commands []Command `json:"commands"`
}

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
	a := &runner{opt: opt, guard: guard{key: cmp.Or(opt.DeviceKey, opt.Key), allow: opt.AllowPowerActions, path: opt.SeenFile}}
	if err := a.guard.load(opt.Now()); err != nil {
		a.logf("agent: remembered command ids: %v (starting with none)", err)
	}
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
	proven    bool
	wantProcs bool
	fails     int
	state     string

	guard   guard
	results []report.ActionResult
}

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
		return fmt.Errorf("collect: %w", err)
	}
	r.Token = a.opt.Key
	r.Agent = "machine-monitor/" + a.opt.Version
	r.Features, r.Actions = a.features(), nil
	if a.opt.AllowPowerActions {
		r.Actions = SupportedActions()
	}
	if len(a.results) > 0 {
		r.ActionResult = &a.results[0]
	}
	body, err := json.Marshal(r)
	if err == nil && a.opt.DeviceKey != "" {
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(body, &fields)
		fields["deviceKey"], _ = json.Marshal(a.opt.DeviceKey)
		body, err = json.Marshal(fields)
	}
	if err != nil {
		return err
	}
	rep, err := a.post(ctx, body)
	if err != nil {
		a.proven, a.wantProcs = false, false
		return err
	}
	if r.ActionResult != nil && len(a.results) > 0 {
		a.results = a.results[1:]
	}
	a.wantProcs = rep.Procs
	a.runCommands(ctx, r.ID, rep.Commands)
	return nil
}

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
		return rep, errors.New("the hub refused this machine's key: if this machine was just paired again, restart this agent; otherwise pair it with machine-monitor connect")
	default:
		json.Unmarshal(raw, &msg)
		if m := cleanMsg(msg.Error); m != "" {
			return rep, fmt.Errorf("hub refused the report: HTTP %d: %s", resp.StatusCode, m)
		}
		return rep, fmt.Errorf("hub refused the report: HTTP %d", resp.StatusCode)
	}
}

func bare(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

func (a *runner) logState(msg, key string) {
	if key == a.state {
		return
	}
	a.state = key
	if a.opt.Logf != nil {
		a.opt.Logf("agent: %s", msg)
	}
}

func jittered(d time.Duration) time.Duration {
	return d + time.Duration(rand.Float64()*0.2*float64(d))
}

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
