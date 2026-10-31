package hub

// Server-sent events: GET /api/stream. Each message is one `data: <json>` line whose JSON
// carries its own "type", as upstream: snapshot, report, status, order, device, removed,
// alert, alerts, actionResult, energySettings (and paired once pairing lands).
//
// Two modes share the event names and shapes:
//
//   - Upstream (the default): a snapshot with each device's last LiveHistory of samples,
//     then every event as it happens. A client that falls streamBuffer events behind is
//     dropped and reconnects for a fresh snapshot. This is what upstream's dashboard expects.
//   - Compact (opt-in: ?compact=1 or the header "X-Machine-Monitor-Stream: compact", which
//     our client sends): a snapshot of the latest report per device without history (views
//     that chart history fetch it from /api/history), then events in batches at most every
//     compactFlush. Reports, status and order events coalesce while a client is behind, so
//     a slow link gets each device's newest state, never a backlog, and the hub's memory per
//     client is bounded by the device count. If other events pile up past compactBacklog,
//     they are replaced with a fresh snapshot in-band instead of dropping the connection.
//     The response is gzip-compressed when the client accepts it.
//
// Skeptic report B1: at 250 devices the upstream snapshot is about 20 MB, so over a link
// slower than a few MB/s it never finished before the stream was dropped.

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

// streamBuffer is how many messages a slow client may fall behind before it is dropped. A
// TUI reconnects and gets a fresh snapshot, so dropping beats blocking every report.
const streamBuffer = 256

// StreamWriteTimeout bounds each write to an SSE client. A client that stops reading fails
// its next write within this time, which ends its handler and frees its connection.
const StreamWriteTimeout = 30 * time.Second

// Compact-mode limits.
const (
	// compactFlush is the least time between two batches to a compact client: the rate
	// cap. A device that reports faster than this sends only its newest report per batch.
	compactFlush = time.Second
	// compactBacklog is how many uncoalescable events (alerts, renames, removals, action
	// results) may wait for a compact client before they're replaced by a fresh snapshot.
	compactBacklog = 256
	// compactKeep is how many alerts, action results and pairings survive a resync. The
	// TUI reloads the alert log after any snapshot, so older ones aren't lost to it.
	compactKeep = 32
	// compactSamples caps the samples one merged report carries: about a minute at 2 s.
	compactSamples = 30
)

// CompactHeader is the request header (value "compact") that asks for the compact stream,
// as ?compact=1 does. The hub echoes it on the response when it serves the compact stream.
const CompactHeader = "X-Machine-Monitor-Stream"

// stream is one SSE client. done is closed when the client is dropped (it fell behind, or
// the hub is closing); ch is never closed, so the handler can't drain a stale queue.
// Upstream-mode streams use ch; compact ones use pend instead.
type stream struct {
	ch   chan []byte
	done chan struct{}
	pend *pending
}

// pending is what a compact stream has yet to send, in order. Guarded by the hub's smu.
//
// A device's report waits in the slot its first waiting report took; newer reports merge
// into it (newest state, every sample), so events keep the order they happened in (a
// report before the actionResult it carried, a removal before a report that brings the
// device back) while each device holds one slot. Status and order events are whole-hub
// maps: only the newest of each waits, and they go last so they reflect every report.
//
// When the backlog overflows, a fresh snapshot replaces what the snapshot covers
// (registry, latest reports, online flags, order). What it doesn't cover is kept and sent
// after it: alerts, action results and pairings (the newest compactKeep), and the newest
// alert and energy settings.
type pending struct {
	queue   []entry        // waiting events, oldest first
	reports map[string]int // device id -> index in queue of its waiting report
	backlog int            // entries in queue that aren't reports
	status  []byte         // the newest "status" event
	order   []byte         // the newest "order" event
	resync  bool           // backlog overflowed: send a fresh snapshot first
	kept    []entry        // with resync: events the snapshot doesn't cover, oldest first
	lean    bool           // the link is slower than the stream: merged reports carry no samples
	wake    chan struct{}  // signalled (cap 1) when something is added
}

