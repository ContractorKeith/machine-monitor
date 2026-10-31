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

// blockedAddr reports whether alerts may not be sent to a: loopback, unspecified, 0.0.0.0/8
// and link-local (which includes the 169.254.169.254 cloud metadata service), so a key-holder
// can't turn the hub into an SSRF bounce point. Private LAN addresses are refused only when
// blockPrivate is set.
func blockedAddr(a netip.Addr, blockPrivate bool) bool {
	a = a.Unmap() // ::ffff:127.0.0.1 is 127.0.0.1
	if b := a.As16(); a.Is6() && [12]byte(b[:12]) == [12]byte{} {
		a = netip.AddrFrom4([4]byte(b[12:])) // ::127.0.0.1 (IPv4-compatible) too
	}
	if a.Is4() && a.As4()[0] == 0 {
		return true
	}
	return a.IsLoopback() || a.IsUnspecified() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		(blockPrivate && a.IsPrivate())
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
		return Result{Error: err.Error()}
	}
	defer res.Body.Close()
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
		return Result{OK: true}
	}
	// A redirect is reported as a failure, never followed: it could point anywhere.
	text, _ := io.ReadAll(io.LimitReader(res.Body, 4<<10))
	msg := []rune(strings.ToValidUTF8(string(text), ""))
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return Result{Error: strings.TrimSpace(fmt.Sprintf("HTTP %d %s", res.StatusCode, string(msg)))}
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
