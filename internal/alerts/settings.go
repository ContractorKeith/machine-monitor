// Package alerts evaluates alert rules against device reports on the hub and delivers what
// fires to the TUI (through the hub's live stream), to phones through ntfy and to chat
// webhooks (Discord, Slack, or anything that accepts JSON).
//
// A rule watches one metric (offline, CPU/GPU temperature, CPU/GPU load, memory, disk,
// power, daily cost) on one device or all of them. It fires once the value has stayed above
// its threshold for the rule's minutes, and resolves only when the value falls back below the
// threshold by the type's hysteresis, so a value hovering at the limit doesn't flap.
//
// The Engine has no timers of its own and no dependency on the hub or internal/power: the
// hub feeds it reports (OnReport), calls Tick and CheckCost (or Run), and supplies device
// status, power and cost through Options. The clock, HTTP client and DNS resolver are
// injectable for tests.
//
// Ported from Forward-Future/device-monitor alerts.js (MIT, © 2026 Matt):
// https://github.com/Forward-Future/device-monitor
// Rule types, limits, defaults, hysteresis, the state machine, message wording and delivery
// payloads follow upstream.
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

// Rule types.
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

// TypeInfo describes one rule type.
type TypeInfo struct {
	Label string
	// HasThreshold is false for offline. Min and Max bound the threshold.
	HasThreshold bool
	Min, Max     float64
	// Hyst is how far a value must fall below the threshold to resolve; 0 means 5 % of the
	// threshold.
	Hyst float64
}

// Types lists every rule type and its limits.
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

// AllDevices is the Rule.Device value that matches every device.
const AllDevices = "*"

// DefaultNtfyServer is used when a channel names no server.
const DefaultNtfyServer = "https://ntfy.sh"

const (
	maxRules = 100
	maxLog   = 300
)

// Rule is one alert rule.
type Rule struct {
	ID        string   `json:"id"` // 12 hex chars
	Type      string   `json:"type"`
	Device    string   `json:"device"`    // device id or AllDevices
	Threshold *float64 `json:"threshold"` // nil for offline
	Minutes   int      `json:"minutes"`   // how long the value must stay over; 1–1440 for offline
	Enabled   bool     `json:"enabled"`
}

// Settings is everything the user configures: rules and delivery channels.
type Settings struct {
	Rules          []Rule   `json:"rules"`
	Channels       Channels `json:"channels"`
	NotifyResolved bool     `json:"notifyResolved"`
	TempUnit       string   `json:"tempUnit"` // "C" or "F"
}

// Channels says where alerts go.
type Channels struct {
	Desktop bool    `json:"desktop"` // in-TUI banner and bell
	Ntfy    Ntfy    `json:"ntfy"`
	Webhook Webhook `json:"webhook"`
}

// Ntfy publishes to an ntfy server (https://ntfy.sh by default).
type Ntfy struct {
	Enabled bool   `json:"enabled"`
	Server  string `json:"server"`
	Topic   string `json:"topic"`
	Email   string `json:"email"`
}

// Webhook POSTs JSON that Discord (content) and Slack (text) both understand.
type Webhook struct {
	Enabled bool   `json:"enabled"`
	URL     string `json:"url"`
}

// DefaultSettings is what a new hub starts with.
func DefaultSettings() Settings {
	s, err := ParseSettings([]byte(`{"rules":[
		{"type":"offline","device":"*","minutes":10},
		{"type":"cpuTemp","device":"*","threshold":90,"minutes":2},
		{"type":"gpuTemp","device":"*","threshold":85,"minutes":2},
		{"type":"disk","device":"*","threshold":90,"minutes":0}
	],"channels":{"desktop":true}}`))
	if err != nil {
		panic(err) // the literal above is valid
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

// Input shapes: as lenient as upstream's validSettings, which takes whatever the dashboard
// (or a hand-edited alerts.json) sends.
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

// ParseSettings validates settings JSON from the API or alerts.json (port of validSettings).
// Rules without a valid id get a new one. Any invalid rule or channel rejects the whole input.
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
		m = 0 // upstream: Number(x) || 0
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

// number converts like JS Number(): numbers, numeric strings, null → 0, bools → 0/1.
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

// truthy is JS truthiness for decoded JSON values.
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
	return true // objects and arrays
}

// jsRound is Math.round: halves round toward +∞.
func jsRound(x float64) float64 { return math.Floor(x + 0.5) }

// clean drops control characters, trims and caps a string; non-strings become "".
func clean(v any, max int) string {
	s, _ := v.(string)
	s = strings.TrimSpace(ctrlRE.ReplaceAllString(s, ""))
	if r := []rune(s); len(r) > max {
		s = string(r[:max])
	}
	return s
}

// httpURL returns an http(s) URL without a trailing slash, or "" if it isn't one or points
// at a local name or address. Where a DNS name resolves is checked again at delivery.
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

// oddIPv4 catches IPv4 spellings Go's parser doesn't treat as addresses but some resolvers
// do ("2130706433", "0x7f.1", "127.1"), which a browser-style parser would turn into
// 127.0.0.1. They are refused outright rather than guessed at.
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
	rand.Read(b) // never fails (crypto/rand panics instead)
	return hex.EncodeToString(b)
}
