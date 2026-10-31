package tui

import (
	"slices"

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
}

// push records one report and its samples.
func (d *device) push(r *report.Report) {
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
	d.cpu.push(p.CPU)
	d.mem.push(p.Mem)
	d.gpu.push(p.GPU)
}

// setRegistry copies a hub device's registry fields and latest report (when it has one).
func (d *device) setRegistry(v *client.Device) {
	d.id, d.name, d.online = v.ID, v.Name, v.Online
	d.order, d.firstSeen, d.lastSeen = v.Order, v.FirstSeen, v.LastSeen
	if v.Latest != nil {
		r := v.Latest.Report
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
	return d
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
	m.devs = slices.Clone(m.devs)
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

// online and offline split the devices for the dashboard grid and its footer, in order.
func (m model) online() []device {
	return slices.DeleteFunc(slices.Clone(m.devs), func(d device) bool { return !d.online })
}

func (m model) offline() []device {
	return slices.DeleteFunc(slices.Clone(m.devs), func(d device) bool { return d.online })
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
