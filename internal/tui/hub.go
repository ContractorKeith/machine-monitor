package tui

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/ContractorKeith/machine-monitor/internal/client"
	"github.com/ContractorKeith/machine-monitor/internal/history"
)

type Hub interface {
	Stream(ctx context.Context) <-chan client.Event
	Rename(ctx context.Context, id, name string) (*client.Device, error)
	Remove(ctx context.Context, id string) error
	SetOrder(ctx context.Context, ids []string) error

	Host() string
}

const actionTimeout = 15 * time.Second

const maxBatch = 512

const staleAfter = 10 * time.Second

const (
	lateAfter = 20 * time.Second
	lateClear = 15 * time.Second
)

const hubTick = time.Second

type (
	streamMsg   struct{ ch <-chan client.Event }
	hubMsg      struct{ ev client.Event }
	hubBatchMsg struct {
		evs  []client.Event
		done bool
	}
	hubDoneMsg struct{}
	sparkMsg   struct {
		id  string
		pts []history.Point
		err error
	}
	hubTickMsg struct{}
	renameMsg  struct {
		dev *client.Device
		err error
	}
	removeMsg struct {
		id  string
		err error
	}
	orderMsg struct {
		prev map[string]int
		gen  int
		err  error
	}
)

type hubConn struct {
	connected bool
	ended     bool
	err       error
	retry     time.Duration
	snapshot  bool
	lastEvent time.Time
	stale     bool
	late      bool

	lag     time.Duration
	refAt   time.Time
	refSeen int64
}

func (c *hubConn) noteReport(now time.Time, seen int64) {
	if seen == 0 {
		return
	}
	if c.refAt.IsZero() {
		c.refAt, c.refSeen = now, seen
	}
	c.lag = now.Sub(c.refAt) - time.Duration(seen-c.refSeen)*time.Millisecond
	if c.lag < 0 {
		c.refAt, c.refSeen, c.lag = now, seen, 0
	}
	switch {
	case c.lag >= lateAfter:
		c.late = true
	case c.lag < lateClear:
		c.late = false
	}
}

func (m model) startStream() tea.Cmd {
	h, ctx := m.opts.Hub, m.ctx
	return func() tea.Msg { return streamMsg{h.Stream(ctx)} }
}

var batchWindow = 250 * time.Millisecond

func waitEvent(ch <-chan client.Event) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return hubDoneMsg{}
		}
		evs := []client.Event{ev}
		window := time.NewTimer(batchWindow)
		defer window.Stop()
		for len(evs) < maxBatch {
			select {
			case ev, ok := <-ch:
				if !ok {
					return hubBatchMsg{evs: evs, done: true}
				}
				evs = append(evs, ev)
			case <-window.C:
				return hubBatchMsg{evs: evs}
			}
		}
		return hubBatchMsg{evs: evs}
	}
}

func tickHub() tea.Cmd {
	return tea.Tick(hubTick, func(time.Time) tea.Msg { return hubTickMsg{} })
}