// entry is one waiting event; typ "" marks a report dropped by a removal.
type entry struct {
	typ  string
	data []byte         // the event as broadcast
	rep  *pendingReport // reports: the merged report, when more than one arrived
}

// pendingReport is a device's newest report with the samples of every report it merged.
type pendingReport struct {
	ev      reportEvent
	samples []history.Point // oldest first, at most compactSamples
}

// compactReport is a merged report: upstream's shape plus "samples", every sample it
// stands for (oldest first; "sample" is the newest). Clients that don't know "samples"
// still get the newest state.
type compactReport struct {
	reportEvent
	Samples []history.Point `json:"samples"`
}

// keptAcrossResync reports whether events of type typ carry something a snapshot doesn't.
func keptAcrossResync(typ string) bool {
	switch typ {
	case "alert", "actionResult", "paired", "alerts", "energySettings":
		return true
	}
	return false
}

// add queues one event for a compact client. msg is the event data was marshalled from.
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
			p.queue[i] = entry{} // a later report takes a new slot, after the removal
			delete(p.reports, id)
		}
		if p.backlog < compactBacklog {
			p.queue = append(p.queue, entry{typ: typ, data: data})
			p.backlog++
			break
		}
		// Overflow: a snapshot replaces the queue; keep what it can't rebuild.
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

// keep adds e to what survives a resync: the newest settings of each kind, and the newest
// compactKeep alerts, action results and pairings.
func (p *pending) keep(e entry) {
	isSettings := func(k entry) bool { return k.typ == "alerts" || k.typ == "energySettings" }
	switch {
	case isSettings(e):
		p.kept = slices.DeleteFunc(p.kept, func(k entry) bool { return k.typ == e.typ })
	case len(p.kept)-countFunc(p.kept, isSettings) >= compactKeep: // drop the oldest event
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

// newReport is a report's entry: its data, and its sample for merging into later.
func newReport(data []byte, msg any) entry {
	e := entry{typ: "report", data: data}
	if ev, ok := msg.(reportEvent); ok {
		e.rep = &pendingReport{ev: ev, samples: []history.Point{ev.Sample}}
	}
	return e
}

// mergeReport folds a newer report for the same device into a waiting one: the newest
// state, with the samples of both.
func mergeReport(old entry, data []byte, msg any) entry {
	e := newReport(data, msg)
	if old.rep != nil && e.rep != nil {
		samples := append(slices.Clone(old.rep.samples), e.rep.ev.Sample)
		e.rep.samples = samples[max(0, len(samples)-compactSamples):]
	}
	return e
}

// reset forgets everything waiting except what was kept across a resync.
func (p *pending) reset() {
	p.reports = map[string]int{}
	p.queue, p.backlog, p.status, p.order, p.resync = nil, 0, nil, nil, false
}

// take returns what is waiting, in the order to send it, and whether a snapshot must go
// first (the batch then starts with what was kept across the resync); it leaves p empty.
// With lean, a merged report goes as its newest report alone: on a link slower than the
// stream, the samples would take most of each batch and keep the state behind.
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

// eventKey is the type and device id of an event, for coalescing.
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

// drop ends s. Caller holds smu and has checked that s is still in h.streams.
func (h *Hub) drop(s *stream) {
	delete(h.streams, s)
	close(s.done)
}

// broadcast sends msg to every connected stream.
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

// addStream registers an upstream-mode stream; see addStreamMode.
func (h *Hub) addStream() *stream { return h.addStreamMode(false) }

// addStreamMode registers a stream, or returns nil when the hub is closed or full. The slot
// is held until the handler calls releaseStream, not merely until the stream is dropped, so
// a handler stuck in a write still counts against MaxStreams.
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

// releaseStream unregisters s (if still registered) and frees its slot.
func (h *Hub) releaseStream(s *stream) {
	h.smu.Lock()
	defer h.smu.Unlock()
	if _, ok := h.streams[s]; ok {
		h.drop(s)
	}
	h.activeStreams--
}

// wantsCompact reports whether a stream or device-list request opted in to compact mode.
func wantsCompact(r *http.Request) bool {
	return r.URL.Query().Get("compact") == "1" || strings.EqualFold(r.Header.Get(CompactHeader), "compact")
}

// compactViews lists every device without its history: the compact snapshot. Caller holds mu.
func (h *Hub) compactViews() []deviceEvent {
	out := []deviceEvent{}
	for _, id := range h.sortedIDs() {
		d := h.registry[id]
		out = append(out, deviceEvent{View{Device: *d, Online: h.online(d), Latest: h.latest[id]}})
	}
	return out
}

// snapshotEvent is the stream's opening "snapshot" message, compact or upstream.
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
	if owner := pairOwner(r); owner != "" { // a pair run: its code ends with its last stream
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
		defer gz.Close() // before hc.close: the client sees a clean end, not a cut-off one
	}
	// write writes one message under a fresh deadline. Deadlines are wall-clock time, so
	// they use time.Now, not the hub's (possibly injected) clock.
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

	// Registered before the snapshot is built, so no event between the two is lost. Events
	// that arrive while it is written wait in s (coalesced, in compact mode).
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
			case <-s.done: // dropped while this message waited: don't write to a slow client
				return
			default:
			}
			if !write(data) || !flush() {
				return
			}
		}
	}
}

