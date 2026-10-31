package tui

// Settings (PLAN §7 "Settings", §3 rows 18, 29 and 31): the temperature unit (saved on this
// machine), the electricity price (the hub's in hub mode, through GET/PUT
// /api/energy/settings; this machine's in local mode), Tailscale status, role, hub link and
// version. Opened with `,`; esc goes back.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/ContractorKeith/machine-monitor/internal/client"
	"github.com/ContractorKeith/machine-monitor/internal/config"
	"github.com/ContractorKeith/machine-monitor/internal/power"
	"github.com/ContractorKeith/machine-monitor/internal/tailscale"
)

// SettingsHub is a hub's price and Tailscale API. *client.Client implements it.
type SettingsHub interface {
	EnergySettings(ctx context.Context) (*power.Settings, error)
	SetEnergySettings(ctx context.Context, s power.Settings) (power.Settings, error)
	Remote(ctx context.Context) (*client.Remote, error)
}

// prefsFile is the TUI's own settings file in the data directory.
const prefsFile = "tui.json"

// prefs is what Settings saves on this machine.
type prefs struct {
	Fahrenheit bool            `json:"fahrenheit,omitempty"`
	Price      *power.Settings `json:"price,omitempty"` // local mode's price
}

// defaultPrefsPath is tui.json in the data directory, or "" when there is none.
func defaultPrefsPath() string {
	p, err := config.Default()
	if err != nil {
		return ""
	}
	return filepath.Join(p.Dir, prefsFile)
}

// loadPrefs reads path; a missing or damaged file is the defaults. The price is checked as
// the hub would check it.
func loadPrefs(path string) prefs {
	var p prefs
	if path == "" {
		return p
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return p
	}
	var raw struct {
		Fahrenheit bool            `json:"fahrenheit"`
		Price      json.RawMessage `json:"price"`
	}
	if json.Unmarshal(b, &raw) != nil {
		return p
	}
	p.Fahrenheit = raw.Fahrenheit
	if len(raw.Price) > 0 && string(raw.Price) != "null" {
		if s, err := power.ParseSettings(raw.Price); err == nil {
			p.Price = &s
		}
	}
	return p
}

