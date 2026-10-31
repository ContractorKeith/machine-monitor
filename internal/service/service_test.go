package service

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/ContractorKeith/machine-monitor/internal/config"
)

var update = flag.Bool("update", false, "rewrite golden files")

// fake is a service manager that records each call and answers from a table.
type fake struct {
	calls   []string
	replies map[string]reply // keyed by "name arg arg..."
}

type reply struct {
	out string
	err bool
}

func (f *fake) run(_ context.Context, name string, args ...string) ([]byte, error) {
	c := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, c)
	r := f.replies[c]
	if r.err {
		return []byte(r.out), errors.New("exit status 1")
	}
	return []byte(r.out), nil
}

// testManager is a Manager for goos whose files all live in a temp dir.
func testManager(t *testing.T, goos string) (*Manager, *fake) {
	t.Helper()
	root := t.TempDir()
	f := &fake{replies: map[string]reply{}}
	return &Manager{OS: goos, Home: filepath.Join(root, "home"), ConfigHome: filepath.Join(root, "home", ".config"),
		DataDir: filepath.Join(root, "data"), UID: 501, User: "alice", Run: f.run}, f
}

// goldenManager has fixed paths so rendered files are byte-for-byte stable.
func goldenManager(goos string) *Manager {
	m := &Manager{OS: goos, Home: "/home/alice", ConfigHome: "/home/alice/.config", DataDir: "/home/alice/.config/machine-monitor", UID: 501, User: "alice"}
	if goos == "darwin" {
		m.Home, m.DataDir = "/Users/alice", "/Users/alice/Library/Application Support/machine-monitor"
	}
	if goos == "windows" {
		m.User = `DESKTOP-1\alice`
	}
	return m
}

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -update)", err)
	}
	if got != string(want) {
		t.Errorf("%s differs (go test -update rewrites it)\n--- got\n%s\n--- want\n%s", name, got, want)
	}
}

func TestGolden(t *testing.T) {
	hub := Spec{Role: RoleHub, Exe: "/usr/local/bin/machine-monitor"}
	agent := Spec{Role: RoleAgent, Exe: "/opt/machine monitor/machine-monitor", HubURL: "http://192.168.1.10:7777"}
	winHub := Spec{Role: RoleHub, Exe: `C:\Users\alice\AppData\Local\Programs\machine-monitor\machine-monitor.exe`, Port: 8080}
	winAgent := Spec{Role: RoleAgent, Exe: `C:\Program Files\machine-monitor\machine-monitor.exe`, HubURL: "http://hub.local:7777/?a=1&b=2"}

	lin := goldenManager("linux")
	checkGolden(t, "hub.service", lin.SystemdUnit(hub))
	lin.DataDirEnv = `/srv/mm $HOME 100% "q"`
	checkGolden(t, "agent.service", lin.SystemdUnit(agent))

	mac := goldenManager("darwin")
	checkGolden(t, "hub.plist", mac.LaunchdPlist(hub))
	mac.DataDirEnv = "/tmp/a&b"
	checkGolden(t, "agent.plist", mac.LaunchdPlist(agent))

	win := goldenManager("windows")
	checkGolden(t, "hub.xml", win.TaskXML(winHub))
	checkGolden(t, "agent.xml", win.TaskXML(winAgent))
}

