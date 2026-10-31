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
	"github.com/ContractorKeith/machine-monitor/internal/power"
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
	viewHistory
	viewEnergy
	viewAlerts
	viewCount
	// Views without a number or tab.
	viewSettings  = viewCount     // `,`
	viewAddDevice = viewCount + 1 // `a` on the dashboard or in Settings (hub mode)
)

var viewNames = [viewCount]string{"Dashboard", "Device", "Processes", "History", "Energy", "Alerts"}

// viewKeys are the number keys, which follow PLAN §7 even where a view isn't built yet.
var viewKeys = [viewCount]string{"1", "2", "3", "4", "5", "6"}

// Messages.
type (
	tickMsg   struct{}
	startMsg  struct{} // Init's reads are due (see Init)
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
	// While a batch of stream events applies: devs is already this model's own copy (owned)
	// and needs sorting at the end (dirty).
	batching, owned, dirty bool

	act actState // remote processes, wake and the power menu (actions.go)

	deviceScroll int

	al alertsUI // the Alerts view and the banner (alerts.go)

	hv    histState      // the History view (history.go)
	ev    energyState    // the Energy view (energy.go)
	guess power.Settings // price guessed from the time zone, until one is saved

	set   settingsState // the Settings view (settings.go)
	add   addState      // the Add device view (add.go)
	today todayState    // cost since midnight, for the dashboard header (energy.go)

	procs       []report.Process
	visible     []report.Process // procs filtered and sorted; refreshProcs keeps it current
	procsErr    error
	procsBusy   bool
	procSort    sortCol
	procRev     bool
	procCursor  int
	procOffset  int
	filter      textinput.Model
	filterValue string // the one filter: device names on the dashboard, processes in Processes
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
	p := loadPrefs(o.PrefsPath)
	th.fahrenheit = p.Fahrenheit
	if o.Price == nil && o.Hub == nil {
		o.Price = p.Price
	}
	return model{
		opts: o, theme: th, keys: newKeyMap(o.Hub != nil), help: h,
		width: 80, height: 24,
		procSort: sortCPU, filter: f,
		ctx: context.Background(), rename: rn,
		hv: histState{rng: histDefaultRange, cursor: 1}, ev: energyState{rng: energyDefaultRange},
		guess: power.DefaultSettings(),
	}
}

// Init starts the feed: the first local sample, or the hub's stream. Bubble Tea throws
// away the model Init runs on, so nothing here may change state: the reads that track
// requests in the model (cost today, the alerts behind the header's count) start when
// startMsg reaches Update.
func (m model) Init() tea.Cmd {
	start := func() tea.Msg { return startMsg{} }
	if m.multi() {
		return tea.Batch(m.startStream(), tickHub(), start)
	}
	return tea.Batch(m.collect(), start)
}

// start begins the reads Init can't: they number their requests in the live model.
func (m model) start() (model, tea.Cmd) {
	today := m.fetchToday()
	var alerts tea.Cmd
	if m.multi() {
		alerts = m.loadAlerts()
	}
	return m, tea.Batch(today, alerts)
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
	if next, cmd, ok := m.updateViews(msg); ok {
		return next, cmd
	}
	if next, cmd, ok := m.updateSettings(msg); ok {
		return next, cmd
	}
	if next, cmd, ok := m.updateToday(msg); ok {
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
			m.recordLocal(r)
			m.sel = localID
			m.editDev(localID, func(d *device) {
				d.name, d.online = str(r.Hostname), true
				d.push(r)
			})
		}
		interval := m.opts.Interval
		return m, tea.Tick(interval, func(time.Time) tea.Msg { return tickMsg{} })

	case startMsg:
		return m.start()

	case tickMsg:
		return m, tea.Batch(m.collect(), m.maybeProcs())

	case procsMsg:
		m.procsBusy = false
		m.procsErr = msg.err
		if msg.err == nil {
			cleanAll(&msg.p)
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
	m.notice = "" // a notice (a refused J/K, a failed action) answers one key press, not the session
	if m.rename.Focused() || m.confirmID != "" {
		return m.hubInputKey(msg)
	}
	if m.act.menu != "" {
		return m.actionKey(msg)
	}
	if m.filter.Focused() {
		return m.filterKey(msg)
	}
	if m.set.form.open && m.view == viewSettings {
		return m.priceFormKey(msg)
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
	case key.Matches(msg, k.History):
		return m.switchTo(viewHistory)
	case key.Matches(msg, k.Energy):
		return m.switchTo(viewEnergy)
	case key.Matches(msg, k.Alerts):
		return m.switchTo(viewAlerts)
	case key.Matches(msg, k.Next):
		return m.switchTo((m.view + 1) % viewCount) // Settings and Add device go to view 1
	case key.Matches(msg, k.Settings) && m.view != viewSettings:
		m.set.prev = m.view
		if m.view == viewAddDevice {
			m.set.prev = m.add.prev
		}
		return m.switchTo(viewSettings)
	case key.Matches(msg, k.Filter) && !m.showHelp && ((m.view == viewDashboard && m.multi()) || m.view == viewProcesses):
		return m.openFilter()
	}

	if m.showHelp { // view keys would act on a view that isn't shown
		return m, nil
	}
	switch m.view {
	case viewDashboard:
		if key.Matches(msg, k.Clear) && m.filterValue != "" {
			m.filter.SetValue("")
			m.filterValue = ""
			m.applyFilter()
			return m, nil
		}
		if m.multi() {
			return m.gridKey(msg)
		}
	case viewDevice:
		m.scrollDevice(msg)
	case viewProcesses:
		return m.procKey(msg)
	case viewHistory:
		return m.historyKey(msg)
	case viewEnergy:
		return m.energyKey(msg)
	case viewAlerts:
		return m.alertsKey(msg)
	case viewSettings:
		return m.settingsKey(msg)
	case viewAddDevice:
		return m.addKey(msg)
	}
	return m, nil
}

// openFilter focuses the filter input for the open view: device names on the dashboard,
// processes in Processes.
func (m model) openFilter() (tea.Model, tea.Cmd) {
	m.filter.Placeholder = "name, user, pid or command"
	if m.view == viewDashboard {
		m.filter.Placeholder = "device name"
	}
	m.filter.SetValue(m.filterValue)
	m.filter.CursorEnd()
	return m, m.filter.Focus()
}

// filterKey handles a key while the filter input is open: typing filters at once, enter
// keeps the filter, esc clears it.
func (m model) filterKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	switch msg.String() {
	case "enter":
		m.filter.Blur()
	case "esc":
		m.filter.Blur()
		m.filter.SetValue("")
	default:
		m.filter, cmd = m.filter.Update(msg)
	}
	m.filterValue = strings.TrimSpace(m.filter.Value())
	m.applyFilter()
	return m, cmd
}