// savePrefs writes the TUI's settings (mode 0600); an empty path saves nothing.
func savePrefs(path string, p prefs) error {
	if path == "" {
		return nil
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return config.WriteFile0600(path, b)
}

// version is the build's version: Options.Version, or the module version Go stamps from
// the git tag, or "dev".
func (m model) version() string {
	if m.opts.Version != "" {
		return m.opts.Version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

// Settings rows the cursor moves over.
const (
	setTemp = iota
	setPrice
	setAdd // hub mode only
)

// settingsState is the Settings view's state.
type settingsState struct {
	prev     view // the view esc returns to
	cursor   int
	price    *power.Settings // hub mode: the hub's saved price, nil when none is saved
	priceSet bool            // price is what the hub said (a read or an event arrived)
	priceErr error
	ts       *tailscale.Status
	tsErr    error
	tsBusy   bool
	form     priceForm
	notice   string
}

// Settings messages.
type (
	priceLoadedMsg struct {
		s   *power.Settings
		err error
	}
	priceSavedMsg struct {
		s   power.Settings
		err error
	}
	remoteMsg struct {
		st  *tailscale.Status
		err error
	}
)

// settingsHub is the hub's settings API, or nil in local mode or for a Hub without one.
func (m model) settingsHub() SettingsHub {
	s, _ := m.opts.Hub.(SettingsHub)
	return s
}

// openSettings reads what the view shows: the hub's price and the Tailscale status.
func (m *model) openSettings() tea.Cmd {
	var cmds []tea.Cmd
	if sh := m.settingsHub(); sh != nil {
		cmds = append(cmds, m.hubCall(func(ctx context.Context, _ Hub) tea.Msg {
			s, err := sh.EnergySettings(ctx)
			return priceLoadedMsg{s, err}
		}))
	}
	if !m.set.tsBusy {
		if f := m.tailscaleReader(); f != nil {
			m.set.tsBusy = true
			parent := m.ctx
			cmds = append(cmds, func() tea.Msg {
				ctx, cancel := context.WithTimeout(parent, viewTimeout)
				defer cancel()
				st, err := f(ctx)
				return remoteMsg{st, err}
			})
		}
	}
	return tea.Batch(cmds...)
}

// tailscaleReader reads the Tailscale status Settings shows: the hub machine's in hub mode,
// this machine's in local mode.
func (m model) tailscaleReader() func(ctx context.Context) (*tailscale.Status, error) {
	if m.multi() {
		sh := m.settingsHub()
		if sh == nil {
			return nil
		}
		return func(ctx context.Context) (*tailscale.Status, error) {
			r, err := sh.Remote(ctx)
			if err != nil {
				return nil, err
			}
			return &r.Status, nil
		}
	}
	return m.opts.Tailscale
}

// updateSettings handles the Settings and Add device messages; ok is false for anything else.
func (m model) updateSettings(msg tea.Msg) (model, tea.Cmd, bool) {
	switch msg := msg.(type) {
	case priceLoadedMsg:
		m.set.priceErr = msg.err
		if msg.err == nil {
			m.set.price, m.set.priceSet = msg.s, true
		}
		return m, nil, true
	case priceSavedMsg:
		if msg.err != nil {
			m.set.form.err = sentence("Saving failed: " + hubErr(msg.err))
			return m, nil, true
		}
		m.set.form = priceForm{}
		m.set.notice = "Price saved on the hub."
		m.applyPrice(&msg.s)
		return m, m.fetchToday(), true
	case remoteMsg:
		m.set.tsBusy = false
		m.set.ts, m.set.tsErr = msg.st, msg.err
		return m, nil, true
	}
	return m.updateAdd(msg)
}

// applyEnergySettings takes a price change the hub broadcast.
func (m *model) applyEnergySettings(ev client.Event) {
	if s := client.EnergySettingsOf(ev); s != nil {
		m.applyPrice(s)
	}
}

// applyPrice makes s the price in force: the hub's in hub mode, this machine's otherwise.
func (m *model) applyPrice(s *power.Settings) {
	if m.multi() {
		m.set.price, m.set.priceSet = s, true
		if m.ev.data != nil { // the Energy view prices with it at once
			d := *m.ev.data
			d.Settings = s
			m.ev.data = &d
		}
		return
	}
	m.opts.Price = s
}

// effectivePrice is the price Settings shows and edits, and whether it is a guess.
func (m model) effectivePrice() (power.Settings, bool) {
	if m.multi() && m.set.priceSet {
		if m.set.price != nil {
			return *m.set.price, false
		}
		return m.guess, true
	}
	return m.priceSettings()
}

// settingsKey handles the Settings view's keys.
func (m model) settingsKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	k := m.keys
	rows := setPrice + 1
	if m.multi() {
		rows = setAdd + 1
	}
	m.set.notice = ""
	switch {
	case key.Matches(msg, k.Clear):
		return m.switchTo(m.set.prev)
	case key.Matches(msg, k.Up):
		m.set.cursor = max(0, m.set.cursor-1)
	case key.Matches(msg, k.Down):
		m.set.cursor = min(rows-1, m.set.cursor+1)
	case msg.String() == "u":
		return m.toggleUnit()
	case msg.String() == "e":
		return m.openPriceForm()
	case msg.String() == "a" && m.multi():
		return m.openAdd()
	case msg.String() == "enter", msg.String() == "space":
		switch m.set.cursor {
		case setTemp:
			return m.toggleUnit()
		case setPrice:
			return m.openPriceForm()
		case setAdd:
			return m.openAdd()
		}
	}
	return m, nil
}

// toggleUnit switches °C and °F and saves the choice on this machine. In hub mode the hub's
// alert settings take the unit too (tempUnit), so the alert texts it writes match.
func (m model) toggleUnit() (tea.Model, tea.Cmd) {
	m.theme.fahrenheit = !m.theme.fahrenheit
	m.saveLocal()
	if m.alerter() == nil {
		return m, nil
	}
	m.al.unitPending = true
	return m.syncAlertsUnit()
}

// hubTempUnit is the alerts tempUnit for the unit in use.
func (m model) hubTempUnit() string {
	if m.theme.fahrenheit {
		return "F"
	}
	return "C"
}

// syncAlertsUnit saves the unit in the hub's alert settings once they are loaded and still
// differ; until then it waits (unitPending) for the load the header's count starts.
func (m model) syncAlertsUnit() (model, tea.Cmd) {
	if !m.al.unitPending || !m.al.loaded {
		return m, nil
	}
	m.al.unitPending = false
	if m.al.cfg.TempUnit == m.hubTempUnit() {
		return m, nil
	}
	s := cloneSettings(m.al.cfg)
	s.TempUnit = m.hubTempUnit()
	return m.saveAlerts(s)
}

// saveLocal writes this machine's settings, reporting a failure in the view.
func (m *model) saveLocal() {
	p := prefs{Fahrenheit: m.theme.fahrenheit}
	if !m.multi() {
		p.Price = m.opts.Price
	} else {
		p.Price = loadPrefs(m.opts.PrefsPath).Price // keep local mode's price
	}
	if err := savePrefs(m.opts.PrefsPath, p); err != nil {
		m.set.notice = sentence("Couldn't save the settings on this machine: " + err.Error())
	}
}

// ---- price editor ------------------------------------------------------------------------

// priceForm is the price editor: region, rate, currency and time-of-use periods.
type priceForm struct {
	open   bool
	fields []formField
	cur    int
	err    string
}

// Price form rows.
const (
	pfRegion = iota
	pfRate
	pfCurrency
	pfTOU
)

// openPriceForm opens the editor on the price in force.
func (m model) openPriceForm() (tea.Model, tea.Cmd) {
	if m.multi() && m.settingsHub() == nil {
		m.set.notice = "This hub has no price settings."
		return m, nil
	}
	s, _ := m.effectivePrice()
	reg := formField{label: "Region", kind: fieldChoice, values: []string{"custom"}, choices: []string{"Custom rate"}}
	for _, r := range power.Regions {
		reg.values = append(reg.values, r.Code)
		reg.choices = append(reg.choices, r.Name)
	}
	reg.idx = max(0, slices.Index(reg.values, s.Region))
	m.set.form = priceForm{open: true, fields: []formField{
		reg,
		textField("Rate", strconv.FormatFloat(s.Rate, 'f', -1, 64), "per kWh", 12),
		textField("Currency", s.Currency, "", 3),
		textField("Time of use", fmtTOU(s.TOU), "", 200),
	}}
	m.focusPriceField()
	return m, nil
}

// focusPriceField focuses the current row's text input and blurs the rest.
func (m *model) focusPriceField() {
	f := &m.set.form
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

// priceFormKey handles a key while the price editor is open.
func (m model) priceFormKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	f := &m.set.form
	f.fields = slices.Clone(f.fields)
	fld := &f.fields[f.cur]
	switch s := msg.String(); s {
	case "esc":
		m.set.form = priceForm{}
		return m, nil
	case "enter":
		return m.savePrice()
	case "up", "shift+tab", "down", "tab":
		if s == "up" || s == "shift+tab" {
			f.cur = max(0, f.cur-1)
		} else {
			f.cur = min(len(f.fields)-1, f.cur+1)
		}
		m.focusPriceField()
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
		// A region fills in its typical rate and currency; custom keeps what is typed.
		if r, ok := power.LookupRegion(fld.values[fld.idx]); ok && fld.values[fld.idx] != "custom" {
			f.fields[pfRate].input.SetValue(strconv.FormatFloat(r.Rate, 'f', -1, 64))
			f.fields[pfCurrency].input.SetValue(r.Currency)
		}
	case fieldText:
		var cmd tea.Cmd
		fld.input, cmd = fld.input.Update(msg)
		return m, cmd
	}
	return m, nil
}

// savePrice checks the form as the hub would and saves it: on the hub in hub mode, on this
// machine otherwise.
func (m model) savePrice() (tea.Model, tea.Cmd) {
	f := m.set.form
	rate, err := strconv.ParseFloat(strings.TrimSpace(f.fields[pfRate].input.Value()), 64)
	if err != nil {
		m.set.form.err = "The rate must be a number, such as 0.165."
		return m, nil
	}
	tou, problem := parseTOU(f.fields[pfTOU].input.Value())
	if problem != "" {
		m.set.form.err = problem
		return m, nil
	}
	region := f.fields[pfRegion].values[f.fields[pfRegion].idx]
	if r, ok := power.LookupRegion(region); ok && region != "custom" && r.Rate != rate {
		region = "custom" // a typed rate is the user's own, as upstream
	}
	in := power.Settings{Region: region, Rate: rate,
		Currency: strings.ToUpper(strings.TrimSpace(f.fields[pfCurrency].input.Value())), TOU: tou}
	if msg := priceProblem(in); msg != "" {
		m.set.form.err = msg
		return m, nil
	}
	b, _ := json.Marshal(in)
	s, err := power.ParseSettings(b)
	if err != nil { // priceProblem covers every rule; this is a backstop
		m.set.form.err = "The hub would refuse this price. Check the rate, the currency and the periods."
		return m, nil
	}
	if sh := m.settingsHub(); m.multi() && sh != nil {
		m.set.form.err = ""
		return m, m.hubCall(func(ctx context.Context, _ Hub) tea.Msg {
			saved, err := sh.SetEnergySettings(ctx, s)
			return priceSavedMsg{saved, err}
		})
	}
	m.set.form = priceForm{}
	m.applyPrice(&s)
	m.saveLocal()
	if m.set.notice == "" {
		m.set.notice = "Price saved on this machine."
	}
	return m, m.fetchToday()
}

// Price limits, as power.ParseSettings (upstream's validEnergySettings) enforces them.
const (
	maxRate    = 1000 // per kWh; a rate must be more than 0 and at most this
	maxPeriods = 12
)

// priceProblem says, as a sentence, what power.ParseSettings would refuse in s, or "".
func priceProblem(s power.Settings) string {
	okRate := func(r float64) bool { return r > 0 && r <= maxRate && math.Round(r*1e4) != 0 }
	okHour := func(h float64) bool { return h >= 0 && h <= 24 }
	switch {
	case !okRate(s.Rate):
		return fmt.Sprintf("The rate must be more than 0 and at most %d per kWh.", maxRate)
	case len(s.Currency) != 3 || strings.Trim(s.Currency, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") != "":
		return "The currency must be a 3-letter code, such as USD or EUR."
	case len(s.TOU) > maxPeriods:
		return fmt.Sprintf("Time of use takes at most %d periods.", maxPeriods)
	}
	for _, p := range s.TOU {
		if !okHour(p.From) || !okHour(p.To) || math.Round(p.From*4) >= math.Round(p.To*4) {
			return fmt.Sprintf("The period %s-%s needs hours from 0 to 24, with the start before the end.", trimFloat(p.From), trimFloat(p.To))
		}
		if !okRate(p.Rate) {
			return fmt.Sprintf("The rate for %s-%s must be more than 0 and at most %d per kWh.", trimFloat(p.From), trimFloat(p.To), maxRate)
		}
	}
	return ""
}

// sentence capitalises s and ends it with a full stop.
func sentence(s string) string {
	s = capitalize(strings.TrimSpace(s))
	if s != "" && !strings.ContainsAny(s[len(s)-1:], ".!?") {
		s += "."
	}
	return s
}

// fmtTOU writes periods as the editor takes them: "16-21 0.35, 0-6 0.08".
func fmtTOU(ps []power.Period) string {
	parts := make([]string, len(ps))
	for i, p := range ps {
		parts[i] = fmt.Sprintf("%s-%s %s", trimFloat(p.From), trimFloat(p.To), trimFloat(p.Rate))
	}
	return strings.Join(parts, ", ")
}

func trimFloat(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// parseTOU reads "16-21 0.35, 0-6 0.08": comma-separated periods of local hours FROM-TO
// (quarter hours allowed, TO up to 24) and the rate for them. Empty is no periods. problem
// is a sentence for the form when a period can't be read.
func parseTOU(s string) (periods []power.Period, problem string) {
	for part := range strings.SplitSeq(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		bad := fmt.Sprintf("The time-of-use period %q should look like 16-21 0.35: local hours from and to, then the rate.", part)
		hours, rate, ok := strings.Cut(part, " ")
		if !ok {
			return nil, bad
		}
		from, to, ok := strings.Cut(hours, "-")
		if !ok {
			return nil, bad
		}
		var p power.Period
		var err1, err2, err3 error
		p.From, err1 = strconv.ParseFloat(strings.TrimSpace(from), 64)
		p.To, err2 = strconv.ParseFloat(strings.TrimSpace(to), 64)
		p.Rate, err3 = strconv.ParseFloat(strings.TrimSpace(rate), 64)
		if err := errors.Join(err1, err2, err3); err != nil {
			return nil, bad
		}
		periods = append(periods, p)
	}
	return periods, ""
}

// ---- drawing -----------------------------------------------------------------------------

// settingsView is the Settings view, or the price editor while it is open.
func (m model) settingsView() string {
	t := m.theme
	if m.set.form.open {
		return m.priceFormView()
	}
	marker := func(row int) string {
		if row != m.set.cursor {
			return "  "
		}
		if t.ascii {
			return "> "
		}
		return "▶ "
	}
	label := func(row int, s string) string {
		l := pad(s, 14)
		if row == m.set.cursor {
			return t.selected.Render(l)
		}
		return t.label.Render(l)
	}
	info := func(s string) string { return "  " + t.label.Render(pad(s, 14)) }

	unit := "°C  Celsius"
	if t.fahrenheit {
		unit = "°F  Fahrenheit"
	}
	lines := []string{t.label.Render("Settings"), ""}
	lines = append(lines, marker(setTemp)+label(setTemp, "Temperature")+" "+unit+t.dim.Render("   u or enter switches · saved on this machine"))

	price, guessed := m.effectivePrice()
	where := "this machine's"
	if m.multi() {
		where = "the hub's"
	}
	priceLine := fmtPrice(price) + t.dim.Render(" · "+priceSource(price, guessed))
	switch {
	case m.multi() && m.settingsHub() == nil:
		priceLine = t.dim.Render("this hub has no price settings")
	case m.multi() && !m.set.priceSet && m.set.priceErr != nil:
		priceLine = t.err.Render("price: "+hubErr(m.set.priceErr)) + t.dim.Render(" · showing "+fmtPrice(price))
	case m.multi() && !m.set.priceSet:
		priceLine = t.dim.Render("loading…")
	}
	lines = append(lines, marker(setPrice)+label(setPrice, "Electricity")+" "+priceLine)
	lines = append(lines, "  "+pad("", 14)+" "+t.dim.Render("e or enter edits "+where+" region, rate and time-of-use periods"))

	if m.multi() {
		lines = append(lines, "", info("Role")+" connected to a hub")
		lines = append(lines, info("Hub link")+" "+m.hubURL())
		lines = append(lines, marker(setAdd)+label(setAdd, "Add device")+" "+t.dim.Render("a or enter: pairing code and one-line installs"))
	} else {
		lines = append(lines, "", info("Role")+" local: this machine only")
		lines = append(lines, info("Hub link")+" "+t.dim.Render("none · run `machine-monitor hub` to watch several machines"))
	}
	lines = append(lines, info("Tailscale")+" "+m.tailscaleText())
	lines = append(lines, info("Version")+" machine-monitor "+m.version()+t.dim.Render(" · `machine-monitor update` checks for a new one"))
	if m.set.notice != "" {
		lines = append(lines, "", m.set.notice)
	}
	return strings.Join(lines, "\n")
}

// tailscaleText is the Tailscale line: state, address and name, or why it isn't known.
func (m model) tailscaleText() string {
	t := m.theme
	st := m.set.ts
	switch {
	case errors.Is(m.set.tsErr, client.ErrNotAvailable):
		return t.dim.Render("not available on this hub")
	case m.set.tsErr != nil:
		return t.err.Render(hubErr(m.set.tsErr))
	case st == nil && m.tailscaleReader() == nil:
		return t.dim.Render("not available")
	case st == nil:
		return t.dim.Render("checking…")
	case !st.Installed:
		return "not installed"
	case !st.Running:
		state := "not running"
		if st.State != nil && *st.State != "" {
			state += " (" + *st.State + ")"
		}
		return state
	}
	parts := []string{"running"}
	if st.IP != nil {
		parts = append(parts, *st.IP)
	}
	if st.DNSName != nil {
		parts = append(parts, *st.DNSName)
	}
	if st.HTTPSURL != nil {
		parts = append(parts, *st.HTTPSURL)
	} else if len(st.URLs) > 0 && m.multi() {
		parts = append(parts, st.URLs[0])
	}
	return strings.Join(parts, " · ")
}

// hubURL is the hub's base URL for display.
func (m model) hubURL() string {
	if u, ok := m.opts.Hub.(interface{ URL() string }); ok {
		return u.URL()
	}
	return "http://" + m.hubHost()
}

// priceFormView is the open price editor.
func (m model) priceFormView() string {
	t := m.theme
	f := m.set.form
	title := "Electricity price on this machine"
	if m.multi() {
		title = "Electricity price on the hub"
	}
	lines := []string{t.label.Render(title), ""}
	for i, fld := range f.fields {
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
		case fieldText:
			switch {
			case fld.input.Focused():
				v = strings.TrimRight(fld.input.View(), " ")
			case fld.input.Value() == "":
				v = t.dim.Render("(none)")
			default:
				v = fld.input.Value()
			}
			if fld.unit != "" {
				v += " " + t.dim.Render(fld.unit)
			}
		}
		label := pad(fld.label, 14)
		if i == f.cur {
			label = t.selected.Render(label)
		}
		lines = append(lines, marker+label+" "+v)
	}
	lines = append(lines, "",
		t.dim.Render("Time of use: local hours and their rate, e.g. 16-21 0.35, 0-6 0.08."),
		t.dim.Render("Every other hour uses the rate above."))
	if f.err != "" {
		lines = append(lines, "", t.err.Render(f.err))
	}
	return strings.Join(lines, "\n")
}

// settingsFooter is the Settings view's footer.
func (m model) settingsFooter() string {
	if m.set.form.open {
		return "↑↓ field · ←→ region · enter save · esc cancel"
	}
	return "↑↓ select · enter change · esc back · tab next view"
}
