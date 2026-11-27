package hub

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

const CommandTTL = 60 * time.Second

var wakeFunc = discovery.Wake

type queuedCmd struct {
	id     string
	action string
	t      time.Time
}

type actionState struct {
	mu       sync.Mutex
	commands map[string][]queuedCmd
	watch    map[string]time.Time
	procs    map[string]*ProcSnapshot
}

func SignCommand(key, device, id, action string, t int64) string {
	m := hmac.New(sha256.New, []byte(key))
	fmt.Fprintf(m, "machine-monitor-cmd:%s:%s:%s:%d", device, id, action, t)
	return hex.EncodeToString(m.Sum(nil))
}

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
	if d == nil {
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
	q := slices.DeleteFunc(h.act.commands[id], func(c queuedCmd) bool { return c.action == action })
	h.act.commands[id] = append(q, queuedCmd{id: cid, action: action, t: h.now()})
	h.act.mu.Unlock()
	sendJSON(w, http.StatusOK, map[string]any{"ok": true, "id": cid})
}

func (h *Hub) replyFor(id string, actions []string) reportReply {
	h.mu.Lock()
	key := h.cfg.DeviceKeys[id]
	if key == "" {
		key = h.cfg.Token
	}
	h.mu.Unlock()
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

func (h *Hub) forgetActions(id string) {
	h.act.mu.Lock()
	defer h.act.mu.Unlock()
	delete(h.act.commands, id)
	delete(h.act.watch, id)
	delete(h.act.procs, id)
}
