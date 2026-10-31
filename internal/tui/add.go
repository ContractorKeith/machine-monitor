package tui

// Add device (PLAN §7 "Add device", §3 rows 26 and 33): a pairing code from the hub with its
// countdown, and the one-line agent installs for Linux/macOS and Windows built from the
// hub's address. Hub mode only; opened with `a` from the dashboard or Settings. The code
// part says what `machine-monitor pair` prints on the hub's own machine, in the same words.

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/ContractorKeith/machine-monitor/internal/client"
	"github.com/ContractorKeith/machine-monitor/internal/hub"
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
	shown  bool // the install lines show the key (s); hidden until asked, for screen sharing
	paired bool // a device used the code
	lost   bool // the stream dropped: the code lived only in that hub's memory
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
	m.add.paired, m.add.lost = false, false
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

// pairedEvent and lostHub follow the code while Add device shows it, as `pair` does.
func (m *model) pairedEvent() {
	if m.add.code != nil && !m.add.lost {
		m.add.paired = true
	}
}

func (m *model) lostHub() {
	if m.add.code != nil && !m.add.paired && m.codeLeft() > 0 {
		m.add.lost = true
	}
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
			m.add.notice = "Copied the Linux/macOS install to the clipboard."
			return m, tea.SetClipboard(m.add.sh)
		}
	case "s":
		m.add.shown = !m.add.shown
	case "p":
		if m.add.ps != "" {
			m.add.notice = "Copied the Windows install to the clipboard."
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
		left := m.codeLeft().Round(time.Second)
		status := t.dim.Render(fmt.Sprintf("Expires in %d:%02d", int(left/time.Minute), int(left/time.Second)%60))
		style := t.title
		switch {
		case m.add.paired:
			status = t.normal.Render("Paired: a device used the code.") + t.dim.Render(" n for another")
		case m.add.lost:
			status, style = t.err.Render("Lost the hub, and the code with it.")+t.dim.Render(" n for a new one once it is back"), t.dim
		case left <= 0:
			status, style = t.dim.Render("The code expired. Press n for a new one."), t.dim
		}
		lines = append(lines,
			t.label.Render("Pairing code:")+" "+style.Render(code)+"   "+status,
			"On the new device, run: machine-monitor connect "+m.add.url,
			t.dim.Render(fmt.Sprintf("and type the code. It works once, for %.0f minutes, with %d tries per device.",
				hub.PairTTL.Minutes(), hub.PairTries)))
	}
	if m.add.sh != "" {
		sh, ps := m.add.sh, m.add.ps
		note := " (they carry this hub's key, hidden: s shows it; c and p copy them whole)"
		if !m.add.shown {
			sh, ps = maskKey(sh), maskKey(ps)
		} else {
			note = " (they carry this hub's key: use them only on your own machines; s hides it)"
		}
		lines = append(lines, "", t.label.Render("Or install the agent with one line")+t.dim.Render(note),
			"", "Linux and macOS", m.wrapCmd(sh), "", "Windows (PowerShell)", m.wrapCmd(ps))
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
	show := "s show key"
	if m.add.shown {
		show = "s hide key"
	}
	return "n new code · c copy Linux/macOS line · p copy Windows line · " + show + " · esc back"
}

// bearerRE finds the key in an install line's Authorization header.
var bearerRE = regexp.MustCompile(`Bearer [^"']+`)

// maskKey hides the hub key in an install line.
func maskKey(s string) string { return bearerRE.ReplaceAllString(s, "Bearer ••••••••") }
