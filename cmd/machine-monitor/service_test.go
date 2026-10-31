package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ContractorKeith/machine-monitor/internal/agent"
	"github.com/ContractorKeith/machine-monitor/internal/config"
	"github.com/ContractorKeith/machine-monitor/internal/service"
)

func init() { built["service"] = true } // keeps main_test.go out of this change

// fakeService points the service command at a Linux manager in a temp dir whose
// systemctl calls are recorded, never run.
func fakeService(t *testing.T) (*[]string, string) {
	t.Helper()
	root := t.TempDir()
	var calls []string
	m := &service.Manager{OS: "linux", Home: root, ConfigHome: filepath.Join(root, ".config"), DataDir: filepath.Join(root, "data"),
		User: "alice", Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			calls = append(calls, name+" "+strings.Join(args, " "))
			return []byte("enabled\n"), nil
		}}
	exe := filepath.Join(root, "bin", "machine-monitor")
	oldM, oldE := newServiceManager, serviceExecutable
	newServiceManager = func() (*service.Manager, error) { return m, nil }
	serviceExecutable = func() (string, error) { return exe, nil }
	t.Cleanup(func() { newServiceManager, serviceExecutable = oldM, oldE })
	return &calls, filepath.Join(m.ConfigHome, "systemd", "user")
}

// pairClient writes a client.json in a fresh data dir, as connect does.
func pairClient(t *testing.T, hubURL string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(config.DirEnv, dir)
	if err := agent.SaveClient(filepath.Join(dir, "client.json"), agent.Client{HubURL: hubURL, Key: "0123456789abcdef"}); err != nil {
		t.Fatal(err)
	}
}

func TestServiceCommand(t *testing.T) {
	pairClient(t, "http://10.0.0.2:7777")
	calls, unitDir := fakeService(t)
	var out, errOut bytes.Buffer
	if code := run([]string{"service", "install", "--hub", "http://10.0.0.2:7777"}, &out, &errOut); code != 0 {
		t.Fatalf("install exit %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), filepath.Join(unitDir, "machine-monitor-agent.service")) ||
		!slices.Contains(*calls, "systemctl --user enable --now machine-monitor-agent.service") {
		t.Errorf("install --hub didn't install the agent:\n%s\n%q", out.String(), *calls)
	}

	out.Reset()
	if code := run([]string{"service", "status"}, &out, &errOut); code != 0 {
		t.Fatalf("status exit %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "hub: not installed") || !strings.Contains(out.String(), "agent: installed, enabled, enabled") {
		t.Errorf("status:\n%s", out.String())
	}

	out.Reset()
	if code := run([]string{"service", "uninstall"}, &out, &errOut); code != 0 {
		t.Fatalf("uninstall exit %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "hub: not installed") || !strings.Contains(out.String(), "removed ") {
		t.Errorf("uninstall:\n%s", out.String())
	}
}

func TestServiceWarnsWhenAgentIsStub(t *testing.T) {
	old := commands["agent"]
	t.Cleanup(func() { commands["agent"] = old })
	pairClient(t, "http://10.0.0.2:7777")
	args := []string{"service", "install", "--hub", "http://10.0.0.2:7777"}
	const warning = "agent command isn't built yet"

	commands["agent"] = stub("agent")
	fakeService(t)
	var out, errOut bytes.Buffer
	if code := run(args, &out, &errOut); code != 0 || !strings.Contains(errOut.String(), warning) {
		t.Errorf("stub agent: exit %d, stderr %q; want 0 and a warning", code, errOut.String())
	}

	commands["agent"] = func(_ []string, _, _ io.Writer) int { return 0 } // a built agent answering -h
	fakeService(t)
	errOut.Reset()
	if code := run(args, &out, &errOut); code != 0 || strings.Contains(errOut.String(), warning) {
		t.Errorf("built agent: exit %d, stderr %q; want 0 and no warning", code, errOut.String())
	}
}

func TestServiceRun(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MACHINE_MONITOR_DIR", dir)
	oldAgent, oldRedirect := commands["agent"], redirectOutput
	t.Cleanup(func() { commands["agent"], redirectOutput = oldAgent, oldRedirect })
	redirected := false
	redirectOutput = func(*os.File) { redirected = true } // keep go test's own output
	var got []string
	commands["agent"] = func(args []string, stdout, _ io.Writer) int {
		got = args
		io.WriteString(stdout, "agent says hi\n")
		return 3
	}

	var out, errOut bytes.Buffer
	if code := run([]string{"service", "run", "--", "agent", "--hub", "http://h:7777"}, &out, &errOut); code != 3 {
		t.Fatalf("exit %d, want the command's 3; stderr %q", code, errOut.String())
	}
	if !slices.Equal(got, []string{"--hub", "http://h:7777"}) || !redirected {
		t.Errorf("args %q, redirected %v", got, redirected)
	}
	b, err := os.ReadFile(service.LogFile(dir, service.RoleAgent))
	if err != nil || !strings.Contains(string(b), "agent says hi") || !strings.Contains(string(b), "exited 3") {
		t.Errorf("log %q, %v", b, err)
	}

	for _, args := range [][]string{{"service", "run"}, {"service", "run", "--", "status"}} {
		if code := run(args, &out, &errOut); code != 2 {
			t.Errorf("run(%q) = %d, want 2", args, code)
		}
	}
}

func TestServiceUsage(t *testing.T) {
	pairClient(t, "http://10.0.0.2:7777")
	fakeService(t)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"service"}, "missing action"},
		{[]string{"service", "--role", "hub"}, "missing action"},
		{[]string{"service", "restart"}, `unknown action "restart"`},
		{[]string{"service", "install", "--role", "both"}, "bad role"},
		{[]string{"service", "install", "--role", "hub", "--hub", "http://h:7777"}, "--hub is for the agent"},
		{[]string{"service", "install", "--hub", "ftp://nope"}, "not a hub address"},
		{[]string{"service", "install", "--port", "99999"}, "bad port"},
		{[]string{"service", "status", "--port", "1"}, "are for install"},
		{[]string{"service", "status", "extra"}, `unexpected argument "extra"`},
	} {
		var out, errOut bytes.Buffer
		if code := run(c.args, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), c.want) {
			t.Errorf("run(%q) = %d, stderr %q; want 2 and %q", c.args, code, errOut.String(), c.want)
		}
	}
}

