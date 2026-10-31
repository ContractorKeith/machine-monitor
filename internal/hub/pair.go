package hub

// Pairing: a short one-time code shown on the hub lets a new machine fetch the hub key
// without anyone typing the key. One code at a time; it dies after PairTTL or pairMaxWrong
// wrong guesses in all. Each source gets PairTries guesses at a code (upstream counts them
// for everyone together, so one peer could burn every code). POST /api/pair is open, so it
// is rate-limited like /api/info (see guard). `machine-monitor pair` issues codes.
//
// Ported from Forward-Future/device-monitor server.js @ c3a4c41
// (MIT, © 2026 Matt): https://github.com/Forward-Future/device-monitor

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"net/http"
	"net/netip"
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
	expires time.Time
	tries   map[string]int // source (pairSource) -> wrong guesses at this code
	wrong   int            // wrong guesses from every source
}

// pairSource is who a guess is counted against: the client's IPv4 address, or its IPv6 /64,
// since one machine can pick any address in its /64.
func pairSource(a netip.Addr) string {
	if a.Is6() && !a.Is4In6() {
		p, _ := a.WithZone("").Prefix(64)
		return p.String()
	}
	return a.Unmap().String()
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
	p.code, p.expires, p.tries, p.wrong = fmt.Sprintf("%06d", n.Int64()), h.now().Add(PairTTL), map[string]int{}, 0
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
	client, _ := h.clientAddr(r)
	src := pairSource(client)
	p := &h.pair
	p.mu.Lock()
	if p.code == "" || h.now().After(p.expires) {
		p.code = ""
		p.mu.Unlock()
		sendJSON(w, http.StatusForbidden, errorBody("No pairing code is active. On the hub, run machine-monitor pair to get one."))
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
	p.mu.Unlock()
	h.broadcast(map[string]any{"type": "paired"})
	sendJSON(w, http.StatusOK, map[string]string{"token": h.Token()})
}

const tooManyTries = "Wrong pairing code. Too many tries: get a new code from the hub."

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
