package client

// The live stream: GET /api/stream, server-sent events, reconnecting with backoff.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/history"
)

// Event types. The hub's own types arrive as it names them; Connected and Disconnected are
// made by the client when the connection opens or drops.
const (
	EventSnapshot       = "snapshot"     // Devices: every device; replaces what you had
	EventReport         = "report"       // ID, Device (no History) and Sample
	EventStatus         = "status"       // Online: id -> online, every few seconds
	EventOrder          = "order"        // Order: id -> position
	EventDevice         = "device"       // Device: renamed (no History)
	EventRemoved        = "removed"      // ID
	EventAlert          = "alert"        // Raw
	EventActionResult   = "actionResult" // ID and Raw
	EventConnected      = "connected"    // the stream is open; a snapshot follows
	EventDisconnected   = "disconnected" // Err; Retry is the wait before the next try
	defaultEventMessage = "message"      // SSE's name for an event without "event:"
)

// Event is one message from the stream. Only the fields its Type uses are set; Raw always
// holds the event's JSON, for types this struct doesn't break out (alert, alerts,
// actionResult, energySettings, and anything a newer hub adds).
type Event struct {
	Type    string
	Devices []Device
	ID      string
	Device  *Device
	Sample  *history.Point
	Online  map[string]bool
	Order   map[string]int
	Raw     json.RawMessage

	Err   error         // Disconnected: why
	Retry time.Duration // Disconnected: delay before reconnecting; 0 when the stream has ended
}

// Stream defaults.
const (
	// streamBuffer is how many events may wait for a slow consumer. Past that the client
	// drops the connection rather than block reading it, then reconnects for a fresh snapshot.
	streamBuffer = 256
	// maxLine caps one SSE line, and maxEvent the data of one event. A snapshot of
	// MaxDevices with history is the largest legitimate event, at a few MB.
	maxLine  = 32 << 20
	maxEvent = 32 << 20
	// idleTimeout drops a stream that has sent nothing for this long: the hub sends a status
	// event every 3 s, so silence means the network or the hub is gone.
	idleTimeout = 20 * time.Second
	// Reconnect backoff: starts at the hub's "retry:" (or retryMin) and doubles to retryMax.
	retryMin = time.Second
	retryMax = 30 * time.Second
)

// streamConfig holds the stream's limits; tests shrink them.
type streamConfig struct {
	buffer             int
	maxLine            int
	maxEvent           int
	idle               time.Duration
	retryMin, retryMax time.Duration
}

var defaultStreamConfig = streamConfig{streamBuffer, maxLine, maxEvent, idleTimeout, retryMin, retryMax}

var (
	errOverflow = errors.New("events arrived faster than they were read; reconnecting for a fresh snapshot")
	errIdle     = errors.New("the hub went quiet")
)

// Stream follows the hub's live stream until ctx ends, reconnecting with backoff whenever
// the connection drops. Every (re)connection sends EventConnected and then the hub's
// snapshot, so a consumer can rebuild its state from scratch each time. The channel closes
// when ctx ends, or after an EventDisconnected whose Err is ErrUnauthorized or ErrBadProof,
// since retrying with the same key can't succeed.
func (c *Client) Stream(ctx context.Context) <-chan Event {
	return c.streamWith(ctx, defaultStreamConfig)
}

func (c *Client) streamWith(ctx context.Context, cfg streamConfig) <-chan Event {
	out := make(chan Event, cfg.buffer)
	go func() {
		defer close(out)
		st := &streamState{cfg: cfg, out: out, retry: cfg.retryMin}
		delay := time.Duration(0)
		for {
			err := c.streamOnce(ctx, st)
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, ErrUnauthorized) || errors.Is(err, ErrBadProof) {
				st.send(ctx, Event{Type: EventDisconnected, Err: err})
				return
			}
			if st.gotSnapshot { // a good connection: start the backoff over
				delay = 0
			}
			delay = nextDelay(delay, st.retry, cfg.retryMax)
			if !st.send(ctx, Event{Type: EventDisconnected, Err: err, Retry: delay}) {
				return
			}
			t := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
		}
	}()
	return out
}

// nextDelay doubles the previous delay from base up to limit, with up to 25 % jitter so
// many clients don't reconnect in lockstep after a hub restart.
func nextDelay(prev, base, limit time.Duration) time.Duration {
	d := base
	if prev > 0 {
		d = min(prev*2, limit)
	}
	d = max(d, base)
	return d + time.Duration(rand.Int64N(int64(d)/4+1))
}

// streamState carries what survives reconnects.
type streamState struct {
	cfg         streamConfig
	out         chan Event
	retry       time.Duration // the hub's "retry:" or retryMin
	lastID      string        // the last "id:", sent back as Last-Event-ID
	gotSnapshot bool          // the current connection delivered its snapshot
}

// send delivers a client-made event, waiting for the consumer. It is only called while no
// connection is being read, so waiting holds up nothing but the next reconnect.
func (st *streamState) send(ctx context.Context, e Event) bool {
	select {
	case st.out <- e:
		return true
	case <-ctx.Done():
		return false
	}
}

