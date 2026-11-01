package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/agent"
	"github.com/ContractorKeith/machine-monitor/internal/config"
	"github.com/ContractorKeith/machine-monitor/internal/tui"
)

// TestNotATerminal: /dev/null is a character device but not a terminal (B17).
func TestNotATerminal(t *testing.T) {
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	if stdoutIsTerminal(null) {
		t.Error("/dev/null counts as a terminal")
	}
	old := os.Stdin
	os.Stdin = null
	defer func() { os.Stdin = old }()
	if stdinIsTerminal() {
		t.Error("stdin from /dev/null counts as a terminal")
	}
}

// TestConnectSaysHowToReport: on a terminal connect still tells the user that pairing alone
// doesn't make this machine report, around the TUI (B8).
func TestConnectSaysHowToReport(t *testing.T) {
	h, srv, _ := connectHub(t)
	stubAgent(t)
	var out, errOut bytes.Buffer
	if rc := run([]string{"connect", srv.URL + "/?key=" + h.Token()}, &out, &errOut); rc != 0 {
		t.Fatalf("exit %d: %s", rc, errOut.String())
	}
	if n := strings.Count(out.String(), "machine-monitor agent"); n != 2 || !strings.Contains(out.String(), "service install --role agent") {
		t.Errorf("want the hint before and after the TUI, got %d:\n%s", n, out.String())
	}
}

// TestConnectFlagsAfterURL: the usage line says why (flag stops at the URL).
func TestConnectFlagsAfterURL(t *testing.T) {
	var out, errOut bytes.Buffer
	if rc := run([]string{"connect", "http://127.0.0.1:7777", "--code", "123456"}, &out, &errOut); rc != 2 ||
		!strings.Contains(errOut.String(), "flags go before the URL") {
		t.Errorf("exit %d, %q", rc, errOut.String())
	}
}

// TestTUIOptionsCarryVersion: Settings shows what --version prints (B22).
func TestTUIOptionsCarryVersion(t *testing.T) {
	old := version
	version = "9.9.9"
	defer func() { version = old }()
	if o := tuiOptions(tui.Options{}); o.Version != "9.9.9" {
		t.Errorf("Version = %q", o.Version)
	}
}

// TestLoadPairingBothSpellings: the agent and the service read the same client.json
// shapes as the TUI and export (B21).
func TestLoadPairingBothSpellings(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "client.json")
	if _, err := loadPairing(p); !errors.Is(err, agent.ErrNotPaired) {
		t.Errorf("missing file: %v", err)
	}
	for _, body := range []string{
		`{"hubUrl":"http://10.0.0.2:7777","key":"0123456789abcdef"}`,
		`{"hub":"http://10.0.0.2:7777","token":"0123456789abcdef"}`,
	} {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := loadPairing(p)
		if err != nil || c.HubURL != "http://10.0.0.2:7777" || c.Key != "0123456789abcdef" {
			t.Errorf("%s: %+v, %v", body, c, err)
		}
	}
	t.Setenv(config.DirEnv, dir)
	var errOut bytes.Buffer
	if hub, code := pairedHub("", &errOut); code != 0 || hub != "http://10.0.0.2:7777" {
		t.Errorf("service pairedHub = %q, %d, %q", hub, code, errOut.String())
	}
}

// TestServiceRefusalNamesNoTUI: the different-hub refusal points at a flag that exists.
func TestServiceRefusalNamesNoTUI(t *testing.T) {
	pairClient(t, "http://10.0.0.2:7777")
	var errOut bytes.Buffer
	if _, code := pairedHub("http://10.0.0.3:7777", &errOut); code != 1 || !strings.Contains(errOut.String(), "connect --no-tui http://10.0.0.3:7777") {
		t.Errorf("%d %q", code, errOut.String())
	}
}

// TestKeyFromEnvAndWarning: --key warns about argv; the environment variable doesn't.
func TestKeyFromEnvAndWarning(t *testing.T) {
	var errOut bytes.Buffer
	if got := keyArg("abc", &errOut); got != "abc" || !strings.Contains(errOut.String(), "process list") {
		t.Errorf("--key: %q, %q", got, errOut.String())
	}
	errOut.Reset()
	t.Setenv(envKey, "fromenv")
	if got := keyArg("", &errOut); got != "fromenv" || errOut.Len() != 0 {
		t.Errorf("env: %q, %q", got, errOut.String())
	}
}

// TestExportKeyHandling: a ?key= URL is refused as everywhere else, a wrong key is called
// wrong, and localhost finds a key saved for 127.0.0.1 (B21).
func TestExportKeyHandling(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(config.DirEnv, dir)
	exportNow = func() time.Time { return time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC) }
	defer func() { exportNow = time.Now }()
	srv := fakeExportHub(t, "0123456789abcdef")
	var out, errOut bytes.Buffer
	if rc := run([]string{"export", "id-1", "--hub", srv.URL + "/?key=0123456789abcdef"}, &out, &errOut); rc != 2 ||
		!strings.Contains(errOut.String(), "?key=") {
		t.Errorf("?key= URL: exit %d, %q", rc, errOut.String())
	}
	errOut.Reset()
	if rc := run([]string{"export", "id-1", "--hub", srv.URL, "--key", "ffffffffffffffff"}, &out, &errOut); rc != 1 ||
		!strings.Contains(errOut.String(), "key was not sent") || strings.Contains(errOut.String(), "has no key for it") {
		t.Errorf("wrong key: exit %d, %q", rc, errOut.String())
	}
	local := strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)
	if err := agent.SaveClient(filepath.Join(dir, "client.json"), agent.Client{HubURL: srv.URL, Key: "0123456789abcdef"}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	if rc := run([]string{"export", "id-1", "--hub", local, "--range", "6h"}, &out, &errOut); rc != 0 {
		t.Errorf("localhost: exit %d, %q", rc, errOut.String())
	}
}

// TestRollbackMessageFollowsStatus: when removing a half-installed service fails but status
// says it isn't installed, the message says rolled back (B21).
func TestRollbackMessageFollowsStatus(t *testing.T) {
	pairClient(t, "http://10.0.0.2:7777")
	fakeService(t)
	m, _ := newServiceManager()
	run0 := m.Run
	m.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if slices.Contains(args, "daemon-reload") { // install fails, and so does the rollback after removing the unit
			return []byte("boom"), errors.New("exit status 1")
		}
		return run0(ctx, name, args...)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"service", "install", "--role", "agent"}, &out, &errOut); code != 1 ||
		strings.Contains(errOut.String(), "partly installed") {
		t.Errorf("exit %d, %q", code, errOut.String())
	}
}
