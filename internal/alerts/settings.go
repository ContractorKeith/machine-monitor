package alerts

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

const (
	TypeOffline   = "offline"
	TypeCPUTemp   = "cpuTemp"
	TypeGPUTemp   = "gpuTemp"
	TypeCPU       = "cpu"
	TypeGPU       = "gpu"
	TypeMem       = "mem"
	TypeDisk      = "disk"
	TypePower     = "power"
	TypeDailyCost = "dailyCost"
)

type TypeInfo struct {
	Label        string
	HasThreshold bool
	Min, Max     float64
	Hyst         float64
}

var Types = map[string]TypeInfo{
	TypeOffline:   {Label: "Offline"},
	TypeCPUTemp:   {Label: "CPU temperature", HasThreshold: true, Min: 20, Max: 150, Hyst: 3},
	TypeGPUTemp:   {Label: "GPU temperature", HasThreshold: true, Min: 20, Max: 150, Hyst: 3},
	TypeCPU:       {Label: "CPU load", HasThreshold: true, Min: 1, Max: 100, Hyst: 5},
	TypeGPU:       {Label: "GPU load", HasThreshold: true, Min: 1, Max: 100, Hyst: 5},
	TypeMem:       {Label: "Memory use", HasThreshold: true, Min: 1, Max: 100, Hyst: 3},
	TypeDisk:      {Label: "Disk space used", HasThreshold: true, Min: 1, Max: 100, Hyst: 1},
	TypePower:     {Label: "Power draw", HasThreshold: true, Min: 1, Max: 100000},
	TypeDailyCost: {Label: "Electricity cost today", HasThreshold: true, Min: 0.01, Max: 1e7},
}

const AllDevices = "*"

const DefaultNtfyServer = "https://ntfy.sh"

const (
	maxRules = 100
	maxLog   = 300
)

type Rule struct {
	ID        string   `json:"id"`
	Type      string   `json:"type"`
	Device    string   `json:"device"`
	Threshold *float64 `json:"threshold"`
	Minutes   int      `json:"minutes"`
	Enabled   bool     `json:"enabled"`
}

type Settings struct {
	Rules          []Rule   `json:"rules"`
	Channels       Channels `json:"channels"`
	NotifyResolved bool     `json:"notifyResolved"`
	TempUnit       string   `json:"tempUnit"`
}

type Channels struct {
	Desktop bool    `json:"desktop"`
	Ntfy    Ntfy    `json:"ntfy"`
	Webhook Webhook `json:"webhook"`
}

type Ntfy struct {
	Enabled bool   `json:"enabled"`
	Server  string `json:"server"`
	Topic   string `json:"topic"`
	Email   string `json:"email"`
}

type Webhook struct {
	Enabled bool   `json:"enabled"`
	URL     string `json:"url"`
}

func DefaultSettings() Settings {
	s, err := ParseSettings([]byte(`{"rules":[
		{"type":"offline","device":"*","minutes":10},
		{"type":"cpuTemp","device":"*","threshold":90,"minutes":2},
		{"type":"gpuTemp","device":"*","threshold":85,"minutes":2},
		{"type":"disk","device":"*","threshold":90,"minutes":0}
	],"channels":{"desktop":true}}`))
	if err != nil {
		panic(err)
	}
	return s
}

var (
	deviceRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)
	ruleIDRE = regexp.MustCompile(`^[a-f0-9]{12}$`)
	topicRE  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	emailRE  = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
	ctrlRE   = regexp.MustCompile(`[\x00-\x1f\x7f]`)
)

type ruleIn struct {
	ID        any   `json:"id"`
	Type      any   `json:"type"`
	Device    any   `json:"device"`
	Threshold any   `json:"threshold"`
	Minutes   any   `json:"minutes"`
	Enabled   *bool `json:"enabled"`
}

type settingsIn struct {
	Rules    []json.RawMessage `json:"rules"`
	Channels *struct {
		Desktop *bool `json:"desktop"`
		Ntfy    *struct {
			Enabled any `json:"enabled"`
			Server  any `json:"server"`
			Topic   any `json:"topic"`
			Email   any `json:"email"`
		} `json:"ntfy"`
		Webhook *struct {
			Enabled any `json:"enabled"`
			URL     any `json:"url"`
		} `json:"webhook"`
	} `json:"channels"`
	NotifyResolved *bool `json:"notifyResolved"`
	TempUnit       any   `json:"tempUnit"`
}

