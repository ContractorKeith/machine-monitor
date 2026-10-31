package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/config"
	"github.com/ContractorKeith/machine-monitor/internal/hub"
	"github.com/ContractorKeith/machine-monitor/internal/report"
)

// fakeCollector returns a minimal report for one device.
type fakeCollector struct{ id string }

func (c fakeCollector) Collect(context.Context) (*report.Report, error) {
	return &report.Report{ID: c.id, Hostname: report.Ptr("box-" + c.id), Platform: "linux",
		CPU: report.CPU{Usage: report.Ptr(12.5)}}, nil
}

func quiet(string, ...any) {}

// realHub serves a real internal/hub over httptest.
func realHub(t *testing.T) (*hub.Hub, *httptest.Server) {
	t.Helper()
	h, err := hub.New(hub.Options{Paths: config.At(t.TempDir()), Hostname: "testhub", Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(func() { srv.Close(); h.Close() })
	return h, srv
}

func keyed(t *testing.T, srv *httptest.Server, key, method, path string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestPairThenReportEndToEnd(t *testing.T) {
	h, srv := realHub(t)
	var code struct{ Code string }
	json.NewDecoder(keyed(t, srv, h.Token(), "POST", "/api/pair/code").Body).Decode(&code)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := NewHTTPClient()
	if _, err := Connect(ctx, c, srv.URL, wrong(code.Code)); err == nil {
		t.Fatal("wrong code paired")
	} else if pe := (*PairError)(nil); !errors.As(err, &pe) || !pe.Retry {
		t.Fatalf("wrong code error: %v", err)
	}
	key, err := Connect(ctx, c, srv.URL, code.Code)
	if err != nil {
		t.Fatal(err)
	}
	if key != h.Token() {
		t.Fatalf("key %q, want the hub's", key)
	}

	path := filepath.Join(t.TempDir(), "client.json")
	if err := SaveClient(path, Client{HubURL: srv.URL, Key: key}); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("client.json mode %v", fi.Mode().Perm())
	}
	cl, err := LoadClient(path)
	if err != nil || cl.HubURL != srv.URL || cl.Key != key {
		t.Fatalf("LoadClient = %+v, %v", cl, err)
	}

	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- Run(runCtx, Options{HubURL: cl.HubURL, Key: cl.Key, Collector: fakeCollector{"e2e-box"},
			Version: "9.9.9", Interval: 20 * time.Millisecond, Logf: t.Logf})
	}()
	for {
		var devs []struct {
			ID     string `json:"id"`
			Online bool   `json:"online"`
			Latest *struct {
				Agent string `json:"agent"`
			} `json:"latest"`
		}
		json.NewDecoder(keyed(t, srv, key, "GET", "/api/devices").Body).Decode(&devs)
		if len(devs) == 1 && devs[0].ID == "e2e-box" && devs[0].Online && devs[0].Latest != nil {
			if devs[0].Latest.Agent != "machine-monitor/9.9.9" {
				t.Errorf("agent = %q", devs[0].Latest.Agent)
			}
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("device never reported: %+v", devs)
		case <-time.After(20 * time.Millisecond):
		}
	}
	stop()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func wrong(code string) string {
	if code == "000000" {
		return "000001"
	}
	return "000000"
}

// fakeHub records every request and answers like a hub whose key is hubKey.
type fakeHub struct {
	t      *testing.T
	hubKey string // the key this hub proves (and hands out when pairing)
	procs  bool   // ask for processes in report replies
	down   bool   // answer every request with 503
	mu     sync.Mutex
	seen   []string // every request: "METHOD path auth body"
	bodies [][]byte // report bodies
}

func (f *fakeHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.seen = append(f.seen, r.Method+" "+r.URL.String()+" "+r.Header.Get("Authorization")+" "+string(b))
	down, procs := f.down, f.procs
	if r.URL.Path == "/api/report" {
		f.bodies = append(f.bodies, b)
	}
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if down {
		w.WriteHeader(503)
		return
	}
	switch r.URL.Path {
	case "/api/info":
		n := r.URL.Query().Get("nonce")
		json.NewEncoder(w).Encode(Info{App: "device-monitor", Locked: true, Proof: Proof(f.hubKey, n)})
	case "/api/pair":
		json.NewEncoder(w).Encode(map[string]string{"token": f.hubKey})
	case "/api/report":
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "procs": procs})
	default:
		w.WriteHeader(404)
	}
}

func (f *fakeHub) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

func (f *fakeHub) reports() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.bodies...)
}

func (f *fakeHub) set(fn func()) { f.mu.Lock(); fn(); f.mu.Unlock() }

func startFake(t *testing.T, f *fakeHub) string {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return srv.URL
}

// waitFor polls cond until it holds or 5 s pass.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

