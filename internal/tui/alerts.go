package tui

// Alerts (PLAN §3 rows 20–22, §7 view 6): the active alerts, the rules with an editor, the
// delivery channels with a test button, and the event log, all through the hub's
// /api/alerts. The hub's alert events also raise a banner and ring the terminal bell in
// every view. Local mode has no alerts engine, so the view points at a hub.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/ContractorKeith/machine-monitor/internal/alerts"
	"github.com/ContractorKeith/machine-monitor/internal/client"
	"github.com/ContractorKeith/machine-monitor/internal/report"
)

// AlertsHub is a hub's alerts API. *client.Client implements it; the Alerts view says a
// Hub that doesn't has no alerts.
type AlertsHub interface {
	Alerts(ctx context.Context) (*client.Alerts, error)
	SetAlerts(ctx context.Context, s alerts.Settings) (alerts.Settings, []alerts.Active, error)
	TestAlerts(ctx context.Context) (alerts.Delivery, error)
	ClearAlertLog(ctx context.Context) error
}

// Limits on what the TUI keeps.
const (
	maxAlertLog = 150 // log entries, as GET /api/alerts returns
	maxSeen     = 64  // alert ids remembered so a repeat never rings twice
)

// Alerts messages.
type (
	alertsLoadedMsg struct {
		a   *client.Alerts
		err error
	}
	alertsSavedMsg struct {
		s      alerts.Settings
		active []alerts.Active
		err    error
	}
	alertsTestMsg struct {
		d   alerts.Delivery
		err error
	}
	alertsClearedMsg struct{ err error }
)

// alertsUI is the Alerts view's state and the banner. It lives in model.al and, like the
// model, is copied on every Update, so slices are cloned before they change.
type alertsUI struct {
	loaded, loading bool
	err             error // the last load's error
	cfg             alerts.Settings
	active          []alerts.Active
	events          []alerts.Event // newest first
	priceSet        bool

	cursor  int // selected rule
	scroll  int
	form    alertForm // the open editor, when form.open
	confirm string    // "rule" or "log" while asking y/n
	notice  string    // the last action's result or error

	banner *alerts.Event // shown in every view until dismissed
	more   int           // alerts that arrived after the banner's, while it showed
	seen   []string      // ids of alerts that already rang, newest last
	bells  int           // bells rung, for tests

	unitPending bool // the hub's tempUnit should follow Settings once the alerts load
}

// alerter is the hub's alerts API, or nil in local mode or for a Hub without one.
func (m model) alerter() AlertsHub {
	a, _ := m.opts.Hub.(AlertsHub)
	return a
}

// alertsCall runs an alerts request off the UI goroutine.
func (m model) alertsCall(f func(ctx context.Context, a AlertsHub) tea.Msg) tea.Cmd {
	a := m.alerter()
	if a == nil {
		return nil
	}
	return m.hubCall(func(ctx context.Context, _ Hub) tea.Msg { return f(ctx, a) })
}

// loadAlerts fetches the alerts unless a fetch is already running.
func (m *model) loadAlerts() tea.Cmd {
	if m.alerter() == nil || m.al.loading {
		return nil
	}
	m.al.loading = true
	return m.alertsCall(func(ctx context.Context, a AlertsHub) tea.Msg {
		r, err := a.Alerts(ctx)
		return alertsLoadedMsg{r, err}
	})
}

// maybeLoadAlerts fetches the alerts when the Alerts view is open.
func (m *model) maybeLoadAlerts() tea.Cmd {
	if m.view != viewAlerts {
		return nil
	}
	return m.loadAlerts()
}

// updateAlerts handles alerts messages and the hub's alert events; ok is false for
// anything else.
func (m model) updateAlerts(msg tea.Msg) (model, tea.Cmd, bool) {
	switch msg := msg.(type) {
	case alertsLoadedMsg:
		m.al.loading = false
		m.al.err = msg.err
		if msg.err == nil && msg.a != nil {
			m.al.loaded = true
			m.al.active, m.al.priceSet = msg.a.Active, msg.a.PriceSet
			m.setRules(msg.a.Settings)
			cleanAll(&msg.a)
			m.al.events = msg.a.Events
			if len(m.al.events) > maxAlertLog {
				m.al.events = m.al.events[:maxAlertLog]
			}
			m.clampAlerts()
			next, cmd := m.syncAlertsUnit()
			return next, cmd, true
		}
		return m, nil, true
	case alertsSavedMsg:
		if msg.err != nil {
			m.al.notice = "save failed: " + hubErr(msg.err)
			return m, nil, true
		}
		m.al.active, m.al.loaded = msg.active, true
		m.setRules(msg.s)
		m.al.notice = "saved"
		m.clampAlerts()
		return m, nil, true
	case alertsTestMsg:
		m.al.notice = testResult(msg.d, msg.err)
		return m, nil, true
	case alertsClearedMsg:
		if msg.err != nil {
			m.al.notice = "clear failed: " + hubErr(msg.err)
		} else {
			m.al.events, m.al.notice = nil, "log cleared"
		}
		return m, nil, true
	}
	return m, nil, false
}

