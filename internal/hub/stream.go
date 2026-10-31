package hub

// Server-sent events: GET /api/stream. Each message is one `data: <json>` line whose JSON
// carries its own "type", as upstream: snapshot, report, status, order, device, removed,
// alert, alerts, actionResult, energySettings (and paired once pairing lands).

import (
	"encoding/json"
	"net/http"
)

// streamBuffer is how many messages a slow client may fall behind before it is dropped. A
// TUI reconnects and gets a fresh snapshot, so dropping beats blocking every report.
const streamBuffer = 256

type stream struct{ ch chan []byte }

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
			delete(h.streams, s)
			close(s.ch)
		}
	}
}

// addStream registers a stream, or returns nil when the hub is closed or full.
func (h *Hub) addStream() *stream {
	h.smu.Lock()
	defer h.smu.Unlock()
	if h.closed || len(h.streams) >= MaxStreams {
		return nil
	}
	s := &stream{ch: make(chan []byte, streamBuffer)}
	h.streams[s] = struct{}{}
	return s
}

func (h *Hub) removeStream(s *stream) {
	h.smu.Lock()
	defer h.smu.Unlock()
	if _, ok := h.streams[s]; ok {
		delete(h.streams, s)
		close(s.ch)
	}
}

func (h *Hub) handleStream(w http.ResponseWriter, r *http.Request) {
	s := h.addStream()
	if s == nil {
		sendJSON(w, http.StatusServiceUnavailable, errorBody("too many connections"))
		return
	}
	defer h.removeStream(s)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)

	// Registered before the snapshot is built, so no event between the two is lost.
	h.mu.Lock()
	snap := map[string]any{"type": "snapshot", "devices": h.views()}
	h.mu.Unlock()
	b, _ := json.Marshal(snap)
	if _, err := w.Write(append(append([]byte("data: "), b...), '\n', '\n')); err != nil {
		return
	}
	if rc.Flush() != nil {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case data, ok := <-s.ch:
			if !ok {
				return
			}
			if _, err := w.Write(data); err != nil || rc.Flush() != nil {
				return
			}
		}
	}
}