func (m model) applyBatch(evs []client.Event, done bool) (model, tea.Cmd) {
	m.batching = true
	cmds := make([]tea.Cmd, 0, 2)
	for _, ev := range evs {

		if ev.Type != client.EventConnected && ev.Type != client.EventDisconnected {
			m.conn.lastEvent, m.conn.stale = m.now(), false
		}
		if ev.Type == client.EventReport && ev.Device != nil {
			m.conn.noteReport(m.now(), ev.Device.LastSeen)
		}
		var cmd tea.Cmd
		m, cmd = m.handleEvent(ev)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	if m.dirty {
		if !m.owned {
			m.devs = slices.Clone(m.devs)
		}
		sortDevices(m.devs)
		m.fixSelection()
	}
	m.batching, m.owned, m.dirty = false, false, false
	m.clampScroll()
	cmds = append(cmds, m.sparkSeed())
	switch {
	case done:
		m.conn.ended, m.conn.connected = true, false
	case m.events != nil:
		cmds = append(cmds, waitEvent(m.events))
	}
	return m, tea.Batch(cmds...)
}

func (m model) handleEvent(ev client.Event) (model, tea.Cmd) {
	switch ev.Type {
	case client.EventAlert, client.EventAlertSettings:
		return m, m.applyAlert(ev)
	case client.EventEnergySettings:
		m.applyEnergySettings(ev)
		return m, nil
	case client.EventPaired:
		m.pairedEvent()
		return m, nil
	case client.EventDisconnected:
		m.lostHub()
	}
	m.apply(ev)
	if ev.Type == client.EventSnapshot && m.al.loaded {
		return m, m.loadAlerts()
	}
	return m, nil
}

func (m model) updateHub(msg tea.Msg) (model, tea.Cmd, bool) {
	if next, cmd, ok := m.updateActions(msg); ok {
		return next, cmd, true
	}
	switch msg := msg.(type) {
	case streamMsg:
		m.events = msg.ch
		return m, waitEvent(m.events), true
	case hubMsg:
		next, cmd := m.applyBatch([]client.Event{msg.ev}, false)
		return next, cmd, true
	case hubBatchMsg:
		next, cmd := m.applyBatch(msg.evs, msg.done)
		return next, cmd, true
	case hubDoneMsg:
		m.conn.ended, m.conn.connected = true, false
		return m, nil, true
	case hubTickMsg:
		if m.conn.snapshot && m.now().Sub(m.conn.lastEvent) >= staleAfter {
			m.conn.stale = true
		}
		seed := m.sparkSeed()
		return m, tea.Batch(tickHub(), seed), true
	case sparkMsg:
		m.seeding = ""
		if m.hasDev(msg.id) {
			m.editDev(msg.id, func(d *device) {
				if msg.err == nil {
					d.seed(msg.pts)
				}
				d.seeded = true
			})
		}
		return m, nil, true
	case renameMsg:
		if msg.err != nil {
			m.notice = "rename failed: " + hubErr(msg.err)
		} else if cleanAll(msg.dev); msg.dev != nil && m.hasDev(msg.dev.ID) {
			m.editDev(msg.dev.ID, func(d *device) { d.setRegistry(msg.dev) })
		}
		return m, nil, true
	case removeMsg:
		if msg.err != nil {
			m.notice = "remove failed: " + hubErr(msg.err)
		} else {
			m.removeDev(msg.id)
		}
		return m, nil, true
	case orderMsg:
		if msg.err != nil {
			m.notice = "reorder failed: " + hubErr(msg.err)
			if msg.gen != m.orderGen {
				return m, nil, true
			}
			m.ownDevs()
			for i := range m.devs {
				if o, ok := msg.prev[m.devs[i].id]; ok {
					m.devs[i].order = o
				}
			}
			m.resort()
		}
		return m, nil, true
	}
	return m, nil, false
}

func (m *model) apply(ev client.Event) {
	switch ev.Type {
	case client.EventConnected:
		m.conn.connected, m.conn.err = true, nil
		m.notice = ""
	case client.EventDisconnected:
		m.conn.connected, m.conn.err, m.conn.retry = false, ev.Err, ev.Retry
		if m.conn.snapshot {
			m.conn.stale = true
		}
		if ev.Retry == 0 {
			m.conn.ended = true
		}
	case client.EventSnapshot:
		m.conn.snapshot = true
		m.conn.lag, m.conn.late, m.conn.refAt = 0, false, time.Time{}
		m.notice = ""
		m.orderGen++
		devs := make([]device, 0, len(ev.Devices))
		for _, v := range ev.Devices {
			devs = append(devs, fromHub(v))
		}
		m.devs, m.owned = devs, m.batching
	case client.EventReport:
		if ev.Device == nil {
			return
		}
		pts := ev.Samples
		if len(pts) == 0 && ev.Sample != nil {
			pts = []history.Point{*ev.Sample}
		}
		m.editDev(ev.Device.ID, func(d *device) {
			d.setRegistry(ev.Device)
			for _, p := range pts {
				d.pushPoint(p)
			}
		})
	case client.EventDevice:
		if ev.Device != nil {
			m.editDev(ev.Device.ID, func(d *device) { d.setRegistry(ev.Device) })
		}
	case client.EventStatus:
		m.ownDevs()
		for i := range m.devs {
			if on, ok := ev.Online[m.devs[i].id]; ok {
				m.devs[i].online = on
			}
		}
	case client.EventOrder:
		m.orderGen++
		m.ownDevs()
		for i := range m.devs {
			if o, ok := ev.Order[m.devs[i].id]; ok {
				m.devs[i].order = o
			}
		}
	case client.EventRemoved:
		m.removeDev(ev.ID)
	case client.EventActionResult:
		if n := actionResultNotice(ev.Raw); n != "" {
			m.notice = n
		}
		return
	default:
		return
	}
	m.resort()
}

const sparkSpan = historyLen * 2 * time.Second

func (m *model) sparkSeed() tea.Cmd {
	d, src := m.cur(), m.source()
	if src == nil || d == nil || d.seeded || m.seeding != "" || !m.conn.snapshot {
		return nil
	}
	id, parent := d.id, m.ctx
	m.seeding = id
	to := float64(m.now().UnixMilli())
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, viewTimeout)
		defer cancel()
		res, err := src.History(ctx, id, to-float64(sparkSpan.Milliseconds()), to, historyLen)
		if err != nil {
			return sparkMsg{id: id, err: err}
		}
		return sparkMsg{id: id, pts: res.Points}
	}
}

