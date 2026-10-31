package hub

// Pairing: a short one-time code shown on the hub lets a new machine fetch the hub key
// without anyone typing the key. One code at a time; it dies after PairTTL or PairTries
// wrong guesses. POST /api/pair is open, so it is rate-limited like /api/info (see guard).
//
// Ported from Forward-Future/device-monitor server.js @ c3a4c41
// (MIT, © 2026 Matt): https://github.com/Forward-Future/device-monitor

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Pairing limits, as upstream.
const (
	PairTTL   = 10 * time.Minute // a code is valid this long
	PairTries = 5                // wrong guesses before the code dies
)

// pairState is the one active pairing code, if any.
type pairState struct {
	mu      sync.Mutex
	code    string // "" when no code is active
	expires time.Time
	tries   int
}

// handlePairCode (key) issues a new code, replacing any earlier one.
func (h *Hub) handlePairCode(w http.ResponseWriter, r *http.Request) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		sendJSON(w, http.StatusInternalServerError, errorBody("internal error"))
		return
	}
	p := &h.pair
	p.mu.Lock()
	p.code, p.expires, p.tries = fmt.Sprintf("%06d", n.Int64()), h.now().Add(PairTTL), 0
	code, expires := p.code, p.expires
	p.mu.Unlock()
	sendJSON(w, http.StatusOK, map[string]any{"code": code, "expires": expires.UnixMilli()})
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
	p := &h.pair
	p.mu.Lock()
	if p.code == "" || h.now().After(p.expires) {
		p.code = ""
		p.mu.Unlock()
		sendJSON(w, http.StatusForbidden, errorBody("No pairing code is active. On the hub, get a new pairing code."))
		return
	}
	if !safeEqual(given, p.code) {
		p.tries++
		dead := p.tries >= PairTries
		if dead {
			p.code = ""
		}
		p.mu.Unlock()
		msg := "Wrong pairing code."
		if dead {
			msg = "Wrong pairing code. Too many tries: get a new code from the hub."
		}
		sendJSON(w, http.StatusForbidden, errorBody(msg))
		return
	}
	p.code = ""
	p.mu.Unlock()
	h.broadcast(map[string]any{"type": "paired"})
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
