package client

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/config"
)

// TestStreamIsCompact (B1): the client opts in to our hub's compact stream, so its
// snapshot carries each device's latest report but no history.
func TestStreamIsCompact(t *testing.T) {
	th := newTestHub(t, config.At(t.TempDir()), "")
	th.report("dev-a", nil)
	ev := stream(t, th.client(th.h.Token()), fastConfig())
	s := ev.next(EventSnapshot)
	if len(s.Devices) != 1 || s.Devices[0].Latest == nil || s.Devices[0].History != nil {
		t.Fatalf("snapshot = %+v; want the latest report and no history", s.Devices)
	}
	th.report("dev-a", nil)
	if r := ev.next(EventReport); r.ID != "dev-a" || r.Sample == nil {
		t.Errorf("report = %+v", r)
	}
}

// TestIdleCountsBytes (B1): a snapshot that arrives slowly, as one long line, isn't taken
// for silence. The watchdog used to reset only per complete line, so a big snapshot over a
// slow link was cancelled part-way, every time.
func TestIdleCountsBytes(t *testing.T) {
	c := fakeHub(t, "k", func(ctx context.Context, w http.ResponseWriter, f http.Flusher) {
		io.WriteString(w, `data: {"type":"snapshot","devices":[`)
		for i := range 20 { // 20 × 25 ms: five idle timeouts, with bytes all along
			if i > 0 {
				io.WriteString(w, ",")
			}
			io.WriteString(w, `{"id":"dev-`+strings.Repeat("x", i+1)+`"}`)
			f.Flush()
			select {
			case <-ctx.Done():
				return
			case <-time.After(25 * time.Millisecond):
			}
		}
		io.WriteString(w, "]}\n\n")
		f.Flush()
		<-ctx.Done()
	})
	cfg := fastConfig()
	cfg.idle = 100 * time.Millisecond
	ev := stream(t, c, cfg)
	ev.next(EventConnected)
	for {
		select {
		case e := <-ev.ch:
			switch e.Type {
			case EventSnapshot:
				if len(e.Devices) != 20 {
					t.Errorf("snapshot has %d devices, want 20", len(e.Devices))
				}
				return
			case EventDisconnected:
				t.Fatalf("dropped mid-snapshot: %v", e.Err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("no snapshot")
		}
	}
}