func TestSystemdQuote(t *testing.T) {
	for in, want := range map[string]string{
		"hub":          `"hub"`,
		`a b`:          `"a b"`,
		`say "hi"\now`: `"say \"hi\"\\now"`,
		"100% $HOME":   `"100%% $$HOME"`,
	} {
		if got := systemdQuote(in); got != want {
			t.Errorf("systemdQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestSystemdEnvQuote(t *testing.T) {
	for in, want := range map[string]string{
		`A=/srv/$HOME`: `"A=/srv/$HOME"`,
		`A=100% "q"\`:  `"A=100%% \"q\"\\"`,
	} {
		if got := systemdEnvQuote(in); got != want {
			t.Errorf("systemdEnvQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestWinQuote(t *testing.T) {
	for in, want := range map[string]string{
		"hub":                  `hub`,
		"":                     `""`,
		`C:\Program Files\x`:   `"C:\Program Files\x"`,
		`C:\dir with space\`:   `"C:\dir with space\\"`,
		`say "hi"`:             `"say \"hi\""`,
		`back\"quote`:          `"back\\\"quote"`,
		`http://h:7777/?a&b=1`: `http://h:7777/?a&b=1`,
	} {
		if got := winQuote(in); got != want {
			t.Errorf("winQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestUTF16File(t *testing.T) {
	b := utf16File("<é>")
	if !bytes.HasPrefix(b, []byte{0xFF, 0xFE}) || len(b) != 8 {
		t.Fatalf("utf16File = % x", b)
	}
	u := make([]uint16, 0, 3)
	for i := 2; i < len(b); i += 2 {
		u = append(u, uint16(b[i])|uint16(b[i+1])<<8)
	}
	if got := string(utf16.Decode(u)); got != "<é>" {
		t.Errorf("round trip = %q", got)
	}
}

func TestValidate(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "machine-monitor")
	ok := []Spec{
		{Role: RoleHub, Exe: exe},
		{Role: RoleHub, Exe: exe, Port: 8080},
		{Role: RoleAgent, Exe: exe, HubURL: "http://10.0.0.2:7777"},
		{Role: RoleAgent, Exe: exe, HubURL: "https://hub.example.ts.net"},
	}
	bad := []Spec{
		{Role: RoleHub, Exe: "machine-monitor"},
		{Role: RoleHub, Exe: exe, Port: 70000},
		{Role: RoleHub, Exe: exe, HubURL: "http://h:7777"},
		{Role: RoleAgent, Exe: exe},
		{Role: RoleAgent, Exe: exe, HubURL: "ftp://h"},
		{Role: RoleAgent, Exe: exe, HubURL: "h:7777"},
		{Role: RoleAgent, Exe: exe, HubURL: "http://h:7777", Port: 1},
		{Role: "tui", Exe: exe},
	}
	for _, s := range ok {
		if err := s.Validate(); err != nil {
			t.Errorf("Validate(%+v) = %v", s, err)
		}
	}
	for _, s := range bad {
		if err := s.Validate(); err == nil {
			t.Errorf("Validate(%+v) = nil, want an error", s)
		}
	}
	if _, err := ParseRole("both"); err == nil {
		t.Error("ParseRole(both) = nil error")
	}
}

func hubSpec(t *testing.T) Spec {
	return Spec{Role: RoleHub, Exe: filepath.Join(t.TempDir(), "machine-monitor")}
}

func wantCalls(t *testing.T, f *fake, want ...string) {
	t.Helper()
	if !slices.Equal(f.calls, want) {
		t.Errorf("calls:\n  %s\nwant:\n  %s", strings.Join(f.calls, "\n  "), strings.Join(want, "\n  "))
	}
	f.calls = nil
}

func TestSystemdFlow(t *testing.T) {
	ctx := context.Background()
	m, f := testManager(t, "linux")
	unit := filepath.Join(m.ConfigHome, "systemd", "user", "machine-monitor-hub.service")
	s := hubSpec(t)
	var out bytes.Buffer

	if st, _ := m.Status(ctx, RoleHub); st.Installed {
		t.Fatalf("installed before install: %+v", st)
	}
	wantCalls(t, f)

	if err := m.Install(ctx, s, &out); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, f, "systemctl --user daemon-reload", "systemctl --user enable --now machine-monitor-hub.service")
	if b, _ := os.ReadFile(unit); string(b) != m.SystemdUnit(s) {
		t.Errorf("unit file = %q", b)
	}
	if !strings.Contains(out.String(), "loginctl enable-linger alice") || !strings.Contains(out.String(), "wrote "+unit) {
		t.Errorf("install output:\n%s", out.String())
	}

	// Same spec again: nothing to restart.
	if err := m.Install(ctx, s, &out); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, f, "systemctl --user daemon-reload", "systemctl --user enable --now machine-monitor-hub.service")

	// New port: rewrite and restart.
	s.Port = 8080
	if err := m.Install(ctx, s, &out); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, f, "systemctl --user daemon-reload", "systemctl --user enable --now machine-monitor-hub.service",
		"systemctl --user restart machine-monitor-hub.service")

	f.replies["systemctl --user is-enabled machine-monitor-hub.service"] = reply{out: "enabled\n"}
	f.replies["systemctl --user is-active machine-monitor-hub.service"] = reply{out: "failed\n", err: true}
	st, err := m.Status(ctx, RoleHub)
	if err != nil || !st.Installed || st.State != "enabled, failed" || st.Path != unit {
		t.Errorf("Status = %+v, %v", st, err)
	}
	f.calls = nil

	// disable failing (already stopped) still removes the unit.
	f.replies["systemctl --user disable --now machine-monitor-hub.service"] = reply{out: "boom", err: true}
	out.Reset()
	if err := m.Uninstall(ctx, RoleHub, &out); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, f, "systemctl --user disable --now machine-monitor-hub.service", "systemctl --user daemon-reload")
	if fileExists(unit) || !strings.Contains(out.String(), "warning:") {
		t.Errorf("unit still there or no warning:\n%s", out.String())
	}

	// Uninstall again: a no-op.
	out.Reset()
	if err := m.Uninstall(ctx, RoleHub, &out); err != nil || !strings.Contains(out.String(), "not installed") {
		t.Errorf("second uninstall: %v\n%s", err, out.String())
	}
	wantCalls(t, f)
}

func TestSystemdInstallError(t *testing.T) {
	m, f := testManager(t, "linux")
	f.replies["systemctl --user daemon-reload"] = reply{out: "Failed to connect to bus", err: true}
	err := m.Install(context.Background(), hubSpec(t), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "Failed to connect to bus") {
		t.Errorf("Install = %v, want the systemctl output in the error", err)
	}
}

func TestLaunchdFlow(t *testing.T) {
	ctx := context.Background()
	old := bootstrapRetry
	bootstrapRetry = 0
	t.Cleanup(func() { bootstrapRetry = old })
	m, f := testManager(t, "darwin")
	plist := filepath.Join(m.Home, "Library", "LaunchAgents", "io.github.contractorkeith.machine-monitor.hub.plist")
	target := "gui/501/io.github.contractorkeith.machine-monitor.hub"
	s := hubSpec(t)
	var out bytes.Buffer

	// First install: nothing to boot out.
	f.replies["launchctl bootout "+target] = reply{out: "No such process", err: true}
	if err := m.Install(ctx, s, &out); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, f, "launchctl bootout "+target, "launchctl bootstrap gui/501 "+plist)
	if b, _ := os.ReadFile(plist); string(b) != m.LaunchdPlist(s) {
		t.Errorf("plist = %q", b)
	}
	if fi, err := os.Stat(filepath.Join(m.DataDir, "logs")); err != nil || !fi.IsDir() {
		t.Errorf("log dir not created: %v", err)
	}

	// Reinstall: boot out the old job; bootstrap fails once while it's going away.
	delete(f.replies, "launchctl bootout "+target)
	n := 0
	m.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if args[0] == "bootstrap" {
			if n++; n == 1 {
				f.calls = append(f.calls, "launchctl bootstrap (busy)")
				return []byte("Bootstrap failed: 5: Input/output error"), errors.New("exit status 5")
			}
		}
		return f.run(ctx, name, args...)
	}
	if err := m.Install(ctx, s, &out); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, f, "launchctl bootout "+target, "launchctl bootstrap (busy)", "launchctl bootstrap gui/501 "+plist)
	m.Run = f.run

	f.replies["launchctl print "+target] = reply{out: "io.github.contractorkeith.machine-monitor.hub = {\n\tactive count = 1\n\tstate = running\n}\n"}
	if st, _ := m.Status(ctx, RoleHub); !st.Installed || st.State != "loaded, running" {
		t.Errorf("Status = %+v", st)
	}
	f.replies["launchctl print "+target] = reply{out: "Could not find service", err: true}
	if st, _ := m.Status(ctx, RoleHub); !st.Installed || st.State != "not loaded" {
		t.Errorf("Status = %+v", st)
	}
	f.calls = nil

	if err := m.Uninstall(ctx, RoleHub, &out); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, f, "launchctl bootout "+target)
	if fileExists(plist) {
		t.Error("plist still there")
	}
	if st, _ := m.Status(ctx, RoleHub); st.Installed {
		t.Errorf("Status after uninstall = %+v", st)
	}
}