func (m *model) removeDev(id string) {
	nav := m.navOrder()
	i := slices.IndexFunc(nav, func(d device) bool { return d.id == id })
	if i < 0 {
		return
	}
	if m.sel == id {
		m.sel = ""
		if i+1 < len(nav) {
			m.sel = nav[i+1].id
		} else if i > 0 {
			m.sel = nav[i-1].id
		}
	}
	m.devs = slices.DeleteFunc(slices.Clone(m.devs), func(d device) bool { return d.id == id })
	m.fixSelection()
}

func (m model) hubInputKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.confirmID != "" {
		id := m.confirmID
		m.confirmID = ""
		if msg.String() == "y" || msg.String() == "Y" {
			m.notice = ""
			return m, m.hubCall(func(ctx context.Context, h Hub) tea.Msg {
				return removeMsg{id: id, err: h.Remove(ctx, id)}
			})
		}
		return m, nil
	}
	switch msg.String() {
	case "enter":
		m.rename.Blur()
		id, name := m.renameID, strings.TrimSpace(m.rename.Value())
		if name == "" {
			return m, nil
		}
		m.notice = ""
		return m, m.hubCall(func(ctx context.Context, h Hub) tea.Msg {
			d, err := h.Rename(ctx, id, name)
			return renameMsg{dev: d, err: err}
		})
	case "esc":
		m.rename.Blur()
		return m, nil
	}
	var cmd tea.Cmd
	m.rename, cmd = m.rename.Update(msg)
	return m, cmd
}

func (m model) hubCall(f func(ctx context.Context, h Hub) tea.Msg) tea.Cmd {
	h, parent := m.opts.Hub, m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, actionTimeout)
		defer cancel()
		return f(ctx, h)
	}
}

func (m model) gridKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	k := m.keys
	if key.Matches(msg, k.Add) {
		return m.openAdd()
	}
	nav := m.cardNav()
	if len(nav) == 0 {
		return m, nil
	}
	i := max(0, slices.IndexFunc(nav, func(d device) bool { return d.id == m.sel }))
	nOnline := len(m.cards())
	ncol := gridColumns(m.width)
	switch {
	case key.Matches(msg, k.Left):
		i--
	case key.Matches(msg, k.Right):
		i++
	case key.Matches(msg, k.Up):
		if i >= nOnline {
			i = nOnline - 1
		} else {
			i -= ncol
		}
	case key.Matches(msg, k.Down):
		switch {
		case i >= nOnline:
		case i+ncol < nOnline:
			i += ncol
		case nOnline < len(nav):
			i = nOnline
		default:
			i = nOnline - 1
		}
	case key.Matches(msg, k.MoveUp), key.Matches(msg, k.MoveDown):
		if m.filterValue != "" {
			m.notice = "clear the filter (esc) to move cards"
			return m, nil
		}
		if key.Matches(msg, k.MoveUp) {
			return m.move(-1)
		}
		return m.move(1)

	case key.Matches(msg, k.Rename):
		if d := m.cur(); d != nil {
			m.renameID = d.id
			m.rename.SetValue(d.name)
			m.rename.CursorEnd()
			m.notice = ""
			return m, m.rename.Focus()
		}
	case key.Matches(msg, k.Remove):
		if d := m.cur(); d != nil {
			m.confirmID = d.id
			m.notice = ""
		}
		return m, nil
	case key.Matches(msg, k.Wake):
		return m.wake()
	case key.Matches(msg, k.Power):
		return m.openPower()
	default:
		return m, nil
	}
	if i >= 0 && i < len(nav) {
		m.sel = nav[i].id
	}
	return m, nil
}