// applyAlert folds an alert or settings event into the state. It returns the bell when the
// event is a new alert the hub wants shown.
func (m *model) applyAlert(ev client.Event) tea.Cmd {
	var msg alerts.Message
	if json.Unmarshal(ev.Raw, &msg) != nil {
		return nil
	}
	cleanAll(&msg)
	if msg.Active != nil {
		m.al.active = msg.Active
	}
	if msg.Settings != nil {
		m.setRules(*msg.Settings)
		if m.al.confirm == "rule" { // the rule it asked about may be gone or moved
			m.al.confirm, m.al.notice = "", "the rules changed on the hub"
		}
	}
	e := msg.Event
	if e == nil || slices.Contains(m.al.seen, e.ID) {
		return nil
	}
	m.al.seen = append(slices.Clone(m.al.seen), e.ID)
	if len(m.al.seen) > maxSeen {
		m.al.seen = m.al.seen[len(m.al.seen)-maxSeen:]
	}
	if !e.Test { // the hub doesn't log test alerts either
		m.al.events = append([]alerts.Event{*e}, m.al.events...)
		if len(m.al.events) > maxAlertLog {
			m.al.events = m.al.events[:maxAlertLog]
		}
	}
	if !msg.Notify {
		return nil
	}
	if m.al.banner != nil {
		m.al.more++
	}
	m.al.banner = e
	m.al.bells++
	return tea.Raw(string(rune(ansi.BEL)))
}

// setRules takes settings from the hub. An open rule editor follows its rule to its new
// place, or closes when the rule is gone, so saving can't overwrite another rule.
func (m *model) setRules(s alerts.Settings) {
	m.al.cfg = s
	if f := &m.al.form; f.open && f.rule >= 0 {
		f.rule = slices.IndexFunc(s.Rules, func(r alerts.Rule) bool { return r.ID == f.ruleID })
		if f.rule < 0 {
			m.al.form = alertForm{}
			m.al.notice = "the rules changed on the hub"
		}
	}
	m.clampAlerts()
}

// hubErr turns an error into a sentence for the footer: the hub's own message for a refused
// request, a plain-words reason for the usual network failures, never Go's "Get \"url\": dial
// tcp …". Whatever is left is cleaned of control characters.
func hubErr(err error) string {
	var se *client.StatusError
	if errors.As(err, &se) && se.Message != "" {
		return clean(se.Message)
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var dns *net.DNSError
	text := err.Error()
	switch {
	case errors.Is(err, syscall.ECONNREFUSED), strings.Contains(text, "refused"):
		return "the hub isn't accepting connections (is it running?)"
	case errors.As(err, &dns):
		return "can't find the hub's address"
	case errors.Is(err, context.DeadlineExceeded), os.IsTimeout(err), strings.Contains(text, "went quiet"):
		return "the hub didn't answer in time"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), strings.Contains(text, "stream ended"):
		return "the hub closed the connection"
	case strings.Contains(text, "no route to host"), strings.Contains(text, "network is unreachable"):
		return "no route to the hub"
	}
	return clean(text)
}

// testResult says how a test alert's deliveries went.
func testResult(d alerts.Delivery, err error) string {
	if err != nil {
		return "test failed: " + hubErr(err)
	}
	parts := []string{"test alert sent"}
	for _, c := range []struct {
		name string
		r    *alerts.Result
	}{{"ntfy", d.Ntfy}, {"webhook", d.Webhook}} {
		switch {
		case c.r == nil:
		case c.r.OK:
			parts = append(parts, c.name+" ok")
		default:
			parts = append(parts, c.name+" failed: "+c.r.Error)
		}
	}
	if d.Ntfy == nil && d.Webhook == nil {
		parts = append(parts, "no ntfy or webhook channel is on")
	}
	return strings.Join(parts, " · ")
}

// clampAlerts keeps the rule cursor and the scroll in range.
func (m *model) clampAlerts() {
	m.al.cursor = max(0, min(m.al.cursor, len(m.al.cfg.Rules)-1))
	lines, _ := m.alertsLines()
	m.al.scroll = max(0, min(m.al.scroll, len(lines)-m.bodyHeight()))
}

