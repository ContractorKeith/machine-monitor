package client

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/config"
	"github.com/ContractorKeith/machine-monitor/internal/hub"
)

// testHub is a real hub served by httptest.
type testHub struct {
	t    *testing.T
	h    *hub.Hub
	srv  *httptest.Server
	stop func() // shuts the hub down and saves its data, as if its machine went away
}

// newTestHub starts a hub on addr ("" for any free port) and runs its timers.
func newTestHub(t *testing.T, paths config.Paths, addr string) *testHub {
	t.Helper()
	h, err := hub.New(hub.Options{Paths: paths, Hostname: "testbox", Location: time.UTC, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	var ln net.Listener
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if ln, err = net.Listen("tcp", addr); err == nil || time.Now().After(deadline) {
			break
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(h.Handler())
	srv.Listener = ln
	srv.Start()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.Run(ctx); close(done) }()
	th := &testHub{t: t, h: h, srv: srv}
	th.stop = sync.OnceFunc(func() {
		srv.CloseClientConnections()
		cancel()
		<-done
		srv.Close()
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})
	t.Cleanup(th.stop)
	return th
}

// report posts the recorded upstream agent.sh report as device id.
func (th *testHub) report(id string, edit func(map[string]any)) {
	th.t.Helper()
	b, err := os.ReadFile("../report/testdata/linux-agent.sh.json")
	if err != nil {
		th.t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(b, &m)
	m["token"], m["id"] = th.h.Token(), id
	if edit != nil {
		edit(m)
	}
	body, _ := json.Marshal(m)
	resp, err := http.Post(th.srv.URL+"/api/report", "application/json", strings.NewReader(string(body)))
	if err != nil {
		th.t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		th.t.Fatalf("report %s: %d", id, resp.StatusCode)
	}
}

func (th *testHub) client(key string) *Client {
	th.t.Helper()
	c, err := New(th.srv.URL, key)
	if err != nil {
		th.t.Fatal(err)
	}
	return c
}

// events reads a stream in the background so tests can wait for one event type at a time.
type events struct {
	t  *testing.T
	ch <-chan Event
}

// next returns the next event of type typ, skipping others.
func (e events) next(typ string) Event {
	e.t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-e.ch:
			if !ok {
				e.t.Fatalf("stream closed waiting for %q", typ)
			}
			if ev.Type == typ {
				return ev
			}
		case <-timeout:
			e.t.Fatalf("no %q event", typ)
		}
	}
}

func stream(t *testing.T, c *Client, cfg streamConfig) events {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return events{t, c.streamWith(ctx, cfg)}
}

// fastConfig reconnects quickly so tests don't wait on the real backoff.
func fastConfig() streamConfig {
	c := defaultStreamConfig
	c.retryMin, c.retryMax = 20*time.Millisecond, 100*time.Millisecond
	return c
}

func TestParseURL(t *testing.T) {
	for in, want := range map[string]string{
		"192.168.1.5":              "http://192.168.1.5:7777",
		"hub.lan:8000":             "http://hub.lan:8000",
		"http://hub.lan":           "http://hub.lan:7777",
		"http://hub.lan:7777/":     "http://hub.lan:7777",
		"https://hub.ts.net":       "https://hub.ts.net",
		"[fd7a::1]":                "http://[fd7a::1]:7777",
		"  http://10.0.0.2:7777  ": "http://10.0.0.2:7777",
	} {
		u, err := ParseURL(in)
		if err != nil || u.String() != want {
			t.Errorf("ParseURL(%q) = %v, %v; want %s", in, u, err, want)
		}
	}
	for _, bad := range []string{"", "ftp://hub", "http://user:pw@hub", "http://hub/api", "http://hub?x=1", "http://",
		"http://user:secret@hub/x", "http://a:secret@[::1", "secret@hub"} {
		_, err := ParseURL(bad)
		if err == nil {
			t.Errorf("ParseURL(%q) accepted", bad)
		} else if strings.Contains(err.Error(), "secret") {
			t.Errorf("ParseURL(%q) error echoes the password: %v", bad, err)
		}
	}
	if !SameHub("hub.lan", "http://hub.lan:7777/") || SameHub("hub.lan", "hub.lan:8000") {
		t.Error("SameHub")
	}
}

// TestAgainstHub drives every read and write call against a real hub, and checks each
// change arrives on the stream as the hub's event.
func TestAgainstHub(t *testing.T) {
	th := newTestHub(t, config.At(t.TempDir()), "")
	th.report("dev-a", nil)
	th.report("dev-b", func(m map[string]any) { m["hostname"] = "nas" })
	c := th.client(th.h.Token())
	ctx := context.Background()

	if err := c.Verify(ctx); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	devs, err := c.Devices(ctx)
	if err != nil || len(devs) != 2 {
		t.Fatalf("Devices = %d, %v", len(devs), err)
	}
	if d := devs[0]; d.ID != "dev-a" || !d.Online || d.Latest == nil || *d.Latest.Hostname != "atlas" ||
		len(d.History) != 1 || d.Latest.PowerEst == nil {
		t.Errorf("device = %+v", d)
	}

	ev := stream(t, c, fastConfig())
	ev.next(EventConnected)
	if s := ev.next(EventSnapshot); len(s.Devices) != 2 || s.Devices[1].Name != "nas" {
		t.Fatalf("snapshot = %+v", s.Devices)
	}

	th.report("dev-a", func(m map[string]any) {
		m["actionResult"] = map[string]any{"id": "0123456789ab", "ok": true, "message": "Restarting"}
	})
	if r := ev.next(EventReport); r.ID != "dev-a" || r.Device == nil || r.Device.Latest == nil || r.Sample == nil || r.Sample.CPU == nil {
		t.Errorf("report = %+v", r)
	}
	if a := ev.next(EventActionResult); a.ID != "dev-a" || !strings.Contains(string(a.Raw), "Restarting") {
		t.Errorf("actionResult = %+v", a)
	}

	d, err := c.Rename(ctx, "dev-b", "Basement NAS")
	if err != nil || d.Name != "Basement NAS" {
		t.Fatalf("Rename = %+v, %v", d, err)
	}
	if e := ev.next(EventDevice); e.Device == nil || e.Device.ID != "dev-b" || e.Device.Name != "Basement NAS" {
		t.Errorf("device event = %+v", e)
	}

	if err := c.SetOrder(ctx, []string{"dev-b", "dev-a"}); err != nil {
		t.Fatal(err)
	}
	if o := ev.next(EventOrder); o.Order["dev-b"] != 0 || o.Order["dev-a"] != 1 {
		t.Errorf("order = %v", o.Order)
	}

	if st := ev.next(EventStatus); !st.Online["dev-a"] { // the 3 s heartbeat
		t.Errorf("status = %v", st.Online)
	}

	req, _ := http.NewRequest("POST", th.srv.URL+"/api/alerts/test", nil)
	req.Header.Set("Authorization", "Bearer "+th.h.Token())
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
	}
	if a := ev.next(EventAlert); !strings.Contains(string(a.Raw), `"test":true`) {
		t.Errorf("alert = %s", a.Raw)
	}

	if err := c.Remove(ctx, "dev-a"); err != nil {
		t.Fatal(err)
	}
	if r := ev.next(EventRemoved); r.ID != "dev-a" {
		t.Errorf("removed = %+v", r)
	}
	var se *StatusError
	if err := c.Remove(ctx, "dev-a"); !errors.As(err, &se) || se.Code != 404 || se.Message != "not found" {
		t.Errorf("second Remove = %v", err)
	}
}

