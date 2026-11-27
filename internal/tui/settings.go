package tui

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
	"sort"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/ContractorKeith/machine-monitor/internal/client"
	"github.com/ContractorKeith/machine-monitor/internal/config"
	"github.com/ContractorKeith/machine-monitor/internal/power"
	"github.com/ContractorKeith/machine-monitor/internal/tailscale"
)

type SettingsHub interface {
	EnergySettings(ctx context.Context) (*power.Settings, error)
	SetEnergySettings(ctx context.Context, s power.Settings) (power.Settings, error)
	Remote(ctx context.Context) (*client.Remote, error)
}

const prefsFile = "tui.json"

type prefs struct {
	Fahrenheit bool            `json:"fahrenheit,omitempty"`
	Price      *power.Settings `json:"price,omitempty"`
}

func defaultPrefsPath() string {
	p, err := config.Default()
	if err != nil {
		return ""
	}
	return filepath.Join(p.Dir, prefsFile)
}

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

		if s, err := power.LoadSettings(raw.Price); err == nil || errors.Is(err, power.ErrOverlappingPeriods) {
			p.Price = &s
		}
	}
	return p
}

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

func (m model) version() string {
	if m.opts.Version != "" {
		return m.opts.Version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

const (
	setTemp = iota
	setPrice
	setAdd
)

type settingsState struct {
	prev     view
	cursor   int
	price    *power.Settings
	priceSet bool
	priceErr error
	ts       *tailscale.Status
	tsErr    error
	tsBusy   bool
	form     priceForm
	notice   string
}

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

func (m model) settingsHub() SettingsHub {
	s, _ := m.opts.Hub.(SettingsHub)
	return s
}

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

func (m model) updateSettings(msg tea.Msg) (model, tea.Cmd, bool) {
	switch msg := msg.(type) {
	case priceLoadedMsg:
		cleanAll(msg.s)
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
		cleanAll(msg.st)
		m.set.ts, m.set.tsErr = msg.st, msg.err
		return m, nil, true
	}
	return m.updateAdd(msg)
}

func (m *model) applyEnergySettings(ev client.Event) {
	if s := client.EnergySettingsOf(ev); s != nil {
		m.applyPrice(s)
	}
}

func (m *model) applyPrice(s *power.Settings) {
	cleanAll(s)
	if m.multi() {
		m.set.price, m.set.priceSet = s, true
		if m.ev.data != nil {
			d := *m.ev.data
			d.Settings = s
			m.ev.data = &d
		}
		return
	}
	m.opts.Price = s
}

func (m model) effectivePrice() (power.Settings, bool) {
	if m.multi() && m.set.priceSet {
		if m.set.price != nil {
			return *m.set.price, false
		}
		return m.guess, true
	}
	return m.priceSettings()
}

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

func (m model) toggleUnit() (tea.Model, tea.Cmd) {
	m.theme.fahrenheit = !m.theme.fahrenheit
	m.saveLocal()
	if m.alerter() == nil {
		return m, nil
	}
	m.al.unitPending = true
	return m.syncAlertsUnit()
}

func (m model) hubTempUnit() string {
	if m.theme.fahrenheit {
		return "F"
	}
	return "C"
}

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

func (m *model) saveLocal() {
	p := prefs{Fahrenheit: m.theme.fahrenheit}
	if !m.multi() {
		p.Price = m.opts.Price
	} else {
		p.Price = loadPrefs(m.opts.PrefsPath).Price
	}
	if err := savePrefs(m.opts.PrefsPath, p); err != nil {
		m.set.notice = sentence("Couldn't save the settings on this machine: " + hubErr(err))
	}
}

type priceForm struct {
	open   bool
	fields []formField
	cur    int
	err    string
}

const (
	pfRegion = iota
	pfRate
	pfCurrency
	pfTOU
)

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
		region = "custom"
	}
	in := power.Settings{Region: region, Rate: rate,
		Currency: strings.ToUpper(strings.TrimSpace(f.fields[pfCurrency].input.Value())), TOU: tou}
	if msg := priceProblem(in); msg != "" {
		m.set.form.err = msg
		return m, nil
	}
	b, _ := json.Marshal(in)
	s, err := power.ParseSettings(b)
	if err != nil {
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

const (
	maxRate    = 1000
	maxPeriods = 12
)

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

	sorted := append([]power.Period(nil), s.TOU...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].From < sorted[j].From })
	for i := 1; i < len(sorted); i++ {
		if a, b := sorted[i-1], sorted[i]; math.Round(b.From*4) < math.Round(a.To*4) {
			return fmt.Sprintf("The periods %s-%s and %s-%s overlap. Give each hour of the day to one period.",
				trimFloat(a.From), trimFloat(a.To), trimFloat(b.From), trimFloat(b.To))
		}
	}
	return ""
}

func sentence(s string) string {
	s = capitalize(strings.TrimSpace(s))
	if s != "" && !strings.ContainsAny(s[len(s)-1:], ".!?") {
		s += "."
	}
	return s
}

func fmtTOU(ps []power.Period) string {
	parts := make([]string, len(ps))
	for i, p := range ps {
		parts[i] = fmt.Sprintf("%s-%s %s", trimFloat(p.From), trimFloat(p.To), trimFloat(p.Rate))
	}
	return strings.Join(parts, ", ")
}

func trimFloat(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

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

func (m model) hubURL() string {
	if u, ok := m.opts.Hub.(interface{ URL() string }); ok {
		return u.URL()
	}
	return "http://" + m.hubHost()
}

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

func (m model) settingsFooter() string {
	if m.set.form.open {
		return "↑↓ field · ←→ region · enter save · esc cancel"
	}
	return "↑↓ select · enter change · esc back · tab next view"
}
