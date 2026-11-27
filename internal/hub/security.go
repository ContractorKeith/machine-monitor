package hub

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

var baseHeaders = map[string]string{
	"X-Content-Type-Options":       "nosniff",
	"Referrer-Policy":              "no-referrer",
	"Cross-Origin-Resource-Policy": "same-origin",
	"Content-Security-Policy":      "default-src 'none'; frame-ancestors 'none'; base-uri 'none'",
	"X-Frame-Options":              "DENY",
}

var hostnameRE = regexp.MustCompile(`^[a-z0-9_]([a-z0-9_-]*[a-z0-9_])?(\.[a-z0-9_]([a-z0-9_-]*[a-z0-9_])?)*$`)

func hostAllowed(hostHeader, me string, extra []string) bool {
	host := strings.ToLower(strings.TrimSpace(hostHeader))
	if host == "" {
		return false
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	if strings.HasPrefix(host, "[") {
		end := strings.Index(host, "]")
		if end < 0 {
			return false
		}
		host = host[1:end]
	} else if i := strings.LastIndex(host, ":"); i >= 0 && allDigits(host[i+1:]) {
		host = host[:i]
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	if len(host) > 253 || !hostnameRE.MatchString(host) {
		return false
	}
	me = strings.ToLower(me)
	switch host {
	case "localhost", me, me + ".local", me + ".lan", me + ".home", me + ".home.arpa":
		return true
	}
	for _, h := range extra {
		if strings.ToLower(h) == host {
			return true
		}
	}
	return strings.HasPrefix(host, me+".") && strings.HasSuffix(host, ".ts.net")
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func isPrivateIPv4(a netip.Addr) bool {
	b := a.As4()
	return b[0] == 10 || b[0] == 127 || (b[0] == 172 && b[1] >= 16 && b[1] <= 31) ||
		(b[0] == 192 && b[1] == 168) || (b[0] == 169 && b[1] == 254) || (b[0] == 100 && b[1] >= 64 && b[1] <= 127)
}

func isLocalAddress(remoteAddr string) bool {
	a, ok := remoteIP(remoteAddr)
	if !ok {
		return false
	}
	if a.Is4() {
		return isPrivateIPv4(a)
	}
	b := a.As16()
	return a == netip.IPv6Loopback() ||
		(b[0] == 0xfe && b[1]&0xc0 == 0x80) ||
		b[0]&0xfe == 0xfc
}

func remoteIP(remoteAddr string) (netip.Addr, bool) {
	if ap, err := netip.ParseAddrPort(remoteAddr); err == nil {
		return ap.Addr().Unmap(), true
	}
	if a, err := netip.ParseAddr(remoteAddr); err == nil {
		return a.Unmap(), true
	}
	return netip.Addr{}, false
}

func (h *Hub) clientAddr(r *http.Request) (client, socket netip.Addr) {
	socket, _ = remoteIP(r.RemoteAddr)
	client = socket
	if socket.IsLoopback() {
		return client, socket
	}
	trusted := h.trustedProxies()
	if !trusted.contains(socket) {
		return client, socket
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0 && i >= len(hops)-maxForwardHops; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			return client, socket
		}
		a = a.Unmap().WithZone("")
		if !trusted.contains(a) {
			return a, socket
		}
	}
	return client, socket
}

const maxForwardHops = 16

func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	if err != nil || u.Host == "" {
		return false
	}
	host := strings.ToLower(u.Host)
	if (u.Scheme == "http" && strings.HasSuffix(host, ":80")) || (u.Scheme == "https" && strings.HasSuffix(host, ":443")) {
		host = host[:strings.LastIndex(host, ":")]
	}
	return host == strings.ToLower(r.Host)
}

func safeEqual(a, b string) bool {
	return a != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func requestKey(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimSpace(auth[len("Bearer "):])
	}
	if c, err := r.Cookie("dm_key"); err == nil {
		return c.Value
	}
	return ""
}

var nonceRE = regexp.MustCompile(`^[a-f0-9]{16,64}$`)

func hubProof(token, nonce string) string {
	m := hmac.New(sha256.New, []byte(token))
	m.Write([]byte("device-monitor-hub:" + nonce))
	return hex.EncodeToString(m.Sum(nil))
}

func ConnProof(token, nonce string, client, hub netip.AddrPort) string {
	norm := func(ap netip.AddrPort) string {
		return netip.AddrPortFrom(ap.Addr().Unmap().WithZone(""), ap.Port()).String()
	}
	m := hmac.New(sha256.New, []byte(token))
	m.Write([]byte("machine-monitor-conn:" + nonce + ":" + norm(client) + ">" + norm(hub)))
	return hex.EncodeToString(m.Sum(nil))
}

func sourceKey(a netip.Addr) string {
	a = a.Unmap().WithZone("")
	if a.Is6() {
		p, _ := a.Prefix(64)
		return p.String()
	}
	return a.String()
}

const (
	RateWindow  = time.Minute
	RateMax     = 120
	rateMaxKeys = 5000
)

type rateEntry struct {
	at time.Time
	n  int
}

type rateLimiter struct {
	now   func() time.Time
	mu    sync.Mutex
	rates map[string]*rateEntry
}

func newRateLimiter(now func() time.Time) *rateLimiter {
	return &rateLimiter{now: now, rates: map[string]*rateEntry{}}
}

func (l *rateLimiter) allow(client, socket netip.Addr, max int) bool {
	ip := sourceKey(client)
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.rates[ip]
	if e == nil && len(l.rates) >= rateMaxKeys {
		ip = sourceKey(socket)
		e = l.rates[ip]
	}
	if e == nil || now.Sub(e.at) > RateWindow {
		e = &rateEntry{at: now}
		l.rates[ip] = e
	}
	e.n++
	return e.n <= max
}

func (l *rateLimiter) over(client netip.Addr, max int) bool {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.rates[sourceKey(client)]
	return e != nil && now.Sub(e.at) <= RateWindow && e.n >= max
}

func (l *rateLimiter) prune() {
	cut := l.now().Add(-RateWindow)
	l.mu.Lock()
	defer l.mu.Unlock()
	for ip, e := range l.rates {
		if e.at.Before(cut) {
			delete(l.rates, ip)
		}
	}
}