func (m model) move(delta int) (tea.Model, tea.Cmd) {
	sel := m.cur()
	if sel == nil {
		return m, nil
	}
	group := m.online()
	if !sel.online {
		group = m.offline()
	}
	gi := slices.IndexFunc(group, func(d device) bool { return d.id == sel.id })
	gj := gi + delta
	if gj < 0 || gj >= len(group) {
		return m, nil
	}
	other := group[gj].id
	ids := make([]string, len(m.devs))
	prev := make(map[string]int, len(m.devs))
	a, b := -1, -1
	for i, d := range m.devs {
		ids[i] = d.id
		prev[d.id] = d.order
		switch d.id {
		case sel.id:
			a = i
		case other:
			b = i
		}
	}
	ids[a], ids[b] = ids[b], ids[a]
	m.devs = slices.Clone(m.devs)
	for i := range m.devs {
		m.devs[i].order = slices.Index(ids, m.devs[i].id)
	}
	sortDevices(m.devs)
	m.notice = ""
	gen := m.orderGen
	return m, m.hubCall(func(ctx context.Context, h Hub) tea.Msg {
		return orderMsg{prev: prev, gen: gen, err: h.SetOrder(ctx, ids)}
	})
}

const gridMinCard = 56

func gridColumns(w int) int {
	if w < 80 {
		return 1
	}
	return max(1, w/gridMinCard)
}

func (m model) hubDashboard() string {
	t := m.theme
	if !m.conn.snapshot {
		return t.dim.Render("Connecting to the hub at " + m.hubHost() + "…")
	}
	nOnline, nOffline := len(m.online()), 0
	nOffline = len(m.devs) - nOnline
	var watts float64
	known := false
	for _, d := range m.devs {
		if m.isOnline(d) && d.watts != nil {
			watts += *d.watts
			known = true
		}
	}
	w := none
	if known {
		w = "~" + fmtWatts(&watts)
	}
	summary := fmt.Sprintf("%d devices · %d online · %d offline · %s at the wall", len(m.devs), nOnline, nOffline, w)
	if len(m.devs) == 1 {
		summary = strings.Replace(summary, "devices", "device", 1)
	}
	if c := m.todayText(); c != "" {
		summary += " · " + c
	}
	head := t.dim.Render(summary)
	if n := m.alertsActive(); n >= 0 {
		head += t.dim.Render(" · ") + m.alertsCount(n)
	}
	switch {
	case m.conn.stale:
		head += t.dim.Render(" · ") + t.warm.Render(fmt.Sprintf("no word from the hub for %s: online state unknown",
			m.now().Sub(m.conn.lastEvent).Truncate(time.Second)))
	case m.conn.late && m.conn.connected:
		head += t.dim.Render(" · ") + t.warm.Render(fmt.Sprintf("data %s behind: the link is slow", m.conn.lag.Truncate(time.Second)))
	}
	if len(m.devs) == 0 {
		return head + "\n\n" +
			t.dim.Render("No devices have reported to this hub yet. Press a to add one.")
	}
	online, offline := m.cards(), m.offCards()
	if m.filterValue != "" {
		head += t.dim.Render(fmt.Sprintf(" · filter %q: %d shown · esc clears", m.filterValue, len(online)+len(offline)))
	}

	avail := m.bodyHeight() - 1
	foot := ""
	if len(offline) > 0 {
		foot = m.offlineRow(offline)
		avail--
	}
	grid := m.grid(online, avail)
	if len(online)+len(offline) == 0 {
		grid = t.dim.Render("No device name matches the filter.")
	}
	lines := []string{head}
	if grid != "" {
		lines = append(lines, grid)
	}
	body := strings.Join(lines, "\n")
	if foot != "" {
		gap := m.bodyHeight() - 1 - lipgloss.Height(body)
		body += strings.Repeat("\n", max(1, gap+1)) + foot
	}
	return body
}

func (m model) grid(online []device, h int) string {
	if len(online) == 0 {
		return m.theme.dim.Render("Every device is offline.")
	}
	ncol := min(gridColumns(m.width), len(online))
	cw := min(maxCardWidth, m.width/ncol)
	nrows := (len(online) + ncol - 1) / ncol
	selRow := -1
	if i := slices.IndexFunc(online, func(d device) bool { return d.id == m.sel }); i >= 0 {
		selRow = i / ncol
	}

	drawn := make(map[int]string)
	row := func(r int) string {
		if s, ok := drawn[r]; ok {
			return s
		}
		var cards []string
		for _, d := range online[r*ncol : min((r+1)*ncol, len(online))] {
			cards = append(cards, m.card(d, cw, m.multi() && d.id == m.sel))
		}
		drawn[r] = lipgloss.JoinHorizontal(lipgloss.Top, cards...)
		return drawn[r]
	}

	fits := func(top int) (end int) {
		used := 0
		for end = top; end < nrows; end++ {
			hgt := lipgloss.Height(row(end))
			if used+hgt > h {
				break
			}
			used += hgt
		}
		return end
	}
	top := 0
	end := fits(top)
	if end < nrows {
		h--
		end = fits(top)
	}
	if selRow >= end {
		top = selRow
		for top > 0 {
			if e := fits(top - 1); e <= selRow {
				break
			}
			top--
		}
		end = fits(top)
	}
	end = max(end, min(top+1, nrows))
	var shown []string
	for r := top; r < end; r++ {
		shown = append(shown, row(r))
	}
	out := strings.Join(shown, "\n")
	var more []string
	if top > 0 {
		more = append(more, fmt.Sprintf("↑ %d more", top*ncol))
	}
	if end < nrows {
		more = append(more, fmt.Sprintf("↓ %d more", len(online)-end*ncol))
	}
	if len(more) > 0 {
		out += "\n" + m.theme.dim.Render(strings.Join(more, " · ")+" · ←↑↓→ select")
	}
	return out
}

