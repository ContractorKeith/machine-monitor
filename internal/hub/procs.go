package hub

// Processes on demand (PLAN §3 row 9, M7): GET /api/devices/:id/processes. A device sends
// its process list only while a viewer keeps asking: each GET extends the watch by
// ProcsWatch, the report reply carries procs:true while it lasts, and the agent stops
// sending once a viewer leaves and the watch runs out.
//
// Ported from Forward-Future/device-monitor server.js (procWatch, /api/devices/:id/processes)
// @ c3a4c41 (MIT, © 2026 Matt): https://github.com/Forward-Future/device-monitor

import (
	"cmp"
	"net/http"
	"slices"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/report"
)

// ProcsWatch is how long one GET keeps a device sending processes, and how old a list may
// be and still be served, as upstream.
const ProcsWatch = 20 * time.Second

// maxProcs is how many rows report.Normalize keeps (upstream normalizeProcesses).
const maxProcs = 120

// ProcSnapshot is a device's process list and when it arrived (Unix ms).
type ProcSnapshot struct {
	T    int64            `json:"t"`
	List []report.Process `json:"list"`
}

// ProcessesReply is the body of GET /api/devices/:id/processes. Procs is nil until the
// device has sent a list in the last ProcsWatch; Supported says whether its agent can.
type ProcessesReply struct {
	Procs     *ProcSnapshot `json:"procs"`
	Supported bool          `json:"supported"`
	Threads   *float64      `json:"threads"`
}

func (h *Hub) handleProcesses(w http.ResponseWriter, r *http.Request) {
	id := h.deviceFromPath(r)
	if id == "" {
		sendJSON(w, http.StatusNotFound, errorBody("not found"))
		return
	}
	var out ProcessesReply
	h.mu.Lock()
	if l := h.latest[id]; l != nil {
		out.Supported = slices.Contains(l.Features, "procs")
		out.Threads = l.CPU.Threads
	}
	h.mu.Unlock()
	now := h.now()
	h.act.mu.Lock()
	if h.act.watch == nil {
		h.act.watch = map[string]time.Time{}
	}
	h.act.watch[id] = now.Add(ProcsWatch)
	if p := h.act.procs[id]; p != nil && now.UnixMilli()-p.T < ProcsWatch.Milliseconds() {
		out.Procs = p
	}
	h.act.mu.Unlock()
	sendJSON(w, http.StatusOK, out)
}

// storeProcs keeps the process list a device just sent, while someone is watching it (a
// list nobody asked for is dropped). Lists are never changed after they are stored, so
// handlers may share them.
func (h *Hub) storeProcs(id string, list []report.Process) {
	h.act.mu.Lock()
	defer h.act.mu.Unlock()
	if !h.watchedLocked(id, h.now()) {
		return
	}
	if h.act.procs == nil {
		h.act.procs = map[string]*ProcSnapshot{}
	}
	h.act.procs[id] = &ProcSnapshot{T: h.nowMS(), List: list}
}

// watchedLocked reports whether a viewer still wants id's processes. An expired watch is
// dropped with the list it gathered. Caller holds act.mu.
func (h *Hub) watchedLocked(id string, now time.Time) bool {
	until, ok := h.act.watch[id]
	if ok && now.Before(until) {
		return true
	}
	if ok {
		delete(h.act.watch, id)
		delete(h.act.procs, id)
	}
	return false
}

// busiest sorts list by CPU, then memory, largest first (unknown last), and keeps n rows,
// so the hub's own list keeps the rows that matter before Normalize cuts it.
func busiest(list []report.Process, n int) []report.Process {
	v := func(p *float64) float64 {
		if p == nil {
			return -1
		}
		return *p
	}
	slices.SortStableFunc(list, func(a, b report.Process) int {
		if c := cmp.Compare(v(b.CPU), v(a.CPU)); c != 0 {
			return c
		}
		return cmp.Compare(v(b.Mem), v(a.Mem))
	})
	return list[:min(n, len(list))]
}
