package client

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

	"github.com/ContractorKeith/machine-monitor/internal/config"
	"github.com/ContractorKeith/machine-monitor/internal/hub"
)

type throttledListener struct {
	net.Listener
	rate    int
	written *atomic.Int64
}

func (l throttledListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &throttledConn{Conn: c, rate: l.rate, written: l.written}, nil
}

type throttledConn struct {
	net.Conn
	rate     int
	mu       sync.Mutex
	deadline time.Time
	sent     int
	start    time.Time
	written  *atomic.Int64
}

func (c *throttledConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadline = t
	c.mu.Unlock()
	return c.Conn.SetWriteDeadline(t)
}

func (c *throttledConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadline = t
	c.mu.Unlock()
	return c.Conn.SetDeadline(t)
}

func (c *throttledConn) Write(b []byte) (int, error) {
	const chunk = 4 << 10
	n := 0
	for n < len(b) {
		c.mu.Lock()
		if c.start.IsZero() {
			c.start = time.Now()
		}
		due := c.start.Add(time.Duration(float64(c.sent) / float64(c.rate) * float64(time.Second)))
		dl := c.deadline
		c.mu.Unlock()
		if wait := time.Until(due); wait > 0 {
			if !dl.IsZero() && due.After(dl) {
				time.Sleep(max(0, time.Until(dl)))
				return n, os.ErrDeadlineExceeded
			}
			time.Sleep(wait)
		}
		end := min(n+chunk, len(b))
		w, err := c.Conn.Write(b[n:end])
		n += w
		c.mu.Lock()
		c.sent += w
		c.mu.Unlock()
		c.written.Add(int64(w))
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func TestSlowLink250(t *testing.T) {
	if raceDetector {
		t.Skip("timing test; the race detector slows everything several times over")
	}
	if testing.Short() {
		t.Skip("runs for 40 s")
	}
	const (
		devices   = hub.MaxDevices
		interval  = 2 * time.Second
		connectBy = 15 * time.Second
		heapMax   = 200 << 20
	)
	duration, rate := 40*time.Second, 500<<10
	if s := os.Getenv("MM_SLOWLINK_RATE"); s != "" {
		if _, err := fmt.Sscan(s, &rate); err != nil {
			t.Fatal(err)
		}
	}
	if s := os.Getenv("MM_SLOWLINK"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			t.Fatal(err)
		}
		duration = d
	}

	var fake atomic.Int64
	clock := func() time.Time {
		if ms := fake.Load(); ms != 0 {
			return time.UnixMilli(ms)
		}
		return time.Now()
	}
	h, err := hub.New(hub.Options{Paths: config.At(t.TempDir()), Hostname: "slowhub", Location: time.UTC, Now: clock, Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(h.Handler())
	var wire atomic.Int64
	srv.Listener = throttledListener{Listener: ln, rate: rate, written: &wire}
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
		r["uptime"] = n
		b, _ := json.Marshal(r)
		return string(b)
	}
	handler := h.Handler()
	start := time.Now().Add(-hub.LiveHistory)
	for n := range int(hub.LiveHistory / interval) {
		fake.Store(start.Add(time.Duration(n) * interval).UnixMilli())
		for i := range devices {
			req := httptest.NewRequest("POST", srv.URL+"/api/report", strings.NewReader(body(i, 0)))
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

	for _, path := range []string{"/api/devices", "/api/devices?compact=1"} {
		req := httptest.NewRequest("GET", srv.URL+path, nil)
		req.RemoteAddr = "127.0.0.1:1"
		req.Header.Set("Authorization", "Bearer "+h.Token())
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		t.Logf("snapshot %s: %d bytes", path, rec.Body.Len())
	}

	loadCtx, stopLoad := context.WithCancel(ctx)
	defer stopLoad()
	var wg sync.WaitGroup
	defer wg.Wait()
	defer stopLoad()
	var round atomic.Int64
	for i := range devices {
		wg.Go(func() {
			first := time.NewTimer(time.Duration(i) * interval / devices)
			defer first.Stop()
			select {
			case <-first.C:
			case <-loadCtx.Done():
				return
			}
			tick := time.NewTicker(interval)
			defer tick.Stop()
			for {
				n := int(round.Load())
				req := httptest.NewRequest("POST", srv.URL+"/api/report", strings.NewReader(body(i, n)))
				req.RemoteAddr = "127.0.0.1:1"
				req.Header.Set("Content-Type", "application/json")
				handler.ServeHTTP(httptest.NewRecorder(), req)
				select {
				case <-tick.C:
				case <-loadCtx.Done():
					return
				}
			}
		})
	}
	began := time.Now()
	go func() {
		for loadCtx.Err() == nil {
			round.Store(int64(time.Since(began) / time.Second))
			time.Sleep(100 * time.Millisecond)
		}
	}()

	wire.Store(0)
	c, err := New(srv.URL, h.Token())
	if err != nil {
		t.Fatal(err)
	}

	c.stream.Transport.(*http.Transport).DisableCompression = os.Getenv("MM_SLOWLINK_GZIP") != "1"
	sctx, stopStream := context.WithCancel(ctx)
	defer stopStream()
	evs := c.Stream(sctx)
	var (
		connects, drops int
		lastDrop        error
		snapAt          time.Duration
		uptime          = map[string]float64{}
		peak            uint64
		ms              runtime.MemStats
	)
	memTick := time.NewTicker(250 * time.Millisecond)
	defer memTick.Stop()
	end := time.After(duration)
loop:
	for {
		select {
		case <-end:
			break loop
		case <-memTick.C:
			runtime.ReadMemStats(&ms)
			peak = max(peak, ms.HeapAlloc)
		case ev := <-evs:
			switch ev.Type {
			case EventConnected:
				connects++
			case EventDisconnected:
				drops++
				lastDrop = ev.Err
			case EventSnapshot:
				if snapAt == 0 {
					snapAt = time.Since(began)
				}
				for _, d := range ev.Devices {
					if d.Latest != nil && d.Latest.Uptime != nil {
						uptime[d.ID] = *d.Latest.Uptime
					}
				}
			case EventReport:
				if ev.Device != nil && ev.Device.Latest != nil && ev.Device.Latest.Uptime != nil {
					uptime[ev.ID] = *ev.Device.Latest.Uptime
				}
			}
		}
	}
	now := float64(time.Since(began) / time.Second)
	t.Logf("%d KB on the wire in %v: %d KB/s of the link's %d KB/s", wire.Load()>>10, duration, wire.Load()>>10/int64(duration/time.Second), rate>>10)
	t.Logf("first snapshot after %v; %d connections, %d drops (last: %v); peak heap %d MB",
		snapAt.Round(time.Millisecond), connects, drops, lastDrop, peak>>20)
	if snapAt == 0 || snapAt > connectBy {
		t.Errorf("the first snapshot arrived after %v; want within %v", snapAt, connectBy)
	}
	if connects != 1 || drops != 0 {
		t.Errorf("the stream connected %d times and dropped %d times (last: %v); want one connection", connects, drops, lastDrop)
	}
	if peak > heapMax {
		t.Errorf("peak heap %d MB, want under %d MB", peak>>20, heapMax>>20)
	}
	behind := 0
	for i := range devices {
		if u, ok := uptime[fmt.Sprintf("load-%03d", i)]; !ok || u < now-10 {
			behind++
		}
	}
	if behind > 0 {
		t.Errorf("%d of %d devices are more than 10 s behind at the end", behind, devices)
	}
}
