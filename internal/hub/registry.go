package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"math"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/alerts"
	"github.com/ContractorKeith/machine-monitor/internal/config"
	"github.com/ContractorKeith/machine-monitor/internal/history"
	"github.com/ContractorKeith/machine-monitor/internal/power"
	"github.com/ContractorKeith/machine-monitor/internal/report"
)

type Device struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Hostname  *string  `json:"hostname"`
	Platform  string   `json:"platform"`
	OS        *string  `json:"os"`
	FirstSeen int64    `json:"firstSeen"`
	LastSeen  int64    `json:"lastSeen"`
	Order     int      `json:"order"`
	Renamed   bool     `json:"renamed,omitempty"`
	HW        *HW      `json:"hw,omitempty"`
	MACs      []string `json:"macs,omitempty"`
}

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

type Latest struct {
	report.Report
	ReceivedAt int64            `json:"receivedAt"`
	PowerEst   *power.Breakdown `json:"powerEst"`
}

type View struct {
	Device
	Online  bool            `json:"online"`
	Latest  *Latest         `json:"latest"`
	History []history.Point `json:"history"`
}

type reportReply struct {
	OK       bool      `json:"ok"`
	Procs    bool      `json:"procs,omitempty"`
	Commands []command `json:"commands,omitempty"`
}

type command struct {
	ID     string `json:"id"`
	Action string `json:"action"`
	T      int64  `json:"t"`
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

func (h *Hub) saveRegistry() error {
	h.saveMu.Lock()
	defer h.saveMu.Unlock()
	h.mu.Lock()
	b, err := json.MarshalIndent(h.registry, "", "  ")
	h.mu.Unlock()
	if err != nil {
		return err
	}
	if bytes.Equal(b, h.lastSaved) {
		return nil
	}
	if err := config.WriteFile0600(h.opt.Paths.Devices, b); err != nil {
		return err
	}
	h.lastSaved = b
	return nil
}

const (
	saveRetryMin = 5 * time.Second
	saveRetryMax = 5 * time.Minute
)

func (h *Hub) scheduleSave() {
	if h.saveT != nil || h.stopped {
		return
	}
	delay := saveDelay
	if h.saveFailing {
		delay = h.saveBackoff
	}
	h.saveWG.Add(1)
	h.saveT = time.AfterFunc(delay, func() {
		defer h.saveWG.Done()
		h.mu.Lock()
		h.saveT = nil
		h.mu.Unlock()
		h.saved(h.saveRegistry())
	})
}

func (h *Hub) saved(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case err != nil && !h.saveFailing:
		h.logf("hub: save devices: %v (retrying less often until it works)", err)
		h.saveFailing, h.saveBackoff = true, saveRetryMin
	case err != nil:
		h.saveBackoff = min(2*h.saveBackoff, saveRetryMax)
	case h.saveFailing:
		h.logf("hub: saved devices again")
		h.saveFailing = false
	}
}

func (h *Hub) nowMS() int64 { return h.now().UnixMilli() }

func (h *Hub) online(d *Device) bool {
	return h.nowMS()-d.LastSeen < OfflineAfter.Milliseconds()
}

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

func (h *Hub) view(id string) View {
	d := h.registry[id]
	hist := h.history.Recent(id, LiveHistory)
	if hist == nil {
		hist = []history.Point{}
	}
	return View{Device: *d, Online: h.online(d), Latest: h.latest[id], History: hist}
}

func (h *Hub) views() []View {
	out := []View{}
	for _, id := range h.sortedIDs() {
		out = append(out, h.view(id))
	}
	return out
}

func (h *Hub) deviceLock(id string) *sync.Mutex {
	h.mu.Lock()
	defer h.mu.Unlock()
	if l := h.ingestLocks[id]; l != nil {
		return l
	}
	if h.registry[id] == nil && len(h.registry) >= MaxDevices {
		return nil
	}
	l := &sync.Mutex{}
	h.ingestLocks[id] = l
	return l
}

var errDeviceKey = errors.New("bad device key")

func (h *Hub) ingest(raw []byte) (reportReply, error) { return h.ingestFrom(raw, false) }

