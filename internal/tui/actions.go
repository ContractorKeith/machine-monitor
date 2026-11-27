package tui

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/ContractorKeith/machine-monitor/internal/client"
	"github.com/ContractorKeith/machine-monitor/internal/report"
)

type ActionHub interface {
	Processes(ctx context.Context, id string) (*client.Processes, error)
	Action(ctx context.Context, id, action string) error
}

var powerKeys = []struct{ key, action string }{{"s", "sleep"}, {"r", "restart"}, {"S", "shutdown"}}

type actState struct {
	procsGen  int
	procsDev  string
	supported *bool

	menu   string
	target string
	action string
}

type (
	remoteProcsMsg struct {
		gen int
		id  string
		p   *client.Processes
		err error
	}
	remoteTickMsg struct{ gen int }
	actionMsg     struct {
		action, name string
		err          error
	}
)

func (m model) actionHub() ActionHub {
	a, _ := m.opts.Hub.(ActionHub)
	return a
}

func (m *model) startRemoteProcs() tea.Cmd {
	d := m.cur()
	if m.view != viewProcesses || d == nil || m.actionHub() == nil {
		return nil
	}
	if m.act.procsDev != d.id {
		m.procs, m.visible, m.procsErr, m.act.supported = nil, nil, nil, nil
		m.act.procsDev = d.id
	}
	m.act.procsGen++
	return m.fetchRemoteProcs(m.act.procsGen, d.id)
}

func (m model) fetchRemoteProcs(gen int, id string) tea.Cmd {
	return m.hubCall(func(ctx context.Context, h Hub) tea.Msg {
		p, err := h.(ActionHub).Processes(ctx, id)
		return remoteProcsMsg{gen: gen, id: id, p: p, err: err}
	})
}

func (m model) updateActions(msg tea.Msg) (model, tea.Cmd, bool) {
	switch msg := msg.(type) {
	case remoteProcsMsg:
		if msg.gen != m.act.procsGen || m.view != viewProcesses || msg.id != m.act.procsDev {
			return m, nil, true
		}
		m.procsErr = msg.err
		if msg.err == nil && msg.p != nil {
			m.act.supported = &msg.p.Supported
			m.procs = nil
			if msg.p.Procs != nil {
				cleanAll(&msg.p.Procs.List)
				m.procs = msg.p.Procs.List
				if m.procs == nil {
					m.procs = []report.Process{}
				}
			}
			m.refreshProcs()
			m.clampScroll()
		}
		gen := msg.gen
		return m, tea.Tick(m.opts.Interval, func(time.Time) tea.Msg { return remoteTickMsg{gen} }), true
	case remoteTickMsg:
		if msg.gen != m.act.procsGen || m.view != viewProcesses {
			return m, nil, true
		}
		return m, m.fetchRemoteProcs(msg.gen, m.act.procsDev), true
	case actionMsg:
		if msg.err != nil {
			m.notice = msg.action + " failed: " + hubErr(msg.err)
		} else if msg.action == "wake" {
			m.notice = "Wake-up signal sent to " + msg.name + "."
		} else {
			m.notice = capitalize(msg.action) + " sent to " + msg.name + "; waiting for it to answer…"
		}
		return m, nil, true
	}
	return m, nil, false
}

func (m model) remoteProcsNote() string {
	t := m.theme
	d := m.cur()
	switch {
	case d == nil:
		return t.dim.Render("No device selected.")
	case m.actionHub() == nil:
		return t.dim.Render("Process lists for devices on a hub are not available.")
	case m.procsErr != nil:
		return t.err.Render("process list: " + hubErr(m.procsErr))
	case !d.online:
		return t.dim.Render(d.name + " is offline.")
	case m.act.supported != nil && !*m.act.supported:
		return t.dim.Render(d.name + " doesn't send its process list (it needs a machine-monitor agent).")
	}
	return t.dim.Render("Asking " + d.name + " for its processes…")
}

func (m model) wake() (tea.Model, tea.Cmd) {
	d := m.cur()
	switch {
	case d == nil:
		return m, nil
	case m.actionHub() == nil:
		m.notice = "this hub connection can't send actions"
		return m, nil
	case d.online:
		m.notice = d.name + " is already on · w wakes an offline device"
		return m, nil
	}
	id, name := d.id, d.name
	m.notice = ""
	return m, m.hubCall(func(ctx context.Context, h Hub) tea.Msg {
		return actionMsg{action: "wake", name: name, err: h.(ActionHub).Action(ctx, id, "wake")}
	})
}

func (d *device) allowedActions() []string {
	if d.last == nil {
		return nil
	}
	var out []string
	for _, pk := range powerKeys {
		if slices.Contains(d.last.Actions, pk.action) {
			out = append(out, pk.action)
		}
	}
	return out
}

func (m model) openPower() (tea.Model, tea.Cmd) {
	d := m.cur()
	switch {
	case d == nil:
		return m, nil
	case m.actionHub() == nil:
		m.notice = "this hub connection can't send actions"
	case !d.online:
		m.notice = d.name + " is offline · w to wake it"
	case len(d.allowedActions()) == 0:
		m.notice = "Remote power control is off on " + d.name + " (run its agent with --allow-power-actions)"
	default:
		m.notice = ""
		m.act.menu, m.act.target = "power", d.id
	}
	return m, nil
}

func (m model) actionKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	menu, id, action := m.act.menu, m.act.target, m.act.action
	m.act.menu, m.act.action = "", ""
	d := m.devByID(id)
	if d == nil {
		return m, nil
	}
	k := msg.String()
	if menu == "power" {
		for _, pk := range powerKeys {
			if k == pk.key && slices.Contains(d.allowedActions(), pk.action) {
				m.act.menu, m.act.action = "confirm", pk.action
			}
		}
		return m, nil
	}
	if k != "y" && k != "Y" {
		return m, nil
	}
	switch {
	case !d.online:
		m.notice = d.name + " went offline · " + action + " not sent"
		return m, nil
	case !slices.Contains(d.allowedActions(), action):
		m.notice = "Remote " + action + " is no longer allowed on " + d.name + " · not sent"
		return m, nil
	}
	name := d.name
	return m, m.hubCall(func(ctx context.Context, h Hub) tea.Msg {
		return actionMsg{action: action, name: name, err: h.(ActionHub).Action(ctx, id, action)}
	})
}

func (m model) actionPrompt() string {
	name := m.act.target
	var allowed []string
	if d := m.devByID(m.act.target); d != nil {
		name, allowed = d.name, d.allowedActions()
	}
	if m.act.menu == "confirm" {
		return capitalize(m.act.action) + " " + name + "? y to confirm, any other key cancels"
	}
	var opts []string
	for _, pk := range powerKeys {
		if slices.Contains(allowed, pk.action) {
			opts = append(opts, pk.key+" "+pk.action)
		}
	}
	return "Power " + name + ": " + strings.Join(opts, " · ") + " · any other key cancels"
}

func (m model) devByID(id string) *device {
	for i := range m.devs {
		if m.devs[i].id == id {
			return &m.devs[i]
		}
	}
	return nil
}

func actionResultNotice(raw json.RawMessage) string {
	var ev struct {
		Name   string `json:"name"`
		Result struct {
			OK      bool    `json:"ok"`
			Message *string `json:"message"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &ev) != nil {
		return ""
	}
	msg := "done"
	if !ev.Result.OK {
		msg = "failed"
	}
	if ev.Result.Message != nil && *ev.Result.Message != "" {
		msg = *ev.Result.Message
	}
	return clean(ev.Name + ": " + msg)
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