// TestStreamReconnect: the stream survives a dropped connection and a hub restart, and each
// reconnection starts with a fresh snapshot.
func TestStreamReconnect(t *testing.T) {
	paths := config.At(t.TempDir())
	th := newTestHub(t, paths, "")
	th.report("dev-a", nil)
	c := th.client(th.h.Token())
	ev := stream(t, c, fastConfig())
	ev.next(EventConnected)
	ev.next(EventSnapshot)

	// 1. The connection drops; the hub is still there.
	th.srv.CloseClientConnections()
	if d := ev.next(EventDisconnected); d.Err == nil || d.Retry <= 0 {
		t.Errorf("disconnected = %+v", d)
	}
	ev.next(EventConnected)
	if s := ev.next(EventSnapshot); len(s.Devices) != 1 {
		t.Errorf("snapshot after reconnect = %d devices", len(s.Devices))
	}

	// 2. The hub goes away and comes back on the same address with the same data.
	addr := th.srv.Listener.Addr().String()
	th.stop()
	ev.next(EventDisconnected)
	ev.next(EventDisconnected) // still down: retrying
	th2 := newTestHub(t, paths, addr)
	ev.next(EventConnected)
	if s := ev.next(EventSnapshot); len(s.Devices) != 1 || s.Devices[0].ID != "dev-a" || s.Devices[0].Latest != nil {
		t.Fatalf("snapshot after hub restart = %+v, want dev-a from devices.json with no report yet", s.Devices)
	}
	th2.report("dev-b", nil)
	if r := ev.next(EventReport); r.ID != "dev-b" {
		t.Errorf("report after restart = %+v", r)
	}
}

