package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/config"
	"github.com/ContractorKeith/machine-monitor/internal/hub"
)

// slowReadConn reads at most rate bytes a second from a real TCP connection whose kernel
// receive buffer is small, as a slow link does: what the link can't carry yet waits in the
// hub's send queue, not in the client's kernel.
type slowReadConn struct {
	net.Conn
	rate  int
	start time.Time
	got   int
	read  *atomic.Int64
}

func (c *slowReadConn) Read(b []byte) (int, error) {
	if c.start.IsZero() {
		c.start = time.Now()
	}
	if wait := time.Until(c.start.Add(time.Duration(float64(c.got) / float64(c.rate) * float64(time.Second)))); wait > 0 {
		time.Sleep(wait)
	}
	n, err := c.Conn.Read(b[:min(len(b), 2<<10)])
	c.got += n
	c.read.Add(int64(n))
	return n, err
}

// TestSlowStreamStaysFresh is skeptic report N1: over a link slower than the compact
// stream's own output, the hub's writes returned as soon as the kernel took the bytes, so
// megabytes queued in its send buffer, nothing coalesced, the data fell minutes behind and
// the hub finally dropped the stream. Through a real socket whose reader is throttled, every
// report must arrive within a bound of when the hub received it, and the stream must stay
// up on one connection.
//
// It runs for 45 s; MM_SLOWSTREAM=3m runs it for longer, and MM_SLOWSTREAM_RATE=N over a
// link of N bytes a second.
func TestSlowStreamStaysFresh(t *testing.T) {
	if raceDetector {
		t.Skip("timing test; the race detector slows everything several times over")
	}
	if testing.Short() {
		t.Skip("runs for 45 s")
	}
	const (
		devices  = hub.MaxDevices
		interval = 2 * time.Second
		warmUp   = 15 * time.Second // connect and the first snapshot
		maxAge   = 10 * time.Second
	)
	// 200 KB/s is well under the stream's own output here (about 340 KB/s uncompressed), as
	// 20 KB/s is under its 33 KB/s with gzip in the field (the report's throttle).
	duration, rate := 45*time.Second, 200<<10
	if s := os.Getenv("MM_SLOWSTREAM_RATE"); s != "" {
		if _, err := fmt.Sscan(s, &rate); err != nil {
			t.Fatal(err)
		}
	}
	if s := os.Getenv("MM_SLOWSTREAM"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			t.Fatal(err)
		}
		duration = d
	}

	h, err := hub.New(hub.Options{Paths: config.At(t.TempDir()), Hostname: "slowhub", Location: time.UTC, Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h.Handler())
	ctx, cancel := context.WithCancel(context.Background())
	hubDone := make(chan struct{})
	go func() { h.Run(ctx); close(hubDone) }()
	t.Cleanup(func() {
		cancel()
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
	handler := h.Handler()
	report := func(i, n int) {
		r := make(map[string]any, len(base))
		for k, v := range base {
			r[k] = v
		}
		r["token"], r["id"], r["hostname"] = h.Token(), fmt.Sprintf("load-%03d", i), fmt.Sprintf("load%03d", i)
		r["uptime"] = n
		b, _ := json.Marshal(r)
		req := httptest.NewRequest("POST", srv.URL+"/api/report", strings.NewReader(string(b)))
		req.RemoteAddr = "127.0.0.1:1"
		req.Header.Set("Content-Type", "application/json")
		handler.ServeHTTP(httptest.NewRecorder(), req)
	}
	// Every device reports every interval, through the handler: only the TUI's link is slow.
	loadCtx, stopLoad := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer wg.Wait()
	defer stopLoad()
	for i := range devices {
		report(i, 0)
		wg.Go(func() {
			tick := time.NewTicker(interval)
			defer tick.Stop()
			for n := 1; ; n++ {
				select {
				case <-tick.C:
					report(i, n)
				case <-loadCtx.Done():
					return
				}
			}
		})
	}

	var wire atomic.Int64
	c, err := New(srv.URL, h.Token())
	if err != nil {
		t.Fatal(err)
	}
	tr := c.stream.Transport.(*http.Transport)
	// Uncompressed: the test's reports are near copies of one another, so gzip would hide
	// how much the link carries.
	tr.DisableCompression = true
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		conn.(*net.TCPConn).SetReadBuffer(16 << 10)
		return &slowReadConn{Conn: conn, rate: rate, read: &wire}, nil
	}
	sctx, stopStream := context.WithCancel(ctx)
	defer stopStream()
	evs := c.Stream(sctx)
	began := time.Now()
	var (
		connects, drops int
		lastDrop        error
		worst           time.Duration
		worstAt         time.Duration
		reports         int
		seen            = map[string]int64{} // device -> newest LastSeen (Unix ms)
	)
	end := time.After(duration)
loop:
	for {
		select {
		case <-end:
			break loop
		case ev := <-evs:
			switch ev.Type {
			case EventConnected:
				connects++
			case EventDisconnected:
				drops++
				lastDrop = ev.Err
			case EventSnapshot:
				for _, d := range ev.Devices {
					seen[d.ID] = d.LastSeen
				}
			case EventReport:
				if ev.Device == nil {
					continue
				}
				reports++
				seen[ev.ID] = ev.Device.LastSeen
				if at := time.Since(began); at > warmUp {
					if age := time.Since(time.UnixMilli(ev.Device.LastSeen)); age > worst {
						worst, worstAt = age, at
					}
				}
			}
		}
	}
	t.Logf("%d KB read in %v over a %d KB/s link; %d reports; %d connections, %d drops (last: %v); oldest report after warm-up %v at %v",
		wire.Load()>>10, duration, rate>>10, reports, connects, drops, lastDrop, worst.Round(100*time.Millisecond), worstAt.Round(time.Second))
	if connects != 1 || drops != 0 {
		t.Errorf("the stream connected %d times and dropped %d times (last: %v); want one connection", connects, drops, lastDrop)
	}
	if worst > maxAge {
		t.Errorf("a report arrived %v after the hub received it (at %v); want under %v", worst.Round(100*time.Millisecond), worstAt.Round(time.Second), maxAge)
	}
	behind := 0
	for i := range devices {
		if ms, ok := seen[fmt.Sprintf("load-%03d", i)]; !ok || time.Since(time.UnixMilli(ms)) > maxAge {
			behind++
		}
	}
	if behind > 0 {
		t.Errorf("%d of %d devices are more than %v behind at the end", behind, devices, maxAge)
	}
}
