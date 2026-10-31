package hub

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/history"
)

func samplePoint(t float64) history.Point { return history.Point{T: t} }

func ftoa(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// TestCompactCoalesces (B1): a compact stream that isn't reading keeps only each device's
// newest report and the newest status and order, however many arrive, and is never
// dropped for it. An upstream-mode stream is dropped after streamBuffer events
// (TestSlowStreamDropped).
func TestCompactCoalesces(t *testing.T) {
	th := newTestHub(t)
	s := th.addStreamMode(true)
	defer th.releaseStream(s)
	for i := range 5000 {
		id := []string{"dev-1", "dev-2"}[i%2]
		th.broadcast(reportEvent{Type: "report", ID: id, Sample: samplePoint(float64(i))})
		th.broadcast(map[string]any{"type": "status", "online": map[string]bool{id: i%3 == 0}, "n": i})
	}
	th.broadcast(map[string]any{"type": "order", "order": map[string]int{"dev-1": 1}})
	select {
	case <-s.done:
		t.Fatal("compact stream dropped for falling behind")
	default:
	}
	th.smu.Lock()
	batch, resync := s.pend.take()
	th.smu.Unlock()
	if resync {
		t.Error("resync after coalescable events only")
	}
	var got []string
	for _, b := range batch {
		var ev struct {
			Type   string
			ID     string
			N      int
			Sample struct{ T float64 }
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(strings.TrimSpace(string(b)), "data: ")), &ev); err != nil {
			t.Fatal(err)
		}
		switch ev.Type {
		case "report":
			got = append(got, ev.ID+"@"+ftoa(ev.Sample.T))
		case "status":
			got = append(got, "status@"+ftoa(float64(ev.N)))
		default:
			got = append(got, ev.Type)
		}
	}
	want := "dev-1@4998 dev-2@4999 order status@4999"
	if strings.Join(got, " ") != want {
		t.Errorf("batch = %v, want %s", got, want)
	}
}

// TestCompactOrdering: events keep their order while reports coalesce: a report waits in
// its first slot (so it precedes the actionResult it carried), a removal drops the
// device's waiting report, and a later report that brings it back goes after the removal.
func TestCompactOrdering(t *testing.T) {
	th := newTestHub(t)
	s := th.addStreamMode(true)
	defer th.releaseStream(s)
	types := func() string {
		th.smu.Lock()
		batch, _ := s.pend.take()
		th.smu.Unlock()
		var out []string
		for _, b := range batch {
			var ev struct{ Type, ID string }
			json.Unmarshal([]byte(strings.TrimPrefix(strings.TrimSpace(string(b)), "data: ")), &ev)
			out = append(out, ev.Type+":"+ev.ID)
		}
		return strings.Join(out, " ")
	}
	th.broadcast(reportEvent{Type: "report", ID: "dev-1"})
	th.broadcast(map[string]any{"type": "removed", "id": "dev-1"})
	if got := types(); got != "removed:dev-1" {
		t.Errorf("report then removal: %q", got)
	}
	th.broadcast(map[string]any{"type": "removed", "id": "dev-1"})
	th.broadcast(reportEvent{Type: "report", ID: "dev-1"})
	if got := types(); got != "removed:dev-1 report:dev-1" {
		t.Errorf("removal then report: %q", got)
	}
	th.broadcast(reportEvent{Type: "report", ID: "dev-1"})
	th.broadcast(map[string]any{"type": "actionResult", "id": "dev-1"})
	th.broadcast(map[string]any{"type": "removed", "id": "dev-1"})
	th.broadcast(reportEvent{Type: "report", ID: "dev-1"})
	if got := types(); got != "actionResult:dev-1 removed:dev-1 report:dev-1" {
		t.Errorf("report, actionResult, removal, report: %q", got)
	}
	th.broadcast(reportEvent{Type: "report", ID: "dev-1"})
	th.broadcast(map[string]any{"type": "actionResult", "id": "dev-1"})
	th.broadcast(reportEvent{Type: "report", ID: "dev-1"})
	if got := types(); got != "report:dev-1 actionResult:dev-1" {
		t.Errorf("report, actionResult, report: %q", got)
	}
}

