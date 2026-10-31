package hub

// Server-sent events: GET /api/stream. Each message is one `data: <json>` line whose JSON
// carries its own "type", as upstream: snapshot, report, status, order, device, removed,
// alert, alerts, actionResult, energySettings (and paired once pairing lands).

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// streamBuffer is how many messages a slow client may fall behind before it is dropped. A
// TUI reconnects and gets a fresh snapshot, so dropping beats blocking every report.
const streamBuffer = 256

// StreamWriteTimeout bounds each write to an SSE client. A client that stops reading fails
// its next write within this time, which ends its handler and frees its connection.
const StreamWriteTimeout = 30 * time.Second

// stream is one SSE client. done is closed when the client is dropped (it fell behind, or
// the hub is closing); ch is never closed, so the handler can't drain a stale queue.
type stream struct {
	ch   chan []byte
	done chan struct{}
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
	h.smu.Lock()
	defer h.smu.Unlock()
	for s := range h.streams {
		select {
		case s.ch <- data:
		default:
			h.drop(s)
		}
	}
}

// addStream registers a stream, or returns nil when the hub is closed or full. The slot is
// held until the handler calls releaseStream, not merely until the stream is dropped, so a
// handler stuck in a write still counts against MaxStreams.
func (h *Hub) addStream() *stream {
	h.smu.Lock()
	defer h.smu.Unlock()
	if h.closed || h.activeStreams >= MaxStreams {
		return nil
	}
	s := &stream{ch: make(chan []byte, streamBuffer), done: make(chan struct{})}
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

func (h *Hub) handleStream(w http.ResponseWriter, r *http.Request) {
	s := h.addStream()
	if s == nil {
		sendJSON(w, http.StatusServiceUnavailable, errorBody("too many connections"))
		return
	}
	defer h.releaseStream(s)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	rc := http.NewResponseController(w)
	timeout := h.streamWriteTimeout
	if timeout == 0 {
		timeout = StreamWriteTimeout
	}
	// send writes and flushes one message under a fresh deadline. Deadlines are wall-clock
	// time, so they use time.Now, not the hub's (possibly injected) clock.
	send := func(data []byte) bool {
		if err := rc.SetWriteDeadline(time.Now().Add(timeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return false
		}
		_, err := w.Write(data)
		return err == nil && rc.Flush() == nil
	}

	// Registered before the snapshot is built, so no event between the two is lost.
	h.mu.Lock()
	snap := map[string]any{"type": "snapshot", "devices": h.views()}
	h.mu.Unlock()
	b, _ := json.Marshal(snap)
	w.WriteHeader(http.StatusOK)
	if !send(append(append([]byte("data: "), b...), '\n', '\n')) {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.done:
			return
		case data := <-s.ch:
			select {
			case <-s.done: // dropped while this message waited: don't write to a slow client
				return
			default:
			}
			if !send(data) {
				return
			}
		}
	}
}
