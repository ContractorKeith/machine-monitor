package tui

// Hub mode (PLAN §5 "Connected TUI", §7 view 1): the dashboard becomes a grid of every
// device on a hub, kept live by the hub's event stream, with reorder, rename and remove.

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
)

// Hub is what hub mode needs from a hub. *client.Client is one; tests use a fake.
type Hub interface {
	// Stream follows the hub's events until ctx ends (see client.Client.Stream).
	Stream(ctx context.Context) <-chan client.Event
	Rename(ctx context.Context, id, name string) (*client.Device, error)
	Remove(ctx context.Context, id string) error
	SetOrder(ctx context.Context, ids []string) error
	// Host is the hub's host:port, for the header.
	Host() string
}

// actionTimeout bounds a rename, remove or reorder request.
const actionTimeout = 15 * time.Second

// Hub-mode messages.
type (
	streamMsg  struct{ ch <-chan client.Event }
	hubMsg     struct{ ev client.Event }
	hubDoneMsg struct{} // the stream ended for good
	renameMsg  struct {
		dev *client.Device
		err error
	}
	removeMsg struct {
		id  string
		err error
	}
	orderMsg struct {
		prev map[string]int // orders before the move, restored if the hub refused it
		gen  int            // orderGen when the move was made
		err  error
	}
)

// hubConn is the stream's state, for the footer.
type hubConn struct {
	connected bool
	ended     bool // the stream gave up (wrong key); nothing more will arrive
	err       error
	retry     time.Duration
	snapshot  bool // a snapshot has arrived since the program started
}

// startStream opens the hub's stream off the UI goroutine.
func (m model) startStream() tea.Cmd {
	h, ctx := m.opts.Hub, m.ctx
	return func() tea.Msg { return streamMsg{h.Stream(ctx)} }
}

// waitEvent waits for the stream's next event.
func waitEvent(ch <-chan client.Event) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return hubDoneMsg{}
		}
		return hubMsg{ev}
	}
}

// updateHub handles hub-mode messages; ok is false for anything else.
func (m model) updateHub(msg tea.Msg) (model, tea.Cmd, bool) {
	switch msg := msg.(type) {
	case streamMsg:
		m.events = msg.ch
		return m, waitEvent(m.events), true
	case hubMsg:
		m.apply(msg.ev)
		m.clampScroll()
		return m, waitEvent(m.events), true
	case hubDoneMsg:
		m.conn.ended, m.conn.connected = true, false
		return m, nil, true
	case renameMsg:
		if msg.err != nil {
			m.notice = "rename failed: " + msg.err.Error()
		} else if msg.dev != nil && m.hasDev(msg.dev.ID) { // not one removed meanwhile
			m.editDev(msg.dev.ID, func(d *device) { d.setRegistry(msg.dev) })
		}
		return m, nil, true
	case removeMsg:
		if msg.err != nil {
			m.notice = "remove failed: " + msg.err.Error()
		} else {
			m.removeDev(msg.id)
		}
		return m, nil, true
	case orderMsg:
		if msg.err != nil {
			m.notice = "reorder failed: " + msg.err.Error()
			if msg.gen != m.orderGen { // the hub has sent an order since: it wins
				return m, nil, true
			}
			m.devs = slices.Clone(m.devs)
			for i := range m.devs {
				if o, ok := msg.prev[m.devs[i].id]; ok {
					m.devs[i].order = o
				}
			}
			sortDevices(m.devs)
		}
		return m, nil, true
	}
	return m, nil, false
}

