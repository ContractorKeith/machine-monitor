package alerts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/config"
	"github.com/ContractorKeith/machine-monitor/internal/report"
)

const (
	TickInterval = 15 * time.Second
	CostInterval = 5 * time.Minute
)

const logDelay = time.Second

type Device struct {
	ID       string
	Name     string
	Online   bool
	LastSeen time.Time
}

type Cost struct {
	Amount   float64
	Currency string
}

type Energy interface {
	Watts(deviceID string, r *report.Report) *float64
	CostToday(device string) (c Cost, ok bool)
}

type Message struct {
	Type     string    `json:"type"`
	Event    *Event    `json:"event,omitempty"`
	Notify   bool      `json:"notify,omitempty"`
	Settings *Settings `json:"settings,omitempty"`
	Active   []Active  `json:"active"`
}

type Event struct {
	ID         string   `json:"id"`
	T          int64    `json:"t"`
	RuleID     string   `json:"ruleId,omitempty"`
	Type       string   `json:"type,omitempty"`
	Device     string   `json:"device,omitempty"`
	DeviceName string   `json:"deviceName,omitempty"`
	State      string   `json:"state"`
	Value      *float64 `json:"value"`
	Title      string   `json:"title"`
	Message    string   `json:"message"`
	Test       bool     `json:"test,omitempty"`
}

type Active struct {
	RuleID     string   `json:"ruleId"`
	Type       string   `json:"type"`
	Device     string   `json:"device"`
	DeviceName string   `json:"deviceName"`
	Since      int64    `json:"since"`
	Value      *float64 `json:"value"`
	Threshold  *float64 `json:"threshold"`
	Title      string   `json:"title"`
	Message    string   `json:"message"`
}

const (
	Firing   = "firing"
	Resolved = "resolved"
)

type Detail struct {
	Since    int64    `json:"since,omitempty"`
	Disk     string   `json:"disk,omitempty"`
	Free     *float64 `json:"free,omitempty"`
	Currency string   `json:"currency,omitempty"`
}

type Options struct {
	Paths        config.Paths
	Devices      func() []Device
	Energy       Energy
	Broadcast    func(Message)
	HubURL       func() string
	Now          func() time.Time
	HTTP         Doer
	LookupIP     func(ctx context.Context, host string) ([]netip.Addr, error)
	BlockPrivate bool
	LocalAddrs   func() []netip.Addr
	Logf         func(format string, args ...any)
}

type state struct {
	since   time.Time
	firing  bool
	firedAt time.Time
	value   *float64
	detail  Detail
	day     string
	carried bool
}

type Engine struct {
	opt    Options
	client Doer

	mu     sync.Mutex
	cfg    Settings
	events []Event
	state  map[string]*state
	wg     sync.WaitGroup

	logDirty bool
	logTimer *time.Timer
}

type savedFile struct {
	Settings json.RawMessage        `json:"settings"`
	Firing   map[string]savedFiring `json:"firing"`
}

type savedFiring struct {
	FiredAt int64    `json:"firedAt"`
	Value   *float64 `json:"value"`
	Detail  Detail   `json:"detail"`
}

func New(opt Options) *Engine {
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Devices == nil {
		opt.Devices = func() []Device { return nil }
	}
	e := &Engine{opt: opt, client: opt.HTTP, state: map[string]*state{}}
	if e.client == nil {
		e.client = newGuardedClient(e.blocked)
	}

	saved, haveSaved := e.load()
	e.cfg = DefaultSettings()
	if haveSaved {
		raw := saved.Settings
		if raw == nil {
			raw, _ = os.ReadFile(opt.Paths.Alerts)
		}
		if s, err := ParseSettings(raw); err == nil {
			e.cfg = s
		}
		for key, f := range saved.Firing {
			at := time.UnixMilli(f.FiredAt)
			e.state[key] = &state{since: at, firing: true, firedAt: at, value: f.Value, detail: f.Detail,
				day: dayOf(at)}
		}
	} else {
		e.save()
	}
	if b, err := os.ReadFile(opt.Paths.AlertsLog); err == nil {
		var evs []Event
		if json.Unmarshal(b, &evs) == nil {
			if len(evs) > maxLog {
				evs = evs[len(evs)-maxLog:]
			}
			e.events = evs
		}
	}

	for _, r := range e.cfg.Rules {
		if !r.Enabled || r.Type != TypeOffline {
			continue
		}
		for _, d := range e.targets(r) {
			if _, saved := e.state[key(r.ID, d.ID)]; !saved && !d.Online && !d.LastSeen.IsZero() {
				e.state[key(r.ID, d.ID)] = &state{since: d.LastSeen, firing: true, firedAt: d.LastSeen, carried: true}
			}
		}
	}
	return e
}

