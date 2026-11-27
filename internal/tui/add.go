package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/ContractorKeith/machine-monitor/internal/client"
	"github.com/ContractorKeith/machine-monitor/internal/hub"
)

type PairHub interface {
	NewPairCode(ctx context.Context) (*client.PairCode, error)
	Info(ctx context.Context) (*client.Info, error)
	InstallURL(info *client.Info) string
	InstallCommands(hubURL string) (sh, ps string)
}

type addState struct {
	prev   view
	code   *client.PairCode
	err    error
	busy   bool
	url    string
	sh, ps string
	theirs bool
	notice string
	shown  bool
	paired bool
	lost   bool
}

type pairMsg struct {
	code   *client.PairCode
	info   *client.Info
	err    error
	theirs bool
}

func (m model) pairHub() PairHub {
	p, _ := m.opts.Hub.(PairHub)
	return p
}

func (m model) openAdd() (tea.Model, tea.Cmd) {
	prev := m.view
	if prev == viewSettings {
		prev = m.set.prev
	}
	m.add = addState{prev: prev}
	m.view, m.showHelp = viewAddDevice, false
	return m, m.newCode()
}

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
		info, _ := ph.Info(ctx)
		return pairMsg{code: code, info: info, theirs: isUpstream(ctx, ph)}
	})
}

func (m model) updateAdd(msg tea.Msg) (model, tea.Cmd, bool) {
	msg2, ok := msg.(pairMsg)
	if !ok {
		return m, nil, false
	}
	m.add.busy, m.add.err = false, msg2.err
	cleanAll(msg2.code)
	cleanAll(msg2.info)
	if msg2.err == nil {
		m.add.code = msg2.code
		ph := m.pairHub()
		m.add.url = hubOwnPort(ph.InstallURL(msg2.info), m.hubHost(), msg2.info)
		m.add.url = clean(m.add.url)
		m.add.sh, m.add.ps = ph.InstallCommands(m.add.url)
		m.add.sh, m.add.ps = clean(m.add.sh), clean(m.add.ps)
		m.add.theirs = msg2.theirs
		if m.add.theirs {
			m.add.sh = strings.TrimSuffix(m.add.sh, "| sh") + "| bash"
		}
	}
	return m, nil, true
}

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

func (m model) codeLeft() time.Duration {
	if m.add.code == nil {
		return 0
	}
	return max(0, time.UnixMilli(m.add.code.Expires).Sub(m.now()))
}

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
			t.dim.Render(m.pairLimits()),
			t.dim.Render("That only lets it watch. To make it report here, also run machine-monitor agent on it (service install --role agent starts it at login)."))
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

func (m model) pairLimits() string {
	if m.add.theirs {
		return fmt.Sprintf("and type the code. It works once, for %.0f minutes, and dies after %d wrong tries in all.",
			hub.PairTTL.Minutes(), hub.PairTries)
	}
	return fmt.Sprintf("and type the code. It works once, for %.0f minutes, with %d tries per device.",
		hub.PairTTL.Minutes(), hub.PairTries)
}

func hubOwnPort(raw, connectedTo string, info *client.Info) string {
	if info == nil || info.Port <= 0 {
		return raw
	}
	host, _, err := net.SplitHostPort(connectedTo)
	if ip := net.ParseIP(host); err != nil || !(ip != nil && ip.IsLoopback() || strings.EqualFold(host, "localhost")) {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if h := u.Hostname(); net.ParseIP(h) != nil && net.ParseIP(h).IsLoopback() || strings.EqualFold(h, "localhost") {
		return raw
	}
	u.Host = net.JoinHostPort(u.Hostname(), strconv.Itoa(info.Port))
	return u.String()
}

func isUpstream(ctx context.Context, ph PairHub) bool {
	u, ok := ph.(interface{ URL() string })
	if !ok {
		return false
	}
	hubURL := u.URL()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(hubURL, "/")+"/api/info", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var body struct{ Impl string }
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&body) != nil {
		return false
	}
	return body.Impl != "machine-monitor"
}

func (m model) wrapCmd(s string) string {
	w := max(20, m.width-2)
	var out []string
	for len(s) > w {
		out = append(out, "  "+s[:w])
		s = s[w:]
	}
	return strings.Join(append(out, "  "+s), "\n")
}

func (m model) addFooter() string {
	show := "s show key"
	if m.add.shown {
		show = "s hide key"
	}
	return "n new code · c copy Linux/macOS line · p copy Windows line · " + show + " · esc back"
}

var bearerRE = regexp.MustCompile(`Bearer [^"']+`)

func maskKey(s string) string { return bearerRE.ReplaceAllString(s, "Bearer ••••••••") }