// ---- keys --------------------------------------------------------------------------------

// alertsPreKey takes keys that come before the global ones: the open editor or question,
// and esc dismissing the banner. ok is false when it didn't take the key.
func (m model) alertsPreKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd, bool) {
	switch {
	case m.al.form.open:
		next, cmd := m.formKey(msg)
		return next, cmd, true
	case m.al.confirm != "":
		what := m.al.confirm
		m.al.confirm = ""
		if msg.String() != "y" && msg.String() != "Y" {
			return m, nil, true
		}
		if what == "log" {
			m.al.notice = ""
			return m, m.alertsCall(func(ctx context.Context, a AlertsHub) tea.Msg {
				return alertsClearedMsg{a.ClearAlertLog(ctx)}
			}), true
		}
		s := cloneSettings(m.al.cfg)
		if m.al.cursor >= len(s.Rules) {
			return m, nil, true
		}
		s.Rules = slices.Delete(s.Rules, m.al.cursor, m.al.cursor+1)
		next, cmd := m.saveAlerts(s)
		return next, cmd, true
	case m.al.banner != nil && msg.String() == "esc" && !m.showHelp:
		m.al.banner, m.al.more = nil, 0
		return m, nil, true
	}
	return m, nil, false
}

// alertsKey handles the Alerts view's keys.
func (m model) alertsKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.alerter() == nil {
		return m, nil
	}
	k := m.keys
	n := len(m.al.cfg.Rules)
	switch s := msg.String(); {
	case !m.al.loaded:
		if s == "r" {
			return m, m.loadAlerts()
		}
		return m, nil
	case s == "space":
		if n == 0 {
			return m, nil
		}
		st := cloneSettings(m.al.cfg)
		st.Rules[m.al.cursor].Enabled = !st.Rules[m.al.cursor].Enabled
		return m.saveAlerts(st)
	case s == "a":
		m.openRuleForm(-1)
		return m, nil
	case s == "e" || s == "enter":
		if n > 0 {
			m.openRuleForm(m.al.cursor)
		}
		return m, nil
	case s == "D":
		if n > 0 {
			m.al.confirm, m.al.notice = "rule", ""
		}
		return m, nil
	case s == "c":
		m.openChannelsForm()
		return m, nil
	case s == "t":
		m.al.notice = "sending a test alert…"
		return m, m.alertsCall(func(ctx context.Context, a AlertsHub) tea.Msg {
			d, err := a.TestAlerts(ctx)
			return alertsTestMsg{d, err}
		})
	case s == "C":
		m.al.confirm, m.al.notice = "log", ""
		return m, nil
	case s == "r":
		return m, m.loadAlerts()
	case key.Matches(msg, k.Up):
		m.al.cursor--
	case key.Matches(msg, k.Down):
		m.al.cursor++
	case key.Matches(msg, k.PageUp):
		m.al.scroll -= m.bodyHeight()
	case key.Matches(msg, k.PageDown):
		m.al.scroll += m.bodyHeight()
	case key.Matches(msg, k.Top):
		m.al.scroll, m.al.cursor = 0, 0
	case key.Matches(msg, k.Bottom):
		m.al.scroll = 1 << 30
	default:
		return m, nil
	}
	if key.Matches(msg, k.Up, k.Down) { // keep the selected rule in view
		m.al.cursor = max(0, min(m.al.cursor, n-1))
		_, sel := m.alertsLines()
		if sel >= 0 {
			m.al.scroll = max(min(m.al.scroll, sel), sel-m.bodyHeight()+1)
		}
	}
	m.clampAlerts()
	return m, nil
}

// saveAlerts checks s the way the hub will and sends it. A problem goes to the open form,
// or the notice.
func (m model) saveAlerts(s alerts.Settings) (model, tea.Cmd) {
	b, _ := json.Marshal(s)
	parsed, err := alerts.ParseSettings(b)
	if err != nil {
		if m.al.form.open {
			m.al.form.err = err.Error()
		} else {
			m.al.notice = "not saved: " + err.Error()
		}
		return m, nil
	}
	m.al.form.open = false
	m.al.notice = "saving…"
	return m, m.alertsCall(func(ctx context.Context, a AlertsHub) tea.Msg {
		got, active, err := a.SetAlerts(ctx, parsed)
		return alertsSavedMsg{got, active, err}
	})
}

func cloneSettings(s alerts.Settings) alerts.Settings {
	s.Rules = slices.Clone(s.Rules)
	return s
}

// ---- the editor --------------------------------------------------------------------------