// apply folds one stream event into the device list.
func (m *model) apply(ev client.Event) {
	switch ev.Type {
	case client.EventConnected:
		m.conn.connected, m.conn.err = true, nil
		m.notice = "" // a stale action error must not hide the connection state
	case client.EventDisconnected:
		m.conn.connected, m.conn.err, m.conn.retry = false, ev.Err, ev.Retry
		if ev.Retry == 0 {
			m.conn.ended = true
		}
	case client.EventSnapshot:
		m.conn.snapshot = true
		m.notice = ""
		m.orderGen++
		devs := make([]device, 0, len(ev.Devices))
		for _, v := range ev.Devices {
			devs = append(devs, fromHub(v))
		}
		m.devs = devs
	case client.EventReport:
		if ev.Device == nil {
			return
		}
		m.editDev(ev.Device.ID, func(d *device) {
			d.setRegistry(ev.Device)
			if ev.Sample != nil {
				d.pushPoint(*ev.Sample)
			}
		})
	case client.EventDevice:
		if ev.Device != nil {
			m.editDev(ev.Device.ID, func(d *device) { d.setRegistry(ev.Device) })
		}
	case client.EventStatus:
		m.devs = slices.Clone(m.devs)
		for i := range m.devs {
			if on, ok := ev.Online[m.devs[i].id]; ok {
				m.devs[i].online = on
			}
		}
	case client.EventOrder:
		m.orderGen++
		m.devs = slices.Clone(m.devs)
		for i := range m.devs {
			if o, ok := ev.Order[m.devs[i].id]; ok {
				m.devs[i].order = o
			}
		}
	case client.EventRemoved:
		m.removeDev(ev.ID)
	default:
		return // alerts and the rest arrive with their own views
	}
	sortDevices(m.devs)
	m.fixSelection()
}

// removeDev drops a device, moving the selection to its neighbour.
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

// ---- keys --------------------------------------------------------------------------------

// hubInputKey handles a key while the rename box or the remove confirmation is open.
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

// hubCall runs a hub request off the UI goroutine.
func (m model) hubCall(f func(ctx context.Context, h Hub) tea.Msg) tea.Cmd {
	h, parent := m.opts.Hub, m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, actionTimeout)
		defer cancel()
		return f(ctx, h)
	}
}

// gridKey handles the hub dashboard's keys: move the selection, reorder, rename, remove.
func (m model) gridKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	k := m.keys
	nav := m.navOrder()
	if len(nav) == 0 {
		return m, nil
	}
	i := max(0, slices.IndexFunc(nav, func(d device) bool { return d.id == m.sel }))
	nOnline := len(m.online())
	ncol := gridColumns(m.width)
	switch {
	case key.Matches(msg, k.Left):
		i--
	case key.Matches(msg, k.Right):
		i++
	case key.Matches(msg, k.Up):
		if i >= nOnline { // from the offline row to the grid's last card
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
			i = nOnline // down off the grid into the offline row
		default:
			i = nOnline - 1
		}
	case key.Matches(msg, k.MoveUp):
		return m.move(-1)
	case key.Matches(msg, k.MoveDown):
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
	default:
		return m, nil
	}
	if i >= 0 && i < len(nav) {
		m.sel = nav[i].id
	}
	return m, nil
}

// move swaps the selected device with its neighbour in its group (the grid or the offline
// row), applies the new order at once and saves it on the hub.
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

// ---- drawing -----------------------------------------------------------------------------

// gridMinCard is the narrowest card the grid uses before it drops a column.
const gridMinCard = 56

// gridColumns is how many cards fit side by side: one under 80 columns.
func gridColumns(w int) int {
	if w < 80 {
		return 1
	}
	return max(1, w/gridMinCard)
}

// hubDashboard is view 1 in hub mode: a summary, the grid of online devices, and a row for
// the offline ones.
func (m model) hubDashboard() string {
	t := m.theme
	if !m.conn.snapshot {
		return t.dim.Render("Connecting to the hub at " + m.hubHost() + "…")
	}
	online, offline := m.online(), m.offline()
	var watts float64
	known := false
	for _, d := range online {
		if d.watts != nil {
			watts += *d.watts
			known = true
		}
	}
	w := none
	if known {
		w = "~" + fmtWatts(&watts)
	}
	summary := fmt.Sprintf("%d devices · %d online · %d offline · %s at the wall", len(m.devs), len(online), len(offline), w)
	if len(m.devs) == 1 {
		summary = strings.Replace(summary, "devices", "device", 1)
	}
	if len(m.devs) == 0 {
		return t.dim.Render(summary) + "\n\n" +
			t.dim.Render("No devices have reported to this hub yet. Run `machine-monitor agent --hub URL` on each machine.")
	}

	avail := m.bodyHeight() - 1 // under the summary
	foot := ""
	if len(offline) > 0 {
		foot = m.offlineRow(offline)
		avail--
	}
	grid := m.grid(online, avail)
	lines := []string{t.dim.Render(summary)}
	if grid != "" {
		lines = append(lines, grid)
	}
	body := strings.Join(lines, "\n")
	if foot != "" { // pinned to the bottom of the body
		gap := m.bodyHeight() - 1 - lipgloss.Height(body)
		body += strings.Repeat("\n", max(1, gap+1)) + foot
	}
	return body
}

