package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/config"
	"github.com/ContractorKeith/machine-monitor/internal/hub"
)

func init() { built["pair"] = true }

// syncBuffer is a bytes.Buffer safe to read while a command writes it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// pairHub starts a hub whose data dir is this machine's (MACHINE_MONITOR_DIR) and returns
// it with its --port value.
func pairHub(t *testing.T) (*hub.Hub, *httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(config.DirEnv, dir)
	h, err := hub.New(hub.Options{Paths: config.At(dir), Hostname: "testhub", Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(func() { srv.Close(); h.Close() })
	u, _ := url.Parse(srv.URL)
	return h, srv, u.Port()
}

// startPair runs `pair --port port` in the background.
func startPair(port string) (out, errOut *syncBuffer, rc chan int) {
	out, errOut, rc = &syncBuffer{}, &syncBuffer{}, make(chan int, 1)
	go func() { rc <- run([]string{"pair", "--port", port}, out, errOut) }()
	return out, errOut, rc
}

func waitExit(t *testing.T, rc chan int, errOut *syncBuffer) int {
	t.Helper()
	select {
	case c := <-rc:
		return c
	case <-time.After(15 * time.Second):
		t.Fatalf("pair didn't finish; stderr %q", errOut.String())
		return 0
	}
}

func TestPairPrintsCodeUntilUsed(t *testing.T) {
	_, srv, port := pairHub(t)
	out, errOut, rc := startPair(port)
	codeRE := regexp.MustCompile(`Pairing code: (\d{3}) (\d{3})`)
	var m []string
	for deadline := time.Now().Add(10 * time.Second); m == nil; time.Sleep(20 * time.Millisecond) {
		if m = codeRE.FindStringSubmatch(out.String()); m == nil && time.Now().After(deadline) {
			t.Fatalf("no code: stdout %q, stderr %q", out.String(), errOut.String())
		}
	}
	if !strings.Contains(out.String(), "Expires in 10:00") && !strings.Contains(out.String(), "Expires in 9:59") {
		t.Errorf("no countdown line: %q", out.String())
	}
	resp, err := http.Post(srv.URL+"/api/pair", "application/json", strings.NewReader(fmt.Sprintf(`{"code":"%s%s"}`, m[1], m[2])))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("pairing with the printed code: %d", resp.StatusCode)
	}
	if c := waitExit(t, rc, errOut); c != 0 || !strings.Contains(out.String(), "Paired") {
		t.Fatalf("exit %d, stdout %q, stderr %q", c, out.String(), errOut.String())
	}
	if strings.Contains(out.String(), "\r") || strings.Contains(out.String(), "\033") {
		t.Errorf("terminal control codes on a non-terminal: %q", out.String())
	}
}

func TestPairCodeExpiresLocally(t *testing.T) {
	_, _, port := pairHub(t)
	oldNow, oldTick := pairNow, pairTick
	pairNow = func() time.Time { return time.Now().Add(hub.PairTTL - 50*time.Millisecond) }
	pairTick = 20 * time.Millisecond
	t.Cleanup(func() { pairNow, pairTick = oldNow, oldTick })
	out, errOut, rc := startPair(port)
	if c := waitExit(t, rc, errOut); c != 1 || !strings.Contains(out.String(), "The code expired") {
		t.Fatalf("exit %d, stdout %q, stderr %q", c, out.String(), errOut.String())
	}
}

func TestPairStopsWhenHubGoes(t *testing.T) {
	_, srv, port := pairHub(t)
	out, errOut, rc := startPair(port)
	for deadline := time.Now().Add(10 * time.Second); !strings.Contains(out.String(), "Pairing code"); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("no code: stdout %q, stderr %q", out.String(), errOut.String())
		}
	}
	srv.CloseClientConnections()
	srv.Close()
	if c := waitExit(t, rc, errOut); c != 1 || !strings.Contains(out.String(), "Lost the hub") {
		t.Fatalf("exit %d, stdout %q", c, out.String())
	}
}

func TestPairNeedsLocalHub(t *testing.T) {
	t.Setenv(config.DirEnv, t.TempDir())
	var out, errOut bytes.Buffer
	if c := run([]string{"pair"}, &out, &errOut); c != 1 || !strings.Contains(errOut.String(), "machine-monitor hub") {
		t.Fatalf("no hub key here: exit %d, %q", c, errOut.String())
	}
	// A key, but no hub answering on the port.
	_, srv, port := pairHub(t)
	srv.Close()
	if c := run([]string{"pair", "--port", port}, &out, &errOut); c != 1 || !strings.Contains(errOut.String(), "is machine-monitor hub running") {
		t.Fatalf("hub not running: exit %d, %q", c, errOut.String())
	}
	if c := run([]string{"pair", "--port", "0"}, &out, &errOut); c != 2 {
		t.Fatalf("bad port: exit %d", c)
	}
}

func TestHubShowKey(t *testing.T) {
	t.Setenv(config.DirEnv, t.TempDir())
	var out, errOut bytes.Buffer
	if c := run([]string{"hub", "--show-key"}, &out, &errOut); c != 1 || !strings.Contains(errOut.String(), "no hub key") {
		t.Fatalf("before any hub: exit %d, %q", c, errOut.String())
	}
	h, _, _ := pairHub(t)
	out.Reset()
	if c := run([]string{"hub", "--show-key"}, &out, &errOut); c != 0 || out.String() != h.Token()+"\n" {
		t.Fatalf("exit %d, stdout %q", c, out.String())
	}
}

// The key is masked when stdout is a pipe or log, as under systemd or launchd.
func TestHubKeyMaskedWhenNotTerminal(t *testing.T) {
	key := "c4bb99e367a5f6fcf4e8ccfa"
	var buf bytes.Buffer
	printKey(&buf, key)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	printKey(w, key)
	w.Close()
	var piped bytes.Buffer
	piped.ReadFrom(r)
	for name, got := range map[string]string{"buffer": buf.String(), "pipe": piped.String()} {
		if strings.Contains(got, key) || !strings.Contains(got, "c4bb") || !strings.Contains(got, "machine-monitor hub --show-key") {
			t.Errorf("%s: %q", name, got)
		}
	}
}