func TestLaunchdBootstrapFailsWithoutBootout(t *testing.T) {
	m, f := testManager(t, "darwin")
	f.replies["launchctl bootout gui/501/io.github.contractorkeith.machine-monitor.hub"] = reply{err: true}
	f.replies["launchctl bootstrap gui/501 "+m.plistPath(RoleHub)] = reply{out: "Bootstrap failed", err: true}
	if err := m.Install(context.Background(), hubSpec(t), &bytes.Buffer{}); err == nil {
		t.Fatal("Install = nil error")
	}
	if len(f.calls) != 2 { // no retries when nothing was booted out
		t.Errorf("calls = %q", f.calls)
	}
}

func TestTaskFlow(t *testing.T) {
	ctx := context.Background()
	m, f := testManager(t, "windows")
	s := Spec{Role: RoleAgent, Exe: hubSpec(t).Exe, HubURL: "http://h:7777"}
	xmlPath := filepath.Join(m.DataDir, "service-machine-monitor-agent.xml")
	query := "schtasks /Query /TN machine-monitor-agent /FO CSV /NH"
	var out bytes.Buffer

	var xmlSeen []byte
	m.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if slices.Contains(args, "/Create") {
			xmlSeen, _ = os.ReadFile(xmlPath)
		}
		return f.run(ctx, name, args...)
	}
	f.replies["schtasks /End /TN machine-monitor-agent"] = reply{out: "ERROR: not running", err: true}
	if err := m.Install(ctx, s, &out); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, f, "schtasks /Create /TN machine-monitor-agent /XML "+xmlPath+" /F",
		"schtasks /End /TN machine-monitor-agent", "schtasks /Run /TN machine-monitor-agent")
	if !bytes.Equal(xmlSeen, utf16File(m.TaskXML(s))) {
		t.Error("schtasks didn't see the task XML as UTF-16")
	}
	if fileExists(xmlPath) {
		t.Error("task XML left behind")
	}

	f.replies[query] = reply{err: true, out: "ERROR: The system cannot find the file specified."}
	if st, _ := m.Status(ctx, RoleAgent); st.Installed {
		t.Errorf("Status = %+v", st)
	}
	f.replies[query] = reply{out: "\"\\machine-monitor-agent\",\"N/A\",\"Running\"\r\n"}
	if st, _ := m.Status(ctx, RoleAgent); !st.Installed || st.State != "Running" {
		t.Errorf("Status = %+v", st)
	}
	f.calls = nil

	if err := m.Uninstall(ctx, RoleAgent, &out); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, f, query, "schtasks /End /TN machine-monitor-agent", "schtasks /Delete /TN machine-monitor-agent /F")

	f.replies[query] = reply{err: true}
	out.Reset()
	if err := m.Uninstall(ctx, RoleAgent, &out); err != nil || !strings.Contains(out.String(), "not installed") {
		t.Errorf("uninstall when absent: %v\n%s", err, out.String())
	}
	wantCalls(t, f, query)
}

