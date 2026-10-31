package tui

import (
	"slices"
	"strings"

	"github.com/ContractorKeith/machine-monitor/internal/client"
	"github.com/ContractorKeith/machine-monitor/internal/history"
	"github.com/ContractorKeith/machine-monitor/internal/report"
)

// device is one machine the views can show. Local mode has one, fed by the collector; hub
// mode has one per device on the hub, fed by its stream. Every view reads devices only, so
// the two modes share them.
type device struct {
	id, name  string
	online    bool
	order     int
	firstSeen int64 // Unix ms; ties in order break on it, as the hub lists devices
	lastSeen  int64 // Unix ms
	last      *report.Report
	watts     *float64 // the hub's at-the-wall estimate; local mode estimates while drawing
	cpu, mem  series
	gpu       series
	// Hub mode: our hub's snapshot carries no history, so the sparklines start with live
	// samples (liveFrom is the first one's time, Unix ms) until sparkSeed fills in the
	// recent past (seeded).
	liveFrom float64
	seeded   bool
}

// push records one report and its samples.
func (d *device) push(r *report.Report) {
	cleanAll(r)
	d.last = r
	d.cpu.push(r.CPU.Usage)
	d.mem.push(pctOf(r.Memory.Used, r.Memory.Total))
	var g *float64
	if len(r.GPUs) > 0 {
		g = r.GPUs[0].Usage
	}
	d.gpu.push(g)
}

// pushPoint records one history sample from the hub.
func (d *device) pushPoint(p history.Point) {
	if d.liveFrom == 0 {
		d.liveFrom = p.T
	}
	d.cpu.push(p.CPU)
	d.mem.push(p.Mem)
	d.gpu.push(p.GPU)
}

// setRegistry copies a hub device's registry fields and latest report (when it has one).
func (d *device) setRegistry(v *client.Device) {
	d.id, d.name, d.online = v.ID, clean(v.Name), v.Online
	d.order, d.firstSeen, d.lastSeen = v.Order, v.FirstSeen, v.LastSeen
	if v.Latest != nil {
		r := v.Latest.Report
		cleanAll(&r)
		d.last = &r
		d.watts = nil
		if v.Latest.PowerEst != nil {
			d.watts = report.Ptr(v.Latest.PowerEst.Watts)
		}
	}
}

// fromHub builds a device from a snapshot entry, its sparklines from the recent history.
func fromHub(v client.Device) device {
	var d device
	d.setRegistry(&v)
	for _, p := range v.History {
		d.pushPoint(p)
	}
	d.seeded = len(v.History) > 0 // an upstream hub's snapshot has it already
	return d
}

// seed puts the recent past (pts, oldest first) in front of the live samples the
// sparklines have so far, skipping points the live ones already cover.
func (d *device) seed(pts []history.Point) {
	var cpu, mem, gpu series
	for _, p := range pts {
		if d.liveFrom == 0 || p.T < d.liveFrom {
			cpu.push(p.CPU)
			mem.push(p.Mem)
			gpu.push(p.GPU)
		}
	}
	d.cpu, d.mem, d.gpu = cpu.then(d.cpu), mem.then(d.mem), gpu.then(d.gpu)
	d.seeded = true
}

// sortDevices puts devices in the hub's display order: order, then first seen, then id.
func sortDevices(ds []device) {
	slices.SortStableFunc(ds, func(a, b device) int {
		switch {
		case a.order != b.order:
			return a.order - b.order
		case a.firstSeen != b.firstSeen:
			if a.firstSeen < b.firstSeen {
				return -1
			}
			return 1
		case a.id < b.id:
			return -1
		case a.id > b.id:
			return 1
		}
		return 0
	})
}

// cur is the selected device, or nil when there is none.
func (m model) cur() *device {
	for i := range m.devs {
		if m.devs[i].id == m.sel {
			return &m.devs[i]
		}
	}
	if len(m.devs) > 0 {
		return &m.devs[0]
	}
	return nil
}

// report is the selected device's last report, or nil.
func (m model) report() *report.Report {
	if d := m.cur(); d != nil {
		return d.last
	}
	return nil
}

// editDev changes the device with id in a fresh copy of the list (Bubble Tea copies the
// model on every update; a shared backing array would leak edits into old copies), adding
// it when it is new. It reports whether the device existed.
func (m *model) editDev(id string, f func(*device)) bool {
	m.ownDevs()
	for i := range m.devs {
		if m.devs[i].id == id {
			f(&m.devs[i])
			return true
		}
	}
	d := device{id: id}
	f(&d)
	m.devs = append(m.devs, d)
	return false
}

// ownDevs gives the model its own copy of the device list before an edit. Within a batch
// of stream events the copy is made once.
func (m *model) ownDevs() {
	if !m.owned {
		m.devs = slices.Clone(m.devs)
		m.owned = m.batching
	}
}

// resort puts the devices back in display order after an edit, or, within a batch, once at
// its end.
func (m *model) resort() {
	if m.batching {
		m.dirty = true
		return
	}
	sortDevices(m.devs)
	m.fixSelection()
}

// hasDev reports whether a device with id is on the list.
func (m model) hasDev(id string) bool {
	return slices.ContainsFunc(m.devs, func(d device) bool { return d.id == id })
}

// isOnline is d's online flag, unless the hub's stream has gone silent: then nothing is
// known to be online.
func (m model) isOnline(d device) bool { return d.online && !m.conn.stale }

// shown reports whether d passes the dashboard's filter (part of its name, any case).
func (m model) shown(d device) bool {
	return m.filterValue == "" || strings.Contains(strings.ToLower(d.name), strings.ToLower(m.filterValue))
}

// online and offline split the devices for the dashboard grid and its footer, in order.
func (m model) online() []device {
	return slices.DeleteFunc(slices.Clone(m.devs), func(d device) bool { return !m.isOnline(d) })
}

func (m model) offline() []device {
	return slices.DeleteFunc(slices.Clone(m.devs), func(d device) bool { return m.isOnline(d) })
}

// cards and offCards are online and offline narrowed by the dashboard's filter; cardNav is
// the dashboard's selection order through them.
func (m model) cards() []device {
	return slices.DeleteFunc(m.online(), func(d device) bool { return !m.shown(d) })
}

func (m model) offCards() []device {
	return slices.DeleteFunc(m.offline(), func(d device) bool { return !m.shown(d) })
}

func (m model) cardNav() []device { return append(m.cards(), m.offCards()...) }

// selectShown moves the selection to the first device the filter shows, when it hides the
// selected one.
func (m *model) selectShown() {
	nav := m.cardNav()
	if len(nav) == 0 || slices.ContainsFunc(nav, func(d device) bool { return d.id == m.sel }) {
		return
	}
	m.sel = nav[0].id
}

// navOrder is the order selection moves through: the grid's cards, then the offline row.
func (m model) navOrder() []device { return append(m.online(), m.offline()...) }

// fixSelection keeps the selection on a device that exists.
func (m *model) fixSelection() {
	for _, d := range m.devs {
		if d.id == m.sel {
			return
		}
	}
	m.sel = ""
	if nav := m.navOrder(); len(nav) > 0 {
		m.sel = nav[0].id
	}
}