func ParseSettings(data []byte) (Settings, error) {
	var in settingsIn
	if err := json.Unmarshal(data, &in); err != nil {
		return Settings{}, err
	}
	if in.Rules == nil {
		return Settings{}, errors.New("rules missing")
	}
	raw := in.Rules
	if len(raw) > maxRules {
		raw = raw[:maxRules]
	}
	s := Settings{Rules: make([]Rule, 0, len(raw)), NotifyResolved: true, TempUnit: "C"}
	for i, rr := range raw {
		r, err := parseRule(rr)
		if err != nil {
			return Settings{}, fmt.Errorf("rule %d: %w", i+1, err)
		}
		s.Rules = append(s.Rules, r)
	}

	s.Channels.Desktop = true
	server, topic, email, hookURL := any(nil), "", "", ""
	var ntfyOn, hookOn bool
	if ch := in.Channels; ch != nil {
		s.Channels.Desktop = ch.Desktop == nil || *ch.Desktop
		if n := ch.Ntfy; n != nil {
			ntfyOn, server = truthy(n.Enabled), n.Server
			topic, email = clean(n.Topic, 64), clean(n.Email, 120)
		}
		if h := ch.Webhook; h != nil {
			hookOn = truthy(h.Enabled)
			if truthy(h.URL) {
				if hookURL = httpURL(h.URL); hookURL == "" {
					return Settings{}, errors.New("webhook url must be http(s) and not a local address")
				}
			}
		}
	}
	if topic != "" && !topicRE.MatchString(topic) {
		return Settings{}, errors.New("ntfy topic may use only letters, digits, - and _")
	}
	if email != "" && !emailRE.MatchString(email) {
		return Settings{}, errors.New("invalid ntfy email")
	}
	srv := DefaultNtfyServer
	if truthy(server) {
		if srv = httpURL(server); srv == "" {
			return Settings{}, errors.New("ntfy server must be http(s) and not a local address")
		}
	}
	s.Channels.Ntfy = Ntfy{Enabled: ntfyOn && topic != "", Server: srv, Topic: topic, Email: email}
	s.Channels.Webhook = Webhook{Enabled: hookOn && hookURL != "", URL: hookURL}
	s.NotifyResolved = in.NotifyResolved == nil || *in.NotifyResolved
	if in.TempUnit == "F" {
		s.TempUnit = "F"
	}
	return s, nil
}

func parseRule(data json.RawMessage) (Rule, error) {
	var in ruleIn
	if err := json.Unmarshal(data, &in); err != nil {
		return Rule{}, errors.New("not an object")
	}
	typ, _ := in.Type.(string)
	t, ok := Types[typ]
	if !ok {
		return Rule{}, fmt.Errorf("unknown type %q", typ)
	}
	dev, _ := in.Device.(string)
	if dev != AllDevices && !deviceRE.MatchString(dev) {
		return Rule{}, errors.New("invalid device")
	}
	r := Rule{Type: typ, Device: dev, Enabled: in.Enabled == nil || *in.Enabled}
	if t.HasThreshold {
		v, ok := number(in.Threshold)
		if !ok || v < t.Min || v > t.Max {
			return Rule{}, fmt.Errorf("%s threshold must be %g–%g", typ, t.Min, t.Max)
		}
		r.Threshold = &v
	}
	m, ok := number(in.Minutes)
	if !ok {
		m = 0
	}
	m = jsRound(m)
	minMinutes := 0.0
	if typ == TypeOffline {
		minMinutes = 1
	}
	if m < minMinutes || m > 1440 {
		return Rule{}, errors.New("minutes out of range")
	}
	r.Minutes = int(m)
	if id, _ := in.ID.(string); ruleIDRE.MatchString(id) {
		r.ID = id
	} else {
		r.ID = newID()
	}
	return r, nil
}

func number(v any) (float64, bool) {
	var f float64
	switch x := v.(type) {
	case nil:
		f = 0
	case float64:
		f = x
	case bool:
		if x {
			f = 1
		}
	case string:
		s := strings.TrimSpace(x)
		if s == "" {
			return 0, true
		}
		var err error
		if f, err = strconv.ParseFloat(s, 64); err != nil {
			return 0, false
		}
	default:
		return 0, false
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case float64:
		return x != 0 && !math.IsNaN(x)
	case string:
		return x != ""
	}
	return true
}

func jsRound(x float64) float64 { return math.Floor(x + 0.5) }

func clean(v any, max int) string {
	s, _ := v.(string)
	s = strings.TrimSpace(ctrlRE.ReplaceAllString(s, ""))
	if r := []rune(s); len(r) > max {
		s = string(r[:max])
	}
	return s
}

func httpURL(v any) string {
	s, _ := v.(string)
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	u.Scheme = strings.ToLower(u.Scheme)
	h := strings.ToLower(u.Hostname())
	if h == "" || h == "localhost" || strings.HasSuffix(h, ".localhost") || oddIPv4(h) {
		return ""
	}
	if a, err := netip.ParseAddr(h); err == nil && blockedAddr(a, false) {
		return ""
	}
	return strings.TrimSuffix(u.String(), "/")
}

func oddIPv4(h string) bool {
	if _, err := netip.ParseAddr(h); err == nil {
		return false
	}
	for _, c := range h {
		if !(c >= '0' && c <= '9' || c == '.' || c == 'x' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return strings.ContainsAny(h, "0123456789") && !strings.ContainsAny(h, "abcdef") ||
		strings.HasPrefix(h, "0x") || strings.Contains(h, ".0x")
}

func newID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}