// TestCompactBacklogResyncs: when events that can't coalesce pile up past compactBacklog,
// the stream gets a fresh snapshot in their place rather than being dropped.
func TestCompactBacklogResyncs(t *testing.T) {
	th := newTestHub(t)
	s := th.addStreamMode(true)
	defer th.releaseStream(s)
	for i := range compactBacklog + 10 {
		th.broadcast(map[string]any{"type": "alert", "n": i})
	}
	th.broadcast(reportEvent{Type: "report", ID: "dev-1"})
	select {
	case <-s.done:
		t.Fatal("compact stream dropped")
	default:
	}
	th.smu.Lock()
	batch, resync := s.pend.take()
	th.smu.Unlock()
	if !resync || len(batch) != 0 {
		t.Errorf("resync = %v with %d events; want a snapshot and nothing else", resync, len(batch))
	}
}

// TestCompactStream: over HTTP, ?compact=1 gets a gzip stream whose snapshot carries the
// latest report but no history, then live events. The upstream stream (no opt-in) is
// unchanged: its snapshot has each device's history.
func TestCompactStream(t *testing.T) {
	th := newTestHub(t)
	for range 3 {
		th.postReport("dev-1", nil)
		th.clk.Add(2 * time.Second)
	}
	srv := httptest.NewServer(th.h)
	t.Cleanup(srv.Close)                            // after the streams below close: Close waits for them
	tr := &http.Transport{DisableCompression: true} // see the encoding as sent
	defer tr.CloseIdleConnections()

	open := func(path string, hdr ...string) (*http.Response, *bufio.Scanner) {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+th.Token())
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		var body io.Reader = resp.Body
		if resp.Header.Get("Content-Encoding") == "gzip" {
			zr, err := gzip.NewReader(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			body = zr
		}
		sc := bufio.NewScanner(body)
		sc.Buffer(nil, 1<<22)
		return resp, sc
	}
	next := func(sc *bufio.Scanner) map[string]any {
		t.Helper()
		for sc.Scan() {
			if line, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				var m map[string]any
				if err := json.Unmarshal([]byte(line), &m); err != nil {
					t.Fatal(err)
				}
				return m
			}
		}
		t.Fatalf("stream ended: %v", sc.Err())
		return nil
	}
	device := func(snap map[string]any) map[string]any {
		t.Helper()
		devs, _ := snap["devices"].([]any)
		if snap["type"] != "snapshot" || len(devs) != 1 {
			t.Fatalf("snapshot = %v", snap)
		}
		return devs[0].(map[string]any)
	}

	for _, c := range []struct{ path, header string }{{"/api/stream?compact=1", ""}, {"/api/stream", "compact"}} {
		resp, sc := open(c.path, "Accept-Encoding", "gzip", CompactHeader, c.header)
		if resp.Header.Get(CompactHeader) != "compact" || resp.Header.Get("Content-Encoding") != "gzip" {
			t.Errorf("%s %q: headers %v", c.path, c.header, resp.Header)
		}
		d := device(next(sc))
		if _, ok := d["history"]; ok || d["latest"] == nil || d["id"] != "dev-1" {
			t.Errorf("compact snapshot device: history %v, latest %v", d["history"], d["latest"] != nil)
		}
		th.postReport("dev-1", nil)
		ev := next(sc)
		for range 10 { // alerts may come first
			if ev["type"] == "report" {
				break
			}
			ev = next(sc)
		}
		if ev["type"] != "report" || ev["id"] != "dev-1" {
			t.Errorf("after a report: %v", ev)
		}
	}

	resp, sc := open("/api/stream", "Accept-Encoding", "gzip")
	if resp.Header.Get(CompactHeader) != "" || resp.Header.Get("Content-Encoding") != "" {
		t.Errorf("upstream stream: headers %v", resp.Header)
	}
	if h, _ := device(next(sc))["history"].([]any); len(h) < 3 {
		t.Errorf("upstream snapshot history has %d points, want the recent samples", len(h))
	}

	var full, compact []map[string]any
	json.Unmarshal(th.keyed("GET", "/api/devices", "").Body.Bytes(), &full)
	json.Unmarshal(th.keyed("GET", "/api/devices?compact=1", "").Body.Bytes(), &compact)
	if len(full) != 1 || full[0]["history"] == nil || len(compact) != 1 || compact[0]["history"] != nil || compact[0]["latest"] == nil {
		t.Errorf("/api/devices: full %v, compact %v", full, compact)
	}
}
