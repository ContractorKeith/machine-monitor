package tui

// Add device (PLAN §7 "Add device", §3 rows 26 and 33): a pairing code from the hub with its
// countdown, and the one-line agent installs for Linux/macOS and Windows built from the
// hub's address. Hub mode only; opened with `a` from the dashboard or Settings.

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/ContractorKeith/machine-monitor/internal/client"
)

// PairHub is what Add device needs from a hub. *client.Client implements it.
type PairHub interface {
	NewPairCode(ctx context.Context) (*client.PairCode, error)
	Info(ctx context.Context) (*client.Info, error)
	InstallURL(info *client.Info) string
	InstallCommands(hubURL string) (sh, ps string)
}

// addState is the Add device view's state.
type addState struct {
	prev   view // the view esc returns to
	code   *client.PairCode
	err    error
	busy   bool
	url    string // the hub address other machines use
	sh, ps string // the one-line installs
	notice string
}

// pairMsg is a new pairing code and the hub's addresses.
type pairMsg struct {
	code *client.PairCode
	info *client.Info
	err  error
}

// pairHub is the hub's pairing API, or nil.
func (m model) pairHub() PairHub {
	p, _ := m.opts.Hub.(PairHub)
	return p
}

// openAdd opens Add device and asks the hub for a fresh code.
func (m model) openAdd() (tea.Model, tea.Cmd) {
	prev := m.view
	if prev == viewSettings {
		prev = m.set.prev
	}
	m.add = addState{prev: prev}
	m.view, m.showHelp = viewAddDevice, false
	return m, m.newCode()
}

// newCode asks the hub for a pairing code (and its LAN addresses, for the installs).
func (m *model) newCode() tea.Cmd {
	ph := m.pairHub()
	if ph == nil || m.add.busy {
		return nil
	}
	m.add.busy, m.add.err, m.add.notice = true, nil, ""
	return m.hubCall(func(ctx context.Context, _ Hub) tea.Msg {
		code, err := ph.NewPairCode(ctx)
		if err != nil {
			return pairMsg{err: err}
		}
		info, _ := ph.Info(ctx) // without it the installs use the address this TUI uses
		return pairMsg{code: code, info: info}
	})
}

// updateAdd handles Add device messages; ok is false for anything else.
func (m model) updateAdd(msg tea.Msg) (model, tea.Cmd, bool) {
	msg2, ok := msg.(pairMsg)
	if !ok {
		return m, nil, false
	}
	m.add.busy, m.add.err = false, msg2.err
	if msg2.err == nil {
		m.add.code = msg2.code
		ph := m.pairHub()
		m.add.url = ph.InstallURL(msg2.info)
		m.add.sh, m.add.ps = ph.InstallCommands(m.add.url)
	}
	return m, nil, true
}

// addKey handles Add device's keys: a new code, copying an install line, back.
func (m model) addKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		return m.switchTo(m.add.prev)
	case "n":
		return m, m.newCode()
	case "c":
		if m.add.sh != "" {
			m.add.notice = "copied the Linux/macOS install to the clipboard"
			return m, tea.SetClipboard(m.add.sh)
		}
	case "p":
		if m.add.ps != "" {
			m.add.notice = "copied the Windows install to the clipboard"
			return m, tea.SetClipboard(m.add.ps)
		}
	}
	return m, nil
}

// codeLeft is how long the code stays valid.
func (m model) codeLeft() time.Duration {
	if m.add.code == nil {
		return 0
	}
	return max(0, time.UnixMilli(m.add.code.Expires).Sub(m.now()))
}

// addView is the Add device view.
func (m model) addView() string {
	t := m.theme
	lines := []string{t.label.Render("Add a device"), ""}
	switch {
	case m.pairHub() == nil:
		return strings.Join(append(lines, t.dim.Render("This hub can't issue pairing codes.")), "\n")
	case m.add.err != nil:
		lines = append(lines, t.err.Render("pairing code: "+hubErr(m.add.err)), "")
	case m.add.code == nil:
		return strings.Join(append(lines, t.dim.Render("asking the hub for a pairing code…")), "\n")
	}
	if c := m.add.code; c != nil {
		code := c.Code
		if len(code) == 6 {
			code = code[:3] + " " + code[3:]
		}
		left := m.codeLeft()
		countdown := fmt.Sprintf("expires in %d:%02d", int(left.Minutes()), int(left.Seconds())%60)
		style := t.title
		if left <= 0 {
			countdown, style = "expired · n for a new code", t.dim
		}
		lines = append(lines,
			t.label.Render(pad("Pairing code", 14))+" "+style.Render(code)+"   "+t.dim.Render(countdown),
			"", "On the new machine run", "  machine-monitor connect "+m.add.url, "and type the code. It works once, for 10 minutes and 5 tries.")
	}
	if m.add.sh != "" {
		lines = append(lines, "", t.label.Render("Or install the agent with one line")+t.dim.Render(" (it carries this hub's key: use it only on your own machines)"),
			"", "Linux and macOS", m.wrapCmd(m.add.sh), "", "Windows (PowerShell)", m.wrapCmd(m.add.ps))
	}
	if m.add.notice != "" {
		lines = append(lines, "", m.add.notice)
	}
	return strings.Join(lines, "\n")
}

// wrapCmd indents a command and breaks it to the window's width, so all of it shows; c and p
// copy it whole.
func (m model) wrapCmd(s string) string {
	w := max(20, m.width-2)
	var out []string
	for len(s) > w {
		out = append(out, "  "+s[:w])
		s = s[w:]
	}
	return strings.Join(append(out, "  "+s), "\n")
}

// addFooter is Add device's footer.
func (m model) addFooter() string {
	return "n new code · c copy Linux/macOS line · p copy Windows line · esc back"
}
