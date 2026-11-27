package tui

import (
	"slices"
	"strings"

	"github.com/ContractorKeith/machine-monitor/internal/client"
	"github.com/ContractorKeith/machine-monitor/internal/history"
	"github.com/ContractorKeith/machine-monitor/internal/report"
)

type device struct {
	id, name  string
	online    bool
	order     int
	firstSeen int64
	lastSeen  int64
	last      *report.Report
	watts     *float64
	cpu, mem  series
	gpu       series
	liveFrom  float64
	seeded    bool
}

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

func (d *device) pushPoint(p history.Point) {
	if d.liveFrom == 0 {
		d.liveFrom = p.T
	}
	d.cpu.push(p.CPU)
	d.mem.push(p.Mem)
	d.gpu.push(p.GPU)
}

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

func fromHub(v client.Device) device {
	var d device
	d.setRegistry(&v)
	for _, p := range v.History {
		d.pushPoint(p)
	}
	d.seeded = len(v.History) > 0
	return d
}

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

func (m model) report() *report.Report {
	if d := m.cur(); d != nil {
		return d.last
	}
	return nil
}

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

func (m *model) ownDevs() {
	if !m.owned {
		m.devs = slices.Clone(m.devs)
		m.owned = m.batching
	}
}

func (m *model) resort() {
	if m.batching {
		m.dirty = true
		return
	}
	sortDevices(m.devs)
	m.fixSelection()
}

func (m model) hasDev(id string) bool {
	return slices.ContainsFunc(m.devs, func(d device) bool { return d.id == id })
}

func (m model) isOnline(d device) bool { return d.online && !m.conn.stale }

func (m model) shown(d device) bool {
	return m.filterValue == "" || strings.Contains(strings.ToLower(d.name), strings.ToLower(m.filterValue))
}

func (m model) online() []device {
	return slices.DeleteFunc(slices.Clone(m.devs), func(d device) bool { return !m.isOnline(d) })
}

func (m model) offline() []device {
	return slices.DeleteFunc(slices.Clone(m.devs), func(d device) bool { return m.isOnline(d) })
}

func (m model) cards() []device {
	return slices.DeleteFunc(m.online(), func(d device) bool { return !m.shown(d) })
}

func (m model) offCards() []device {
	return slices.DeleteFunc(m.offline(), func(d device) bool { return !m.shown(d) })
}

func (m model) cardNav() []device { return append(m.cards(), m.offCards()...) }

func (m *model) selectShown() {
	nav := m.cardNav()
	if len(nav) == 0 || slices.ContainsFunc(nav, func(d device) bool { return d.id == m.sel }) {
		return
	}
	m.sel = nav[0].id
}

func (m model) navOrder() []device { return append(m.online(), m.offline()...) }

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