// streamOnce runs one connection: proof, request, then events until it fails.
func (c *Client) streamOnce(ctx context.Context, st *streamState) error {
	st.gotSnapshot = false
	if err := c.Verify(ctx); err != nil {
		return err
	}
	cctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	req, err := http.NewRequestWithContext(cctx, "GET", c.base.String()+"/api/stream", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	if st.lastID != "" {
		req.Header.Set("Last-Event-ID", st.lastID)
	}
	resp, err := c.stream.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b := make([]byte, 4096)
		n, _ := resp.Body.Read(b)
		return statusErr(resp.StatusCode, b[:n])
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		return fmt.Errorf("hub: stream has Content-Type %q", ct)
	}
	if !st.send(ctx, Event{Type: EventConnected}) {
		return ctx.Err()
	}

	// The watchdog ends a connection that goes quiet; every line read pushes it back.
	idle := time.AfterFunc(st.cfg.idle, func() { cancel(errIdle) })
	defer idle.Stop()
	r := newSSEReader(resp.Body, st.cfg.maxLine, st.cfg.maxEvent, func() { idle.Reset(st.cfg.idle) })
	for {
		ev, err := r.next()
		if r.retry > 0 {
			st.retry = r.retry
		}
		if err != nil {
			if cause := context.Cause(cctx); cause != nil && ctx.Err() == nil {
				return cause
			}
			return err
		}
		if ev.id != nil {
			st.lastID = *ev.id
		}
		e, ok := decodeEvent(ev)
		if !ok {
			continue // not JSON, or not a shape we know: skip it, keep the stream
		}
		select {
		case st.out <- e:
		default:
			return errOverflow
		}
		if e.Type == EventSnapshot {
			st.gotSnapshot = true
		}
	}
}

// wireEvent is every field any hub event carries.
type wireEvent struct {
	Type    string          `json:"type"`
	Devices []Device        `json:"devices"`
	ID      string          `json:"id"`
	Device  *Device         `json:"device"`
	Sample  *history.Point  `json:"sample"`
	Online  map[string]bool `json:"online"`
	Order   map[string]int  `json:"order"`
}

// decodeEvent turns one SSE message into an Event. The hub names the type inside the JSON;
// an "event:" name is the fallback for servers that use SSE's own field.
func decodeEvent(ev sseEvent) (Event, bool) {
	var w wireEvent
	if err := json.Unmarshal([]byte(ev.data), &w); err != nil {
		return Event{}, false
	}
	typ := w.Type
	if typ == "" && ev.name != defaultEventMessage {
		typ = ev.name
	}
	if typ == "" || typ == EventConnected || typ == EventDisconnected {
		return Event{}, false
	}
	if typ == EventSnapshot && w.Devices == nil {
		w.Devices = []Device{}
	}
	return Event{Type: typ, Devices: w.Devices, ID: w.ID, Device: w.Device, Sample: w.Sample,
		Online: w.Online, Order: w.Order, Raw: json.RawMessage(ev.data)}, true
}

// ---- SSE parsing (WHATWG HTML "server-sent events") ----------------------------------------

// sseEvent is one dispatched message.
type sseEvent struct {
	name string  // "event:", or "message"
	data string  // "data:" lines joined with "\n"
	id   *string // "id:", when the message set one
}

// sseReader parses an event stream: lines end in LF, CR or CRLF; ":" starts a comment; data
// lines accumulate until a blank line dispatches them; "retry:" sets the reconnect delay.
type sseReader struct {
	sc       *bufio.Scanner
	maxEvent int
	onLine   func()
	retry    time.Duration
	first    bool
}

func newSSEReader(r io.Reader, maxLine, maxEvent int, onLine func()) *sseReader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, min(64<<10, maxLine)), maxLine) // a bigger initial buffer would raise the cap
	sc.Split(splitSSELines)
	return &sseReader{sc: sc, maxEvent: maxEvent, onLine: onLine, first: true}
}

// next returns the next message with data. It returns an error when the stream ends or a
// line or an event is over its cap.
func (r *sseReader) next() (sseEvent, error) {
	var (
		name    string
		data    bytes.Buffer
		hasData bool
		id      *string
	)
	for r.sc.Scan() {
		if r.onLine != nil {
			r.onLine()
		}
		line := r.sc.Bytes()
		if r.first {
			line = bytes.TrimPrefix(line, []byte("\xEF\xBB\xBF")) // a leading BOM is ignored
			r.first = false
		}
		if len(line) == 0 { // dispatch
			if !hasData {
				name, id = "", nil
				continue
			}
			if name == "" {
				name = defaultEventMessage
			}
			return sseEvent{name: name, data: strings.TrimSuffix(data.String(), "\n"), id: id}, nil
		}
		if line[0] == ':' {
			continue // comment, e.g. a keep-alive
		}
		field, value, _ := bytes.Cut(line, []byte(":"))
		value = bytes.TrimPrefix(value, []byte(" "))
		switch string(field) {
		case "event":
			name = string(value)
		case "data":
			if data.Len()+len(value)+1 > r.maxEvent {
				return sseEvent{}, errors.New("hub: stream event too large")
			}
			data.Write(value)
			data.WriteByte('\n')
			hasData = true
		case "id":
			if !bytes.ContainsRune(value, 0) {
				s := string(value)
				id = &s
			}
		case "retry":
			if n, err := strconv.ParseUint(string(value), 10, 31); err == nil && n > 0 {
				r.retry = time.Duration(n) * time.Millisecond
			}
		}
	}
	if err := r.sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return sseEvent{}, errors.New("hub: stream line too long")
		}
		return sseEvent{}, err
	}
	return sseEvent{}, errors.New("hub: stream ended") // an unfinished message is dropped, as the spec says
}

// splitSSELines is a bufio.SplitFunc for LF, CR and CRLF line ends.
func splitSSELines(data []byte, atEOF bool) (int, []byte, error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		if data[i] == '\n' {
			return i + 1, data[:i], nil
		}
		if i+1 < len(data) { // CR: swallow a following LF
			if data[i+1] == '\n' {
				return i + 2, data[:i], nil
			}
			return i + 1, data[:i], nil
		}
		if !atEOF {
			return 0, nil, nil // need the next byte to tell CR from CRLF
		}
		return i + 1, data[:i], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}