// TestWrongKey: a wrong key fails the hub's proof, so it is never sent, and the stream ends.
func TestWrongKey(t *testing.T) {
	th := newTestHub(t, config.At(t.TempDir()), "")
	c := th.client("0123456789abcdef01234567")
	if err := c.Verify(context.Background()); !errors.Is(err, ErrBadProof) {
		t.Errorf("Verify = %v", err)
	}
	if _, err := c.Devices(context.Background()); !errors.Is(err, ErrBadProof) {
		t.Errorf("Devices = %v", err)
	}
	ev := stream(t, c, fastConfig())
	if d := ev.next(EventDisconnected); !errors.Is(d.Err, ErrBadProof) || d.Retry != 0 {
		t.Errorf("disconnected = %+v", d)
	}
	select {
	case _, ok := <-ev.ch:
		if ok {
			t.Error("stream still open after a bad proof")
		}
	case <-time.After(5 * time.Second):
		t.Error("stream not closed after a bad proof")
	}
}

// fakeHub serves /api/info with a correct proof and /api/stream from fn.
func fakeHub(t *testing.T, key string, fn func(ctx context.Context, w http.ResponseWriter, f http.Flusher)) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/info":
			m := hmac.New(sha256.New, []byte(key))
			m.Write([]byte("device-monitor-hub:" + r.URL.Query().Get("nonce")))
			fmt.Fprintf(w, `{"proof":%q}`, hex.EncodeToString(m.Sum(nil)))
		case "/api/stream":
			if r.Header.Get("Authorization") != "Bearer "+key {
				w.WriteHeader(401)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			f := w.(http.Flusher)
			f.Flush()
			fn(r.Context(), w, f)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, key)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestSlowConsumer: a consumer that stops reading never stalls the network reader. Events
// that can't be merged fill the queue; the client then drops the connection, and
// reconnects (fresh snapshot) once the consumer catches up.
func TestSlowConsumer(t *testing.T) {
	const sent = 200
	finished := make(chan int, 4)
	c := fakeHub(t, "k", func(ctx context.Context, w http.ResponseWriter, f http.Flusher) {
		io.WriteString(w, "data: {\"type\":\"snapshot\",\"devices\":[]}\n\n")
		n := 0
		for ; n < sent; n++ {
			if _, err := fmt.Fprintf(w, "data: {\"type\":\"alert\",\"n\":%d}\n\n", n); err != nil {
				break
			}
			f.Flush()
		}
		finished <- n
		<-ctx.Done() // hold the connection open; the client must hang up
	})
	cfg := fastConfig()
	cfg.buffer = 4
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := c.streamWith(ctx, cfg)

	select { // the handler wrote everything without the consumer reading a single event
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("the server's writes blocked on a slow consumer")
	}
	time.Sleep(100 * time.Millisecond)
	var got []string
	for ev := range ch {
		got = append(got, ev.Type)
		if ev.Type == EventDisconnected {
			if !errors.Is(ev.Err, errOverflow) {
				t.Fatalf("disconnected with %v, want overflow", ev.Err)
			}
			break
		}
	}
	// connected, snapshot, then alerts until more than buffer were waiting (give or take the
	// one the pump holds while it waits for the consumer).
	if len(got) < cfg.buffer+1 || len(got) > cfg.buffer+3 || got[0] != EventConnected || got[1] != EventSnapshot {
		t.Errorf("events before the drop: %v", got)
	}
	ev := events{t, ch}
	ev.next(EventConnected)
	ev.next(EventSnapshot)
}