// Field kinds.
const (
	fieldChoice = iota // ←/→ cycles choices
	fieldToggle        // space or ←/→ flips it
	fieldText          // a text input
)

// formField is one row of an editor.
type formField struct {
	label   string
	kind    int
	choices []string // fieldChoice: what is shown
	values  []string // fieldChoice: what is saved
	idx     int      // fieldChoice: the chosen one
	on      bool     // fieldToggle
	input   textinput.Model
	unit    string // fieldText: shown after the value
	hidden  bool   // not used by the current choices (the threshold of an offline rule)
}

// alertForm is the rule or channels editor.
type alertForm struct {
	open   bool
	rule   int    // index of the rule being edited, -1 for a new one; -2 for the channels
	ruleID string // the edited rule's id, to find it again when the rules change
	fields []formField
	cur    int
	err    string
}

// Rule form rows.
const (
	ruleType = iota
	ruleDevice
	ruleThreshold
	ruleMinutes
	ruleEnabled
)

// Channel form rows.
const (
	chDesktop = iota
	chResolved
	chNtfy
	chNtfyServer
	chNtfyTopic
	chWebhook
	chWebhookURL
)

const channelsForm = -2

// ruleTypes is the type picker's order, as upstream's dashboard lists them.
var ruleTypes = []string{alerts.TypeOffline, alerts.TypeCPUTemp, alerts.TypeGPUTemp, alerts.TypeCPU,
	alerts.TypeGPU, alerts.TypeMem, alerts.TypeDisk, alerts.TypePower, alerts.TypeDailyCost}

// defaultThreshold is what a new rule of a type starts at.
var defaultThreshold = map[string]float64{alerts.TypeCPUTemp: 90, alerts.TypeGPUTemp: 85, alerts.TypeCPU: 90,
	alerts.TypeGPU: 90, alerts.TypeMem: 90, alerts.TypeDisk: 90, alerts.TypePower: 500, alerts.TypeDailyCost: 5}

func textField(label, value, unit string, limit int) formField {
	in := textinput.New()
	in.Prompt = ""
	in.CharLimit = limit
	in.SetValue(value)
	st := in.Styles()
	st.Cursor.Blink = false // a steady cursor: the form needs no blink ticks
	in.SetStyles(st)
	return formField{label: label, kind: fieldText, input: in, unit: unit}
}

// openRuleForm opens the editor on rule i, or on a new rule for i < 0.
func (m *model) openRuleForm(i int) {
	r := alerts.Rule{Type: alerts.TypeCPUTemp, Device: alerts.AllDevices, Minutes: 2, Enabled: true}
	if i >= 0 {
		r = m.al.cfg.Rules[i]
	} else {
		r.Threshold = report.Ptr(defaultThreshold[r.Type])
	}
	typ := formField{label: "Type", kind: fieldChoice, values: ruleTypes, idx: max(0, slices.Index(ruleTypes, r.Type))}
	for _, t := range ruleTypes {
		typ.choices = append(typ.choices, alerts.Types[t].Label)
	}
	dev := formField{label: "Device", kind: fieldChoice, values: []string{alerts.AllDevices}, choices: []string{"All devices"}}
	for _, d := range m.navOrder() {
		dev.values, dev.choices = append(dev.values, d.id), append(dev.choices, d.name)
	}
	if dev.idx = slices.Index(dev.values, r.Device); dev.idx < 0 { // a device the hub no longer has
		dev.values, dev.choices = append(dev.values, r.Device), append(dev.choices, r.Device)
		dev.idx = len(dev.values) - 1
	}
	th := ""
	if r.Threshold != nil {
		th = m.thresholdIn(r.Type, *r.Threshold)
	}
	m.al.form = alertForm{open: true, rule: i, ruleID: r.ID, fields: []formField{
		typ, dev,
		textField("Threshold", th, "", 12),
		textField("Minutes", strconv.Itoa(r.Minutes), "min", 4),
		{label: "Enabled", kind: fieldToggle, on: r.Enabled},
	}}
	m.al.notice = ""
	m.syncRuleForm()
	m.focusField()
}

// syncRuleForm shows the threshold's unit and hides it for offline rules.
func (m *model) syncRuleForm() {
	f := &m.al.form
	typ := ruleTypes[f.fields[ruleType].idx]
	f.fields[ruleThreshold].hidden = !alerts.Types[typ].HasThreshold
	f.fields[ruleThreshold].unit = m.thresholdUnit(typ)
	f.fields[ruleMinutes].unit = "min"
	if typ == alerts.TypeDailyCost {
		f.fields[ruleMinutes].hidden = true // checked once per day, not over minutes
	} else {
		f.fields[ruleMinutes].hidden = false
	}
}

