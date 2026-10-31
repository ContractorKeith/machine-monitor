package hub

// Pairing: a short one-time code shown on the hub lets a new machine fetch the hub key
// without anyone typing the key. One code at a time; it dies after PairTTL or pairMaxWrong
// wrong guesses in all. Each source gets PairTries guesses at a code (upstream counts them
// for everyone together, so one peer could burn every code). POST /api/pair is open, so it
// is rate-limited like /api/info (see guard). `machine-monitor pair` and the TUI's Add
// device issue codes. Each code has an id, carried by the "paired" and "pairEnded" events,
// so whoever shows a code knows when that code, not some other one, was used or replaced.
//
// `machine-monitor pair` names itself with a random owner on both its stream
// (/api/stream?pair=OWNER) and its code request. When the last stream with that owner ends,
// its code ends too, so a pair killed without a chance to cancel (SIGKILL, a crash) doesn't
// leave a code nobody shows (skeptic report N3).
//
// Ported from Forward-Future/device-monitor server.js @ c3a4c41
// (MIT, © 2026 Matt): https://github.com/Forward-Future/device-monitor

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Pairing limits. PairTTL is upstream's; upstream's 5 tries were for everyone together.
const (
	PairTTL   = 10 * time.Minute // a code is valid this long
	PairTries = 5                // wrong guesses one source may make at a code
	// pairMaxWrong wrong guesses from all sources kill the code, which caps the odds of
	// guessing it at 1 in 10,000 however many addresses a peer can use.
	pairMaxWrong = 100
)

// pairState is the one active pairing code, if any.
type pairState struct {
	mu      sync.Mutex
	code    string // "" when no code is active
	id      int64  // the current or last code's id
	expires time.Time
	tries   map[string]int // source (sourceKey) -> wrong guesses at this code
	wrong   int            // wrong guesses from every source
	owner   string         // the pair run showing this code; "" for the TUI's Add device
	streams map[string]int // owner -> open streams that named it
}

// pairOwnerRE is what an owner may look like: pair sends 32 hex digits.
var pairOwnerRE = regexp.MustCompile(`^[0-9a-f]{16,64}$`)

// pairOwner is the stream's ?pair= owner, or "" when it has none (or a malformed one).
func pairOwner(r *http.Request) string {
	if o := r.URL.Query().Get("pair"); pairOwnerRE.MatchString(o) {
		return o
	}
	return ""
}

// watchPair counts a stream that names owner; the returned func, called when the stream
// ends, ends owner's code if that was its last stream.
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
		var id int64
		if p.streams[owner] <= 0 {
			delete(p.streams, owner)
			if p.activeLocked(h.now()) && p.owner == owner {
				p.code, id = "", p.id
			}
		}
		p.mu.Unlock()
		if id != 0 {
			h.broadcast(map[string]any{"type": "pairEnded", "id": id, "reason": "abandoned"})
		}
	}
}

// activeLocked reports whether a code is live, dropping an expired one. Caller holds mu.
func (p *pairState) activeLocked(now time.Time) bool {
	if p.code != "" && now.After(p.expires) {
		p.code = ""
	}
	return p.code != ""
}

// Pairing refusals, in the words clients show.
const (
	tooManyTries = "Wrong pairing code. Too many tries: get a new code from the hub."
	codeActive   = "A pairing code is already active (from another machine-monitor pair, or the TUI's Add device). Use that code, or wait until it is used or expires."
)

// fullMessage is why a full hub refuses to pair.
var fullMessage = fmt.Sprintf("This hub already has %d devices, its limit. Remove one (Delete in the TUI) before adding another.", MaxDevices)

// full reports whether the registry is at MaxDevices, so a new device could never report.
func (h *Hub) full() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.registry) >= MaxDevices
}

// handlePairCode (key) issues a new code. A body of {"replace": false} asks for a code only
// if none is active (409 otherwise), which is what `machine-monitor pair` sends, so two runs
// can't hand out two codes of which only one works. "owner" ties the code to the caller's
// open stream (see watchPair); with no such stream open, no code is issued. Without it the new code replaces the
// old one, as upstream, and a "pairEnded" event tells whoever showed the old one.
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
	if owner != "" && p.streams[owner] == 0 { // its stream already ended: nobody would show the code
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

// handleCancelPairCode (key) is DELETE /api/pair/code?id=N: `machine-monitor pair` ends its
// own code when it stops, so the next run needn't wait out the old one. Another id is left
// alone.
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

// handlePair (open) trades the active code for the hub key. Non-digits in the code are
// ignored, so "123 456" works. The compare is constant-time.
func (h *Hub) handlePair(w http.ResponseWriter, r *http.Request) {
	body, _, err := readObject(w, r)
	if err != nil {
		sendJSON(w, http.StatusBadRequest, errorBody(err.Error()))
		return
	}
	given := digitsOnly(fmt.Sprint(body["code"]))
	client, _ := h.clientAddr(r)
	src := sourceKey(client)
	full := h.full() // before p.mu: never hold p.mu while waiting on h.mu
	p := &h.pair
	p.mu.Lock()
	if !p.activeLocked(h.now()) {
		p.mu.Unlock()
		sendJSON(w, http.StatusForbidden, errorBody("No pairing code is active. On the hub, run machine-monitor pair to get one."))
		return
	}
	if full { // filled up since the code was issued; no guess is counted
		p.mu.Unlock()
		sendJSON(w, http.StatusConflict, map[string]any{"error": fullMessage, "retry": false})
		return
	}
	if p.tries[src] >= PairTries { // even the right code: this source has had its guesses
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
		// retry and triesLeft are additive to upstream's reply, so clients needn't parse msg.
		sendJSON(w, http.StatusForbidden, map[string]any{"error": msg, "retry": left > 0, "triesLeft": left})
		return
	}
	p.code = ""
	id := p.id
	p.mu.Unlock()
	h.broadcast(map[string]any{"type": "paired", "id": id})
	sendJSON(w, http.StatusOK, map[string]string{"token": h.Token()})
}

// digitsOnly drops everything but ASCII digits. A missing code (JSON null) reads as "".
func digitsOnly(s string) string {
	var b strings.Builder
	for _, c := range s {
		if c >= '0' && c <= '9' {
			b.WriteRune(c)
		}
	}
	return b.String()
}
