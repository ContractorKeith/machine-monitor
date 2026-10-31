// Package hub is the machine-monitor hub: it receives reports from agents over HTTP, keeps
// the device registry and history, estimates power, evaluates alerts and streams everything
// to connected TUIs over server-sent events (PLAN §5–§6).
//
// The API paths and JSON shapes match upstream device-monitor's hub, so upstream agents
// (agent.sh, agent-macos.sh, agent.ps1) report to it unmodified and upstream clients can
// read it. Every report goes through report.Normalize before the hub stores or streams it.
//
// Ported from Forward-Future/device-monitor server.js @ c3a4c41
// (MIT, © 2026 Matt): https://github.com/Forward-Future/device-monitor
package hub

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/alerts"
	"github.com/ContractorKeith/machine-monitor/internal/collect"
	"github.com/ContractorKeith/machine-monitor/internal/config"
	"github.com/ContractorKeith/machine-monitor/internal/history"
	"github.com/ContractorKeith/machine-monitor/internal/power"
	"github.com/ContractorKeith/machine-monitor/internal/report"
)

// Limits and timings, as upstream.
const (
	OfflineAfter   = 10 * time.Second // a device with no report for this long is offline
	StatusInterval = 3 * time.Second  // SSE "status" heartbeat with every device's online flag
	LiveHistory    = 10 * time.Minute // raw samples sent with each device in snapshots
	MaxDevices     = 250              // caps registry and history growth from bogus reports
	MaxStreams     = 64               // concurrent SSE connections
	maxBody        = 1_000_000        // request body limit in bytes
	saveDelay      = time.Second      // devices.json is written at most this often
)

// DefaultPort is the hub's TCP port, as upstream.
const DefaultPort = 7777

// Options configures a Hub. Paths is required; everything else has a default.
type Options struct {
	Paths config.Paths
	// Port is the port the hub listens on, reported by /api/info and used in links.
	Port int
	// Collector samples this machine so the hub monitors itself, as upstream's hub runs its
	// own agent. nil turns self-monitoring off.
	Collector collect.Collector
	// Procs lists this machine's processes, added to its own report while a viewer has its
	// Processes open (procs.go). nil never lists them.
	Procs func(ctx context.Context) ([]report.Process, error)
	// Interval is how often Run samples Collector; 2 s when zero.
	Interval time.Duration
	// Now is the clock; time.Now when nil.
	Now func() time.Time
	// Hostname is this machine's name for /api/info and the Host check; os.Hostname when "".
	Hostname string
	// Location is the hub's local time zone for "today" and time-of-use prices; time.Local
	// when nil.
	Location *time.Location
	// Logf logs problems that don't fail a request; dropped when nil.
	Logf func(format string, args ...any)
}

// Hub is a running hub. Create one with New, serve Handler, call Run, then Close.
type Hub struct {
	opt     Options
	now     func() time.Time
	history *history.Store
	alerts  *alerts.Engine
	rate    *rateLimiter
	pair    pairState   // the active pairing code (pair.go)
	act     actionState // command queue and processes on demand (commands.go, procs.go)

	mu       sync.Mutex
	cfg      hubConfig
	registry map[string]*Device // id -> device
	latest   map[string]*Latest // id -> last report (memory only)
	// id -> lock held for the whole of one report's intake; see ingest
	ingestLocks map[string]*sync.Mutex
	saveT       *time.Timer
	stopped     bool // Close has run: no more delayed saves

	saveMu sync.Mutex     // serializes devices.json writes
	saveWG sync.WaitGroup // delayed saves scheduled or running

	smu           sync.Mutex
	streams       map[*stream]struct{}
	activeStreams int // stream handlers running, including dropped ones still finishing a write
	closed        bool

	streamWriteTimeout time.Duration // tests shorten StreamWriteTimeout
}

