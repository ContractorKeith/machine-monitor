package hub

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sync"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/alerts"
	"github.com/ContractorKeith/machine-monitor/internal/collect"
	"github.com/ContractorKeith/machine-monitor/internal/config"
	"github.com/ContractorKeith/machine-monitor/internal/discovery"
	"github.com/ContractorKeith/machine-monitor/internal/history"
	"github.com/ContractorKeith/machine-monitor/internal/power"
	"github.com/ContractorKeith/machine-monitor/internal/report"
	"github.com/ContractorKeith/machine-monitor/internal/tailscale"
)

const (
	OfflineAfter   = 10 * time.Second
	StatusInterval = 3 * time.Second
	LiveHistory    = 10 * time.Minute
	MaxDevices     = 250
	MaxStreams     = 64
	maxBody        = 1_000_000
	saveDelay      = time.Second
)

const DefaultPort = 7777

type Options struct {
	Paths     config.Paths
	Port      int
	Collector collect.Collector
	Agent     string
	Procs     func(ctx context.Context) ([]report.Process, error)
	Interval  time.Duration
	Now       func() time.Time
	Hostname  string
	Location  *time.Location
	Logf      func(format string, args ...any)
	Discovery string
	Tailscale func(ctx context.Context, hubPort int) tailscale.Status
}

type Hub struct {
	opt         Options
	now         func() time.Time
	history     *history.Store
	alerts      *alerts.Engine
	lock        io.Closer
	rate        *rateLimiter
	reportFails *rateLimiter
	pair        pairState
	act         actionState

	mu          sync.Mutex
	sendQOnce   sync.Once
	cfg         hubConfig
	registry    map[string]*Device
	latest      map[string]*Latest
	ingestLocks map[string]*sync.Mutex
	saveT       *time.Timer
	stopped     bool

	saveMu sync.Mutex
	saveWG sync.WaitGroup

	smu           sync.Mutex
	streams       map[*stream]struct{}
	activeStreams int
	closed        bool

	streamWriteTimeout time.Duration
	pairGrace          time.Duration
	statusInterval     time.Duration

	sentOnline map[string]bool

	rmu         sync.Mutex
	remote      tailscale.Status
	remoteAt    time.Time
	remoteReads sync.Mutex

	saveFailing bool
	saveBackoff time.Duration
	lastSaved   []byte
}

func New(opt Options) (*Hub, error) {
	if opt.Paths.Dir == "" {
		return nil, errors.New("hub: no data directory")
	}
	if err := os.MkdirAll(opt.Paths.Dir, 0o700); err != nil {
		return nil, err
	}
	lock, err := lockDir(opt.Paths.Dir)
	if err != nil {
		return nil, err
	}
	h, err := newHub(opt)
	if err != nil {
		lock.Close()
		return nil, err
	}
	h.lock = lock
	return h, nil
}

func newHub(opt Options) (*Hub, error) {
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
	h := &Hub{opt: opt, now: opt.Now, rate: newRateLimiter(opt.Now), reportFails: newRateLimiter(opt.Now),
		registry: map[string]*Device{}, latest: map[string]*Latest{}, ingestLocks: map[string]*sync.Mutex{},
		streams: map[*stream]struct{}{}}

	var err error
	if h.cfg, err = loadConfig(opt.Paths.Config); err != nil {
		return nil, err
	}
	for _, n := range h.cfg.notes {
		h.logf("%s", n)
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
	if h.history, err = history.Open(opt.Paths.History, opt.Now); h.history == nil {
		return nil, err
	} else if err != nil {
		h.logf("hub: history: %v (skipped)", err)
	}
	for _, warning := range h.history.Warnings() {
		h.logf("hub: %s", warning)
	}
	h.remote = tailscale.Status{URLs: []string{}}
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

func (h *Hub) Token() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cfg.Token
}

func (h *Hub) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Go(func() { h.alerts.Run(ctx) })
	if h.opt.Collector != nil {
		wg.Go(func() { h.collectLoop(ctx) })
	}
	if h.opt.Tailscale != nil {
		wg.Go(func() { h.remoteLoop(ctx) })
	}
	if h.opt.Discovery != "" {
		if r, err := discovery.Respond(h.opt.Discovery, h.opt.Hostname, h.opt.Port); err != nil {
			h.logf("hub: LAN discovery off: %v", err)
		} else {
			defer r.Close()
		}
	}
	every := StatusInterval
	if h.statusInterval != 0 {
		every = h.statusInterval
	}
	status := time.NewTicker(every)
	prune := time.NewTicker(RateWindow)
	offline := time.NewTimer(h.checkOnline())
	defer status.Stop()
	defer prune.Stop()
	defer offline.Stop()
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			h.closeStreams()
			return
		case <-status.C:
			h.broadcastStatus()
		case <-offline.C:
			offline.Reset(h.checkOnline())
		case <-prune.C:
			h.rate.prune()
			h.reportFails.prune()
		}
	}
}

