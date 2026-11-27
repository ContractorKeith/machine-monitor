package hub

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math/big"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/report"
)

const (
	PairTTL      = 10 * time.Minute
	PairTries    = 5
	pairMaxWrong = 100
)

type pairState struct {
	mu      sync.Mutex
	code    string
	id      int64
	expires time.Time
	tries   map[string]int
	wrong   int
	owner   string
	streams map[string]int
}

var pairOwnerRE = regexp.MustCompile(`^[0-9a-f]{16,64}$`)

func pairOwner(r *http.Request) string {
	if o := r.URL.Query().Get("pair"); pairOwnerRE.MatchString(o) {
		return o
	}
	return ""
}

const PairGrace = 15 * time.Second

func (h *Hub) watchPair(owner string) (done func()) {
	p := &h.pair
	p.mu.Lock()
	if p.streams == nil {
		p.streams = map[string]int{}
	}
	p.streams[owner]++
	p.mu.Unlock()
	return func() {
		p.mu.Lock()
		p.streams[owner]--
		last := p.streams[owner] <= 0
		if last {
			delete(p.streams, owner)
		}
		id := p.id
		p.mu.Unlock()
		if !last {
			return
		}
		grace := h.pairGrace
		if grace == 0 {
			grace = PairGrace
		}
		time.AfterFunc(grace, func() {
			p.mu.Lock()
			end := p.activeLocked(h.now()) && p.owner == owner && p.id == id && p.streams[owner] == 0
			if end {
				p.code = ""
			}
			p.mu.Unlock()
			if end {
				h.broadcast(map[string]any{"type": "pairEnded", "id": id, "reason": "abandoned"})
			}
		})
	}
}

func (p *pairState) activeLocked(now time.Time) bool {
	if p.code != "" && now.After(p.expires) {
		p.code = ""
	}
	return p.code != ""
}

const (
	tooManyTries = "Wrong pairing code. Too many tries: get a new code from the hub."
	codeActive   = "A pairing code is already active (from another machine-monitor pair, or the TUI's Add device). Use that code, or wait until it is used or expires."
)

var fullMessage = fmt.Sprintf("This hub already has %d devices, its limit. Remove one (Delete in the TUI) before adding another.", MaxDevices)

func (h *Hub) full() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.registry) >= MaxDevices
}

func (h *Hub) handlePairCode(w http.ResponseWriter, r *http.Request) {
	body, _, err := readObject(w, r)
	if err != nil {
		sendJSON(w, http.StatusBadRequest, errorBody(err.Error()))
		return
	}
	replace, set := body["replace"].(bool)
	replace = replace || !set
	owner, _ := body["owner"].(string)
	if owner != "" && !pairOwnerRE.MatchString(owner) {
		sendJSON(w, http.StatusBadRequest, errorBody("bad owner"))
		return
	}
	if h.full() {
		sendJSON(w, http.StatusConflict, errorBody(fullMessage))
		return
	}
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		sendJSON(w, http.StatusInternalServerError, errorBody("internal error"))
		return
	}
	p := &h.pair
	p.mu.Lock()
	var ended map[string]any
	if owner != "" && p.streams[owner] == 0 {
		p.mu.Unlock()
		sendJSON(w, http.StatusConflict, errorBody("This pair run's live stream from the hub has closed. Run machine-monitor pair again."))
		return
	}
	if p.activeLocked(h.now()) {
		if !replace {
			expires := p.expires
			p.mu.Unlock()
			sendJSON(w, http.StatusConflict, map[string]any{"error": codeActive, "active": true, "expires": expires.UnixMilli()})
			return
		}
		ended = map[string]any{"type": "pairEnded", "id": p.id, "reason": "replaced"}
	}
	p.id++
	p.code, p.expires, p.tries, p.wrong = fmt.Sprintf("%06d", n.Int64()), h.now().Add(PairTTL), map[string]int{}, 0
	p.owner = owner
	code, expires, id := p.code, p.expires, p.id
	p.mu.Unlock()
	if ended != nil {
		h.broadcast(ended)
	}
	sendJSON(w, http.StatusOK, map[string]any{"code": code, "expires": expires.UnixMilli(), "id": id})
}

func (h *Hub) handleCancelPairCode(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err != nil {
		sendJSON(w, http.StatusBadRequest, errorBody("bad id"))
		return
	}
	p := &h.pair
	p.mu.Lock()
	cancelled := p.activeLocked(h.now()) && p.id == id
	if cancelled {
		p.code = ""
	}
	p.mu.Unlock()
	if cancelled {
		h.broadcast(map[string]any{"type": "pairEnded", "id": id, "reason": "cancelled"})
	}
	sendJSON(w, http.StatusOK, map[string]bool{"ok": cancelled})
}

func (h *Hub) handlePair(w http.ResponseWriter, r *http.Request) {
	body, _, err := readObject(w, r)
	if err != nil {
		sendJSON(w, http.StatusBadRequest, errorBody(err.Error()))
		return
	}
	device, _ := body["deviceId"].(string)
	if device != "" && !report.ValidID(device) {
		sendJSON(w, http.StatusBadRequest, errorBody("invalid device id"))
		return
	}
	given := digitsOnly(fmt.Sprint(body["code"]))
	client, _ := h.clientAddr(r)
	src := sourceKey(client)
	full := h.full()
	p := &h.pair
	p.mu.Lock()
	if !p.activeLocked(h.now()) {
		p.mu.Unlock()
		sendJSON(w, http.StatusForbidden, errorBody("No pairing code is active. On the hub, run machine-monitor pair to get one."))
		return
	}
	if full {
		p.mu.Unlock()
		sendJSON(w, http.StatusConflict, map[string]any{"error": fullMessage, "retry": false})
		return
	}
	if p.tries[src] >= PairTries {
		p.mu.Unlock()
		sendJSON(w, http.StatusForbidden, map[string]any{"error": tooManyTries, "retry": false, "triesLeft": 0})
		return
	}
	if !safeEqual(given, p.code) {
		p.tries[src]++
		p.wrong++
		left := PairTries - p.tries[src]
		if p.wrong >= pairMaxWrong {
			p.code, left = "", 0
		}
		p.mu.Unlock()
		msg := "Wrong pairing code."
		if left == 0 {
			msg = tooManyTries
		}
		sendJSON(w, http.StatusForbidden, map[string]any{"error": msg, "retry": left > 0, "triesLeft": left})
		return
	}
	p.code = ""
	id := p.id
	p.mu.Unlock()
	reply := map[string]string{"token": h.Token()}
	if device != "" {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			sendJSON(w, http.StatusInternalServerError, errorBody("internal error"))
			return
		}
		key := hex.EncodeToString(b)
		h.mu.Lock()
		if h.cfg.DeviceKeys == nil {
			h.cfg.DeviceKeys = map[string]string{}
		}
		old := h.cfg.DeviceKeys[device]
		h.cfg.DeviceKeys[device] = key
		err := h.saveConfig()
		if err != nil {
			if old == "" {
				delete(h.cfg.DeviceKeys, device)
			} else {
				h.cfg.DeviceKeys[device] = old
			}
		}
		h.mu.Unlock()
		if err != nil {
			sendJSON(w, http.StatusInternalServerError, errorBody("could not save device key"))
			return
		}
		reply["deviceKey"] = key
	}
	h.broadcast(map[string]any{"type": "paired", "id": id})
	sendJSON(w, http.StatusOK, reply)
}

func digitsOnly(s string) string {
	var b strings.Builder
	for _, c := range s {
		if c >= '0' && c <= '9' {
			b.WriteRune(c)
		}
	}
	return b.String()
}
