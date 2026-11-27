package alerts

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

const deliveryTimeout = 10 * time.Second

type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

type Result struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

type Delivery struct {
	Ntfy    *Result `json:"ntfy,omitempty"`
	Webhook *Result `json:"webhook,omitempty"`
}

var tailscaleService = []netip.Addr{netip.MustParseAddr("100.100.100.100"), netip.MustParseAddr("fd7a:115c:a1e0::53")}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

func blockedAddr(a netip.Addr, blockPrivate bool) bool {
	a = a.Unmap().WithZone("")
	if b := a.As16(); a.Is6() && [12]byte(b[:12]) == [12]byte{} {
		a = netip.AddrFrom4([4]byte(b[12:]))
	}
	if a.Is4() && a.As4()[0] == 0 {
		return true
	}
	return a.IsLoopback() || a.IsUnspecified() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		slices.Contains(tailscaleService, a) || (blockPrivate && (a.IsPrivate() || cgnat.Contains(a)))
}

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

func scrubURLs(msg string) string {
	return urlInText.ReplaceAllStringFunc(msg, func(raw string) string {
		if u, err := url.Parse(raw); err == nil && u.Host != "" {
			return (&url.URL{Scheme: u.Scheme, Host: u.Host}).String()
		}
		return "[url]"
	})
}

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
	io.Copy(io.Discard, io.LimitReader(res.Body, 4<<10))
	return Result{Error: strings.TrimSpace(fmt.Sprintf("HTTP %d %s", res.StatusCode, http.StatusText(res.StatusCode)))}
}

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
