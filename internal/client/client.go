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
	"sync"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/hub"
)

type Device = hub.View

const DefaultPort = hub.DefaultPort

const requestTimeout = 15 * time.Second

const maxResponse = 64 << 20

var ErrUnauthorized = errors.New("the hub refused the key")

var ErrBadProof = errors.New("the hub could not prove it knows the key (wrong key, or not your hub)")

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

type Client struct {
	base *url.URL
	key  string

	vmu      sync.Mutex
	verified bool
	http     *http.Client

	stream *http.Client

	owner string
}

func New(hubURL, key string) (*Client, error) {
	u, err := ParseURL(hubURL)
	if err != nil {
		return nil, err
	}
	if key == "" {
		return nil, errors.New("no hub key")
	}
	tr := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ResponseHeaderTimeout: 10 * time.Second,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
	}
	ob := make([]byte, 16)
	rand.Read(ob)
	return &Client{base: u, key: key, owner: hex.EncodeToString(ob),
		http:   &http.Client{Transport: tr, Timeout: requestTimeout},
		stream: &http.Client{Transport: tr}}, nil
}

func (c *Client) URL() string { return c.base.String() }

func (c *Client) Host() string { return c.base.Host }

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
		return nil, errors.New("hub URL: not a valid address; want http://host:port")
	}
	if u.User != nil {
		return nil, errors.New("hub URL: must not contain a user name or password")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("hub URL: scheme %q, want http or https", u.Scheme)
	}
	if u.Hostname() == "" || strings.Trim(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("hub URL for %q: want http://host:port with no path or query", u.Host)
	}
	host := u.Host
	if u.Port() == "" && u.Scheme == "http" {
		host = net.JoinHostPort(u.Hostname(), fmt.Sprint(DefaultPort))
	}
	return &url.URL{Scheme: u.Scheme, Host: host}, nil
}

func SameHub(a, b string) bool {
	ua, err1 := ParseURL(a)
	ub, err2 := ParseURL(b)
	return err1 == nil && err2 == nil && strings.EqualFold(ua.String(), ub.String())
}

func (c *Client) verifyOnce(ctx context.Context) error {
	c.vmu.Lock()
	defer c.vmu.Unlock()
	if c.verified {
		return nil
	}
	if err := c.Verify(ctx); err != nil {
		return err
	}
	c.verified = true
	return nil
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	if err := c.verifyOnce(ctx); err != nil {
		return err
	}
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
		msg = strings.TrimSpace(string(body))
	}
	return &StatusError{Code: code, Message: msg}
}

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

func (c *Client) Devices(ctx context.Context) ([]Device, error) {
	var out []Device
	if err := c.do(ctx, "GET", "/api/devices", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) Rename(ctx context.Context, id, name string) (*Device, error) {
	var out Device
	if err := c.do(ctx, "PATCH", "/api/devices/"+url.PathEscape(id), map[string]string{"name": name}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Remove(ctx context.Context, id string) error {
	return c.do(ctx, "DELETE", "/api/devices/"+url.PathEscape(id), nil, nil)
}

func (c *Client) SetOrder(ctx context.Context, ids []string) error {
	if ids == nil {
		ids = []string{}
	}
	return c.do(ctx, "PUT", "/api/order", map[string][]string{"ids": ids}, nil)
}

type Processes = hub.ProcessesReply

func (c *Client) Processes(ctx context.Context, id string) (*Processes, error) {
	var out Processes
	if err := c.do(ctx, "GET", "/api/devices/"+url.PathEscape(id)+"/processes", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Action(ctx context.Context, id, action string) error {
	return c.do(ctx, "POST", "/api/devices/"+url.PathEscape(id)+"/action", map[string]string{"action": action}, nil)
}
