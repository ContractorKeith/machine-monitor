// Package agent reports this machine to a hub (machine-monitor agent) and pairs with one
// (machine-monitor connect).
//
// The hub key and enrolled device key are sent only to a hub that has proven it holds
// the same key: the agent sends a random nonce to GET /api/info and checks the HMAC proof
// before every first report and after every failed one, so a stale address, a spoofed
// DHCP lease or a fake hub on the LAN never receives the key. The pairing code is sent
// before any proof is possible (the client doesn't know the key yet); it is single-use and
// short-lived, and the key it buys is proven before it is used or saved.
//
// The proof only shows the server holds the key; it is not bound to the connection. On
// plain HTTP the key travels in cleartext in every report, so anyone who can read or
// intercept traffic on the path can take it. Confidentiality relies on the LAN or tailnet
// being trusted, or on HTTPS (for example through tailscale serve).
//
// Ported from Forward-Future/device-monitor main.js (fetchInfo, pairWithHub, hubAnswers,
// the connect flow) and server.js (hubProof) @ c3a4c41
// (MIT, © 2026 Matt): https://github.com/Forward-Future/device-monitor
package agent

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
	"regexp"
	"strings"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/textclean"
)

// DefaultPort is the hub port added to an http:// URL that has none, as upstream.
const DefaultPort = 7777

const maxReply = 1 << 20 // bytes read from any hub reply

// keyRE is what a hub key may look like. Anything else a hub hands over is refused, so a
// key can never carry header-breaking characters into requests (upstream TOKEN_RE).
var keyRE = regexp.MustCompile(`^[A-Za-z0-9_-]{8,128}$`)

// ErrNotHub means the server answered but is not a device-monitor / machine-monitor hub.
var ErrNotHub = errors.New("not a machine-monitor hub")

// ErrBadProof means the server could not prove it holds the key: it is not the hub the key
// belongs to. The key was not sent.
var ErrBadProof = errors.New("the server that answered didn't prove it holds this key: the key may be mistyped, or this isn't the hub it belongs to")

// NewHTTPClient is the client used for hub calls: a short timeout, no proxy (hubs are on
// the LAN or tailnet, and a proxy would see the key) and no redirects (a redirect would
// re-send a report, key included, to wherever it points).
func NewHTTPClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	return &http.Client{
		Timeout:       6 * time.Second,
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// NormalizeURL turns what a user typed ("192.168.1.20", "host:7777", a dashboard link)
// into a hub origin like "http://192.168.1.20:7777". It also returns a key carried in a
// dashboard link's ?key=, or "".
func NormalizeURL(s string) (origin, key string, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", "", errors.New("no hub address")
	}
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return "", "", fmt.Errorf("%q is not a hub address (e.g. 192.168.1.20 or http://hub:7777)", s)
	}
	host := u.Host
	if u.Port() == "" && u.Scheme == "http" {
		host = net.JoinHostPort(u.Hostname(), fmt.Sprint(DefaultPort))
	}
	return u.Scheme + "://" + host, u.Query().Get("key"), nil
}

// Proof is hex(HMAC-SHA256(key, "device-monitor-hub:" + nonce)), the hub's answer to a
// nonce on GET /api/info.
func Proof(key, nonce string) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte("device-monitor-hub:" + nonce))
	return hex.EncodeToString(m.Sum(nil))
}

// Info is the unauthenticated GET /api/info reply.
type Info struct {
	App      string `json:"app"`
	Impl     string `json:"impl"`
	Hostname string `json:"hostname"`
	Port     int    `json:"port"`
	Locked   bool   `json:"locked"`
	Proof    string `json:"proof"`
	Token    string `json:"token"` // only hubs before upstream 1.2.16 send this openly
}

// FetchInfo reads the hub's identity without sending anything secret. nonce may be "".
func FetchInfo(ctx context.Context, c *http.Client, hubURL, nonce string) (Info, error) {
	u := hubURL + "/api/info"
	if nonce != "" {
		u += "?nonce=" + nonce
	}
	var info Info
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return info, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return info, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return info, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxReply)).Decode(&info); err != nil || info.App != "device-monitor" {
		return info, ErrNotHub
	}
	return info, nil
}

// Prove checks that the hub at hubURL holds key, without sending the key: a fresh random
// nonce goes out, and the HMAC proof that comes back must match. Hubs that hand out their
// key openly (before upstream 1.2.16) are refused.
func Prove(ctx context.Context, c *http.Client, hubURL, key string) error {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	nonce := hex.EncodeToString(b)
	info, err := FetchInfo(ctx, c, hubURL, nonce)
	if err != nil {
		return err
	}
	if !hmac.Equal([]byte(info.Proof), []byte(Proof(key, nonce))) {
		return ErrBadProof
	}
	return nil
}

// PairError is a hub's refusal of a pairing code. Retry is true for a plain wrong code
// while the code is still alive.
type PairError struct {
	Msg   string
	Retry bool
}

func (e *PairError) Error() string { return e.Msg }

// DeviceCredentials binds pairing to an agent ID and receives its private key.
type DeviceCredentials struct {
	DeviceID  string
	DeviceKey string
}

// Pair trades a pairing code for the hub key. The key is not yet trusted: call Prove.
func Pair(ctx context.Context, c *http.Client, hubURL, code string, enrollment *DeviceCredentials) (string, error) {
	fields := map[string]string{"code": code}
	if enrollment != nil {
		fields["deviceId"] = enrollment.DeviceID
	}
	body, _ := json.Marshal(fields)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hubURL+"/api/pair", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var reply struct {
		DeviceKey string `json:"deviceKey"`
		Token     string `json:"token"`
		Error     string `json:"error"`
		Retry     *bool  `json:"retry"` // machine-monitor hubs only
	}
	json.NewDecoder(io.LimitReader(resp.Body, maxReply)).Decode(&reply)
	if resp.StatusCode != http.StatusOK || reply.Token == "" {
		msg := cleanMsg(reply.Error)
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		retry := resp.StatusCode == http.StatusForbidden && msg == "Wrong pairing code."
		if reply.Retry != nil {
			retry = *reply.Retry
		} // else an upstream hub: its server.js wording is the only signal (pinned at c3a4c41)
		return "", &PairError{Msg: msg, Retry: retry}
	}
	if !keyRE.MatchString(reply.Token) {
		return "", errors.New("the hub handed over an unusable key")
	}
	if enrollment != nil {
		if reply.DeviceKey != "" && !keyRE.MatchString(reply.DeviceKey) {
			return "", errors.New("unusable device key")
		}
		enrollment.DeviceKey = reply.DeviceKey
	}
	return reply.Token, nil
}

// Connect pairs with the hub at hubURL using code and returns its key, proven: the server
// that issued the key must also prove it holds it, so a server that just made one up is
// refused before the key is used.
func Connect(ctx context.Context, c *http.Client, hubURL, code string, enrollment *DeviceCredentials) (string, error) {
	if _, err := FetchInfo(ctx, c, hubURL, ""); err != nil {
		return "", fmt.Errorf("couldn't reach a hub at %s: %w", hubURL, err)
	}
	key, err := Pair(ctx, c, hubURL, code, enrollment)
	if err != nil {
		return "", err
	}
	if err := Prove(ctx, c, hubURL, key); err != nil {
		return "", err
	}
	return key, nil
}

// cleanMsg keeps a hub's error message printable and short; it comes from the network.
func cleanMsg(s string) string {
	s = textclean.Clean(s)
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200])
	}
	return strings.TrimSpace(s)
}
