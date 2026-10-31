package hub

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/config"
	"github.com/ContractorKeith/machine-monitor/internal/discovery"
	"github.com/ContractorKeith/machine-monitor/internal/tailscale"
)

func strp(s string) *string { return &s }

// fakeTailscale is a Tailscale reader that counts its calls.
func fakeTailscale(calls *atomic.Int32) func(context.Context, int) tailscale.Status {
	return func(_ context.Context, port int) tailscale.Status {
		calls.Add(1)
		return tailscale.Status{Installed: true, Running: true, State: strp("Running"), IP: strp("100.101.1.2"),
			DNSName: strp("nas.tail1.ts.net"), HTTPSURL: strp("https://nas.tail1.ts.net"),
			URLs: []string{"https://nas.tail1.ts.net", "http://100.101.1.2:7777"}}
	}
}

func TestRemoteStatus(t *testing.T) {
	var calls atomic.Int32
	th := openTestHub(t, config.At(t.TempDir()), &clock{t: time.Unix(1e9, 0)},
		func(o *Options) { o.Tailscale = fakeTailscale(&calls) })

	// The MagicDNS name isn't known until Tailscale has been read.
	if w := th.req("GET", "/api/devices", th.Token(), "", "Host", "nas.tail1.ts.net"); w.Code != 421 {
		t.Fatalf("unknown tailnet name before a read: %d", w.Code)
	}
	if w := th.req("GET", "/api/remote", "", ""); w.Code != 401 {
		t.Fatalf("without key: %d", w.Code)
	}
	w := th.keyed("GET", "/api/remote", "")
	if w.Code != 200 {
		t.Fatalf("remote: %d %s", w.Code, w.Body)
	}
	got := decode[map[string]any](t, w)
	for k, want := range map[string]any{"installed": true, "running": true, "state": "Running", "ip": "100.101.1.2",
		"dnsName": "nas.tail1.ts.net", "httpsUrl": "https://nas.tail1.ts.net", "port": float64(7777),
		"canConfigure": false, "tailnet": nil} {
		if v, ok := got[k]; !ok || v != want {
			t.Errorf("%s = %v (present %v), want %v", k, v, ok, want)
		}
	}
	if p, _ := got["platform"].(string); p == "" || p == "windows" {
		t.Errorf("platform = %q, want Node's name", p)
	}

	// Cached for 30 s, unless ?fresh.
	th.keyed("GET", "/api/remote", "")
	if n := calls.Load(); n != 1 {
		t.Errorf("%d reads, want 1 (cached)", n)
	}
	th.keyed("GET", "/api/remote?fresh", "")
	if n := calls.Load(); n != 2 {
		t.Errorf("%d reads, want 2 (fresh)", n)
	}

	// The status feeds the Host check, /api/info and alert links, as upstream.
	if w := th.req("GET", "/api/devices", th.Token(), "", "Host", "nas.tail1.ts.net"); w.Code != 200 {
		t.Errorf("tailnet name after a read: %d", w.Code)
	}
	info := decode[struct {
		Remote struct{ URLs []string } `json:"remote"`
	}](t, th.keyed("GET", "/api/info", ""))
	if strings.Join(info.Remote.URLs, " ") != "https://nas.tail1.ts.net http://100.101.1.2:7777" {
		t.Errorf("info remote urls = %v", info.Remote.URLs)
	}
	if u := th.preferredURL(); u != "https://nas.tail1.ts.net" {
		t.Errorf("preferredURL = %q", u)
	}
}

func TestRemoteWithoutTailscale(t *testing.T) {
	th := newTestHub(t)
	got := decode[map[string]any](t, th.keyed("GET", "/api/remote", ""))
	if got["installed"] != false || got["urls"] == nil || len(got["urls"].([]any)) != 0 {
		t.Errorf("remote = %v", got)
	}
}

// Run refreshes Tailscale in the background, so the first request needn't wait on the CLI.
func TestRunReadsTailscale(t *testing.T) {
	var calls atomic.Int32
	th := openTestHub(t, config.At(t.TempDir()), &clock{t: time.Unix(1e9, 0)},
		func(o *Options) { o.Tailscale = fakeTailscale(&calls) })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { th.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	for deadline := time.Now().Add(5 * time.Second); calls.Load() == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("Run never read Tailscale")
		}
	}
}

// probeOnce sends the discovery probe to addr and returns the reply, or nil after wait.
func probeOnce(t *testing.T, addr string, wait time.Duration) map[string]any {
	t.Helper()
	c, err := net.Dial("udp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte(discovery.Probe))
	c.SetReadDeadline(time.Now().Add(wait))
	buf := make([]byte, 1024)
	n, err := c.Read(buf)
	if err != nil {
		return nil
	}
	var m map[string]any
	json.Unmarshal(buf[:n], &m)
	return m
}

func TestRunAnswersDiscovery(t *testing.T) {
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0") // a free port
	if err != nil {
		t.Fatal(err)
	}
	addr := pc.LocalAddr().String()
	pc.Close()
	th := openTestHub(t, config.At(t.TempDir()), &clock{t: time.Unix(1e9, 0)},
		func(o *Options) { o.Discovery, o.Port = addr, 17777 })

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { th.Run(ctx) })
	var reply map[string]any
	for deadline := time.Now().Add(5 * time.Second); reply == nil && time.Now().Before(deadline); {
		reply = probeOnce(t, addr, 200*time.Millisecond)
	}
	if reply["app"] != "device-monitor" || reply["name"] != "testbox" || reply["port"] != float64(17777) ||
		reply["impl"] != "machine-monitor" {
		t.Fatalf("reply = %v", reply)
	}
	cancel()
	wg.Wait()
	if r := probeOnce(t, addr, 300*time.Millisecond); r != nil {
		t.Errorf("still answering after Run returned: %v", r)
	}
}