// TestCoalesce: while the consumer is busy, a device's reports merge into its newest one,
// keeping every sample, and status events merge into one; the stream stays up, events
// keep their order around anything that can't merge, and the queue stays one slot per
// device however much the hub sends.
func TestCoalesce(t *testing.T) {
	const devices, rounds = 50, 40
	finished := make(chan struct{}, 4)
	c := fakeHub(t, "k", func(ctx context.Context, w http.ResponseWriter, f http.Flusher) {
		io.WriteString(w, "data: {\"type\":\"snapshot\",\"devices\":[")
		for d := range devices {
			if d > 0 {
				io.WriteString(w, ",")
			}
			fmt.Fprintf(w, "{\"id\":\"d%02d\"}", d)
		}
		io.WriteString(w, "]}\n\n")
		for r := range rounds {
			for d := range devices {
				fmt.Fprintf(w, "data: {\"type\":\"report\",\"device\":{\"id\":\"d%02d\",\"name\":\"r%d\"},\"sample\":{\"t\":%d,\"cpu\":%d}}\n\n", d, r, r, r)
			}
			fmt.Fprintf(w, "data: {\"type\":\"status\",\"online\":{\"d%02d\":true,\"d00\":%v}}\n\n", r%devices, r%2 == 0)
			if r == rounds/2 {
				io.WriteString(w, "data: {\"type\":\"order\",\"order\":{\"d00\":1}}\n\n")
			}
		}
		f.Flush()
		select { // reconnects after a failure call this again: never block on it
		case finished <- struct{}{}:
		default:
		}
		<-ctx.Done()
	})
	cfg := fastConfig()
	cfg.buffer = 8 // far below the 2000 reports: only merging keeps the stream up
	// The consumer starts reading only once the reader has queued the hub's last event, so
	// what merges doesn't depend on how fast either goroutine runs (it used to sleep 200 ms
	// and hope, which flaked under -race).
	last := fmt.Sprintf("d%02d", (rounds-1)%devices)
	allQueued := make(chan struct{})
	cfg.queued = func(e Event) {
		if e.Type == EventStatus && len(e.Online) == 2 && e.Online[last] {
			select {
			case <-allQueued:
			default:
				close(allQueued)
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := c.streamWith(ctx, cfg)
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("the server's writes blocked on a slow consumer")
	}
	select {
	case <-allQueued:
	case <-time.After(30 * time.Second): // a failure guard only; nothing waits on it to pass
		t.Fatal("the reader never queued the last event")
	}

	samples := map[string][]float64{}
	names := map[string]string{}
	var types []string
	var lastOnline map[string]bool
	for ev := range ch {
		if ev.Type == EventDisconnected {
			t.Fatalf("stream dropped: %v", ev.Err)
		}
		types = append(types, ev.Type)
		switch ev.Type {
		case EventReport:
			for _, p := range ev.Samples {
				samples[ev.Device.ID] = append(samples[ev.Device.ID], *p.CPU)
			}
			if ev.Sample == nil || *ev.Sample.CPU != *ev.Samples[len(ev.Samples)-1].CPU {
				t.Fatalf("Sample is not the newest of Samples")
			}
			names[ev.Device.ID] = ev.Device.Name
		case EventStatus:
			lastOnline = ev.Online
		}
		if len(names) == devices && names["d49"] == fmt.Sprintf("r%d", rounds-1) && lastOnline != nil && len(lastOnline) > 1 {
			break
		}
	}
	// Merged: one report per device, one status, the order event.
	if len(types) > 2+devices+2 {
		t.Errorf("%d events delivered; reports were not merged", len(types))
	}
	if i := slices.Index(types, EventOrder); i < 0 {
		t.Error("the order event was lost")
	}
	for d := range devices {
		id := fmt.Sprintf("d%02d", d)
		got := samples[id]
		if len(got) != rounds {
			t.Fatalf("%s: %d samples, want %d (%v)", id, len(got), rounds, got)
		}
		for i, v := range got {
			if v != float64(i) {
				t.Fatalf("%s: samples out of order: %v", id, got)
			}
		}
		if names[id] != fmt.Sprintf("r%d", rounds-1) {
			t.Errorf("%s: name %q, want the newest", id, names[id])
		}
	}
	if lastOnline["d00"] != ((rounds-1)%2 == 0) {
		t.Errorf("merged status kept an old flag: %v", lastOnline)
	}
}

// TestIdleStream: a stream that goes silent (a pulled cable) is dropped and retried.
func TestIdleStream(t *testing.T) {
	c := fakeHub(t, "k", func(ctx context.Context, w http.ResponseWriter, f http.Flusher) {
		io.WriteString(w, "data: {\"type\":\"snapshot\",\"devices\":[]}\n\n")
		f.Flush()
		<-ctx.Done()
	})
	cfg := fastConfig()
	cfg.idle = 100 * time.Millisecond
	ev := stream(t, c, cfg)
	ev.next(EventSnapshot)
	if d := ev.next(EventDisconnected); !errors.Is(d.Err, errIdle) {
		t.Errorf("disconnected with %v, want idle", d.Err)
	}
	ev.next(EventConnected)
}

func TestSSEParse(t *testing.T) {
	in := "\xEF\xBB\xBF: hello comment\n" +
		"retry: 2500\n" +
		"data: {\"type\":\"status\",\n" + // multi-line data
		"data:  \"online\":{\"a\":true}}\n" +
		"\n" +
		"event: order\r\n" + // CRLF, and the type from "event:"
		"id: 7\r\n" +
		"data:{\"order\":{\"a\":2}}\r\n" +
		"\r\n" +
		"data: {\"type\":\"removed\",\"id\":\"a\"}\r" + // bare CR
		"\r" +
		"\n" + // a blank line with nothing to dispatch
		"bogus line\n" +
		"data: not json\n\n" +
		"data: {\"type\":\"removed\",\"id\":\"half\"}\n" // unfinished at EOF: dropped
	r := newSSEReader(strings.NewReader(in), 1<<10, 1<<10, nil)
	var got []Event
	for {
		ev, err := r.next()
		if err != nil {
			break
		}
		if e, ok := decodeEvent(ev); ok {
			got = append(got, e)
		}
	}
	if r.retry != 2500*time.Millisecond {
		t.Errorf("retry = %v", r.retry)
	}
	if len(got) != 3 {
		t.Fatalf("got %d events: %+v", len(got), got)
	}
	if got[0].Type != EventStatus || !got[0].Online["a"] {
		t.Errorf("multi-line event = %+v", got[0])
	}
	if got[1].Type != EventOrder || got[1].Order["a"] != 2 {
		t.Errorf("named event = %+v", got[1])
	}
	if got[2].Type != EventRemoved || got[2].ID != "a" {
		t.Errorf("CR event = %+v", got[2])
	}

	long := "data: " + strings.Repeat("x", 2000) + "\n\n"
	if _, err := newSSEReader(strings.NewReader(long), 1<<10, 1<<20, nil).next(); err == nil || !strings.Contains(err.Error(), "too long") {
		t.Errorf("line over the cap: %v", err)
	}
	many := strings.Repeat("data: "+strings.Repeat("x", 100)+"\n", 20) + "\n"
	if _, err := newSSEReader(strings.NewReader(many), 1<<10, 1<<10, nil).next(); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("event over the cap: %v", err)
	}
}

func TestNextDelay(t *testing.T) {
	d := time.Duration(0)
	for range 10 {
		d = nextDelay(d, time.Second, 30*time.Second)
		if d < time.Second || d > 30*time.Second*5/4 {
			t.Fatalf("delay %v out of range", d)
		}
	}
	if d < 30*time.Second {
		t.Errorf("backoff never reached the cap: %v", d)
	}
}

func TestConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "client.json")
	if c, err := LoadConfig(path); err != nil || c != (Config{}) {
		t.Errorf("missing file: %+v, %v", c, err)
	}
	if err := SaveConfig(path, Config{Hub: "http://hub.lan:7777", Key: "abc"}); err != nil {
		t.Fatal(err)
	}
	if c, err := LoadConfig(path); err != nil || c.Hub != "http://hub.lan:7777" || c.Key != "abc" {
		t.Errorf("round trip: %+v, %v", c, err)
	}
	os.WriteFile(path, []byte(`{"hubUrl":"http://up:7777","token":"t"}`), 0o600)
	if c, _ := LoadConfig(path); c.Hub != "http://up:7777" || c.Key != "t" {
		t.Errorf("upstream names: %+v", c)
	}
	os.WriteFile(path, []byte(`{`), 0o600)
	if _, err := LoadConfig(path); err == nil {
		t.Error("damaged file accepted")
	}
}