func (e *Engine) load() (savedFile, bool) {
	var s savedFile
	if e.opt.Paths.Alerts == "" {
		return s, false
	}
	b, err := os.ReadFile(e.opt.Paths.Alerts)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			e.logf("alerts: %v", err)
		}
		return s, false
	}
	if err := json.Unmarshal(b, &s); err != nil {
		e.logf("alerts: %s: %v", e.opt.Paths.Alerts, err)
		return s, false
	}
	return s, true
}

func key(ruleID, deviceID string) string { return ruleID + "|" + deviceID }

func dayOf(t time.Time) string { return t.Local().Format("2006-01-02") }

func (e *Engine) logf(format string, args ...any) {
	if e.opt.Logf != nil {
		e.opt.Logf(format, args...)
	}
}

func (e *Engine) Settings() Settings {
	e.mu.Lock()
	defer e.mu.Unlock()
	return cloneSettings(e.cfg)
}

func cloneSettings(s Settings) Settings {
	s.Rules = append([]Rule(nil), s.Rules...)
	return s
}

func (e *Engine) Update(data []byte) (Settings, error) {
	s, err := ParseSettings(data)
	if err != nil {
		return Settings{}, err
	}
	sig := func(r Rule) string { r.Enabled = true; b, _ := json.Marshal(r); return string(b) }
	e.mu.Lock()
	old := map[string]string{}
	for _, r := range e.cfg.Rules {
		old[r.ID] = sig(r)
	}
	rules := map[string]Rule{}
	for _, r := range s.Rules {
		rules[r.ID] = r
	}
	e.cfg = s
	for k := range e.state {
		rid, _, _ := strings.Cut(k, "|")
		r, ok := rules[rid]
		if !ok || !r.Enabled || sig(r) != old[rid] {
			delete(e.state, k)
		}
	}
	e.save()
	out := []Message{{Type: "alerts", Settings: ptr(cloneSettings(s)), Active: e.active()}}
	e.mu.Unlock()
	e.send(out)
	e.Tick()
	return cloneSettings(s), nil
}

func ptr[T any](v T) *T { return &v }

func (e *Engine) save() {
	if e.opt.Paths.Alerts == "" {
		return
	}
	firing := map[string]savedFiring{}
	for k, st := range e.state {
		if st.firing {
			firing[k] = savedFiring{FiredAt: st.firedAt.UnixMilli(), Value: st.value, Detail: st.detail}
		}
	}
	settings, _ := json.Marshal(e.cfg)
	b, _ := json.MarshalIndent(savedFile{Settings: settings, Firing: firing}, "", "  ")
	if err := config.WriteFile0600(e.opt.Paths.Alerts, b); err != nil {
		e.logf("alerts: save: %v", err)
	}
}

func (e *Engine) saveLog() {
	e.logDirty = false
	if e.logTimer != nil {
		e.logTimer.Stop()
		e.logTimer = nil
	}
	if e.opt.Paths.AlertsLog == "" {
		return
	}
	b, _ := json.Marshal(e.events)
	if err := config.WriteFile0600(e.opt.Paths.AlertsLog, b); err != nil {
		e.logf("alerts: save log: %v", err)
	}
}

func (e *Engine) queueLog() {
	e.logDirty = true
	if e.logTimer == nil {
		e.logTimer = time.AfterFunc(logDelay, e.flushLog)
	}
}

func (e *Engine) flushLog() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.logDirty {
		e.saveLog()
	}
}

func (e *Engine) targets(r Rule) []Device {
	all := e.opt.Devices()
	if r.Device == AllDevices {
		return all
	}
	for _, d := range all {
		if d.ID == r.Device {
			return []Device{d}
		}
	}
	return nil
}

func (e *Engine) OnReport(deviceID string, r *report.Report) {
	var watts *float64
	if e.opt.Energy != nil && r != nil && e.wantsPower(deviceID) {
		watts = e.opt.Energy.Watts(deviceID, r)
	}
	e.mu.Lock()
	var out []Message
	for _, rule := range e.cfg.Rules {
		if !rule.Enabled || (rule.Device != AllDevices && rule.Device != deviceID) {
			continue
		}
		switch rule.Type {
		case TypeOffline:
			out = e.resolveOffline(out, rule, deviceID)
		case TypeDailyCost:
		default:
			value, detail := metric(rule.Type, r, watts)
			if value != nil {
				out = e.evaluate(out, rule, deviceID, *value, detail)
			}
		}
	}
	e.mu.Unlock()
	e.send(out)
}

func (e *Engine) wantsPower(deviceID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, r := range e.cfg.Rules {
		if r.Enabled && r.Type == TypePower && (r.Device == AllDevices || r.Device == deviceID) {
			return true
		}
	}
	return false
}

