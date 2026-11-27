package hub

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"slices"
	"strings"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/alerts"
	"github.com/ContractorKeith/machine-monitor/internal/history"
)

const streamBuffer = 256

const StreamWriteTimeout = 30 * time.Second

const (
	compactFlush   = time.Second
	compactBacklog = 256
	compactKeep    = 32
	compactSamples = 30
)

const CompactHeader = "X-Machine-Monitor-Stream"

type stream struct {
	ch   chan []byte
	done chan struct{}
	pend *pending
}

type pending struct {
	queue   []entry
	reports map[string]int
	backlog int
	status  []byte
	order   []byte
	resync  bool
	kept    []entry
	lean    bool
	wake    chan struct{}
}

type entry struct {
	typ  string
	data []byte
	rep  *pendingReport
}

type pendingReport struct {
	ev      reportEvent
	samples []history.Point
}

type compactReport struct {
	reportEvent
	Samples []history.Point `json:"samples"`
}

func keptAcrossResync(typ string) bool {
	switch typ {
	case "alert", "actionResult", "paired", "alerts", "energySettings":
		return true
	}
	return false
}

func (p *pending) add(typ, id string, data []byte, msg any) {
	switch {
	case p.resync:
		if keptAcrossResync(typ) {
			p.keep(entry{typ: typ, data: data})
		}
	case typ == "report":
		if i, ok := p.reports[id]; ok {
			p.queue[i] = mergeReport(p.queue[i], data, msg)
		} else {
			p.reports[id] = len(p.queue)
			p.queue = append(p.queue, newReport(data, msg))
		}
	case typ == "status":
		p.status = data
	case typ == "order":
		p.order = data
	default:
		if i, ok := p.reports[id]; ok && typ == "removed" {
			p.queue[i] = entry{}
			delete(p.reports, id)
		}
		if p.backlog < compactBacklog {
			p.queue = append(p.queue, entry{typ: typ, data: data})
			p.backlog++
			break
		}
		old := p.queue
		p.reset()
		p.resync = true
		for _, e := range old {
			if keptAcrossResync(e.typ) {
				p.keep(e)
			}
		}
		if keptAcrossResync(typ) {
			p.keep(entry{typ: typ, data: data})
		}
	}
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *pending) keep(e entry) {
	isSettings := func(k entry) bool { return k.typ == "alerts" || k.typ == "energySettings" }
	switch {
	case isSettings(e):
		p.kept = slices.DeleteFunc(p.kept, func(k entry) bool { return k.typ == e.typ })
	case len(p.kept)-countFunc(p.kept, isSettings) >= compactKeep:
		i := slices.IndexFunc(p.kept, func(k entry) bool { return !isSettings(k) })
		p.kept = slices.Delete(p.kept, i, i+1)
	}
	p.kept = append(p.kept, e)
}

func countFunc(es []entry, f func(entry) bool) int {
	n := 0
	for _, e := range es {
		if f(e) {
			n++
		}
	}
	return n
}

func newReport(data []byte, msg any) entry {
	e := entry{typ: "report", data: data}
	if ev, ok := msg.(reportEvent); ok {
		e.rep = &pendingReport{ev: ev, samples: []history.Point{ev.Sample}}
	}
	return e
}

func mergeReport(old entry, data []byte, msg any) entry {
	e := newReport(data, msg)
	if old.rep != nil && e.rep != nil {
		samples := append(slices.Clone(old.rep.samples), e.rep.ev.Sample)
		e.rep.samples = samples[max(0, len(samples)-compactSamples):]
	}
	return e
}

func (p *pending) reset() {
	p.reports = map[string]int{}
	p.queue, p.backlog, p.status, p.order, p.resync = nil, 0, nil, nil, false
}