// serveCompact sends a compact stream's batches: whatever is waiting, at most once every
// compactFlush. Each batch is built only once the previous one has been written; on a
// hijacked connection that means the link has carried all but streamUnsent bytes of it
// (boundSendQueue), so a slow client spends longer on each batch, more coalesces into the
// next one (lean: without samples), and it always receives the newest state.
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
		// A batch that took longer to send than the time between batches means the link
		// is slower than the stream: send the next one lean, until one goes quickly again.
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

// hijacked is a compact stream's connection, taken over from net/http (hijackStream). It
// writes the response body chunked.
type hijacked struct {
	conn   net.Conn
	bw     *bufio.Writer
	body   io.WriteCloser // chunked, into bw
	ctx    context.Context
	cancel context.CancelFunc
}

func (c *hijacked) Write(b []byte) (int, error) { return c.body.Write(b) }

// close ends the chunked body and the connection.
func (c *hijacked) close(timeout time.Duration) {
	c.conn.SetWriteDeadline(time.Now().Add(timeout))
	if c.body.Close() == nil {
		c.bw.WriteString("\r\n")
		c.bw.Flush()
	}
	c.conn.Close()
	c.cancel()
}

// hijackStream takes a compact stream's connection over from net/http and writes the
// response head (w's headers), so the stream can bound the connection's kernel send queue
// (boundSendQueue). Through net/http a write returns as soon as the kernel takes the bytes,
// and Linux takes megabytes: over a link slower than the stream, batches queued there for
// minutes, so nothing ever coalesced and the TUI showed data minutes old (skeptic report
// N1). Bounded, a write returns only once the link has carried most of it. It returns nil
// when the connection can't be taken over (HTTP/2, a test recorder); the stream then goes
// through w, unbounded.
func hijackStream(w http.ResponseWriter, r *http.Request) *hijacked {
	if r.ProtoMajor != 1 || r.ProtoMinor < 1 {
		return nil // chunked needs HTTP/1.1
	}
	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return nil
	}
	boundSendQueue(conn)
	ctx, cancel := context.WithCancel(r.Context())
	c := &hijacked{conn: conn, bw: brw.Writer, body: httputil.NewChunkedWriter(brw.Writer), ctx: ctx, cancel: cancel}
	// net/http no longer watches the connection, so notice the client leaving here. The
	// server's read deadline (ReadTimeout) would end the stream: clear it.
	conn.SetReadDeadline(time.Time{})
	go func() {
		io.Copy(io.Discard, brw.Reader)
		cancel()
	}()
	brw.WriteString("HTTP/1.1 200 OK\r\n")
	w.Header().Write(brw)
	brw.WriteString("Transfer-Encoding: chunked\r\nConnection: close\r\n\r\n")
	return c
}

// acceptsGzip reports whether the request's Accept-Encoding allows gzip.
func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if strings.EqualFold(strings.TrimSpace(name), "gzip") && strings.ReplaceAll(strings.TrimSpace(params), " ", "") != "q=0" {
			return true
		}
	}
	return false
}
