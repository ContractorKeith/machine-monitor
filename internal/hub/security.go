package hub

// HTTP hardening: Host-header (DNS-rebinding) check, LAN-only peers, CSRF/Origin check,
// response headers, the hub proof and the per-IP rate limit for open endpoints.
//
// Ported from Forward-Future/device-monitor security.js and server.js @ c3a4c41
// (MIT, © 2026 Matt): https://github.com/Forward-Future/device-monitor

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

// baseHeaders go on every response. The hub serves no HTML, so the CSP forbids everything:
// a browser that is somehow pointed at an API response can't run or embed anything from it.
var baseHeaders = map[string]string{
	"X-Content-Type-Options":       "nosniff",
	"Referrer-Policy":              "no-referrer",
	"Cross-Origin-Resource-Policy": "same-origin",
	"Content-Security-Policy":      "default-src 'none'; frame-ancestors 'none'; base-uri 'none'",
	"X-Frame-Options":              "DENY",
}

// hostnameRE is a DNS name. The Host value may later be written into served install
// scripts, so no quotes or shell characters may get through.
var hostnameRE = regexp.MustCompile(`^[a-z0-9_]([a-z0-9_-]*[a-z0-9_])?(\.[a-z0-9_]([a-z0-9_-]*[a-z0-9_])?)*$`)

// hostAllowed is the DNS-rebinding guard: only answer requests addressed to this machine by
// IP, localhost, this machine's own name (also .local, .lan, .home, .home.arpa and Tailscale
// MagicDNS) or a name in extra (config allowedHosts).
func hostAllowed(hostHeader, me string, extra []string) bool {
	host := strings.ToLower(strings.TrimSpace(hostHeader))
	if host == "" {
		return false
	}
	if _, err := netip.ParseAddr(host); err == nil { // bare IP, including unbracketed IPv6
		return true
	}
	if strings.HasPrefix(host, "[") { // [IPv6]:port
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

// isPrivateIPv4 is loopback, RFC 1918, link-local or Tailscale CGNAT (100.64/10).
func isPrivateIPv4(a netip.Addr) bool {
	b := a.As4()
	return b[0] == 10 || b[0] == 127 || (b[0] == 172 && b[1] >= 16 && b[1] <= 31) ||
		(b[0] == 192 && b[1] == 168) || (b[0] == 169 && b[1] == 254) || (b[0] == 100 && b[1] >= 64 && b[1] <= 127)
}

// isLocalAddress decides who may connect at all: this machine, the home network and
// Tailscale. Anything else (a router port forward, UPnP, a public IPv6 address) is refused
// even if a firewall lets it through.
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
		(b[0] == 0xfe && b[1]&0xc0 == 0x80) || // fe80::/10 link-local
		b[0]&0xfe == 0xfc // fc00::/7 unique-local
}

// remoteIP parses http.Request.RemoteAddr ("ip:port" or a bare ip), unmapping ::ffff:a.b.c.d.
func remoteIP(remoteAddr string) (netip.Addr, bool) {
	if ap, err := netip.ParseAddrPort(remoteAddr); err == nil {
		return ap.Addr().Unmap(), true
	}
	if a, err := netip.ParseAddr(remoteAddr); err == nil {
		return a.Unmap(), true
	}
	return netip.Addr{}, false
}

// clientAddr is the address a request comes from, for rate limits and pairing tries: the
// socket's address, or the last X-Forwarded-For address when the socket is a proxy listed
// in config.json "trustedProxies" (tailscale serve hands tailnet clients over from
// loopback). Proxies append the address they saw, so only the last entry is the trusted
// proxy's; everything before it is whatever the client sent. Anyone else's X-Forwarded-For is whatever they typed, so it is ignored, also
// from loopback: any local process could otherwise dodge the limits by rotating it.
// (Upstream honours it from every loopback peer.)
func (h *Hub) clientAddr(r *http.Request) (client, socket netip.Addr) {
	socket, _ = remoteIP(r.RemoteAddr)
	client = socket
	if h.trustedProxy(r.RemoteAddr) {
		hops := r.Header.Values("X-Forwarded-For")
		last := ""
		if len(hops) > 0 {
			list := strings.Split(hops[len(hops)-1], ",")
			last = list[len(list)-1]
		}
		if a, err := netip.ParseAddr(strings.TrimSpace(last)); err == nil {
			client = a.Unmap().WithZone("")
		}
	}
	return client, socket
}

// sameOrigin refuses cross-site requests: browsers send Origin on cross-site POST, PUT,
// PATCH and DELETE (including pages that target 127.0.0.1). Agents and the TUI send none.
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
	// A URL's host drops the scheme's default port, as JavaScript's URL does.
	if (u.Scheme == "http" && strings.HasSuffix(host, ":80")) || (u.Scheme == "https" && strings.HasSuffix(host, ":443")) {
		host = host[:strings.LastIndex(host, ":")]
	}
	return host == strings.ToLower(r.Host)
}

// safeEqual compares secrets in constant time. An empty secret never matches.
func safeEqual(a, b string) bool {
	return a != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// requestKey is the hub key a request carries: "Authorization: Bearer <key>" or the dm_key
// cookie, as upstream.
func requestKey(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimSpace(auth[len("Bearer "):])
	}
	if c, err := r.Cookie("dm_key"); err == nil {
		return c.Value
	}
	return ""
}

// nonceRE is the challenge a client sends to GET /api/info?nonce=.
var nonceRE = regexp.MustCompile(`^[a-f0-9]{16,64}$`)

// hubProof lets a client check that a hub knows its key before sending the key:
// hex(HMAC-SHA256(key, "device-monitor-hub:" + nonce)).
func hubProof(token, nonce string) string {
	m := hmac.New(sha256.New, []byte(token))
	m.Write([]byte("device-monitor-hub:" + nonce))
	return hex.EncodeToString(m.Sum(nil))
}

// Rate limit for endpoints that don't need the key: RateMax requests per RateWindow per
// client address, so a noisy LAN peer can't make the hub do work nonstop.
const (
	RateWindow  = time.Minute
	RateMax     = 120
	rateMaxKeys = 5000 // distinct addresses tracked at once
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

// allow counts one request from client (see Hub.clientAddr) and reports whether it is
// within max per RateWindow. socket is the connection's own address: when the table is
// full, new clients share its allowance, so rotating forwarded addresses can't grow the
// table without bound.
func (l *rateLimiter) allow(client, socket netip.Addr, max int) bool {
	ip := client.String()
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.rates[ip]
	if e == nil && len(l.rates) >= rateMaxKeys { // table full: share the socket's allowance
		ip = socket.String()
		e = l.rates[ip]
	}
	if e == nil || now.Sub(e.at) > RateWindow {
		e = &rateEntry{at: now}
		l.rates[ip] = e
	}
	e.n++
	return e.n <= max
}

// over reports whether client has already used up max in the current window, without
// counting a request.
func (l *rateLimiter) over(client netip.Addr, max int) bool {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.rates[client.String()]
	return e != nil && now.Sub(e.at) <= RateWindow && e.n >= max
}

// forget drops client's count.
func (l *rateLimiter) forget(client netip.Addr) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.rates, client.String())
}

// prune forgets windows that have ended.
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
