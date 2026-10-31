package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/ContractorKeith/machine-monitor/internal/client"
	"github.com/ContractorKeith/machine-monitor/internal/config"
	"github.com/ContractorKeith/machine-monitor/internal/hub"
)

// countingHub wraps the real client and counts the stream's connections.
type countingHub struct {
	*client.Client
	connects, drops atomic.Int32
	lastDrop        atomic.Value // error
}

func (c *countingHub) Stream(ctx context.Context) <-chan client.Event {
	in := c.Client.Stream(ctx)
	out := make(chan client.Event)
	go func() {
		defer close(out)
		for ev := range in {
			switch ev.Type {
			case client.EventConnected:
				c.connects.Add(1)
			case client.EventDisconnected:
				c.drops.Add(1)
				if ev.Err != nil {
					c.lastDrop.Store(ev.Err)
				}
			}
			select {
			case out <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

// program drives a model the way Bubble Tea does: commands run on goroutines, their
// messages go through Update one at a time, and every update is followed by a full frame.
type program struct {
	mu   sync.Mutex
	m    model
	msgs chan tea.Msg
}

func (p *program) exec(ctx context.Context, cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	go func() {
		msg := cmd()
		if b, ok := msg.(tea.BatchMsg); ok {
			for _, c := range b {
				p.exec(ctx, c)
			}
			return
		}
		if msg == nil {
			return
		}
		select {
		case p.msgs <- msg:
		case <-ctx.Done():
		}
	}()
}

func (p *program) run(ctx context.Context) {
	p.exec(ctx, p.m.Init())
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-p.msgs:
			p.mu.Lock()
			next, cmd := p.m.Update(msg)
			p.m = next.(model)
			_ = p.m.render()
			p.mu.Unlock()
			p.exec(ctx, cmd)
		}
	}
}

// TestHub250: a connected TUI follows a real hub with 250 busy devices (PLAN MaxDevices),
// each reporting every 2 s, without falling behind: one connection for the whole run (no
// drop-and-resnapshot loop), every device on the dashboard with a recent report, and the
// heap bounded. Skeptic report H3: before the fix the TUI redrew every card for every
// report, its 256-event buffer overflowed within seconds, and each reconnect fetched
// another full snapshot.
func TestHub250(t *testing.T) {
	if raceDetector {
		t.Skip("timing test; the race detector slows everything several times over")
	}
	if testing.Short() {
		t.Skip("runs for 15 s")
	}
	const (
		devices  = hub.MaxDevices
		interval = 2 * time.Second
		duration = 15 * time.Second
		heapMax  = 300 << 20 // hub and TUI together: about 140 MB; 570 MB here before the fix
	)
	// The hub's clock replays the last ten minutes while the history is filled in, then
	// runs in real time.
	var fake atomic.Int64 // Unix ms; 0 is the real clock
	clock := func() time.Time {
		if ms := fake.Load(); ms != 0 {
			return time.UnixMilli(ms)
		}
		return time.Now()
	}
	paths := config.At(t.TempDir())
	h, err := hub.New(hub.Options{Paths: paths, Hostname: "hub250", Location: time.UTC, Now: clock, Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(h.Handler())
	srv.Listener = ln
	srv.Start()
	ctx, cancel := context.WithCancel(context.Background())
	hubDone := make(chan struct{})
	go func() { h.Run(ctx); close(hubDone) }()
	t.Cleanup(func() {
		cancel()
		srv.CloseClientConnections()
		<-hubDone
		srv.Close()
		h.Close()
	})

	// The recorded upstream agent report, one per device.
	raw, err := os.ReadFile("../report/testdata/linux-agent.sh.json")
	if err != nil {
		t.Fatal(err)
	}
	var base map[string]any
	if err := json.Unmarshal(raw, &base); err != nil {
		t.Fatal(err)
	}
	body := func(i, n int) string {
		r := make(map[string]any, len(base))
		for k, v := range base {
			r[k] = v
		}
		r["token"], r["id"], r["hostname"] = h.Token(), fmt.Sprintf("load-%03d", i), fmt.Sprintf("load%03d", i)
		r["uptime"] = 1000 + n
		b, _ := json.Marshal(r)
		return string(b)
	}
	post := func(i, n int) error {
		resp, err := http.Post(srv.URL+"/api/report", "application/json", strings.NewReader(body(i, n)))
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("report %d: HTTP %d", i, resp.StatusCode)
		}
		return nil
	}
	// Ten minutes of history for every device (hub.LiveHistory), so the snapshot the TUI
	// gets on connecting is as big as a busy hub's: about 20 MB.
	start := time.Now().Add(-hub.LiveHistory)
	handler := h.Handler()
	for n := range int(hub.LiveHistory / interval) {
		fake.Store(start.Add(time.Duration(n) * interval).UnixMilli())
		for i := range devices {
			req := httptest.NewRequest("POST", srv.URL+"/api/report", strings.NewReader(body(i, -1000)))
			req.RemoteAddr = "127.0.0.1:1"
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != 200 {
				t.Fatalf("prefill report: %d %s", rec.Code, rec.Body)
			}
		}
	}
	fake.Store(0)
	var sent atomic.Int64
	loadCtx, stopLoad := context.WithTimeout(ctx, duration)
	defer stopLoad()
	var wg sync.WaitGroup
	for i := range devices {
		wg.Go(func() {
			// Spread the devices over the interval, as real agents are.
			first := time.NewTimer(time.Duration(i) * interval / devices)
			defer first.Stop()
			select {
			case <-first.C:
			case <-loadCtx.Done():
				return
			}
			tick := time.NewTicker(interval)
			defer tick.Stop()
			for n := 1; ; n++ {
				if err := post(i, n); err == nil {
					sent.Add(1)
				}
				select {
				case <-tick.C:
				case <-loadCtx.Done():
					return
				}
			}
		})
	}

	c, err := client.New(srv.URL, h.Token())
	if err != nil {
		t.Fatal(err)
	}
	ch := &countingHub{Client: c}
	m := New(Options{Hub: ch}).(model)
	m.ctx = ctx
	next, _ := m.Update(tea.WindowSizeMsg{Width: 200, Height: 50})
	p := &program{m: next.(model), msgs: make(chan tea.Msg, 64)}
	tuiCtx, stopTUI := context.WithCancel(ctx)
	defer stopTUI()
	go p.run(tuiCtx)

	var peak uint64
	var ms runtime.MemStats
	for deadline := time.Now().Add(duration); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
		runtime.ReadMemStats(&ms)
		peak = max(peak, ms.HeapAlloc)
	}
	wg.Wait()
	time.Sleep(interval) // let the last reports arrive

	p.mu.Lock()
	got := p.m
	p.mu.Unlock()
	stopTUI()

	t.Logf("%d reports sent; %d connections, %d drops; peak heap %d MB", sent.Load(), ch.connects.Load(), ch.drops.Load(), peak>>20)
	if n := ch.connects.Load(); n != 1 || ch.drops.Load() != 0 {
		t.Errorf("the stream connected %d times and dropped %d times (last: %v); want one connection", n, ch.drops.Load(), ch.lastDrop.Load())
	}
	if peak > heapMax {
		t.Errorf("peak heap %d MB, want under %d MB", peak>>20, heapMax>>20)
	}
	if len(got.devs) != devices {
		t.Fatalf("dashboard has %d devices, want %d", len(got.devs), devices)
	}
	stale := 0
	for _, d := range got.devs {
		if d.last == nil || d.last.Uptime == nil || *d.last.Uptime < 1000+float64(duration/interval)-2 {
			stale++
		}
	}
	if stale > 0 {
		t.Errorf("%d of %d devices are behind on their reports: the TUI fell behind the hub", stale, devices)
	}
}