func (h *Hub) ingestFrom(raw []byte, own bool) (reportReply, error) {
	r, err := report.Normalize(raw)
	if errors.Is(err, report.ErrInvalidID) {
		return reportReply{}, errors.New("invalid id")
	}
	if err != nil {
		return reportReply{}, err
	}
	procs := r.Processes
	r.Processes = nil

	lk := h.deviceLock(r.ID)
	if lk == nil {
		return reportReply{}, errTooManyDevices
	}
	lk.Lock()
	defer lk.Unlock()
	now := h.nowMS()
	est := power.Estimate(&r)
	sort.SliceStable(r.GPUs, func(i, j int) bool { return val(r.GPUs[i].MemTotal) > val(r.GPUs[j].MemTotal) })
	latest := &Latest{Report: r, ReceivedAt: now, PowerEst: est}

	h.mu.Lock()
	var auth struct {
		DeviceKey string `json:"deviceKey"`
	}
	_ = json.Unmarshal(raw, &auth)
	if key := h.cfg.DeviceKeys[r.ID]; !own && key != "" && !safeEqual(key, auth.DeviceKey) {
		h.mu.Unlock()
		return reportReply{}, errDeviceKey
	}
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
	h.alerts.OnReport(r.ID, &r)
	if procs != nil {
		h.storeProcs(r.ID, procs)
	}
	return h.replyFor(r.ID, r.Actions), nil
}

func sampleOf(now int64, r *report.Report, est *power.Breakdown) history.Point {
	var g report.GPU
	if len(r.GPUs) > 0 {
		g = r.GPUs[0]
	}
	p := history.Point{T: float64(now), CPU: r.CPU.Usage, GPU: g.Usage, CPUTemp: r.CPU.Temp,
		GPUTemp: g.Temp, GPUPower: g.Power}
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

func (h *Hub) onlineFlags() map[string]bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	online := make(map[string]bool, len(h.registry))
	for id, d := range h.registry {
		online[id] = h.online(d)
	}
	return online
}

func (h *Hub) broadcastStatus() {
	h.sentOnline = h.onlineFlags()
	h.broadcast(map[string]any{"type": "status", "online": h.sentOnline})
}

func (h *Hub) checkOnline() time.Duration {
	online := h.onlineFlags()
	changed := false
	for id, on := range online {
		if prev, ok := h.sentOnline[id]; !on && (prev || !ok) {
			changed = true
		}
	}
	if changed {
		h.sentOnline = online
		h.broadcast(map[string]any{"type": "status", "online": online})
	}
	next := time.Second
	h.mu.Lock()
	now := h.nowMS()
	for _, d := range h.registry {
		if left := time.Duration(d.LastSeen+OfflineAfter.Milliseconds()-now) * time.Millisecond; left > 0 && left < next {
			next = left
		}
	}
	h.mu.Unlock()
	return max(next, 10*time.Millisecond)
}

func (h *Hub) collectLoop(ctx context.Context) {
	t := time.NewTicker(h.opt.Interval)
	defer t.Stop()
	warned, wantProcs := false, false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		r, err := h.opt.Collector.Collect(cctx)
		if err == nil && r != nil && h.opt.Agent != "" {
			r.Agent = h.opt.Agent
		}
		if err == nil && r != nil && h.opt.Procs != nil {
			r.Features = append(r.Features, "procs")
			if wantProcs {
				if list, perr := h.opt.Procs(cctx); perr == nil {
					r.Processes = busiest(list, maxProcs)
				}
			}
		}
		cancel()
		if err != nil || r == nil {
			if err != nil && ctx.Err() == nil {
				h.logf("hub: collect: %v", err)
			}
			continue
		}
		if !report.ValidID(r.ID) {
			if !warned {
				h.logf("hub: this machine's collector reports no device id; not monitoring it")
				warned = true
			}
			continue
		}
		r.Token = ""
		raw, err := json.Marshal(r)
		if err == nil {
			var rep reportReply
			rep, err = h.ingestFrom(raw, true)
			wantProcs = rep.Procs
		}
		if err != nil {
			h.logf("hub: own report: %v", err)
		}
	}
}