// openChannelsForm opens the editor on the delivery channels.
func (m *model) openChannelsForm() {
	ch := m.al.cfg.Channels
	m.al.form = alertForm{open: true, rule: channelsForm, fields: []formField{
		{label: "Banner and bell", kind: fieldToggle, on: ch.Desktop},
		{label: "Resolved notices", kind: fieldToggle, on: m.al.cfg.NotifyResolved},
		{label: "ntfy", kind: fieldToggle, on: ch.Ntfy.Enabled},
		textField("ntfy server", ch.Ntfy.Server, "", 200),
		textField("ntfy topic", ch.Ntfy.Topic, "", 64),
		{label: "Webhook", kind: fieldToggle, on: ch.Webhook.Enabled},
		textField("Webhook URL", ch.Webhook.URL, "", 500),
	}}
	m.al.notice = ""
	m.focusField()
}

// focusField focuses the current row's text input, if it has one, and blurs the rest.
func (m *model) focusField() {
	f := &m.al.form
	f.fields = slices.Clone(f.fields)
	for i := range f.fields {
		if i == f.cur && f.fields[i].kind == fieldText {
			f.fields[i].input.Focus()
			f.fields[i].input.CursorEnd()
		} else {
			f.fields[i].input.Blur()
		}
	}
}

// formKey handles a key while the editor is open.
func (m model) formKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	f := &m.al.form
	f.fields = slices.Clone(f.fields)
	fld := &f.fields[f.cur]
	switch s := msg.String(); s {
	case "esc":
		m.al.form = alertForm{}
		return m, nil
	case "enter":
		f.err = ""
		if f.rule == channelsForm {
			return m.saveChannels()
		}
		return m.saveRule()
	case "up", "shift+tab", "down", "tab":
		step := 1
		if s == "up" || s == "shift+tab" {
			step = -1
		}
		for i := f.cur + step; i >= 0 && i < len(f.fields); i += step {
			if !f.fields[i].hidden {
				f.cur = i
				break
			}
		}
		m.focusField()
		return m, nil
	}
	switch fld.kind {
	case fieldChoice:
		switch msg.String() {
		case "left", "h":
			fld.idx = (fld.idx + len(fld.values) - 1) % len(fld.values)
		case "right", "l", "space":
			fld.idx = (fld.idx + 1) % len(fld.values)
		default:
			return m, nil
		}
		if f.rule != channelsForm && f.cur == ruleType {
			m.ruleTypeChanged()
		}
	case fieldToggle:
		switch msg.String() {
		case "left", "right", "h", "l", "space", "y", "n":
			fld.on = !fld.on
		}
	case fieldText:
		var cmd tea.Cmd
		fld.input, cmd = fld.input.Update(msg)
		return m, cmd
	}
	return m, nil
}

// ruleTypeChanged resets the threshold to the new type's default. A new rule also takes the
// type's default minutes; an edited one keeps its own, but offline needs 1 or more.
func (m *model) ruleTypeChanged() {
	f := &m.al.form
	typ := ruleTypes[f.fields[ruleType].idx]
	th := ""
	if v, ok := defaultThreshold[typ]; ok {
		th = m.thresholdIn(typ, v)
	}
	f.fields[ruleThreshold].input.SetValue(th)
	mins := strings.TrimSpace(f.fields[ruleMinutes].input.Value())
	switch {
	case f.rule < 0:
		f.fields[ruleMinutes].input.SetValue(strconv.Itoa(defaultMinutes(typ)))
	case typ == alerts.TypeOffline && (mins == "" || mins == "0"):
		f.fields[ruleMinutes].input.SetValue("10")
	}
	m.syncRuleForm()
}

// defaultMinutes is how long a new rule of a type waits, as the default rules do.
func defaultMinutes(typ string) int {
	switch typ {
	case alerts.TypeOffline:
		return 10
	case alerts.TypeDisk, alerts.TypeDailyCost:
		return 0
	}
	return 2
}