// applyFilter re-applies the one filter to both the device cards and the process list.
func (m *model) applyFilter() {
	m.selectShown()
	m.procCursor, m.procOffset = 0, 0
	m.refreshProcs()
}

// switchTo changes view and closes help; opening Processes, History, Energy or Alerts
// reads its data at once.
func (m model) switchTo(v view) (tea.Model, tea.Cmd) {
	m.view, m.showHelp = v, false
	// Each starter bumps state through m, so call them before returning m.
	procs := m.maybeProcs()
	alerts := m.maybeLoadAlerts()
	open := m.openView()
	var set tea.Cmd
	if v == viewSettings {
		m.set.cursor, m.set.form, m.set.notice = 0, priceForm{}, ""
		set = m.openSettings()
	}
	return m, tea.Batch(procs, alerts, open, set)
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
	if m.view == viewEnergy {
		m.clampEnergy()
	}
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
	case m.view == viewHistory:
		body, foot = m.historyView(), m.historyFooter()
	case m.view == viewEnergy:
		body, foot = m.energyView(), m.energyFooter()
	case m.view == viewAlerts:
		body, foot = m.alertsView(), m.alertsFooter()
	case m.view == viewSettings:
		body, foot = m.settingsView(), m.settingsFooter()
	case m.view == viewAddDevice:
		body, foot = m.addView(), m.addFooter()
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

// header shows the app, the hub or machine, the view tabs and the help hint.
func (m model) header() string {
	t := m.theme
	title := t.title.Render("machine-monitor")
	sep := " " + t.dim.Render("·") + " "
	left := title
	switch {
	case m.multi():
		left += sep + "hub " + m.hubHost()
		// Views about one device name it after the hub, so the hub never drops out.
		if d := m.cur(); d != nil && (m.view == viewDevice || m.view == viewProcesses || m.view == viewHistory) {
			arrow := " › "
			if t.ascii {
				arrow = " > "
			}
			left += t.dim.Render(arrow) + d.name
		}
	case m.report() != nil && m.report().Hostname != nil:
		left += sep + *m.report().Hostname
	}
	switch m.view {
	case viewSettings:
		left += sep + t.label.Render("Settings")
	case viewAddDevice:
		left += sep + t.label.Render("Add device")
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
	// A long hub address or a narrow window: shorten the hint first, then the tabs, then
	// drop the hint, then the hub or machine name, then the title, so the open view's name
	// stays in sight.
	for _, try := range []struct {
		left    string
		compact bool
		hint    string
	}{{left, false, "? help  q quit"}, {left, false, "?/q"}, {left, true, "?/q"}, {left, true, ""}, {title, true, ""}} {
		l, r := try.left+tabs(try.compact), t.dim.Render(try.hint)
		if lipgloss.Width(l)+1+lipgloss.Width(r) <= m.width {
			return spread(l, r, m.width)
		}
	}
	return spread(strings.TrimPrefix(tabs(true), "  "), "", m.width)
}

// statusLine is the footer for views without their own: collection state.
func (m model) statusLine() string {
	switch {
	case m.collectErr != nil:
		return "collect error: " + hubErr(m.collectErr)
	case m.report() == nil:
		return "collecting…"
	default:
		return "local · every " + m.opts.Interval.String() + " · tab next view · ? help"
	}
}