func (m model) offlineRow(offline []device) string {
	t := m.theme
	parts := make([]string, len(offline))
	sel := 0
	for i, d := range offline {
		name := d.name
		if d.id == m.sel {
			marker := "▶ "
			if t.ascii {
				marker = "> "
			}
			name, sel = t.selected.Render(marker+d.name), i
		}
		parts[i] = name + " " + t.dim.Render("("+m.ago(d.lastSeen)+")")
	}
	const sep = "   "
	head := t.label.Render(fmt.Sprintf("Offline (%d)", len(offline))) + "  "
	tail := func(hidden int) string {
		if hidden == 0 {
			return ""
		}
		return sep + t.dim.Render(fmt.Sprintf("+%d more", hidden))
	}
	width := func(lo, hi int) int {
		return lipgloss.Width(head + strings.Join(parts[lo:hi], sep) + tail(len(parts)-(hi-lo)))
	}
	lo, hi := sel, sel+1
	for grew := true; grew; {
		grew = false
		if hi < len(parts) && width(lo, hi+1) <= m.width {
			hi, grew = hi+1, true
		}
		if lo > 0 && width(lo-1, hi) <= m.width {
			lo, grew = lo-1, true
		}
	}
	return head + strings.Join(parts[lo:hi], sep) + tail(len(parts)-(hi-lo))
}

func (m model) ago(ms int64) string {
	if ms <= 0 {
		return "never seen"
	}
	d := m.now().Sub(time.UnixMilli(ms))
	switch {
	case d < 5*time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func (m model) hubHost() string {
	if m.opts.Hub == nil {
		return ""
	}
	return m.opts.Hub.Host()
}

func (m model) hubStatus() string {
	switch {
	case m.act.menu != "":
		return m.actionPrompt()
	case m.rename.Focused():
		name := m.renameID
		if d := m.cur(); d != nil && d.id == m.renameID {
			name = d.name
		}
		return "Rename " + name + ": " + m.rename.View() + "   enter save · esc cancel"
	case m.confirmID != "":
		name := m.confirmID
		for _, d := range m.devs {
			if d.id == m.confirmID {
				name = d.name
			}
		}
		return "Remove " + name + " and its history from the hub? y to confirm, any other key cancels"
	case m.filter.Focused():
		return m.filter.View() + "   enter keep · esc clear"
	case m.notice != "":
		return m.notice
	case m.conn.ended:
		return "hub " + m.hubHost() + ": " + errText(m.conn.err) + " · no longer updating · q quit"
	case !m.conn.connected && m.conn.err != nil:
		retry := max(time.Second, m.conn.retry.Round(time.Second))
		return fmt.Sprintf("hub %s unreachable: %s · retrying in %s", m.hubHost(), errText(m.conn.err), retry)
	case !m.conn.connected:
		return "connecting to " + m.hubHost() + "…"
	case m.conn.stale:
		return "hub " + m.hubHost() + ": the stream has gone quiet · waiting for it"
	case m.conn.late:
		return "hub " + m.hubHost() + ": the link is too slow to keep up · showing the newest data it can"
	}
	full := "hub " + m.hubHost() + " · ←↑↓→ select · J/K move · R rename · D remove · a add · / filter · , settings"
	if lipgloss.Width(full) <= m.width {
		return full
	}
	return "hub " + m.hubHost() + " · ←↑↓→ select · a add · / filter · , settings · ? help"
}

func errText(err error) string {
	if err == nil {
		return "the stream ended"
	}
	return hubErr(err)
}