func (p *pending) take() (batch [][]byte, resync bool) {
	resync = p.resync
	for _, e := range p.kept {
		batch = append(batch, e.data)
	}
	p.kept = nil
	for _, e := range p.queue {
		switch {
		case e.typ == "":
		case e.rep != nil && len(e.rep.samples) > 1 && !p.lean:
			b, err := json.Marshal(compactReport{e.rep.ev, e.rep.samples})
			if err != nil {
				b = bytes.TrimSuffix(bytes.TrimPrefix(e.data, []byte("data: ")), []byte("\n\n"))
			}
			batch = append(batch, append(append([]byte("data: "), b...), '\n', '\n'))
		default:
			batch = append(batch, e.data)
		}
	}
	if p.order != nil {
		batch = append(batch, p.order)
	}
	if p.status != nil {
		batch = append(batch, p.status)
	}
	p.reset()
	return batch, resync
}

func eventKey(msg any) (typ, id string) {
	switch m := msg.(type) {
	case reportEvent:
		return m.Type, m.ID
	case alerts.Message:
		return m.Type, ""
	case map[string]any:
		typ, _ = m["type"].(string)
		id, _ = m["id"].(string)
	}
	return typ, id
}

func (h *Hub) drop(s *stream) {
	delete(h.streams, s)
	close(s.done)
}

func (h *Hub) broadcast(msg any) {
	b, err := json.Marshal(msg)
	if err != nil {
		h.logf("hub: broadcast: %v", err)
		return
	}
	data := append(append([]byte("data: "), b...), '\n', '\n')
	typ, id := eventKey(msg)
	h.smu.Lock()
	defer h.smu.Unlock()
	for s := range h.streams {
		if s.pend != nil {
			s.pend.add(typ, id, data, msg)
			continue
		}
		select {
		case s.ch <- data:
		default:
			h.drop(s)
		}
	}
}

func (h *Hub) addStream() *stream { return h.addStreamMode(false) }

func (h *Hub) addStreamMode(compact bool) *stream {
	h.smu.Lock()
	defer h.smu.Unlock()
	if h.closed || h.activeStreams >= MaxStreams {
		return nil
	}
	s := &stream{done: make(chan struct{})}
	if compact {
		s.pend = &pending{reports: map[string]int{}, wake: make(chan struct{}, 1)}
	} else {
		s.ch = make(chan []byte, streamBuffer)
	}
	h.streams[s] = struct{}{}
	h.activeStreams++
	return s
}

func (h *Hub) releaseStream(s *stream) {
	h.smu.Lock()
	defer h.smu.Unlock()
	if _, ok := h.streams[s]; ok {
		h.drop(s)
	}
	h.activeStreams--
}

func wantsCompact(r *http.Request) bool {
	return r.URL.Query().Get("compact") == "1" || strings.EqualFold(r.Header.Get(CompactHeader), "compact")
}

func (h *Hub) compactViews() []deviceEvent {
	out := []deviceEvent{}
	for _, id := range h.sortedIDs() {
		d := h.registry[id]
		out = append(out, deviceEvent{View{Device: *d, Online: h.online(d), Latest: h.latest[id]}})
	}
	return out
}

func (h *Hub) snapshotEvent(compact bool) []byte {
	h.mu.Lock()
	var devices any
	if compact {
		devices = h.compactViews()
	} else {
		devices = h.views()
	}
	h.mu.Unlock()
	b, _ := json.Marshal(map[string]any{"type": "snapshot", "devices": devices})
	return append(append([]byte("data: "), b...), '\n', '\n')
}

