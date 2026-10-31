package tui

import (
	"context"
	"strings"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/ContractorKeith/machine-monitor/internal/client"
	"github.com/ContractorKeith/machine-monitor/internal/report"
)

// localID is the one device of local mode.
const localID = "local"

// view is one of the numbered screens.
type view int

const (
	viewDashboard view = iota
	viewDevice
	viewProcesses
	viewAlerts
	viewCount
)

var viewNames = [viewCount]string{"Dashboard", "Device", "Processes", "Alerts"}

// viewKeys are the number keys, which follow PLAN §7 even where a view isn't built yet.
var viewKeys = [viewCount]string{"1", "2", "3", "6"}

// Messages.
type (
	tickMsg   struct{}
	reportMsg struct {
		r   *report.Report
		err error
	}
	procsMsg struct {
		p   []report.Process
		err error
	}
)

// model is the whole TUI state. Bubble Tea copies it by value on every Update.
type model struct {
	opts  Options
	theme theme
	keys  keyMap
	help  help.Model

	width, height int
	view          view
	showHelp      bool

	// devs are the devices the views show, in display order; sel is the selected one's id.
	// Local mode has one device, hub mode one per device on the hub.
	devs       []device
	sel        string
	collectErr error

	// Hub mode.
	ctx       context.Context // ends the stream and requests; Run cancels it on quit
	events    <-chan client.Event
	conn      hubConn
	rename    textinput.Model
	renameID  string // device being renamed while rename is focused
	confirmID string // device awaiting remove confirmation
	notice    string // last action's error, until the next action or reconnect
	orderGen  int    // counts order and snapshot events, so a late reorder failure can't undo them

	act actState // remote processes, wake and the power menu (actions.go)

	deviceScroll int

	al alertsUI // the Alerts view and the banner (alerts.go)

	procs       []report.Process
	visible     []report.Process // procs filtered and sorted; refreshProcs keeps it current
	procsErr    error
	procsBusy   bool
	procSort    sortCol
	procRev     bool
	procCursor  int
	procOffset  int
	filter      textinput.Model
	filterValue string
}

// New builds the model. Tests drive it through teatest; Run wraps it in a program.
func New(o Options) tea.Model {
	if o.Interval <= 0 {
		o.Interval = DefaultInterval
	}
	th := newTheme(o.NoColor, o.ASCII)
	h := help.New()
	h.Styles.FullKey = th.label
	h.Styles.FullDesc = lipglossNone
	h.Styles.FullSeparator = th.dim
	h.Styles.ShortKey = th.label
	h.Styles.ShortDesc = th.dim
	h.Styles.ShortSeparator = th.dim
	f := textinput.New()
	f.Prompt = "/"
	f.Placeholder = "name, user, pid or command"
	rn := textinput.New()
	rn.Prompt = ""
	rn.CharLimit = 60 // the hub's limit
	if o.Hub != nil {
		o.Procs = nil // the hub supplies process lists (actions.go)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return model{
		opts: o, theme: th, keys: newKeyMap(o.Hub != nil), help: h,
		width: 80, height: 24,
		procSort: sortCPU, filter: f,
		ctx: context.Background(), rename: rn,
	}
}

// Init starts the feed: the first local sample, or the hub's stream.
func (m model) Init() tea.Cmd {
	if m.multi() {
		return m.startStream()
	}
	return m.collect()
}

// multi reports whether the TUI follows a hub (many devices) rather than this machine.
func (m model) multi() bool { return m.opts.Hub != nil }

// now is the clock, for "last seen" times.
func (m model) now() time.Time { return m.opts.Now() }

// collect samples the machine off the UI goroutine.
func (m model) collect() tea.Cmd {
	c := m.opts.Collector
	return func() tea.Msg {
		if c == nil {
			return reportMsg{}
		}
		ctx, cancel := context.WithTimeout(context.Background(), collectTimeout)
		defer cancel()
		r, err := c.Collect(ctx)
		return reportMsg{r: r, err: err}
	}
}

// listProcs fetches the process list off the UI goroutine.
func (m model) listProcs() tea.Cmd {
	l := m.opts.Procs
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), collectTimeout)
		defer cancel()
		p, err := l.Processes(ctx)
		return procsMsg{p: p, err: err}
	}
}

