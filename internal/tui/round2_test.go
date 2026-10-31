package tui

import (
	"context"
	"errors"
	"net"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/ContractorKeith/machine-monitor/internal/client"
	"github.com/ContractorKeith/machine-monitor/internal/report"
)

// TestCleanStripsTerminalControls: escape sequences, controls and invisible format
// characters never reach the screen (skeptic pass 2, B4).
func TestCleanStripsTerminalControls(t *testing.T) {
	for in, want := range map[string]string{
		"plain name": "plain name",
		"zz\x1b]8;;http://evil.example/\aLINKPWN\x1b]8;;\a end":  "zzLINKPWN end",
		"zz\x1b]8;;http://evil.example/\x1b\\LINK\x1b]8;;\x1b\\": "zzLINK",
		"a\x1b[44mCMDPWN\x1b[0m":                                 "aCMDPWN",
		"ev\x1b[41mPWN\x1b]0;T":                                  "evPWN",
		"c1\u009b41mx\u0085y":                                    "c141mxy",
		"bidi\u202eAB\u2066CD\u061cEF\u200bG\ufeffH":             "bidiABCDEFGH",
		"tab\there\nnew":                                         "tab here new",
		"bad\xffutf":                                             "badutf",
		"héllo — 日本語 🚀":                                          "héllo — 日本語 🚀",
	} {
		if got := clean(in); got != want {
			t.Errorf("clean(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestProcessNamesCannotWriteEscapes: a process named with SGR and an OSC 8 link renders as
// text. Without clean the frame carries the link.
func TestProcessNamesCannotWriteEscapes(t *testing.T) {
	f := newActionFake()
	m := actModel(t, f, 120, 30)
	m.view = viewProcesses
	name := "zz\x1b]8;;http://evil.example/\aLINKPWN\x1b]8;;\a"
	cmd := "x\x1b[5mBLINKPWN\x1b[0m"
	m = runActs(t, m, procsMsg{p: []report.Process{{PID: report.Ptr(1.0), Name: &name, Cmd: &cmd}}})
	frame := m.render()
	for _, bad := range []string{"\x1b]8", "evil.example", "\x1b[5m", "\a"} {
		if strings.Contains(frame, bad) {
			t.Errorf("frame carries %q:\n%q", bad, frame)
		}
	}
	if out := plain(m); !strings.Contains(out, "zzLINKPWN") {
		t.Errorf("the text itself should still show:\n%s", out)
	}
}

// TestDeviceFieldsAreCleaned: names and report strings from the hub are cleaned on arrival.
func TestDeviceFieldsAreCleaned(t *testing.T) {
	devs := threeDevices()
	devs[0].Name = "ws\x1b[41m-red\u202e"
	devs[0].Latest.Report.Hostname = report.Ptr("host\x1b]0;T\a")
	m := hubModel(t, newFakeHub(devs), 100, 30)
	d := m.devs[0]
	for _, x := range m.devs {
		if x.id == devs[0].ID {
			d = x
		}
	}
	if d.name != "ws-red" || *d.last.Hostname != "host" {
		t.Errorf("name %q, hostname %q", d.name, *d.last.Hostname)
	}
	if strings.Contains(m.render(), "\x1b[41m") {
		t.Error("frame carries the device name's colour")
	}
}

// TestFilterIsGlobal: the one `/` filter applies on the dashboard and in Processes.
func TestFilterIsGlobal(t *testing.T) {
	m := actModel(t, newActionFake(), 120, 30)
	m = runActs(t, m, keyMsg("/"), keyMsg("s"), keyMsg("s"), keyMsg("h"), keyMsg("enter"), keyMsg("3"))
	if m.filterValue != "ssh" || !strings.Contains(plain(m), `filter "ssh"`) {
		t.Errorf("Processes lost the dashboard's filter (%q):\n%s", m.filterValue, plain(m))
	}
	m = runActs(t, m, keyMsg("esc"), keyMsg("1"))
	if m.filterValue != "" || strings.Contains(plain(m), `filter "`) {
		t.Errorf("esc in Processes left the filter on the dashboard: %q", m.filterValue)
	}
}

// TestMoveNoticeClearsOnNextKey: the J/K refusal answers one key press.
func TestMoveNoticeClearsOnNextKey(t *testing.T) {
	m := hubModel(t, newFakeHub(threeDevices()), 100, 30)
	m.filterValue = "o"
	m = run(t, m, keyMsg("K"))
	if m.notice == "" {
		t.Fatal("K with a filter gave no notice")
	}
	m = run(t, m, keyMsg("down"))
	if m.notice != "" {
		t.Errorf("notice still %q", m.notice)
	}
}

// TestStaleRightAfterDisconnect: when the stream drops, online flags are unknown at once,
// not after staleAfter.
func TestStaleRightAfterDisconnect(t *testing.T) {
	m := hubModel(t, newFakeHub(threeDevices()), 100, 30)
	m = run(t, m, hubMsg{client.Event{Type: client.EventDisconnected, Err: errors.New("connection refused"), Retry: time.Second}})
	if !m.conn.stale {
		t.Error("devices still trusted right after the stream dropped")
	}
	m = run(t, m, hubMsg{client.Event{Type: client.EventSnapshot, Devices: threeDevices()}})
	if m.conn.stale {
		t.Error("still stale after a fresh snapshot")
	}
}

// TestHubErrIsASentence: Go's network errors become plain words, without the URL.
func TestHubErrIsASentence(t *testing.T) {
	refused := &url.Error{Op: "Get", URL: "http://127.0.0.1:17840/api/info?nonce=89c9",
		Err: &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}}
	for _, c := range []struct {
		err  error
		want string
	}{
		{refused, "isn't accepting connections"},
		{context.DeadlineExceeded, "didn't answer in time"},
		{&net.DNSError{Err: "no such host", Name: "hub.lan"}, "can't find the hub"},
		{errors.New("hub: stream ended"), "closed the connection"},
		{errors.New("weird\x1b[31m failure"), "weird failure"},
	} {
		got := hubErr(c.err)
		if !strings.Contains(got, c.want) || strings.ContainsAny(got, "\x1b") || strings.Contains(got, "nonce") || strings.Contains(got, "dial tcp") {
			t.Errorf("hubErr(%v) = %q, want it to say %q", c.err, got, c.want)
		}
	}
}

// TestHubOwnPort: through a port forward the install URL uses the hub's port, not the TUI's.
func TestHubOwnPort(t *testing.T) {
	info := &client.Info{Port: 17840}
	for _, c := range []struct{ raw, via, want string }{
		{"http://192.168.77.5:17842", "127.0.0.1:17842", "http://192.168.77.5:17840"},
		{"http://192.168.77.5:7777", "localhost:7777", "http://192.168.77.5:17840"},
		{"http://hub.lan:7777", "hub.lan:7777", "http://hub.lan:7777"},          // not loopback: as is
		{"http://127.0.0.1:17842", "127.0.0.1:17842", "http://127.0.0.1:17842"}, // nothing better
	} {
		if got := hubOwnPort(c.raw, c.via, info); got != c.want {
			t.Errorf("hubOwnPort(%q via %q) = %q, want %q", c.raw, c.via, got, c.want)
		}
	}
	if got := hubOwnPort("http://192.168.77.5:17842", "127.0.0.1:17842", nil); got != "http://192.168.77.5:17842" {
		t.Errorf("without info: %q", got)
	}
}

// TestAddDeviceUpstreamText: an upstream hub has 5 tries in all and a bash installer.
func TestAddDeviceUpstreamText(t *testing.T) {
	m := hubModel(t, newFakeHub(nil), 100, 30)
	if s := m.pairLimits(); !strings.Contains(s, "5 tries per device") {
		t.Errorf("ours: %q", s)
	}
	m.add.theirs = true
	if s := m.pairLimits(); !strings.Contains(s, "5 wrong tries in all") || strings.Contains(s, "per device") {
		t.Errorf("upstream: %q", s)
	}
}

// TestVersionOption: Settings shows the version the caller gives.
func TestVersionOption(t *testing.T) {
	m := New(Options{Hub: newFakeHub(nil), Version: "1.2.3"}).(model)
	if m.version() != "1.2.3" {
		t.Errorf("version = %q", m.version())
	}
}

var _ tea.Msg = hubMsg{}
