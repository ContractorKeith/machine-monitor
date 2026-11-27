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

const DefaultPort = 7777

const maxReply = 1 << 20

var keyRE = regexp.MustCompile(`^[A-Za-z0-9_-]{8,128}$`)

var ErrNotHub = errors.New("not a machine-monitor hub")

var ErrBadProof = errors.New("the server that answered didn't prove it holds this key: the key may be mistyped, or this isn't the hub it belongs to")

func NewHTTPClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	return &http.Client{
		Timeout:       6 * time.Second,
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

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

func Proof(key, nonce string) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte("device-monitor-hub:" + nonce))
	return hex.EncodeToString(m.Sum(nil))
}

type Info struct {
	App      string `json:"app"`
	Impl     string `json:"impl"`
	Hostname string `json:"hostname"`
	Port     int    `json:"port"`
	Locked   bool   `json:"locked"`
	Proof    string `json:"proof"`
	Token    string `json:"token"`
}

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

type PairError struct {
	Msg   string
	Retry bool
}

func (e *PairError) Error() string { return e.Msg }

type DeviceCredentials struct {
	DeviceID  string
	DeviceKey string
}

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
		Retry     *bool  `json:"retry"`
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
		}
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

func cleanMsg(s string) string {
	s = textclean.Clean(s)
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200])
	}
	return strings.TrimSpace(s)
}
