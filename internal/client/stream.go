package client

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
	"github.com/ContractorKeith/machine-monitor/internal/hub"
)

const (
	EventSnapshot       = "snapshot"
	EventReport         = "report"
	EventStatus         = "status"
	EventOrder          = "order"
	EventDevice         = "device"
	EventRemoved        = "removed"
	EventAlert          = "alert"
	EventActionResult   = "actionResult"
	EventConnected      = "connected"
	EventDisconnected   = "disconnected"
	defaultEventMessage = "message"
)

type Event struct {
	Type    string
	Devices []Device
	ID      string
	Device  *Device
	Sample  *history.Point
	Samples []history.Point
	Online  map[string]bool
	Order   map[string]int
	Raw     json.RawMessage

	Err   error
	Retry time.Duration
}

const (
	streamBuffer = 256

	maxLine  = maxResponse
	maxEvent = maxResponse

	maxSamples = 64

	idleTimeout = 20 * time.Second

	retryMin = time.Second
	retryMax = 30 * time.Second
)

type streamConfig struct {
	buffer             int
	maxLine            int
	maxEvent           int
	idle               time.Duration
	retryMin, retryMax time.Duration
	queued             func(Event)
}

var defaultStreamConfig = streamConfig{streamBuffer, maxLine, maxEvent, idleTimeout, retryMin, retryMax, nil}

var (
	errOverflow = errors.New("events arrived faster than they were read; reconnecting for a fresh snapshot")
	errIdle     = errors.New("the hub went quiet")
)

func (c *Client) Stream(ctx context.Context) <-chan Event {
	return c.streamWith(ctx, defaultStreamConfig)
}

func (c *Client) streamWith(ctx context.Context, cfg streamConfig) <-chan Event {
	out := make(chan Event)
	q := newEventQueue()
	go q.pump(ctx, out)
	go func() {
		defer q.close()
		st := &streamState{cfg: cfg, q: q, retry: cfg.retryMin}
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
			if st.gotSnapshot {
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

func nextDelay(prev, base, limit time.Duration) time.Duration {
	d := base
	if prev > 0 {
		d = min(prev*2, limit)
	}
	d = max(d, base)
	return d + time.Duration(rand.Int64N(int64(d)/4+1))
}

type streamState struct {
	cfg         streamConfig
	q           *eventQueue
	devices     int
	retry       time.Duration
	lastID      string
	gotSnapshot bool
}

func (st *streamState) send(ctx context.Context, e Event) bool {
	if ctx.Err() != nil {
		return false
	}
	st.q.push(e, -1)
	return true
}

func (c *Client) streamOnce(ctx context.Context, st *streamState) error {
	st.gotSnapshot = false
	if err := c.Verify(ctx); err != nil {
		return err
	}
	c.vmu.Lock()
	c.verified = true
	c.vmu.Unlock()
	cctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	req, err := http.NewRequestWithContext(cctx, "GET", c.base.String()+"/api/stream?compact=1&pair="+c.owner, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set(hub.CompactHeader, "compact")
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
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return statusErr(resp.StatusCode, b)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		return fmt.Errorf("hub: stream has Content-Type %q", ct)
	}
	if !st.send(ctx, Event{Type: EventConnected}) {
		return ctx.Err()
	}

	idle := time.AfterFunc(st.cfg.idle, func() { cancel(errIdle) })
	defer idle.Stop()
	body := activityReader{resp.Body, func() { idle.Reset(st.cfg.idle) }}
	r := newSSEReader(body, st.cfg.maxLine, st.cfg.maxEvent, nil)
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
			continue
		}
		if e.Type == EventSnapshot {
			st.gotSnapshot = true
			st.devices = len(e.Devices)
		}
		ok = st.q.push(e, st.cfg.buffer+2*st.devices)
		if st.cfg.queued != nil {
			st.cfg.queued(e)
		}
		if !ok {
			return errOverflow
		}
	}
}

type activityReader struct {
	r      io.Reader
	onRead func()
}

func (a activityReader) Read(p []byte) (int, error) {
	n, err := a.r.Read(p)
	if n > 0 {
		a.onRead()
	}
	return n, err
}

type wireEvent struct {
	Type    string          `json:"type"`
	Devices []Device        `json:"devices"`
	ID      string          `json:"id"`
	Device  *Device         `json:"device"`
	Sample  *history.Point  `json:"sample"`
	Samples []history.Point `json:"samples"`
	Online  map[string]bool `json:"online"`
	Order   map[string]int  `json:"order"`
}

func decodeEvent(ev sseEvent) (Event, bool) {
	var w wireEvent
	if err := json.Unmarshal(ev.data, &w); err != nil {
		return Event{}, false
	}
	typ := w.Type
	if typ == "" && ev.name != defaultEventMessage {
		typ = ev.name
	}
	if typ == "" || typ == EventConnected || typ == EventDisconnected {
		return Event{}, false
	}
	raw := json.RawMessage(ev.data)
	if typ == EventSnapshot {
		raw = nil
		if w.Devices == nil {
			w.Devices = []Device{}
		}
	}
	e := Event{Type: typ, Devices: w.Devices, ID: w.ID, Device: w.Device, Sample: w.Sample,
		Online: w.Online, Order: w.Order, Raw: raw}
	switch {
	case len(w.Samples) > 0:
		e.Samples = w.Samples
		e.Sample = &e.Samples[len(e.Samples)-1]
	case e.Sample != nil:
		e.Samples = []history.Point{*e.Sample}
	}
	return e, true
}

type sseEvent struct {
	name string
	data []byte
	id   *string
}

type sseReader struct {
	sc       *bufio.Scanner
	maxEvent int
	onLine   func()
	retry    time.Duration
	first    bool
}

func newSSEReader(r io.Reader, maxLine, maxEvent int, onLine func()) *sseReader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, min(64<<10, maxLine)), maxLine)
	sc.Split(splitSSELines)
	return &sseReader{sc: sc, maxEvent: maxEvent, onLine: onLine, first: true}
}

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
			line = bytes.TrimPrefix(line, []byte("\xEF\xBB\xBF"))
			r.first = false
		}
		if len(line) == 0 {
			if !hasData {
				name, id = "", nil
				continue
			}
			if name == "" {
				name = defaultEventMessage
			}
			b := data.Bytes()
			return sseEvent{name: name, data: b[:len(b)-1], id: id}, nil
		}
		if line[0] == ':' {
			continue
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
	return sseEvent{}, errors.New("hub: stream ended")
}

func splitSSELines(data []byte, atEOF bool) (int, []byte, error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		if data[i] == '\n' {
			return i + 1, data[:i], nil
		}
		if i+1 < len(data) {
			if data[i+1] == '\n' {
				return i + 2, data[:i], nil
			}
			return i + 1, data[:i], nil
		}
		if !atEOF {
			return 0, nil, nil
		}
		return i + 1, data[:i], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}