// grid lays out the online cards in rows that fit h lines, scrolled so the selected card
// shows, with a line saying how many rows are out of view.
func (m model) grid(online []device, h int) string {
	if len(online) == 0 {
		return m.theme.dim.Render("Every device is offline.")
	}
	ncol := min(gridColumns(m.width), len(online))
	cw := min(maxCardWidth, m.width/ncol)
	var rows []string
	selRow := -1
	for start := 0; start < len(online); start += ncol {
		var cards []string
		for _, d := range online[start:min(start+ncol, len(online))] {
			if d.id == m.sel {
				selRow = len(rows)
			}
			cards = append(cards, m.card(d, cw, m.multi() && d.id == m.sel))
		}
		rows = append(rows, lipgloss.JoinHorizontal(lipgloss.Top, cards...))
	}
	// Show whole rows from top; move top down until the selected row fits (one line is kept
	// for the "more" note whenever rows are hidden).
	fits := func(top int) (end int) {
		used := 0
		for end = top; end < len(rows); end++ {
			hgt := lipgloss.Height(rows[end])
			if used+hgt > h {
				break
			}
			used += hgt
		}
		return end
	}
	top := 0
	end := fits(top)
	if end < len(rows) {
		h-- // room for the note
		end = fits(top)
	}
	for selRow >= end && top < selRow {
		top++
		end = fits(top)
	}
	end = max(end, min(top+1, len(rows))) // always at least one row, clipped by the frame
	out := strings.Join(rows[top:end], "\n")
	var more []string
	if top > 0 {
		more = append(more, fmt.Sprintf("↑ %d more", top*ncol))
	}
	if end < len(rows) {
		more = append(more, fmt.Sprintf("↓ %d more", len(online)-end*ncol))
	}
	if len(more) > 0 {
		out += "\n" + m.theme.dim.Render(strings.Join(more, " · ")+" · ←↑↓→ select")
	}
	return out
}

// offlineRow is the footer row of offline devices, the selected one highlighted. When they
// don't all fit, it shows a window around the selection and a "+N more" tail.
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
	width := func(lo, hi int) int { // the row showing parts[lo:hi]
		return lipgloss.Width(head + strings.Join(parts[lo:hi], sep) + tail(len(parts)-(hi-lo)))
	}
	lo, hi := sel, sel+1
	for grew := true; grew; { // grow right first, then left, while the row fits
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

// ago is how long ago a Unix-ms time was: "3h ago", or "never".
func (m model) ago(ms int64) string {
	if ms <= 0 {
		return "never seen"
	}
	d := m.now().Sub(time.UnixMilli(ms))
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// hubHost is the hub's address for display.
func (m model) hubHost() string {
	if m.opts.Hub == nil {
		return ""
	}
	return m.opts.Hub.Host()
}

// hubStatus is the hub-mode footer: an open prompt, the connection state, or the keys.
func (m model) hubStatus() string {
	switch {
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
	case m.notice != "":
		return m.notice
	case m.conn.ended:
		return "hub " + m.hubHost() + ": " + errText(m.conn.err) + " · no longer updating · q quit"
	case !m.conn.connected && m.conn.err != nil:
		retry := max(time.Second, m.conn.retry.Round(time.Second)) // never "retrying in 0s"
		return fmt.Sprintf("hub %s unreachable: %s · retrying in %s", m.hubHost(), errText(m.conn.err), retry)
	case !m.conn.connected:
		return "connecting to " + m.hubHost() + "…"
	}
	return "hub " + m.hubHost() + " · ←↑↓→ select · J/K move · R rename · D remove · ? help"
}

func errText(err error) string {
	if err == nil {
		return "the stream ended"
	}
	return err.Error()
}