func (e *Engine) evaluate(out []Message, r Rule, id string, value float64, detail Detail) []Message {
	k, now := key(r.ID, id), e.opt.Now()
	st := e.state[k]
	if st == nil {
		st = &state{}
	}
	threshold := *r.Threshold
	hyst := Types[r.Type].Hyst
	if hyst == 0 {
		hyst = threshold * 0.05
	}
	switch {
	case value > threshold:
		if st.since.IsZero() {
			st.since = now
		}
		if !st.firing && now.Sub(st.since) >= time.Duration(r.Minutes)*time.Minute {
			st.firing, st.firedAt, st.value, st.detail = true, now, &value, detail
			e.state[k] = st
			return e.emit(out, r, id, Firing, &value, detail)
		}
		if st.firing {
			st.value, st.detail = &value, detail
		}
	case st.firing && value <= threshold-hyst:
		delete(e.state, k)
		return e.emit(out, r, id, Resolved, &value, detail)
	case !st.firing:
		st.since = time.Time{}
	}
	e.state[k] = st
	return out
}

func (e *Engine) resolveOffline(out []Message, r Rule, id string) []Message {
	k := key(r.ID, id)
	if st := e.state[k]; st != nil && st.firing {
		delete(e.state, k)
		if st.carried {
			return out
		}
		return e.emit(out, r, id, Resolved, nil, Detail{Since: st.since.UnixMilli()})
	}
	return out
}

func (e *Engine) Tick() {
	devices := e.opt.Devices()
	e.mu.Lock()
	now := e.opt.Now()
	var out []Message
	for _, r := range e.cfg.Rules {
		if !r.Enabled || r.Type != TypeOffline {
			continue
		}
		for _, d := range devices {
			if r.Device != AllDevices && r.Device != d.ID {
				continue
			}
			k := key(r.ID, d.ID)
			if d.Online || d.LastSeen.IsZero() || (e.state[k] != nil && e.state[k].firing) {
				continue
			}
			if now.Sub(d.LastSeen) >= time.Duration(r.Minutes)*time.Minute {
				e.state[k] = &state{since: d.LastSeen, firing: true, firedAt: now}
				out = e.emit(out, r, d.ID, Firing, nil, Detail{Since: d.LastSeen.UnixMilli()})
			}
		}
	}
	e.mu.Unlock()
	e.send(out)
}

func (e *Engine) CheckCost() {
	if e.opt.Energy == nil {
		return
	}
	e.mu.Lock()
	rules := append([]Rule(nil), e.cfg.Rules...)
	e.mu.Unlock()
	for _, r := range rules {
		if !r.Enabled || r.Type != TypeDailyCost {
			continue
		}
		c, ok := e.opt.Energy.CostToday(r.Device)
		e.mu.Lock()
		now := e.opt.Now()
		today, k := dayOf(now), key(r.ID, r.Device)
		var out []Message
		if st := e.state[k]; st != nil && st.firing && st.day != today {
			delete(e.state, k)
		}
		if st := e.state[k]; (st == nil || !st.firing) && ok && c.Amount > *r.Threshold && e.hasRule(r) {
			v, d := c.Amount, Detail{Currency: c.Currency}
			e.state[k] = &state{since: now, firing: true, firedAt: now, value: &v, detail: d, day: today}
			out = e.emit(out, r, r.Device, Firing, &v, d)
		}
		e.mu.Unlock()
		e.send(out)
	}
}

func (e *Engine) hasRule(r Rule) bool {
	for _, x := range e.cfg.Rules {
		if x.ID == r.ID {
			return x.Type == r.Type && x.Device == r.Device && x.Enabled && *x.Threshold == *r.Threshold
		}
	}
	return false
}

func (e *Engine) Run(ctx context.Context) {
	tick, cost := time.NewTicker(TickInterval), time.NewTicker(CostInterval)
	defer tick.Stop()
	defer cost.Stop()
	for {
		select {
		case <-ctx.Done():
			e.Wait()
			return
		case <-tick.C:
			e.Tick()
		case <-cost.C:
			e.CheckCost()
		}
	}
}

func (e *Engine) Active() []Active {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.active()
}

func (e *Engine) active() []Active {
	out := []Active{}
	rules := map[string]Rule{}
	for _, r := range e.cfg.Rules {
		rules[r.ID] = r
	}
	for k, st := range e.state {
		if !st.firing {
			continue
		}
		rid, dev, _ := strings.Cut(k, "|")
		r, ok := rules[rid]
		if !ok {
			continue
		}
		title, msg := e.text(r, dev, Firing, st.value, st.detail)
		out = append(out, Active{RuleID: rid, Type: r.Type, Device: dev, DeviceName: e.nameOf(dev),
			Since: st.firedAt.UnixMilli(), Value: st.value, Threshold: r.Threshold, Title: title, Message: msg})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Since != out[j].Since {
			return out[i].Since > out[j].Since
		}
		return out[i].RuleID+out[i].Device < out[j].RuleID+out[j].Device
	})
	return out
}

