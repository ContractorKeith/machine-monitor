// Package client is the read side of a hub's API (PLAN §5 "Connected TUI", §6): the device
// snapshot, the live SSE stream with reconnects, and rename, remove and reorder.
//
// Every request carries the hub key as a Bearer token. Before the first request on each
// connection the client checks the hub's proof of the key (GET /api/info?nonce=), so the key
// is never sent to a machine that doesn't already know it.
package client

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/hub"
)

// Device is one device as the hub shows it: registry entry, online flag, last report and
// recent history. It is the hub's own wire type, so the two can't drift apart.
type Device = hub.View

// DefaultPort is used when a hub URL names no port.
const DefaultPort = hub.DefaultPort

// requestTimeout bounds every request except the stream.
const requestTimeout = 15 * time.Second

// maxResponse caps a response body read into memory (a full snapshot of MaxDevices with
// ten minutes of history each stays well under it).
const maxResponse = 64 << 20

// ErrUnauthorized means the hub refused the key. Retrying won't help.
var ErrUnauthorized = errors.New("the hub refused the key")

// ErrBadProof means the machine at the URL could not prove it knows the key, so the key was
// not sent. Either the key is wrong or this is not the hub it claims to be.
var ErrBadProof = errors.New("the hub could not prove it knows the key (wrong key, or not your hub)")

// StatusError is an HTTP error from the hub, with its {"error": "..."} message when it sent one.
type StatusError struct {
	Code    int
	Message string
}

func (e *StatusError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("hub: %d %s", e.Code, e.Message)
	}
	return fmt.Sprintf("hub: %d %s", e.Code, http.StatusText(e.Code))
}

// Client talks to one hub. It is safe for concurrent use.
type Client struct {
	base *url.URL
	key  string
	http *http.Client // requests (with requestTimeout)
	// stream is for the SSE stream: no overall timeout, since it stays open; a stalled
	// stream is caught by the idle watchdog instead.
	stream *http.Client
}

// New returns a client for the hub at hubURL ("http://host:7777", "host:7777" or "host").
func New(hubURL, key string) (*Client, error) {
	u, err := ParseURL(hubURL)
	if err != nil {
		return nil, err
	}
	if key == "" {
		return nil, errors.New("no hub key")
	}
	tr := &http.Transport{
		Proxy:                 nil, // LAN and tailnet only; a proxy would see the key
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ResponseHeaderTimeout: 10 * time.Second,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
	}
	return &Client{base: u, key: key,
		http:   &http.Client{Transport: tr, Timeout: requestTimeout},
		stream: &http.Client{Transport: tr}}, nil
}

// URL is the hub's base URL, e.g. "http://192.168.1.5:7777".
func (c *Client) URL() string { return c.base.String() }

// Host is the hub's host:port, for display.
func (c *Client) Host() string { return c.base.Host }

// ParseURL normalizes a hub address: a missing scheme is http, a missing port is 7777, and
// any path, query or user info is refused.
func ParseURL(s string) (*url.URL, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("no hub URL")
	}
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("hub URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("hub URL: scheme %q, want http or https", u.Scheme)
	}
	if u.Hostname() == "" || u.User != nil || strings.Trim(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("hub URL %q: want http://host:port", s)
	}
	host := u.Host
	if u.Port() == "" && u.Scheme == "http" {
		host = net.JoinHostPort(u.Hostname(), fmt.Sprint(DefaultPort))
	}
	return &url.URL{Scheme: u.Scheme, Host: host}, nil
}

// SameHub reports whether two hub addresses name the same hub after normalizing.
func SameHub(a, b string) bool {
	ua, err1 := ParseURL(a)
	ub, err2 := ParseURL(b)
	return err1 == nil && err2 == nil && strings.EqualFold(ua.String(), ub.String())
}

// ---- requests ----------------------------------------------------------------------------

// do sends one keyed request and decodes a JSON reply into out (unless out is nil).
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base.String()+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return err
	}
	if len(b) > maxResponse {
		return errors.New("hub: response too large")
	}
	if err := statusErr(resp.StatusCode, b); err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("hub: bad reply to %s %s: %w", method, path, err)
	}
	return nil
}

// statusErr turns a non-2xx status into an error, ErrUnauthorized for 401.
func statusErr(code int, body []byte) error {
	if code >= 200 && code < 300 {
		return nil
	}
	if code == http.StatusUnauthorized {
		return ErrUnauthorized
	}
	var e struct {
		Error string `json:"error"`
	}
	msg := ""
	if json.Unmarshal(body, &e) == nil {
		msg = e.Error
	} else if len(body) > 0 && len(body) < 200 {
		msg = strings.TrimSpace(string(body)) // the guard answers in plain text
	}
	return &StatusError{Code: code, Message: msg}
}

// Verify checks the hub's proof of the key without sending the key: it sends a random nonce
// to the open /api/info and compares the reply with HMAC-SHA256(key, "device-monitor-hub:"+nonce).
func (c *Client) Verify(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	n := make([]byte, 16)
	if _, err := rand.Read(n); err != nil {
		return err
	}
	nonce := hex.EncodeToString(n)
	req, err := http.NewRequestWithContext(ctx, "GET", c.base.String()+"/api/info?nonce="+nonce, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return err
	}
	if err := statusErr(resp.StatusCode, b); err != nil {
		return err
	}
	var info struct {
		Proof string `json:"proof"`
	}
	if json.Unmarshal(b, &info) != nil || info.Proof == "" {
		return ErrBadProof
	}
	m := hmac.New(sha256.New, []byte(c.key))
	m.Write([]byte("device-monitor-hub:" + nonce))
	want := hex.EncodeToString(m.Sum(nil))
	if !hmac.Equal([]byte(strings.ToLower(info.Proof)), []byte(want)) {
		return ErrBadProof
	}
	return nil
}

// Devices is GET /api/devices: every device with its last report and recent history.
func (c *Client) Devices(ctx context.Context) ([]Device, error) {
	var out []Device
	if err := c.do(ctx, "GET", "/api/devices", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Rename is PATCH /api/devices/:id. It returns the device as the hub now has it.
func (c *Client) Rename(ctx context.Context, id, name string) (*Device, error) {
	var out Device
	if err := c.do(ctx, "PATCH", "/api/devices/"+url.PathEscape(id), map[string]string{"name": name}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Remove is DELETE /api/devices/:id: the hub forgets the device and its history.
func (c *Client) Remove(ctx context.Context, id string) error {
	return c.do(ctx, "DELETE", "/api/devices/"+url.PathEscape(id), nil, nil)
}

// SetOrder is PUT /api/order: ids in display order. Unknown ids are ignored by the hub.
func (c *Client) SetOrder(ctx context.Context, ids []string) error {
	if ids == nil {
		ids = []string{}
	}
	return c.do(ctx, "PUT", "/api/order", map[string][]string{"ids": ids}, nil)
}
