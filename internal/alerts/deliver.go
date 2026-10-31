package alerts

// SSRF guard ported from Forward-Future/device-monitor alerts.js (MIT, © 2026 Matt):
// https://github.com/Forward-Future/device-monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"
)

// deliveryTimeout bounds one ntfy or webhook request, as upstream.
const deliveryTimeout = 10 * time.Second

// Doer sends an HTTP request. *http.Client satisfies it.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Result is how one delivery went.
type Result struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// Delivery is the result per channel; a nil field means the channel is off.
type Delivery struct {
	Ntfy    *Result `json:"ntfy,omitempty"`
	Webhook *Result `json:"webhook,omitempty"`
}

// tailscaleService is tailscaled's on-host service address ("quad-100": its DNS and local
// web client), IPv4 and IPv6. Every machine running Tailscale answers it itself, and
// net.InterfaceAddrs doesn't list it, so the own-address check below can't catch it.
var tailscaleService = []netip.Addr{netip.MustParseAddr("100.100.100.100"), netip.MustParseAddr("fd7a:115c:a1e0::53")}

// cgnat is 100.64.0.0/10, where Tailscale gives out tailnet addresses.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// blockedAddr reports whether alerts may not be sent to a: loopback, unspecified, 0.0.0.0/8,
// link-local (which includes the 169.254.169.254 cloud metadata service) and Tailscale's
// on-host service, so a key-holder can't turn the hub into an SSRF bounce point. Private LAN
// and tailnet addresses (RFC 1918, fc00::/7, 100.64.0.0/10) are refused only when
// blockPrivate is set.
func blockedAddr(a netip.Addr, blockPrivate bool) bool {
	a = a.Unmap().WithZone("") // ::ffff:127.0.0.1 is 127.0.0.1
	if b := a.As16(); a.Is6() && [12]byte(b[:12]) == [12]byte{} {
		a = netip.AddrFrom4([4]byte(b[12:])) // ::127.0.0.1 (IPv4-compatible) too
	}
	if a.Is4() && a.As4()[0] == 0 {
		return true
	}
	return a.IsLoopback() || a.IsUnspecified() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		slices.Contains(tailscaleService, a) || (blockPrivate && (a.IsPrivate() || cgnat.Contains(a)))
}

// ownAddrs lists this machine's interface addresses (LAN, Tailscale, ...). A webhook to one
// would reach the hub's own API from the hub's own address.
func ownAddrs() []netip.Addr {
	var out []netip.Addr
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(ipn.IP); ok {
				out = append(out, ip.Unmap())
			}
		}
	}
	return out
}

// blocked is blockedAddr plus the hub machine's own interface addresses.
func (e *Engine) blocked(a netip.Addr) bool {
	if blockedAddr(a, e.opt.BlockPrivate) {
		return true
	}
	local := e.opt.LocalAddrs
	if local == nil {
		local = ownAddrs
	}
	return slices.Contains(local(), a.Unmap())
}

// checkHost refuses a URL whose host is, or resolves to, a blocked address. One blocked
// address is enough to refuse, since the dialer may pick any of them.
func (e *Engine) checkHost(ctx context.Context, host string) error {
	if a, err := netip.ParseAddr(host); err == nil {
		if e.blocked(a) {
			return errors.New("blocked: local address")
		}
		return nil
	}
	lookup := e.opt.LookupIP
	if lookup == nil {
		lookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}
	addrs, err := lookup(ctx, host)
	if err != nil {
		return fmt.Errorf("lookup %s: %w", host, err)
	}
	for _, a := range addrs {
		if e.blocked(a) {
			return errors.New("blocked: resolves to a local address")
		}
	}
	return nil
}

// redactErr words a delivery error without the URL's userinfo, path, query or fragment: webhook
// URLs carry their secret there, and the error reaches the log and POST /api/alerts/test. It is
// rebuilt from the *url.Error's op and inner error, and any URL the inner error quotes (a bad
// redirect Location, say) is cut down to scheme://host as well.
func redactErr(err error, u *url.URL) string {
	short := url.URL{Scheme: u.Scheme, Host: u.Host}
	op, inner := "Post", err
	var ue *url.Error
	if errors.As(err, &ue) {
		op, inner = ue.Op, ue.Err
	}
	return fmt.Sprintf("%s %q: %s", op, short.Redacted(), scrubURLs(inner.Error()))
}

var urlInText = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s"'<>]+`)

// scrubURLs cuts every URL in msg down to scheme://host; one that won't parse becomes [url].
func scrubURLs(msg string) string {
	return urlInText.ReplaceAllStringFunc(msg, func(raw string) string {
		if u, err := url.Parse(raw); err == nil && u.Host != "" {
			return (&url.URL{Scheme: u.Scheme, Host: u.Host}).String()
		}
		return "[url]"
	})
}

// post sends body as JSON to rawURL after the SSRF check. It never returns an error, only a
// Result, since delivery is best effort.
func (e *Engine) post(ctx context.Context, rawURL string, body any) Result {
	u, err := url.Parse(rawURL)
	if err != nil || httpURL(rawURL) == "" {
		return Result{Error: "blocked: not an allowed http(s) URL"}
	}
	if err := e.checkHost(ctx, strings.ToLower(u.Hostname())); err != nil {
		return Result{Error: err.Error()}
	}
	b, _ := json.Marshal(body)
	ctx, cancel := context.WithTimeout(ctx, deliveryTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(b))
	if err != nil {
		return Result{Error: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := e.client.Do(req)
	if err != nil {
		return Result{Error: redactErr(err, u)}
	}
	defer res.Body.Close()
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
		return Result{OK: true}
	}
	// A redirect is reported as a failure, never followed: it could point anywhere. The
	// reply's body isn't passed on: POST /api/alerts/test would echo it to the caller,
	// a read-back channel into whatever the URL reaches.
	io.Copy(io.Discard, io.LimitReader(res.Body, 4<<10))
	return Result{Error: strings.TrimSpace(fmt.Sprintf("HTTP %d %s", res.StatusCode, http.StatusText(res.StatusCode)))}
}

// newGuardedClient is the default Doer: no redirects, no proxy (a proxy would hide where the
// request really goes), and a dialer that re-checks the address it actually connects to, so a
// DNS answer that changes between the check and the dial (rebinding) is refused too.
func newGuardedClient(blocked func(netip.Addr) bool) *http.Client {
	dialer := &net.Dialer{
		Timeout: deliveryTimeout,
		Control: func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil || blocked(ap.Addr()) {
				return fmt.Errorf("blocked: local address %s", address)
			}
			return nil
		},
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.DialContext = dialer.DialContext
	return &http.Client{
		Transport:     tr,
		Timeout:       deliveryTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}