func (e *Engine) Events(n int) []Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	if n <= 0 || n > len(e.events) {
		n = len(e.events)
	}
	out := make([]Event, 0, n)
	for i := len(e.events) - 1; i >= len(e.events)-n; i-- {
		out = append(out, e.events[i])
	}
	return out
}

func (e *Engine) ClearLog() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = []Event{}
	e.saveLog()
}

func (e *Engine) RemoveDevice(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for k := range e.state {
		if strings.HasSuffix(k, "|"+id) {
			delete(e.state, k)
		}
	}
	e.save()
}

func (e *Engine) nameOf(id string) string {
	if id == AllDevices {
		return "All devices"
	}
	for _, d := range e.opt.Devices() {
		if d.ID == id && d.Name != "" {
			return d.Name
		}
	}
	return id
}

func (e *Engine) emit(out []Message, r Rule, id, st string, value *float64, detail Detail) []Message {
	title, msg := e.text(r, id, st, value, detail)
	ev := Event{ID: newID(), T: e.opt.Now().UnixMilli(), RuleID: r.ID, Type: r.Type, Device: id,
		DeviceName: e.nameOf(id), State: st, Value: value, Title: title, Message: msg}
	e.events = append(e.events, ev)
	if len(e.events) > maxLog {
		e.events = append([]Event(nil), e.events[len(e.events)-maxLog:]...)
	}
	e.queueLog()
	e.save()
	notify := st == Firing || e.cfg.NotifyResolved
	if notify {
		ch, link := e.cfg.Channels, e.link()
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), deliveryTimeout)
			defer cancel()
			e.deliver(ctx, ch, link, ev)
		}()
	}
	return append(out, Message{Type: "alert", Event: &ev, Notify: notify && e.cfg.Channels.Desktop, Active: e.active()})
}

func (e *Engine) send(msgs []Message) {
	if e.opt.Broadcast == nil {
		return
	}
	for _, m := range msgs {
		e.opt.Broadcast(m)
	}
}

func (e *Engine) link() string {
	u := ""
	if e.opt.HubURL != nil {
		u = strings.TrimSuffix(e.opt.HubURL(), "/")
	}
	return u + "/#/alerts"
}

func (e *Engine) Wait() {
	e.wg.Wait()
	e.flushLog()
}

func (e *Engine) Test(ctx context.Context) Delivery {
	e.mu.Lock()
	ch, link := e.cfg.Channels, e.link()
	ev := Event{ID: newID(), T: e.opt.Now().UnixMilli(), State: Firing, Title: "Machine Monitor test alert",
		Message: "Alerts from your hub will arrive like this.", Test: true}
	m := Message{Type: "alert", Event: &ev, Notify: ch.Desktop, Active: e.active()}
	e.mu.Unlock()
	e.send([]Message{m})
	return e.deliver(ctx, ch, link, ev)
}

func (e *Engine) deliver(ctx context.Context, ch Channels, link string, ev Event) Delivery {
	var d Delivery
	if ch.Ntfy.Enabled {
		priority, tag := 3, "white_check_mark"
		if ev.State == Firing {
			priority, tag = 4, "warning"
		}
		body := map[string]any{"topic": ch.Ntfy.Topic, "title": ev.Title, "message": ev.Message,
			"priority": priority, "tags": []string{tag}, "click": link}
		if ch.Ntfy.Email != "" {
			body["email"] = ch.Ntfy.Email
		}
		d.Ntfy = ptr(e.post(ctx, ch.Ntfy.Server, body))
	}
	if ch.Webhook.Enabled {
		icon := "✅"
		if ev.State == Firing {
			icon = "⚠️"
		}
		line := fmt.Sprintf("%s %s\n%s", icon, ev.Title, ev.Message)
		body := map[string]any{"content": line, "text": line, "title": ev.Title, "message": ev.Message,
			"device": ev.DeviceName, "state": ev.State,
			"time": time.UnixMilli(ev.T).UTC().Format("2006-01-02T15:04:05.000Z"), "link": link}
		d.Webhook = ptr(e.post(ctx, ch.Webhook.URL, body))
	}
	if (d.Ntfy != nil && !d.Ntfy.OK) || (d.Webhook != nil && !d.Webhook.OK) {
		b, _ := json.Marshal(d)
		e.logf("alert delivery failed: %s", b)
	}
	return d
}