func (h *Hub) Close() error {
	h.closeStreams()
	h.mu.Lock()
	h.stopped = true
	pending := h.saveT != nil && h.saveT.Stop()
	if pending {
		h.saveWG.Done()
	}
	h.saveT = nil
	h.mu.Unlock()
	h.saveWG.Wait()
	var errs []error
	if pending {
		errs = append(errs, h.saveRegistry())
	}
	h.alerts.Wait()
	errs = append(errs, h.history.Close())
	if h.lock != nil {
		errs = append(errs, h.lock.Close())
	}
	return errors.Join(errs...)
}

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

type hubConfig struct {
	DeviceKeys   map[string]string `json:"deviceKeys,omitempty"`
	Token        string            `json:"token"`
	Energy       *power.Settings   `json:"energy,omitempty"`
	AllowedHosts []string          `json:"allowedHosts,omitempty"`
	extra        map[string]json.RawMessage
	notes        []string
}

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
		return c, fmt.Errorf("hub: %s is damaged (%v); restore it from a backup, or delete it to start over (that makes a new hub key and un-pairs every device, so pair each machine again with machine-monitor connect)", path, err)
	}
	if err := json.Unmarshal(b, &c); err != nil {
		if bad := json.Unmarshal(c.extra["deviceKeys"], &map[string]string{}); bad != nil && c.extra["deviceKeys"] != nil {
			return c, fmt.Errorf("hub: %s: deviceKeys is damaged (%v); remove the \"deviceKeys\" entry and pair each machine again with machine-monitor connect", path, bad)
		}
		return c, fmt.Errorf("hub: %s is damaged (%v); restore it from a backup, or delete it to start over (that makes a new hub key and un-pairs every device, so pair each machine again with machine-monitor connect)", path, err)
	}
	if c.Energy != nil {
		raw, _ := json.Marshal(c.Energy)
		s, err := power.LoadSettings(raw)
		switch {
		case err == nil:
			c.Energy = &s
		case errors.Is(err, power.ErrOverlappingPeriods):
			c.Energy = &s
			c.notes = append(c.notes, "hub: config.json: the electricity price's time-of-use periods overlap, so only the flat rate is used; set the periods again in Settings")
		default:
			c.Energy = nil
			c.notes = append(c.notes, "hub: config.json: the electricity price is invalid and was ignored")
		}
	}
	delete(c.extra, "deviceKeys")
	delete(c.extra, "token")
	delete(c.extra, "energy")
	delete(c.extra, "allowedHosts")
	return c, nil
}

func LocalKey(paths config.Paths) (string, error) {
	c, err := loadConfig(paths.Config)
	if err != nil {
		return "", err
	}
	if c.Token == "" {
		return "", fmt.Errorf("no hub key in %s: start the hub on this machine first (machine-monitor hub)", paths.Dir)
	}
	return c.Token, nil
}

func (h *Hub) saveConfig() error {
	out := map[string]any{}
	for k, v := range h.cfg.extra {
		out[k] = v
	}
	out["token"] = h.cfg.Token
	if len(h.cfg.DeviceKeys) > 0 {
		out["deviceKeys"] = h.cfg.DeviceKeys
	}
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

func newToken() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