// saveRule builds the rule from the form and saves every rule with it.
func (m model) saveRule() (tea.Model, tea.Cmd) {
	f := m.al.form
	if f.rule >= len(m.al.cfg.Rules) { // setRules keeps it in range; never index past the end
		m.al.form, m.al.notice = alertForm{}, "the rules changed on the hub"
		return m, nil
	}
	typ := ruleTypes[f.fields[ruleType].idx]
	dev := f.fields[ruleDevice]
	r := alerts.Rule{ID: f.ruleID, Type: typ, Device: dev.values[dev.idx], Enabled: f.fields[ruleEnabled].on}
	if alerts.Types[typ].HasThreshold {
		v, err := strconv.ParseFloat(strings.TrimSpace(f.fields[ruleThreshold].input.Value()), 64)
		if err != nil {
			m.al.form.err = "threshold must be a number"
			return m, nil
		}
		v = m.thresholdOut(typ, v)
		r.Threshold = &v
	}
	if typ != alerts.TypeDailyCost {
		n, err := strconv.Atoi(strings.TrimSpace(f.fields[ruleMinutes].input.Value()))
		if err != nil {
			m.al.form.err = "minutes must be a whole number"
			return m, nil
		}
		r.Minutes = n
	}
	s := cloneSettings(m.al.cfg)
	if f.rule >= 0 {
		s.Rules[f.rule] = r
	} else {
		s.Rules = append(s.Rules, r)
	}
	next, cmd := m.saveAlerts(s)
	if f.rule < 0 && !next.al.form.open { // select the new rule
		next.al.cursor = len(s.Rules) - 1
	}
	return next, cmd
}

// saveChannels builds the channels from the form and saves them with the rules.
func (m model) saveChannels() (tea.Model, tea.Cmd) {
	fs := m.al.form.fields
	val := func(i int) string { return strings.TrimSpace(fs[i].input.Value()) }
	s := cloneSettings(m.al.cfg)
	s.NotifyResolved = fs[chResolved].on
	s.Channels.Desktop = fs[chDesktop].on
	s.Channels.Ntfy.Enabled, s.Channels.Ntfy.Server, s.Channels.Ntfy.Topic = fs[chNtfy].on, val(chNtfyServer), val(chNtfyTopic)
	s.Channels.Webhook.Enabled, s.Channels.Webhook.URL = fs[chWebhook].on, val(chWebhookURL)
	switch { // the hub would quietly switch these off
	case s.Channels.Ntfy.Enabled && s.Channels.Ntfy.Topic == "":
		m.al.form.err = "ntfy needs a topic"
		return m, nil
	case s.Channels.Webhook.Enabled && s.Channels.Webhook.URL == "":
		m.al.form.err = "the webhook needs a URL"
		return m, nil
	}
	return m.saveAlerts(s)
}

// ---- drawing -----------------------------------------------------------------------------

// isTempRule reports whether a rule type's threshold is a temperature.
func isTempRule(typ string) bool { return typ == alerts.TypeCPUTemp || typ == alerts.TypeGPUTemp }