const ourKey = "0123456789abcdef01234567"

func TestFakeHubNeverGetsKey(t *testing.T) {
	f := &fakeHub{t: t, hubKey: "ffffffffffffffffffffffff"} // proves a different key
	u := startFake(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	var logs []string
	var lmu sync.Mutex
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{HubURL: u, Key: ourKey, Collector: fakeCollector{"x"}, Interval: time.Millisecond,
			MaxBackoff: 5 * time.Millisecond, Logf: func(format string, a ...any) {
				lmu.Lock()
				logs = append(logs, format)
				lmu.Unlock()
			}})
	}()
	waitFor(t, "a few proof attempts", func() bool { return len(f.requests()) >= 5 })
	cancel()
	<-done
	for _, r := range f.requests() {
		if !strings.HasPrefix(r, "GET /api/info?nonce=") || strings.Contains(r, ourKey) {
			t.Errorf("fake hub got %q", r)
		}
	}
	if len(f.reports()) != 0 {
		t.Error("a report was sent to a hub that failed the proof")
	}
	lmu.Lock()
	defer lmu.Unlock()
	if len(logs) != 1 { // the same failure is logged once
		t.Errorf("logs = %v", logs)
	}
}

func TestConnectRefusesHubThatCannotProveKey(t *testing.T) {
	// The server hands out one key when pairing but proves another: it made the key up.
	f := &fakeHub{t: t, hubKey: "ffffffffffffffffffffffff"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/pair" {
			json.NewEncoder(w).Encode(map[string]string{"token": ourKey})
			return
		}
		f.ServeHTTP(w, r)
	}))
	defer srv.Close()
	_, err := Connect(context.Background(), NewHTTPClient(), srv.URL, "123456")
	if !errors.Is(err, ErrBadProof) {
		t.Fatalf("Connect = %v, want ErrBadProof", err)
	}
	for _, r := range f.requests() {
		if strings.Contains(r, ourKey) {
			t.Errorf("the made-up key was sent back: %q", r)
		}
	}
}

func TestConnectRefusesNonHub(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/pair" {
			t.Error("pairing code sent to a server that is not a hub")
		}
		io.WriteString(w, `{"app":"something-else"}`)
	}))
	defer srv.Close()
	if _, err := Connect(context.Background(), NewHTTPClient(), srv.URL, "123456"); !errors.Is(err, ErrNotHub) {
		t.Fatalf("Connect = %v, want ErrNotHub", err)
	}
}