// TestNoKeyBeforeProof: no request carries the key until the hub has proved it knows it,
// and a hub that can't prove it never sees the key at all.
func TestNoKeyBeforeProof(t *testing.T) {
	var mu sync.Mutex
	var keyed []string // paths requested with a key
	proof := "wrong"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if r.Header.Get("Authorization") != "" {
			keyed = append(keyed, r.URL.Path)
		}
		p := proof
		mu.Unlock()
		if r.URL.Path == "/api/info" {
			if p == "" {
				m := hmac.New(sha256.New, []byte("k"))
				m.Write([]byte("device-monitor-hub:" + r.URL.Query().Get("nonce")))
				p = hex.EncodeToString(m.Sum(nil))
			}
			fmt.Fprintf(w, `{"proof":%q}`, p)
			return
		}
		io.WriteString(w, "[]")
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "k")
	ctx := context.Background()
	_, err1 := c.Devices(ctx)
	_, err2 := c.Rename(ctx, "a", "b")
	for _, err := range []error{err1, err2, c.Remove(ctx, "a"), c.SetOrder(ctx, nil)} {
		if !errors.Is(err, ErrBadProof) {
			t.Errorf("request to an unproven hub: %v", err)
		}
	}
	mu.Lock()
	if len(keyed) != 0 {
		t.Errorf("key sent to an unproven hub: %v", keyed)
	}
	proof = "" // now the hub proves itself
	mu.Unlock()
	if _, err := c.Devices(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Devices(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(keyed) != 2 {
		t.Errorf("keyed requests after the proof: %v", keyed)
	}
}

// TestStreamStatusMessage: a refused stream keeps the hub's sentence, e.g. the 503 at the
// connection limit.
func TestStreamStatusMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/info" {
			m := hmac.New(sha256.New, []byte("k"))
			m.Write([]byte("device-monitor-hub:" + r.URL.Query().Get("nonce")))
			fmt.Fprintf(w, `{"proof":%q}`, hex.EncodeToString(m.Sum(nil)))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(503)
		io.WriteString(w, `{"error":"too many connections"}`)
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "k")
	ev := stream(t, c, fastConfig())
	d := ev.next(EventDisconnected)
	var se *StatusError
	if !errors.As(d.Err, &se) || se.Code != 503 || se.Message != "too many connections" {
		t.Errorf("disconnected with %v", d.Err)
	}
}
