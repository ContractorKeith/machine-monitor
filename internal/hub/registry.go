package hub

// Device registry, devices.json, and report intake.
//
// Ported from Forward-Future/device-monitor server.js (handleReport, deviceView) @ c3a4c41
// (MIT, © 2026 Matt): https://github.com/Forward-Future/device-monitor

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"math"
	"os"
	"sort"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/alerts"
	"github.com/ContractorKeith/machine-monitor/internal/config"
	"github.com/ContractorKeith/machine-monitor/internal/history"
	"github.com/ContractorKeith/machine-monitor/internal/power"
	"github.com/ContractorKeith/machine-monitor/internal/report"
)

// Device is one registry entry, as stored in devices.json. Times are Unix ms.
type Device struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Hostname  *string  `json:"hostname"`
	Platform  string   `json:"platform,omitempty"`
	OS        *string  `json:"os"`
	FirstSeen int64    `json:"firstSeen"`
	LastSeen  int64    `json:"lastSeen"`
	Order     int      `json:"order"`
	Renamed   bool     `json:"renamed,omitempty"` // the user named it; reports no longer rename it
	HW        *HW      `json:"hw,omitempty"`      // for power estimates while offline
	MACs      []string `json:"macs,omitempty"`    // for Wake-on-LAN while it's off
}

// HW is the hardware a device last reported.
type HW struct {
	FormFactor *string  `json:"formFactor"`
	Threads    *float64 `json:"threads"`
}

func (d *Device) powerHW() power.HW {
	if d.HW == nil {
		return power.HW{}
	}
	return power.HW{FormFactor: d.HW.FormFactor, Threads: d.HW.Threads}
}

// Latest is a device's last normalized report plus what the hub adds to it.
type Latest struct {
	report.Report
	ReceivedAt int64            `json:"receivedAt"` // Unix ms
	PowerEst   *power.Breakdown `json:"powerEst"`
}

// View is a device as /api/devices and SSE snapshots show it.
type View struct {
	Device
	Online  bool            `json:"online"`
	Latest  *Latest         `json:"latest"`
	History []history.Point `json:"history"` // raw samples from the last LiveHistory
}

// reportReply is the body of a successful POST /api/report: it tells the agent what clients
// want from it. Procs (send the process list) and Commands (signed power actions) arrive
// with M7; until then the reply is always {"ok":true}.
type reportReply struct {
	OK       bool      `json:"ok"`
	Procs    bool      `json:"procs,omitempty"`
	Commands []command `json:"commands,omitempty"`
}

// command is a queued remote action, HMAC-signed so an agent can tell it from one a LAN
// MITM injected into the plain-HTTP reply (M7).
type command struct {
	ID     string `json:"id"`
	Action string `json:"action"`
	Sig    string `json:"sig"`
}

var errTooManyDevices = errors.New("too many devices")

func (h *Hub) loadRegistry() error {
	b, err := os.ReadFile(h.opt.Paths.Devices)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var saved map[string]*Device
	if err := json.Unmarshal(b, &saved); err != nil {
		h.logf("hub: %s: %v (starting with no devices)", h.opt.Paths.Devices, err)
		return nil
	}
	for id, d := range saved {
		if report.ValidID(id) && d != nil {
			d.ID = id
			h.registry[id] = d
		}
	}
	return nil
}

// saveRegistry writes devices.json now.
func (h *Hub) saveRegistry() error {
	h.saveMu.Lock()
	defer h.saveMu.Unlock()
	h.mu.Lock()
	b, err := json.MarshalIndent(h.registry, "", "  ")
	h.mu.Unlock()
	if err != nil {
		return err
	}
	return config.WriteFile0600(h.opt.Paths.Devices, b)
}