// New loads (or creates) config.json with the hub key, devices.json and history, and wires
// alerts. It starts nothing; call Run.
func New(opt Options) (*Hub, error) {
	if opt.Paths.Dir == "" {
		return nil, errors.New("hub: no data directory")
	}
	if opt.Port == 0 {
		opt.Port = DefaultPort
	}
	if opt.Interval == 0 {
		opt.Interval = 2 * time.Second
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Hostname == "" {
		opt.Hostname, _ = os.Hostname()
	}
	if opt.Location == nil {
		opt.Location = time.Local
	}
	if err := os.MkdirAll(opt.Paths.Dir, 0o700); err != nil {
		return nil, err
	}
	h := &Hub{opt: opt, now: opt.Now, rate: newRateLimiter(opt.Now),
		registry: map[string]*Device{}, latest: map[string]*Latest{}, ingestLocks: map[string]*sync.Mutex{},
		streams: map[*stream]struct{}{}}

	var err error
	if h.cfg, err = loadConfig(opt.Paths.Config); err != nil {
		return nil, err
	}
	if h.cfg.Token == "" {
		if h.cfg.Token, err = newToken(); err != nil {
			return nil, err
		}
		if err := h.saveConfig(); err != nil {
			return nil, err
		}
	}
	if err := h.loadRegistry(); err != nil {
		return nil, err
	}
	if h.history, err = history.Open(opt.Paths.History, opt.Now); err != nil {
		return nil, err
	}
	h.alerts = alerts.New(alerts.Options{
		Paths:     opt.Paths,
		Devices:   h.alertDevices,
		Energy:    energySource{h},
		Broadcast: func(m alerts.Message) { h.broadcast(m) },
		HubURL:    h.preferredURL,
		Now:       opt.Now,
		Logf:      opt.Logf,
	})
	return h, nil
}

// Token is the hub key: agents put it in reports, clients send it as a Bearer token.
func (h *Hub) Token() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cfg.Token
}

// Run runs the hub's timers until ctx is done: the SSE status heartbeat (which is also how
// clients see devices go offline), alert checks, self-monitoring and rate-limit cleanup.
// When it returns, every SSE stream has ended, so http.Server.Shutdown won't wait on them.
func (h *Hub) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Go(func() { h.alerts.Run(ctx) })
	if h.opt.Collector != nil {
		wg.Go(func() { h.collectLoop(ctx) })
	}
	status := time.NewTicker(StatusInterval)
	prune := time.NewTicker(RateWindow)
	defer status.Stop()
	defer prune.Stop()
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			h.closeStreams()
			return
		case <-status.C:
			h.broadcastStatus()
		case <-prune.C:
			h.rate.prune()
		}
	}
}

// Close ends every SSE stream and writes devices.json and unfinished history minutes. Call
// it after Run has returned and the HTTP server has shut down.
func (h *Hub) Close() error {
	h.closeStreams()
	h.mu.Lock()
	h.stopped = true
	pending := h.saveT != nil && h.saveT.Stop()
	if pending {
		h.saveWG.Done() // the stopped save will never run
	}
	h.saveT = nil
	h.mu.Unlock()
	h.saveWG.Wait() // a save that already started
	var errs []error
	if pending {
		errs = append(errs, h.saveRegistry())
	}
	h.alerts.Wait()
	errs = append(errs, h.history.Close())
	return errors.Join(errs...)
}

// closeStreams ends every SSE stream and refuses new ones.
func (h *Hub) closeStreams() {
	h.smu.Lock()
	defer h.smu.Unlock()
	h.closed = true
	for s := range h.streams {
		h.drop(s)
	}
}

func (h *Hub) logf(format string, args ...any) {
	if h.opt.Logf != nil {
		h.opt.Logf(format, args...)
	}
}

// ---- config.json -------------------------------------------------------------------------

// hubConfig is config.json. Keys other hub versions or tools wrote are kept in extra.
type hubConfig struct {
	Token        string          `json:"token"`
	Energy       *power.Settings `json:"energy,omitempty"` // nil until the user sets a price
	AllowedHosts []string        `json:"allowedHosts,omitempty"`
	extra        map[string]json.RawMessage
}

// loadConfig reads config.json. A missing file is an empty config; a damaged one is an error,
// since silently starting over would issue a new key and lock out every agent.
func loadConfig(path string) (hubConfig, error) {
	var c hubConfig
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c.extra); err != nil {
		return c, fmt.Errorf("hub: %s: %w", path, err)
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("hub: %s: %w", path, err)
	}
	if c.Energy != nil { // re-validate: the file may be hand-edited
		raw, _ := json.Marshal(c.Energy)
		if s, err := power.ParseSettings(raw); err == nil {
			c.Energy = &s
		} else {
			c.Energy = nil
		}
	}
	delete(c.extra, "token")
	delete(c.extra, "energy")
	delete(c.extra, "allowedHosts")
	return c, nil
}

// saveConfig writes config.json. Caller holds mu, or is New.
func (h *Hub) saveConfig() error {
	out := map[string]any{}
	for k, v := range h.cfg.extra {
		out[k] = v
	}
	out["token"] = h.cfg.Token
	if h.cfg.Energy != nil {
		out["energy"] = h.cfg.Energy
	}
	if len(h.cfg.AllowedHosts) > 0 {
		out["allowedHosts"] = h.cfg.AllowedHosts
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return config.WriteFile0600(h.opt.Paths.Config, b)
}

// newToken is 12 random bytes in hex, as upstream.
func newToken() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