// maybeProcs starts a process fetch when the Processes view is open and none is running.
func (m *model) maybeProcs() tea.Cmd {
	if m.multi() {
		return m.startRemoteProcs()
	}
	if m.view != viewProcesses || m.opts.Procs == nil || m.procsBusy {
		return nil
	}
	m.procsBusy = true
	return m.listProcs()
}

// Update handles one message.
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if next, cmd, ok := m.updateAlerts(msg); ok {
		return next, cmd
	}
	if next, cmd, ok := m.updateHub(msg); ok {
		return next, cmd
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.help.SetWidth(msg.Width)
		m.clampScroll()
		return m, nil

	case reportMsg:
		m.collectErr = msg.err
		if msg.r != nil {
			r := msg.r
			m.sel = localID
			m.editDev(localID, func(d *device) {
				d.name, d.online = str(r.Hostname), true
				d.push(r)
			})
		}
		interval := m.opts.Interval
		return m, tea.Tick(interval, func(time.Time) tea.Msg { return tickMsg{} })

	case tickMsg:
		return m, tea.Batch(m.collect(), m.maybeProcs())

	case procsMsg:
		m.procsBusy = false
		m.procsErr = msg.err
		if msg.err == nil {
			m.procs = msg.p
			m.refreshProcs()
		}
		m.clampScroll()
		return m, nil

	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

// handleKey routes a key press: filter input first, then global keys, then the view's keys.
func (m model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "ctrl+c" { // always quits; plain q is text while filtering
		return m, tea.Quit
	}
	if m.rename.Focused() || m.confirmID != "" {
		return m.hubInputKey(msg)
	}
	if m.act.menu != "" {
		return m.actionKey(msg)
	}
	if m.filter.Focused() {
		switch msg.String() {
		case "enter":
			m.filter.Blur()
			m.filterValue = strings.TrimSpace(m.filter.Value())
		case "esc":
			m.filter.Blur()
			m.filter.SetValue("")
			m.filterValue = ""
		default:
			var cmd tea.Cmd
			m.filter, cmd = m.filter.Update(msg)
			m.filterValue = strings.TrimSpace(m.filter.Value())
			m.procCursor, m.procOffset = 0, 0
			m.refreshProcs()
			return m, cmd
		}
		m.procCursor, m.procOffset = 0, 0
		m.refreshProcs()
		return m, nil
	}
	if next, cmd, ok := m.alertsPreKey(msg); ok {
		return next, cmd
	}

	k := m.keys
	switch {
	case key.Matches(msg, k.Quit):
		return m, tea.Quit
	case key.Matches(msg, k.Help):
		m.showHelp = !m.showHelp
		return m, nil
	case m.showHelp && key.Matches(msg, k.Clear):
		m.showHelp = false
		return m, nil
	case key.Matches(msg, k.Dashboard):
		return m.switchTo(viewDashboard)
	case key.Matches(msg, k.Device):
		return m.switchTo(viewDevice)
	case key.Matches(msg, k.Processes):
		return m.switchTo(viewProcesses)
	case key.Matches(msg, k.Alerts):
		return m.switchTo(viewAlerts)
	case key.Matches(msg, k.Next):
		return m.switchTo((m.view + 1) % viewCount)
	}

	if m.showHelp { // view keys would act on a view that isn't shown
		return m, nil
	}
	switch m.view {
	case viewDashboard:
		if m.multi() {
			return m.gridKey(msg)
		}
	case viewDevice:
		m.scrollDevice(msg)
	case viewProcesses:
		return m.procKey(msg)
	case viewAlerts:
		return m.alertsKey(msg)
	}
	return m, nil
}

// switchTo changes view and closes help; opening Processes fetches the list at once.
func (m model) switchTo(v view) (tea.Model, tea.Cmd) {
	m.view, m.showHelp = v, false
	return m, tea.Batch(m.maybeProcs(), m.maybeLoadAlerts())
}

// bodyHeight is the rows between the header (and the alert banner) and the footer.
func (m model) bodyHeight() int { return max(1, m.height-2-m.bannerRows()) }

// scrollDevice moves the Device view.
func (m *model) scrollDevice(msg tea.KeyPressMsg) {
	k := m.keys
	switch {
	case key.Matches(msg, k.Up):
		m.deviceScroll--
	case key.Matches(msg, k.Down):
		m.deviceScroll++
	case key.Matches(msg, k.PageUp):
		m.deviceScroll -= m.bodyHeight()
	case key.Matches(msg, k.PageDown):
		m.deviceScroll += m.bodyHeight()
	case key.Matches(msg, k.Top):
		m.deviceScroll = 0
	case key.Matches(msg, k.Bottom):
		m.deviceScroll = 1 << 30
	}
	m.clampScroll()
}

// clampScroll keeps scroll offsets and the process cursor in range after any change.
func (m *model) clampScroll() {
	maxDev := max(0, len(m.deviceLines())-m.bodyHeight())
	m.deviceScroll = max(0, min(m.deviceScroll, maxDev))

	n := len(m.visibleProcs())
	m.procCursor = max(0, min(m.procCursor, n-1))
	rows := m.procRows()
	if m.procCursor < m.procOffset {
		m.procOffset = m.procCursor
	}
	if m.procCursor >= m.procOffset+rows {
		m.procOffset = m.procCursor - rows + 1
	}
	m.procOffset = max(0, min(m.procOffset, max(0, n-rows)))
}

// View renders the frame: header, the current view, footer, clipped to the window.
func (m model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	v.WindowTitle = "machine-monitor"
	return v
}

// render builds the frame as a string; the benchmark and goldens call it directly.
func (m model) render() string {
	w, h := m.width, m.height
	if w <= 0 || h <= 0 {
		return ""
	}
	var body, foot string
	switch {
	case m.showHelp:
		body, foot = m.help.FullHelpView(m.keys.FullHelp()), "esc or ? to close"
	case m.view == viewDashboard && m.multi():
		body, foot = m.hubDashboard(), m.hubStatus()
	case m.view == viewDashboard:
		body, foot = m.dashboard(), m.statusLine()
	case m.view == viewDevice:
		lines := m.deviceLines()
		end := min(len(lines), m.deviceScroll+m.bodyHeight())
		body, foot = strings.Join(lines[min(m.deviceScroll, end):end], "\n"), m.deviceFooter(len(lines))
	case m.view == viewProcesses:
		body, foot = m.processes(), m.procFooter()
	case m.view == viewAlerts:
		body, foot = m.alertsView(), m.alertsFooter()
	}
	if h < 3 { // no room for the header and footer: the body alone
		return fit(body, w, h)
	}
	top := fit(m.header(), w, 1)
	if m.bannerRows() > 0 {
		top += "\n" + fit(m.bannerLine(), w, 1)
	}
	return top + "\n" + fit(body, w, m.bodyHeight()) + "\n" + fit(m.theme.dim.Render(foot), w, 1)
}

// header shows the app, the machine, the view tabs and the help hint.
func (m model) header() string {
	t := m.theme
	left := t.title.Render("machine-monitor")
	switch {
	case m.multi() && (m.view == viewDashboard || m.view == viewAlerts || m.cur() == nil):
		left += " " + t.dim.Render("·") + " hub " + m.hubHost()
	case m.multi():
		left += " " + t.dim.Render("·") + " " + m.cur().name
	case m.report() != nil && m.report().Hostname != nil:
		left += " " + t.dim.Render("·") + " " + *m.report().Hostname
	}
	// Tabs are named; when they don't fit, only the open one keeps its name.
	tabs := func(compact bool) string {
		out := make([]string, viewCount)
		for i := range viewCount {
			label := " " + viewKeys[i] + " " + viewNames[i] + " "
			if i == m.view && !m.showHelp {
				out[i] = t.tabOn.Render(label)
				continue
			}
			if compact {
				label = " " + viewKeys[i] + " "
			}
			out[i] = t.tabOff.Render(label)
		}
		return "  " + strings.Join(out, "")
	}
	// A long hub address or many tabs: shorten the hint first, then the tabs, then drop the
	// hint.
	for _, try := range []struct {
		compact bool
		hint    string
	}{{false, "? help  q quit"}, {false, "?/q"}, {true, "?/q"}} {
		l, r := left+tabs(try.compact), t.dim.Render(try.hint)
		if lipgloss.Width(l)+1+lipgloss.Width(r) <= m.width {
			return spread(l, r, m.width)
		}
	}
	return spread(left+tabs(true), "", m.width)
}

// statusLine is the footer for views without their own: collection state.
func (m model) statusLine() string {
	switch {
	case m.collectErr != nil:
		return "collect error: " + m.collectErr.Error()
	case m.report() == nil:
		return "collecting…"
	default:
		return "local · every " + m.opts.Interval.String() + " · tab next view · ? help"
	}
}