// scheduleSave writes devices.json within saveDelay, coalescing bursts. Caller holds mu.
func (h *Hub) scheduleSave() {
	if h.saveT != nil || h.stopped {
		return
	}
	h.saveWG.Add(1)
	h.saveT = time.AfterFunc(saveDelay, func() {
		defer h.saveWG.Done()
		h.mu.Lock()
		h.saveT = nil
		h.mu.Unlock()
		if err := h.saveRegistry(); err != nil {
			h.logf("hub: save devices: %v", err)
		}
	})
}

func (h *Hub) nowMS() int64 { return h.now().UnixMilli() }

// online reports whether d has reported within OfflineAfter.
func (h *Hub) online(d *Device) bool {
	return h.nowMS()-d.LastSeen < OfflineAfter.Milliseconds()
}

// sortedIDs lists registered ids by first sighting (upstream's insertion order). Caller
// holds mu.
func (h *Hub) sortedIDs() []string {
	ids := make([]string, 0, len(h.registry))
	for id := range h.registry {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := h.registry[ids[i]], h.registry[ids[j]]
		if a.FirstSeen != b.FirstSeen {
			return a.FirstSeen < b.FirstSeen
		}
		return a.ID < b.ID
	})
	return ids
}

// view builds a device's View. Caller holds mu.
func (h *Hub) view(id string) View {
	d := h.registry[id]
	hist := h.history.Recent(id, LiveHistory)
	if hist == nil {
		hist = []history.Point{}
	}
	return View{Device: *d, Online: h.online(d), Latest: h.latest[id], History: hist}
}

// views lists every device's View. Caller holds mu.
func (h *Hub) views() []View {
	out := []View{}
	for _, id := range h.sortedIDs() {
		out = append(out, h.view(id))
	}
	return out
}

// ingest takes one raw report body (already authenticated) through Normalize into the
// registry, history, power estimate, SSE and alerts.
func (h *Hub) ingest(raw []byte) (reportReply, error) {
	r, err := report.Normalize(raw)
	if errors.Is(err, report.ErrInvalidID) {
		return reportReply{}, errors.New("invalid id")
	}
	if err != nil {
		return reportReply{}, err
	}
	r.Processes = nil // processes on demand arrive with M7; never stored with the report

	now := h.nowMS()
	est := power.Estimate(&r)
	// Biggest GPU first, so the main gauge shows e.g. the GB300 rather than a small display card.
	sort.SliceStable(r.GPUs, func(i, j int) bool { return val(r.GPUs[i].MemTotal) > val(r.GPUs[j].MemTotal) })
	latest := &Latest{Report: r, ReceivedAt: now, PowerEst: est}

	h.mu.Lock()
	d := h.registry[r.ID]
	if d == nil {
		if len(h.registry) >= MaxDevices {
			h.mu.Unlock()
			return reportReply{}, errTooManyDevices
		}
		order := -1
		for _, x := range h.registry {
			order = max(order, x.Order)
		}
		d = &Device{ID: r.ID, Name: r.ID, FirstSeen: now, Order: order + 1}
		h.registry[r.ID] = d
	}
	d.Hostname = r.Hostname
	if !d.Renamed && r.Hostname != nil {
		d.Name = *r.Hostname
	}
	d.Platform = r.Platform
	d.OS = r.OS
	d.LastSeen = now
	d.HW = &HW{FormFactor: r.FormFactor, Threads: r.CPU.Threads}
	if len(r.MACs) > 0 {
		d.MACs = r.MACs
	}
	h.latest[r.ID] = latest
	h.scheduleSave()
	dev := View{Device: *d, Online: true, Latest: latest}
	h.mu.Unlock()

	sample := sampleOf(now, &r, est)
	if err := h.history.Add(r.ID, sample); err != nil {
		h.logf("hub: history %s: %v", r.ID, err)
	}
	h.broadcast(reportEvent{Type: "report", ID: r.ID, Device: deviceEvent{dev}, Sample: sample})
	if r.ActionResult != nil {
		h.broadcast(map[string]any{"type": "actionResult", "id": r.ID, "name": dev.Name, "result": r.ActionResult})
	}
	h.alerts.OnReport(r.ID, &r) // outside mu: the engine calls back into alertDevices
	return reportReply{OK: true}, nil
}