func (h *Hub) handleStream(w http.ResponseWriter, r *http.Request) {
	compact := wantsCompact(r)
	s := h.addStreamMode(compact)
	if s == nil {
		sendJSON(w, http.StatusServiceUnavailable, errorBody("too many connections"))
		return
	}
	defer h.releaseStream(s)
	if owner := pairOwner(r); owner != "" {
		defer h.watchPair(owner)()
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	timeout := h.streamWriteTimeout
	if timeout == 0 {
		timeout = StreamWriteTimeout
	}
	gzipped := compact && acceptsGzip(r)
	if compact {
		w.Header().Set(CompactHeader, "compact")
		w.Header().Add("Vary", "Accept-Encoding")
		if gzipped {
			w.Header().Set("Content-Encoding", "gzip")
		}
	}
	ctx := r.Context()
	rc := http.NewResponseController(w)
	var base io.Writer = w
	setDeadline, flushBase := rc.SetWriteDeadline, rc.Flush
	if compact {
		if hc := hijackStream(w, r); hc != nil {
			defer hc.close(timeout)
			if hc.sendQ != nil {
				h.sendQOnce.Do(func() {
					h.logf("hub: compact streams run with an unbounded send queue: %v", hc.sendQ)
				})
			}
			ctx, base, setDeadline, flushBase = hc.ctx, hc, hc.conn.SetWriteDeadline, hc.bw.Flush
		} else {
			w.WriteHeader(http.StatusOK)
		}
	} else {
		w.WriteHeader(http.StatusOK)
	}
	out := base
	var gz *gzip.Writer
	if gzipped {
		gz, _ = gzip.NewWriterLevel(base, gzip.BestSpeed)
		out = gz
		defer gz.Close()
	}
	deadline := func() bool {
		err := setDeadline(time.Now().Add(timeout))
		return err == nil || errors.Is(err, http.ErrNotSupported)
	}
	write := func(data []byte) bool {
		if !deadline() {
			return false
		}
		_, err := out.Write(data)
		return err == nil
	}
	flush := func() bool {
		if !deadline() {
			return false
		}
		if gz != nil && gz.Flush() != nil {
			return false
		}
		return flushBase() == nil
	}

	if !write(h.snapshotEvent(compact)) || !flush() {
		return
	}
	if compact {
		h.serveCompact(ctx, s, write, flush)
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.done:
			return
		case data := <-s.ch:
			select {
			case <-s.done:
				return
			default:
			}
			if !write(data) || !flush() {
				return
			}
		}
	}
}

func (h *Hub) serveCompact(ctx context.Context, s *stream, write func([]byte) bool, flush func() bool) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.done:
			return
		case <-s.pend.wake:
		}
		h.smu.Lock()
		batch, resync := s.pend.take()
		h.smu.Unlock()
		began := time.Now()
		if resync && !write(h.snapshotEvent(true)) {
			return
		}
		for _, data := range batch {
			if !write(data) {
				return
			}
		}
		if !flush() {
			return
		}
		h.smu.Lock()
		s.pend.lean = time.Since(began) > compactFlush
		h.smu.Unlock()
		t := time.NewTimer(compactFlush)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-s.done:
			t.Stop()
			return
		case <-t.C:
		}
	}
}

type hijacked struct {
	conn   net.Conn
	bw     *bufio.Writer
	body   io.WriteCloser
	ctx    context.Context
	cancel context.CancelFunc
	sendQ  error
}

func (c *hijacked) Write(b []byte) (int, error) { return c.body.Write(b) }

func (c *hijacked) close(timeout time.Duration) {
	c.conn.SetWriteDeadline(time.Now().Add(timeout))
	if c.body.Close() == nil {
		c.bw.WriteString("\r\n")
		c.bw.Flush()
	}
	c.conn.Close()
	c.cancel()
}

func hijackStream(w http.ResponseWriter, r *http.Request) *hijacked {
	if r.ProtoMajor != 1 || r.ProtoMinor < 1 {
		return nil
	}
	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return nil
	}
	sendQ := boundSendQueue(conn)
	ctx, cancel := context.WithCancel(r.Context())
	c := &hijacked{conn: conn, bw: brw.Writer, body: httputil.NewChunkedWriter(brw.Writer), ctx: ctx, cancel: cancel, sendQ: sendQ}
	conn.SetReadDeadline(time.Time{})
	go func() {
		io.Copy(io.Discard, brw.Reader)
		cancel()
	}()
	w.Header().Set("Date", time.Now().UTC().Format(http.TimeFormat))
	brw.WriteString("HTTP/1.1 200 OK\r\n")
	w.Header().Write(brw)
	brw.WriteString("Transfer-Encoding: chunked\r\nConnection: close\r\n\r\n")
	return c
}

func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if strings.EqualFold(strings.TrimSpace(name), "gzip") && strings.ReplaceAll(strings.TrimSpace(params), " ", "") != "q=0" {
			return true
		}
	}
	return false
}
