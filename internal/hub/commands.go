package hub

// Remote actions (PLAN §3 rows 24–25, M7): POST /api/devices/:id/action. "wake" sends
// Wake-on-LAN packets from the hub to the device's stored MACs. "sleep", "restart" and
// "shutdown" are queued for the device's next report, HMAC-signed with the hub key, and
// only for a device whose agent advertises the action (it runs with --allow-power-actions).
//
// Upstream signs "device-monitor-cmd:<id>:<action>". Here the signature also covers the
// device id and the time the command was queued, so an agent can refuse a command copied
// from another device's reply or replayed after its TTL; see SignCommand.
//
// What the signature does not do: the key is the hub token, which every paired agent and
// TUI holds. It stops someone on the network path who lacks the key from injecting or
// altering a command, but any key holder can sign a valid command for any device. Per-device
// keys are issue #43.
//
// Ported from Forward-Future/device-monitor server.js (commands, /api/devices/:id/action)
// @ c3a4c41 (MIT, © 2026 Matt): https://github.com/Forward-Future/device-monitor

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/discovery"
	"github.com/ContractorKeith/machine-monitor/internal/report"
)

// CommandTTL is how long a queued power action waits for the device's next report, and
// how long an agent accepts it after it was queued, as upstream.
const CommandTTL = 60 * time.Second

// wakeFunc sends the magic packets; tests replace it.
var wakeFunc = discovery.Wake

// queuedCmd is a power action waiting for the device's next report.
type queuedCmd struct {
	id     string
	action string
	t      time.Time
}

// actionState is the command queue and the processes-on-demand state (procs.go). It has
// its own lock, never held while taking mu.
type actionState struct {
	mu       sync.Mutex
	commands map[string][]queuedCmd   // device id -> commands for its next report
	watch    map[string]time.Time     // device id -> send processes until
	procs    map[string]*ProcSnapshot // device id -> last process list it sent
}

// SignCommand is hex(HMAC-SHA256(key, "machine-monitor-cmd:<device>:<id>:<action>:<t>")),
// t in Unix ms. Agents check it before running a command.
func SignCommand(key, device, id, action string, t int64) string {
	m := hmac.New(sha256.New, []byte(key))
	fmt.Fprintf(m, "machine-monitor-cmd:%s:%s:%s:%d", device, id, action, t)
	return hex.EncodeToString(m.Sum(nil))
}

// newCommandID is 6 random bytes in hex: the 12 hex chars actionResult.id must match.
func newCommandID() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (h *Hub) handleAction(w http.ResponseWriter, r *http.Request) {
	id := h.deviceFromPath(r)
	if id == "" {
		sendJSON(w, http.StatusNotFound, errorBody("not found"))
		return
	}
	body, _, err := readObject(w, r)
	if err != nil {
		sendJSON(w, http.StatusBadRequest, errorBody(err.Error()))
		return
	}
	action, _ := body["action"].(string)

	h.mu.Lock()
	d := h.registry[id]
	if d == nil { // removed meanwhile
		h.mu.Unlock()
		sendJSON(w, http.StatusNotFound, errorBody("not found"))
		return
	}
	name, macs, online := d.Name, slices.Clone(d.MACs), h.online(d)
	var allowed []string
	if l := h.latest[id]; l != nil {
		allowed = l.Actions
	}
	h.mu.Unlock()

	switch {
	case action == "wake":
		if len(macs) == 0 {
			sendJSON(w, http.StatusBadRequest, errorBody("This device hasn't reported a network card address yet."))
			return
		}
		if err := wakeFunc(macs); err != nil {
			h.logf("hub: wake %s: %v", id, err)
			sendJSON(w, http.StatusInternalServerError, errorBody("Couldn't send the wake-up signal."))
			return
		}
		h.broadcast(map[string]any{"type": "actionResult", "id": id, "name": name,
			"result": map[string]any{"ok": true, "message": "Wake-up signal sent."}})
		sendJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	case !report.ValidAction(action):
		sendJSON(w, http.StatusBadRequest, errorBody("unknown action"))
		return
	case !online:
		sendJSON(w, http.StatusConflict, errorBody(name+" is offline."))
		return
	case !slices.Contains(allowed, action):
		sendJSON(w, http.StatusConflict, errorBody("Remote power control is turned off on "+name+"."))
		return
	}
	cid, err := newCommandID()
	if err != nil {
		sendJSON(w, http.StatusInternalServerError, errorBody("internal error"))
		return
	}
	h.act.mu.Lock()
	if h.act.commands == nil {
		h.act.commands = map[string][]queuedCmd{}
	}
	// One of each action at a time: asking again replaces the earlier one, as upstream.
	q := slices.DeleteFunc(h.act.commands[id], func(c queuedCmd) bool { return c.action == action })
	h.act.commands[id] = append(q, queuedCmd{id: cid, action: action, t: h.now()})
	h.act.mu.Unlock()
	sendJSON(w, http.StatusOK, map[string]any{"ok": true, "id": cid})
}

// replyFor builds the report reply for device id: whether a viewer wants its processes,
// and its queued commands still within CommandTTL that its report allows (actions), signed.
// The queue is emptied either way: a command is delivered at most once.
func (h *Hub) replyFor(id string, actions []string) reportReply {
	key := h.Token()
	now := h.now()
	rep := reportReply{OK: true}
	h.act.mu.Lock()
	queued := h.act.commands[id]
	delete(h.act.commands, id)
	rep.Procs = h.watchedLocked(id, now)
	h.act.mu.Unlock()
	for _, c := range queued {
		if now.Sub(c.t) >= CommandTTL || !slices.Contains(actions, c.action) {
			continue
		}
		t := c.t.UnixMilli()
		rep.Commands = append(rep.Commands, command{ID: c.id, Action: c.action, T: t,
			Sig: SignCommand(key, id, c.id, c.action, t)})
	}
	return rep
}

// forgetActions drops a removed device's queue, watch and process list.
func (h *Hub) forgetActions(id string) {
	h.act.mu.Lock()
	defer h.act.mu.Unlock()
	delete(h.act.commands, id)
	delete(h.act.watch, id)
	delete(h.act.procs, id)
}