// thresholdIn shows a threshold as the editor and table take it: temperatures in the unit
// in use (the hub keeps them in °C), to one decimal at most.
func (m model) thresholdIn(typ string, v float64) string {
	if isTempRule(typ) {
		v = math.Round(m.theme.temp(v)*10) / 10
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// thresholdOut turns a threshold typed in the editor into what the hub keeps: °C for
// temperatures, to one decimal.
func (m model) thresholdOut(typ string, v float64) float64 {
	if isTempRule(typ) && m.theme.fahrenheit {
		return math.Round((v-32)*5/9*10) / 10
	}
	return v
}

// thresholdUnit is the unit a type's threshold is entered in.
func (m model) thresholdUnit(typ string) string {
	switch typ {
	case alerts.TypeCPUTemp, alerts.TypeGPUTemp:
		if m.theme.fahrenheit {
			return "°F"
		}
		return "°C"
	case alerts.TypePower:
		return "W"
	case alerts.TypeDailyCost:
		return "per day"
	case alerts.TypeOffline:
		return ""
	}
	return "%"
}

// limitText is a rule's threshold for the rules table.
func (m model) limitText(r alerts.Rule) string {
	if r.Threshold == nil {
		return none
	}
	v := m.thresholdIn(r.Type, *r.Threshold)
	switch u := m.thresholdUnit(r.Type); u {
	case "%", "°C", "°F":
		return v + u
	case "per day":
		return v + "/day"
	default:
		return v + " " + u
	}
}

// forText is a rule's duration for the rules table.
func forText(r alerts.Rule) string {
	switch {
	case r.Type == alerts.TypeDailyCost:
		return "today"
	case r.Minutes == 0:
		return "at once"
	}
	return fmt.Sprintf("%d min", r.Minutes)
}

// devName is a device id's display name.
func (m model) devName(id string) string {
	if id == alerts.AllDevices {
		return "All devices"
	}
	for _, d := range m.devs {
		if d.id == id {
			return d.name
		}
	}
	return id
}

// clock is a Unix-ms time as "15:04", with the date when it isn't today.
func (m model) clock(ms int64) string {
	now := m.now()
	t := time.UnixMilli(ms).In(now.Location())
	if y, mo, d := t.Date(); y == now.Year() && mo == now.Month() && d == now.Day() {
		return t.Format("15:04")
	}
	return t.Format("Jan 2 15:04")
}

// glyph marks an alert's state.
func (m model) glyph(state string) string {
	switch {
	case state == alerts.Resolved && m.theme.ascii:
		return "+"
	case state == alerts.Resolved:
		return "✓"
	case m.theme.ascii:
		return "!"
	}
	return "⚠"
}

// icon is glyph in the state's colour.
func (m model) icon(state string) string {
	if state == alerts.Resolved {
		return m.theme.normal.Render(m.glyph(state))
	}
	return m.theme.hot.Render(m.glyph(state))
}

// alertsView is view 6's body.
func (m model) alertsView() string {
	t := m.theme
	switch {
	case !m.multi():
		return t.label.Render("Alerts") + "\n\n" +
			"Alerts run on a hub. Connect to a hub for alerts:\n\n" +
			"  machine-monitor connect URL     pair this machine with a hub\n" +
			"  machine-monitor --hub URL       open the TUI against a hub\n\n" +
			t.dim.Render("Or start one here with `machine-monitor hub`.")
	case m.alerter() == nil:
		return t.dim.Render("This hub connection has no alerts API.")
	case m.al.form.open:
		return m.formView()
	case !m.al.loaded && m.al.err != nil:
		return t.err.Render("Could not load alerts: "+m.al.err.Error()) + "\n" + t.dim.Render("r retries")
	case !m.al.loaded:
		return t.dim.Render("Loading alerts from " + m.hubHost() + "…")
	}
	lines, _ := m.alertsLines()
	end := min(len(lines), m.al.scroll+m.bodyHeight())
	return strings.Join(lines[min(m.al.scroll, end):end], "\n")
}

// alertsLines is the whole Alerts view, and the line index of the selected rule (-1 when
// there are no rules).
func (m model) alertsLines() ([]string, int) {
	t := m.theme
	a := m.al
	var lines []string
	section := func(title string, n int) {
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, t.label.Render(fmt.Sprintf("%s (%d)", title, n)))
	}

	section("Active", len(a.active))
	if len(a.active) == 0 {
		lines = append(lines, t.dim.Render("  Nothing is firing."))
	}
	for _, x := range a.active {
		lines = append(lines, "  "+m.icon(alerts.Firing)+" "+x.Title+t.dim.Render(" · "+x.Message+" · since "+m.clock(x.Since)))
	}

	section("Rules", len(a.cfg.Rules))
	sel := -1
	w := max(20, m.width)
	typeW, devW := 22, min(24, max(10, w-62))
	if len(a.cfg.Rules) == 0 {
		lines = append(lines, t.dim.Render("  No rules. a adds one."))
	} else {
		lines = append(lines, t.dim.Render("    On   "+pad("Type", typeW)+" "+pad("Device", devW)+" "+pad("Limit", 9)+" For"))
	}
	for i, r := range a.cfg.Rules {
		on := t.dim.Render("off")
		if r.Enabled {
			on = t.normal.Render("on ")
		}
		row := pad(alerts.Types[r.Type].Label, typeW) + " " + pad(m.devName(r.Device), devW) + " " +
			pad(m.limitText(r), 9) + " " + forText(r)
		if r.Type == alerts.TypeDailyCost && !a.priceSet {
			row += t.dim.Render(" (no price)")
		}
		marker := "  "
		if i == a.cursor {
			sel = len(lines)
			marker = "▶ "
			if t.ascii {
				marker = "> "
			}
			row = t.selected.Render(row)
		}
		lines = append(lines, "  "+marker+on+"  "+row)
	}

	ch := a.cfg.Channels
	onOff := func(b bool) string {
		if b {
			return t.normal.Render("on")
		}
		return t.dim.Render("off")
	}
	if len(lines) > 0 {
		lines = append(lines, "")
	}
	lines = append(lines, t.label.Render("Channels")+"  banner and bell "+onOff(ch.Desktop)+t.dim.Render(" · ")+
		"resolved notices "+onOff(a.cfg.NotifyResolved))
	ntfy, hook := onOff(false), onOff(false)
	if ch.Ntfy.Enabled {
		ntfy = onOff(true) + "  " + ch.Ntfy.Server + "/" + ch.Ntfy.Topic
	}
	if ch.Webhook.Enabled {
		hook = onOff(true) + "  " + ch.Webhook.URL
	}
	lines = append(lines, "  ntfy     "+ntfy, "  webhook  "+hook)

	section("Log", len(a.events))
	if len(a.events) == 0 {
		lines = append(lines, t.dim.Render("  No alerts yet."))
	}
	clocks, cw := make([]string, len(a.events)), 5
	for i, e := range a.events {
		clocks[i] = m.clock(e.T)
		cw = max(cw, len(clocks[i]))
	}
	for i, e := range a.events {
		lines = append(lines, "  "+t.dim.Render(padLeft(clocks[i], cw))+" "+m.icon(e.State)+" "+e.Title+t.dim.Render(" · "+e.Message))
	}
	return lines, sel
}

// formView is the open editor.
func (m model) formView() string {
	t := m.theme
	f := m.al.form
	title := "New rule"
	switch {
	case f.rule == channelsForm:
		title = "Channels"
	case f.rule >= 0:
		title = "Edit rule"
	}
	lines := []string{t.label.Render(title), ""}
	for i, fld := range f.fields {
		if fld.hidden {
			continue
		}
		marker := "  "
		if i == f.cur {
			marker = "▶ "
			if t.ascii {
				marker = "> "
			}
		}
		var v string
		switch fld.kind {
		case fieldChoice:
			v = "◀ " + fld.choices[fld.idx] + " ▶"
			if t.ascii {
				v = "< " + fld.choices[fld.idx] + " >"
			}
		case fieldToggle:
			v = "[ ] off"
			if fld.on {
				v = "[x] on"
			}
		case fieldText:
			switch {
			case fld.input.Focused():
				v = strings.TrimRight(fld.input.View(), " ")
			case fld.input.Value() == "":
				v = t.dim.Render("(empty)")
			default:
				v = fld.input.Value()
			}
			if fld.unit != "" {
				v += " " + t.dim.Render(fld.unit)
			}
		}
		label := pad(fld.label, 18)
		if i == f.cur {
			label = t.selected.Render(label)
		}
		lines = append(lines, marker+label+" "+v)
	}
	if f.rule != channelsForm {
		unit := "Temperatures are in °C."
		if t.fahrenheit {
			unit = "Temperatures are in °F (, settings changes it); the hub keeps them in °C."
		}
		lines = append(lines, "", t.dim.Render(unit+" Disk is the fullest drive over 1 GB."))
	} else {
		lines = append(lines, "", t.dim.Render("Save first, then t sends a test alert to every channel that is on."))
	}
	if f.err != "" {
		lines = append(lines, "", t.err.Render(f.err))
	}
	return strings.Join(lines, "\n")
}

// alertsFooter is view 6's footer: an open question, the notice, or the keys.
func (m model) alertsFooter() string {
	switch {
	case !m.multi() || m.alerter() == nil:
		return "tab next view · ? help"
	case m.al.form.open:
		return "↑↓ field · ←→ change · space toggle · enter save · esc cancel"
	case m.al.confirm == "log":
		return "Clear the alert log on the hub? y to confirm, any other key cancels"
	case m.al.confirm == "rule" && m.al.cursor < len(m.al.cfg.Rules):
		r := m.al.cfg.Rules[m.al.cursor]
		return "Delete the " + alerts.Types[r.Type].Label + " rule for " + m.devName(r.Device) + "? y to confirm, any other key cancels"
	case m.al.notice != "":
		return m.al.notice
	}
	return "a add · enter edit · space on/off · D delete · c channels · t test · C clear log"
}

// bannerRows is the banner's height: one line while an alert is shown and the window has
// room for it.
func (m model) bannerRows() int {
	if m.al.banner != nil && m.height >= 4 {
		return 1
	}
	return 0
}

// bannerLine is the alert banner shown under the header in every view.
func (m model) bannerLine() string {
	e := m.al.banner
	if e == nil {
		return ""
	}
	t := m.theme
	style := t.hot
	if e.State == alerts.Resolved {
		style = t.normal
	}
	text := " " + e.Title + " · " + e.Message
	if m.al.more > 0 {
		text += fmt.Sprintf(" (+%d more)", m.al.more)
	}
	return spread(style.Reverse(true).Render(" "+m.glyph(e.State)+text+" "), t.dim.Render("esc dismiss · 6 alerts"), m.width)
}

// alertsActive is how many alerts are firing, or -1 before the hub has said.
func (m model) alertsActive() int {
	if !m.al.loaded {
		return -1
	}
	return len(m.al.active)
}

// alertsCount is the dashboard header's alert count, red while any is firing.
func (m model) alertsCount(n int) string {
	switch n {
	case 0:
		return m.theme.dim.Render("no active alerts")
	case 1:
		return m.theme.hot.Render("1 active alert")
	}
	return m.theme.hot.Render(fmt.Sprintf("%d active alerts", n))
}