func TestUnsupportedOS(t *testing.T) {
	m, _ := testManager(t, "plan9")
	if err := m.Install(context.Background(), hubSpec(t), &bytes.Buffer{}); err == nil {
		t.Error("Install on plan9 = nil error")
	}
	if _, err := m.Status(context.Background(), RoleHub); err == nil {
		t.Error("Status on plan9 = nil error")
	}
}

func TestStrippedEnv(t *testing.T) {
	t.Setenv("MM_SECRET_TEST", "x")
	t.Setenv("HOME", "/home/test")
	env := strippedEnv()
	if !slices.Contains(env, "LC_ALL=C") || slices.Contains(env, "MM_SECRET_TEST=x") {
		t.Errorf("strippedEnv = %q", env)
	}
}

func TestExecRunnerUnknownTool(t *testing.T) {
	if _, err := ExecRunner(context.Background(), "rm", "-rf", "/"); err == nil {
		t.Error("ExecRunner ran a tool outside the allow list")
	}
}

// A relative MACHINE_MONITOR_DIR is made absolute at install time, since the service runs
// from another directory (skeptic L5).
func TestDefaultMakesDataDirEnvAbsolute(t *testing.T) {
	t.Setenv(config.DirEnv, "rel")
	m, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(m.DataDirEnv) || filepath.Base(m.DataDirEnv) != "rel" {
		t.Errorf("DataDirEnv = %q, want an absolute path ending in rel", m.DataDirEnv)
	}
}