// sampleOf is the history sample for one report: main (first) GPU, memory as a percentage.
func sampleOf(now int64, r *report.Report, est *power.Breakdown) history.Point {
	var g report.GPU
	if len(r.GPUs) > 0 {
		g = r.GPUs[0]
	}
	p := history.Point{T: float64(now), CPU: r.CPU.Usage, GPU: g.Usage, CPUTemp: r.CPU.Temp,
		GPUTemp: g.Temp, GPUPower: g.Power}
	// Upstream treats a missing "used" as 0 %; unknown stays nil here, per the project rule.
	if t, u := r.Memory.Total, r.Memory.Used; t != nil && *t > 0 && u != nil {
		p.Mem = report.Ptr(*u / *t * 100)
	}
	if est != nil {
		p.Power = report.Ptr(est.Watts)
	}
	if r.Net != nil {
		p.RX, p.TX = r.Net.RX, r.Net.TX
	}
	if r.DiskIO != nil {
		p.DR, p.DW = r.DiskIO.Read, r.DiskIO.Write
	}
	return p
}

func val(v *float64) float64 {
	if v == nil || math.IsNaN(*v) {
		return 0
	}
	return *v
}

// deviceEvent is a View without history, for "report" events (upstream sends
// { ...reg, online: true, latest }).
type deviceEvent struct{ v View }

func (d deviceEvent) MarshalJSON() ([]byte, error) {
	type noHistory struct {
		Device
		Online bool    `json:"online"`
		Latest *Latest `json:"latest"`
	}
	return json.Marshal(noHistory{d.v.Device, d.v.Online, d.v.Latest})
}

type reportEvent struct {
	Type   string        `json:"type"`
	ID     string        `json:"id"`
	Device deviceEvent   `json:"device"`
	Sample history.Point `json:"sample"`
}

// alertDevices lists devices for the alerts engine. The engine may hold its own lock, so
// this must never call into it.
func (h *Hub) alertDevices() []alerts.Device {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]alerts.Device, 0, len(h.registry))
	for _, id := range h.sortedIDs() {
		d := h.registry[id]
		var seen time.Time
		if d.LastSeen != 0 {
			seen = time.UnixMilli(d.LastSeen)
		}
		out = append(out, alerts.Device{ID: id, Name: d.Name, Online: h.online(d), LastSeen: seen})
	}
	return out
}

// broadcastStatus sends every device's online flag: the heartbeat that lets clients notice
// devices that went offline.
func (h *Hub) broadcastStatus() {
	h.mu.Lock()
	online := make(map[string]bool, len(h.registry))
	for id, d := range h.registry {
		online[id] = h.online(d)
	}
	h.mu.Unlock()
	h.broadcast(map[string]any{"type": "status", "online": online})
}

// collectLoop samples this machine every Interval and ingests it like any agent's report.
// The report gets the hub key implicitly: it never leaves the process.
func (h *Hub) collectLoop(ctx context.Context) {
	t := time.NewTicker(h.opt.Interval)
	defer t.Stop()
	warned := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		r, err := h.opt.Collector.Collect(cctx)
		cancel()
		if err != nil || r == nil {
			if err != nil && ctx.Err() == nil {
				h.logf("hub: collect: %v", err)
			}
			continue
		}
		if !report.ValidID(r.ID) { // the collector doesn't know this machine's id yet
			if !warned {
				h.logf("hub: this machine's collector reports no device id; not monitoring it")
				warned = true
			}
			continue
		}
		r.Token = ""
		raw, err := json.Marshal(r)
		if err == nil {
			_, err = h.ingest(raw)
		}
		if err != nil {
			h.logf("hub: own report: %v", err)
		}
	}
}
