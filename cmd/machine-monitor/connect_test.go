package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ContractorKeith/machine-monitor/internal/agent"
	"github.com/ContractorKeith/machine-monitor/internal/config"
	"github.com/ContractorKeith/machine-monitor/internal/hub"
)

// connectHub starts a real hub and points MACHINE_MONITOR_DIR at a fresh client dir.
func connectHub(t *testing.T) (*hub.Hub, *httptest.Server, string) {
	t.Helper()
	h, err := hub.New(hub.Options{Paths: config.At(t.TempDir()), Hostname: "testhub", Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(func() { srv.Close(); h.Close() })
	dir := t.TempDir()
	t.Setenv(config.DirEnv, dir)
	return h, srv, filepath.Join(dir, "client.json")
}

func pairCode(t *testing.T, h *hub.Hub, srv *httptest.Server) string {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+"/api/pair/code", nil)
	req.Header.Set("Authorization", "Bearer "+h.Token())
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var v struct{ Code string }
	json.NewDecoder(resp.Body).Decode(&v)
	return v.Code
}

// stubAgent replaces the agent connect starts and records what it was given.
func stubAgent(t *testing.T) *[2]string {
	got := new([2]string)
	old := connectAgent
	connectAgent = func(hubURL, key string, _ io.Writer) int { got[0], got[1] = hubURL, key; return 0 }
	t.Cleanup(func() { connectAgent = old })
	return got
}

func setStdin(t *testing.T, s string) {
	old, oldTTY := stdin, stdinIsTerminal
	stdin = strings.NewReader(s)
	stdinIsTerminal = func() bool { return true }
	t.Cleanup(func() { stdin, stdinIsTerminal = old, oldTTY })
}

func TestConnectWithoutTerminalNeedsCode(t *testing.T) {
	_, srv, _ := connectHub(t)
	old := stdinIsTerminal
	stdinIsTerminal = func() bool { return false }
	t.Cleanup(func() { stdinIsTerminal = old })
	var out, errOut bytes.Buffer
	if rc := run([]string{"connect", srv.URL}, &out, &errOut); rc != 2 || !strings.Contains(errOut.String(), "--code") {
		t.Fatalf("exit %d, %q", rc, errOut.String())
	}
	if strings.Contains(out.String(), "Pairing code") {
		t.Error("prompted with no terminal")
	}
}

func TestConnectNamesReplacedHub(t *testing.T) {
	h, srv, clientPath := connectHub(t)
	stubAgent(t)
	if err := agent.SaveClient(clientPath, agent.Client{HubURL: "http://10.0.0.9:7777", Key: "0123456789abcdef"}); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if rc := run([]string{"connect", "--no-agent", srv.URL + "/?key=" + h.Token()}, &out, &errOut); rc != 0 {
		t.Fatalf("exit %d: %s", rc, errOut.String())
	}
	if !strings.Contains(out.String(), "Replacing the pairing with http://10.0.0.9:7777.") {
		t.Errorf("stdout %q", out.String())
	}
	out.Reset()
	if rc := run([]string{"connect", "--no-agent", srv.URL + "/?key=" + h.Token()}, &out, &errOut); rc != 0 ||
		strings.Contains(out.String(), "Replacing") {
		t.Errorf("same hub again: exit %d, stdout %q", rc, out.String())
	}
}

func TestConnectPromptsThenRunsAgent(t *testing.T) {
	h, srv, clientPath := connectHub(t)
	code := pairCode(t, h, srv)
	wrong := "000000"
	if code == wrong {
		wrong = "000001"
	}
	setStdin(t, wrong+"\n"+code+"\n")
	got := stubAgent(t)
	var out, errOut bytes.Buffer
	if rc := run([]string{"connect", srv.URL}, &out, &errOut); rc != 0 {
		t.Fatalf("exit %d: %s", rc, errOut.String())
	}
	if !strings.Contains(out.String(), "Wrong pairing code.") || strings.Count(out.String(), "Pairing code") != 2 {
		t.Errorf("stdout %q", out.String())
	}
	if got[0] != srv.URL || got[1] != h.Token() {
		t.Errorf("agent started with %q %q", got[0], got[1])
	}
	cl, err := agent.LoadClient(clientPath)
	if err != nil || cl.HubURL != srv.URL || cl.Key != h.Token() {
		t.Errorf("client.json = %+v, %v", cl, err)
	}
}

func TestConnectWithCodeFlagAndNoAgent(t *testing.T) {
	h, srv, clientPath := connectHub(t)
	got := stubAgent(t)
	code := pairCode(t, h, srv)
	wrong := "000000"
	if code == wrong {
		wrong = "000001"
	}
	var out, errOut bytes.Buffer
	// A wrong --code is tried once, not re-prompted.
	if rc := run([]string{"connect", "--code", wrong, "--no-agent", srv.URL}, &out, &errOut); rc != 1 ||
		!strings.Contains(errOut.String(), "Wrong pairing code") {
		t.Fatalf("bad --code: exit %d, %q", rc, errOut.String())
	}
	if _, err := agent.LoadClient(clientPath); err == nil {
		t.Fatal("client.json written after a failed pairing")
	}
	if rc := run([]string{"connect", "--code", code[:3] + " " + code[3:], "--no-agent", srv.URL}, &out, &errOut); rc != 0 {
		t.Fatalf("exit %d: %s", rc, errOut.String())
	}
	if got[0] != "" || !strings.Contains(out.String(), "machine-monitor agent") {
		t.Errorf("--no-agent: agent %q, stdout %q", got[0], out.String())
	}
}

func TestConnectWithDashboardLink(t *testing.T) {
	h, srv, clientPath := connectHub(t)
	stubAgent(t)
	var out, errOut bytes.Buffer
	if rc := run([]string{"connect", "--no-agent", srv.URL + "/?key=" + h.Token()}, &out, &errOut); rc != 0 {
		t.Fatalf("exit %d: %s", rc, errOut.String())
	}
	if cl, _ := agent.LoadClient(clientPath); cl.Key != h.Token() {
		t.Errorf("client.json key %q", cl.Key)
	}
	// A link with a key this hub doesn't hold fails the proof and saves nothing new.
	if rc := run([]string{"connect", "--no-agent", srv.URL + "/?key=0000000000000000aaaa"}, &out, &errOut); rc != 1 ||
		!strings.Contains(errOut.String(), "does not hold") {
		t.Fatalf("wrong key: exit %d, %q", rc, errOut.String())
	}
	if cl, _ := agent.LoadClient(clientPath); cl.Key != h.Token() {
		t.Errorf("client.json overwritten with %q", cl.Key)
	}
}

func TestConnectKeyFromStdin(t *testing.T) {
	h, srv, clientPath := connectHub(t)
	stubAgent(t)
	setStdin(t, h.Token()+"\n")
	var out, errOut bytes.Buffer
	if rc := run([]string{"connect", "--no-agent", "--key-stdin", srv.URL}, &out, &errOut); rc != 0 {
		t.Fatalf("exit %d: %s", rc, errOut.String())
	}
	if cl, _ := agent.LoadClient(clientPath); cl.Key != h.Token() {
		t.Errorf("client.json key %q", cl.Key)
	}
	setStdin(t, "")
	if rc := run([]string{"connect", "--no-agent", "--key-stdin", srv.URL}, &out, &errOut); rc != 2 {
		t.Errorf("empty stdin: exit %d", rc)
	}
	setStdin(t, h.Token()+"\n")
	if rc := run([]string{"connect", "--key-stdin", "--code", "123456", srv.URL}, &out, &errOut); rc != 2 {
		t.Errorf("--key-stdin with --code: exit %d", rc)
	}
}

func TestAgentNeedsPairing(t *testing.T) {
	t.Setenv(config.DirEnv, t.TempDir())
	var out, errOut bytes.Buffer
	if rc := run([]string{"agent"}, &out, &errOut); rc != 1 || !strings.Contains(errOut.String(), "connect") {
		t.Fatalf("exit %d, %q", rc, errOut.String())
	}
	if rc := run([]string{"agent", "x"}, &out, &errOut); rc != 2 {
		t.Fatalf("extra arg: exit %d", rc)
	}
	if rc := run([]string{"connect"}, &out, &errOut); rc != 2 {
		t.Fatalf("connect without URL: exit %d", rc)
	}
}
