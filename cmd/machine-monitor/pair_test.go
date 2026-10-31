package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
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

// A program squatting on the port pair targets, relaying to the real hub, never gets the
// key: not an HTTP relay that forwards every request, nor a raw TCP relay.
func TestPairKeyNotSentToRelay(t *testing.T) {
	h, srv, _ := pairHub(t)
	target, _ := url.Parse(srv.URL)
	var mu sync.Mutex
	var seen []string // what reached the squatter

	httpRelay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.String()+" "+r.Header.Get("Authorization"))
		mu.Unlock()
		httputil.NewSingleHostReverseProxy(target).ServeHTTP(w, r)
	}))
	t.Cleanup(func() { httpRelay.CloseClientConnections(); httpRelay.Close() })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				up, err := net.Dial("tcp", target.Host)
				if err != nil {
					return
				}
				defer up.Close()
				go io.Copy(up, io.TeeReader(c, writerFunc(func(p []byte) {
					mu.Lock()
					seen = append(seen, string(p))
					mu.Unlock()
				})))
				io.Copy(c, up)
			}()
		}
	}()

	t.Cleanup(srv.CloseClientConnections) // a pair that wrongly got through holds a stream
	leaked := func() string {
		mu.Lock()
		defer mu.Unlock()
		for _, s := range seen {
			if strings.Contains(s, h.Token()) {
				return s
			}
		}
		return ""
	}
	for name, addr := range map[string]string{"http relay": httpRelay.Listener.Addr().String(), "tcp relay": ln.Addr().String()} {
		_, port, _ := net.SplitHostPort(addr)
		out, errOut, rc := startPair(port)
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			if s := leaked(); s != "" {
				t.Fatalf("%s: the key reached the relay: %.200q", name, s)
			}
			select {
			case c := <-rc:
				if c != 1 || !strings.Contains(errOut.String(), "can't prove it is the hub") {
					t.Errorf("%s: exit %d, stderr %q", name, c, errOut.String())
				}
			default:
				if time.Now().After(deadline) {
					t.Fatalf("%s: pair still running: stdout %q", name, out.String())
				}
				continue
			}
			break
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("pair never reached the relays")
	}
}

type writerFunc func(p []byte)

func (f writerFunc) Write(p []byte) (int, error) { f(p); return len(p), nil }

// Two pair runs at once: the second says a code is already active instead of quietly
// killing the first one's code. --replace ends the first, which then says so.
func TestPairOneCodeAtATime(t *testing.T) {
	_, srv, port := pairHub(t)
	out1, err1, rc1 := startPair(port)
	for deadline := time.Now().Add(10 * time.Second); !strings.Contains(out1.String(), "Pairing code"); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("no code: stdout %q, stderr %q", out1.String(), err1.String())
		}
	}
	var out, errOut bytes.Buffer
	if c := run([]string{"pair", "--port", port}, &out, &errOut); c != 1 || !strings.Contains(errOut.String(), "already active") ||
		!strings.Contains(errOut.String(), "--replace") || strings.Contains(out.String(), "Pairing code") {
		t.Fatalf("second pair: exit %d, stdout %q, stderr %q", c, out.String(), errOut.String())
	}
	out3, err3, rc3 := &syncBuffer{}, &syncBuffer{}, make(chan int, 1)
	go func() { rc3 <- run([]string{"pair", "--port", port, "--replace"}, out3, err3) }()
	if c := waitExit(t, rc1, err1); c != 1 || !strings.Contains(out1.String(), "no longer works") {
		t.Fatalf("replaced pair: exit %d, stdout %q", c, out1.String())
	}
	for deadline := time.Now().Add(10 * time.Second); !strings.Contains(out3.String(), "Pairing code"); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("--replace: no code: stdout %q, stderr %q", out3.String(), err3.String())
		}
	}
	m := regexp.MustCompile(`Pairing code: (\d{3}) (\d{3})`).FindStringSubmatch(out3.String())
	resp, err := http.Post(srv.URL+"/api/pair", "application/json", strings.NewReader(fmt.Sprintf(`{"code":"%s%s"}`, m[1], m[2])))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if c := waitExit(t, rc3, err3); c != 0 || !strings.Contains(out3.String(), "Paired") {
		t.Fatalf("--replace pair: exit %d, stdout %q", c, out3.String())
	}
}

// The banner's pair hint names the port when it isn't the default.
func TestPairCommandHint(t *testing.T) {
	if got := pairCommand(hub.DefaultPort); got != "machine-monitor pair" {
		t.Errorf("default port: %q", got)
	}
	if got := pairCommand(17800); got != "machine-monitor pair --port 17800" {
		t.Errorf("other port: %q", got)
	}
}

// A hub from before connProof (still running after `update`) proves the key the old way.
// pair still doesn't send the key, and says the hub must be restarted.
func TestPairRefusesOlderHub(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(config.DirEnv, dir)
	h, err := hub.New(hub.Options{Paths: config.At(dir), Hostname: "testhub", Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	var mu sync.Mutex
	keyed := 0
	inner := h.Handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			mu.Lock()
			keyed++
			mu.Unlock()
		}
		rec := httptest.NewRecorder()
		inner.ServeHTTP(rec, r)
		var body map[string]any
		if r.URL.Path == "/api/info" && json.Unmarshal(rec.Body.Bytes(), &body) == nil {
			delete(body, "connProof")
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(body)
			return
		}
		w.WriteHeader(rec.Code)
		w.Write(rec.Body.Bytes())
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	var out, errOut bytes.Buffer
	if c := run([]string{"pair", "--port", u.Port()}, &out, &errOut); c != 1 || !strings.Contains(errOut.String(), "predates this pair command") ||
		!strings.Contains(errOut.String(), "start it again") || strings.Contains(errOut.String(), "can't prove") {
		t.Fatalf("exit %d, stderr %q", c, errOut.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if keyed != 0 {
		t.Fatalf("%d keyed requests reached the older hub", keyed)
	}
}