func TestProcsOnlyWhenAsked(t *testing.T) {
	f := &fakeHub{t: t, hubKey: ourKey}
	u := startFake(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	procs := func(context.Context) ([]report.Process, error) {
		var out []report.Process
		for i := range MaxProcesses + 30 {
			out = append(out, report.Process{PID: report.Ptr(float64(i)), Name: report.Ptr("p"), CPU: report.Ptr(float64(i))})
		}
		return out, nil
	}
	go Run(ctx, Options{HubURL: u, Key: ourKey, Collector: fakeCollector{"x"}, Procs: procs, Interval: time.Millisecond, Logf: quiet})

	type body struct {
		Token     string           `json:"token"`
		Processes []report.Process `json:"processes"`
	}
	decode := func(b []byte) body {
		var v body
		json.Unmarshal(b, &v)
		return v
	}
	waitFor(t, "two reports", func() bool { return len(f.reports()) >= 2 })
	for _, b := range f.reports() {
		if v := decode(b); v.Processes != nil || v.Token != ourKey {
			t.Fatalf("unasked report: %s", b)
		}
	}
	f.set(func() { f.procs = true })
	n := len(f.reports())
	waitFor(t, "a report with processes", func() bool {
		r := f.reports()
		return len(r) > n+1 && decode(r[len(r)-1]).Processes != nil
	})
	r := f.reports()
	p := decode(r[len(r)-1]).Processes
	if len(p) != MaxProcesses || *p[0].CPU != float64(MaxProcesses+29) {
		t.Fatalf("processes: %d rows, first cpu %v", len(p), *p[0].CPU)
	}
	f.set(func() { f.procs = false })
	n = len(f.reports())
	waitFor(t, "reports without processes again", func() bool {
		r := f.reports()
		return len(r) > n+1 && decode(r[len(r)-1]).Processes == nil
	})
}

func TestRecoversWhenHubComesBack(t *testing.T) {
	f := &fakeHub{t: t, hubKey: ourKey, down: true}
	u := startFake(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, Options{HubURL: u, Key: ourKey, Collector: fakeCollector{"x"}, Interval: time.Millisecond,
		MaxBackoff: 10 * time.Millisecond, Logf: quiet})
	waitFor(t, "tries while down", func() bool { return len(f.requests()) >= 3 })
	if len(f.reports()) != 0 {
		t.Fatal("report sent before the hub proved itself")
	}
	f.set(func() { f.down = false })
	waitFor(t, "reports after recovery", func() bool { return len(f.reports()) >= 2 })

	// A failed report means the next one proves the hub again first.
	f.set(func() { f.down = true })
	n := len(f.requests())
	waitFor(t, "a failed try", func() bool { return len(f.requests()) > n })
	f.set(func() { f.down = false; f.seen = nil })
	waitFor(t, "a report", func() bool { return len(f.requests()) >= 2 })
	if r := f.requests(); !strings.HasPrefix(r[0], "GET /api/info?nonce=") {
		t.Fatalf("after a failure the first request was %q", r[0])
	}
}

func TestBackoff(t *testing.T) {
	s := time.Second
	for n, want := range []time.Duration{1: 2 * s, 2: 4 * s, 3: 8 * s, 4: 16 * s, 5: 30 * s, 6: 30 * s} {
		if n == 0 {
			continue
		}
		if got := backoff(2*s, 30*s, n); got != want {
			t.Errorf("backoff(%d) = %v, want %v", n, got, want)
		}
	}
	if got := backoff(2*s, 30*s, 1000); got != 30*s {
		t.Errorf("backoff(1000) = %v", got)
	}
}

func TestNormalizeURL(t *testing.T) {
	for _, c := range []struct{ in, url, key string }{
		{"192.168.1.20", "http://192.168.1.20:7777", ""},
		{" hub.local:8000 ", "http://hub.local:8000", ""},
		{"http://hub/?key=abc123abc", "http://hub:7777", "abc123abc"},
		{"https://box.tail1.ts.net", "https://box.tail1.ts.net", ""},
		{"[fd12::5]", "http://[fd12::5]:7777", ""},
	} {
		u, k, err := NormalizeURL(c.in)
		if err != nil || u != c.url || k != c.key {
			t.Errorf("NormalizeURL(%q) = %q %q %v", c.in, u, k, err)
		}
	}
	for _, bad := range []string{"", "ftp://hub", "http://user:pw@hub", "http://"} {
		if _, _, err := NormalizeURL(bad); err == nil {
			t.Errorf("NormalizeURL(%q) accepted", bad)
		}
	}
}

func TestPairRejectsOddKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"token":"bad key\r\nX-Evil: 1"}`)
	}))
	defer srv.Close()
	if _, err := Pair(context.Background(), NewHTTPClient(), srv.URL, "1"); err == nil {
		t.Fatal("accepted a malformed key")
	}
}

func TestLoadClientMissing(t *testing.T) {
	if _, err := LoadClient(filepath.Join(t.TempDir(), "client.json")); !errors.Is(err, ErrNotPaired) {
		t.Fatalf("err = %v", err)
	}
}

func TestHubDownLoggedOnce(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	u := srv.URL
	srv.Close() // nothing listens: every try fails to dial
	var mu sync.Mutex
	var logs []string
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{HubURL: u, Key: ourKey, Collector: fakeCollector{"x"}, Interval: time.Millisecond,
			MaxBackoff: 2 * time.Millisecond, Logf: func(format string, a ...any) {
				mu.Lock()
				logs = append(logs, fmt.Sprintf(format, a...))
				mu.Unlock()
			}})
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if len(logs) != 1 || strings.Contains(logs[0], "nonce") {
		t.Errorf("logs = %q", logs)
	}
}

func TestJitter(t *testing.T) {
	d := time.Second
	seen := map[time.Duration]bool{}
	for range 200 {
		j := jittered(d)
		if j < d || j > d*12/10 {
			t.Fatalf("jittered(%v) = %v, want within [1s, 1.2s]", d, j)
		}
		seen[j] = true
	}
	if len(seen) < 2 {
		t.Error("jitter never varies")
	}
}

func TestPairRetrySignal(t *testing.T) {
	for _, c := range []struct {
		name, body string
		retry      bool
	}{
		{"field says retry", `{"error":"Falscher Code","retry":true,"triesLeft":3}`, true},
		{"field says dead", `{"error":"Wrong pairing code.","retry":false,"triesLeft":0}`, false},
		{"upstream wording", `{"error":"Wrong pairing code."}`, true},
		{"upstream dead", `{"error":"Wrong pairing code. Too many tries: get a new code from the hub."}`, false},
		{"upstream no code", `{"error":"No pairing code is active."}`, false},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(403)
			io.WriteString(w, c.body)
		}))
		_, err := Pair(context.Background(), NewHTTPClient(), srv.URL, "1")
		srv.Close()
		var pe *PairError
		if !errors.As(err, &pe) || pe.Retry != c.retry {
			t.Errorf("%s: err %v, want retry %v", c.name, err, c.retry)
		}
	}
}
