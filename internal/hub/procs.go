package hub

import (
	"cmp"
	"net/http"
	"slices"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/report"
)

const ProcsWatch = 20 * time.Second

const maxProcs = 120

type ProcSnapshot struct {
	T    int64            `json:"t"`
	List []report.Process `json:"list"`
}

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
	if out.Supported {
		h.act.watch[id] = now.Add(ProcsWatch)
	}
	if p := h.act.procs[id]; p != nil && now.UnixMilli()-p.T < ProcsWatch.Milliseconds() {
		out.Procs = p
	}
	h.act.mu.Unlock()
	sendJSON(w, http.StatusOK, out)
}

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