func TestInTempDir(t *testing.T) {
	dir := t.TempDir()
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	if !inTempDir(filepath.Join(dir, "go-build1", "exe", "machine-monitor")) {
		t.Error("temp path not detected")
	}
	if inTempDir(filepath.Join(filepath.VolumeName(dir)+string(filepath.Separator), "usr-local-bin-x", "machine-monitor")) {
		t.Error("non-temp path flagged")
	}
}

// install --role agent with no --hub uses client.json's hub, so the hint connect prints
// works as printed (skeptic M2).
func TestServiceInstallAgentUsesPairedHub(t *testing.T) {
	pairClient(t, "http://10.0.0.2:7777")
	_, unitDir := fakeService(t)
	var out, errOut bytes.Buffer
	if code := run([]string{"service", "install", "--role", "agent"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	b, err := os.ReadFile(filepath.Join(unitDir, "machine-monitor-agent.service"))
	if err != nil || !strings.Contains(string(b), `"--hub" "http://10.0.0.2:7777"`) {
		t.Errorf("unit %q, %v", b, err)
	}
}

// An unpaired machine, or one paired with another hub, gets no agent unit (skeptic M3).
func TestServiceInstallAgentRefusesWithoutPairing(t *testing.T) {
	t.Setenv(config.DirEnv, t.TempDir())
	calls, unitDir := fakeService(t)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"service", "install", "--role", "agent"}, "not paired"},
		{[]string{"service", "install", "--hub", "http://10.0.0.2:7777"}, "not paired"},
	} {
		var out, errOut bytes.Buffer
		if code := run(c.args, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), c.want) {
			t.Errorf("run(%q) = %d, stderr %q; want 1 and %q", c.args, code, errOut.String(), c.want)
		}
	}
	pairClient(t, "http://10.0.0.9:7777")
	var out, errOut bytes.Buffer
	if code := run([]string{"service", "install", "--hub", "http://10.0.0.2:7777"}, &out, &errOut); code != 1 ||
		!strings.Contains(errOut.String(), "paired with http://10.0.0.9:7777, not http://10.0.0.2:7777") {
		t.Errorf("other hub: exit %d, stderr %q", code, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(unitDir, "machine-monitor-agent.service")); err == nil || len(*calls) > 0 {
		t.Errorf("a unit was written or systemctl ran: %q", *calls)
	}
}

// A ?key= in --hub is refused and never reaches a unit file (skeptic L4).
func TestServiceInstallRejectsKeyInURL(t *testing.T) {
	pairClient(t, "http://10.0.0.2:7777")
	_, unitDir := fakeService(t)
	var out, errOut bytes.Buffer
	code := run([]string{"service", "install", "--hub", "http://10.0.0.2:7777/?key=b798765aee072d854cc2a7c1"}, &out, &errOut)
	if code != 2 || !strings.Contains(errOut.String(), "connect --key-stdin") {
		t.Fatalf("exit %d, %q", code, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(unitDir, "machine-monitor-agent.service")); err == nil {
		t.Error("a unit was written")
	}
}

// A failed install removes what it wrote, unless the role was already installed.
func TestServiceInstallRollsBack(t *testing.T) {
	pairClient(t, "http://10.0.0.2:7777")
	calls, unitDir := fakeService(t)
	m, _ := newServiceManager()
	run0 := m.Run
	fail := true
	m.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if fail && slices.Contains(args, "enable") {
			return []byte("boom"), errors.New("exit status 1")
		}
		return run0(ctx, name, args...)
	}
	unit := filepath.Join(unitDir, "machine-monitor-agent.service")
	var out, errOut bytes.Buffer
	if code := run([]string{"service", "install", "--role", "agent"}, &out, &errOut); code != 1 ||
		!strings.Contains(errOut.String(), "Rolled back") {
		t.Fatalf("exit %d, %q", code, errOut.String())
	}
	if _, err := os.Stat(unit); err == nil {
		t.Errorf("unit left behind after a failed install; calls %q", *calls)
	}

	// An existing install is left alone when a re-install fails.
	fail = false
	errOut.Reset()
	if code := run([]string{"service", "install", "--role", "agent"}, &out, &errOut); code != 0 {
		t.Fatalf("install exit %d: %s", code, errOut.String())
	}
	fail = true
	errOut.Reset()
	if code := run([]string{"service", "install", "--role", "agent"}, &out, &errOut); code != 1 ||
		strings.Contains(errOut.String(), "Rolled back") {
		t.Errorf("re-install: exit %d, %q", code, errOut.String())
	}
	if _, err := os.Stat(unit); err != nil {
		t.Errorf("the existing unit was removed: %v", err)
	}
}
