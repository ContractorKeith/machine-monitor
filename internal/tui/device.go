package tui

import (
	"fmt"
	"strings"

	"github.com/ContractorKeith/machine-monitor/internal/power"
	"github.com/ContractorKeith/machine-monitor/internal/report"
)

// deviceLines is view 2, one string per line; the view scrolls through them.
func (m model) deviceLines() []string {
	r := m.report()
	switch {
	case r == nil && !m.multi():
		return []string{m.theme.dim.Render("Collecting the first sample…")}
	case r == nil && m.cur() == nil:
		return []string{m.theme.dim.Render("No device to show yet.")}
	case r == nil:
		return []string{m.theme.dim.Render(m.cur().name + " has not reported since the hub started.")}
	}
	t := m.theme
	w := m.width
	var out []string
	section := func(title string) {
		if len(out) > 0 {
			out = append(out, "")
		}
		out = append(out, t.title.Render(title))
	}
	kv := func(k, v string) { out = append(out, "  "+t.label.Render(pad(k, labelWidth))+v) }

	section("System")
	kv("Host", str(r.Hostname))
	kv("OS", str(r.OS))
	kv("Platform", orNone(r.Platform))
	kv("Form factor", str(r.FormFactor))
	kv("Uptime", fmtUptime(r.Uptime))
	kv("ID", orNone(r.ID))

	section("CPU")
	kv("Model", str(r.CPU.Name))
	threads := none
	if r.CPU.Threads != nil {
		threads = fmt.Sprintf("%.0f", *r.CPU.Threads)
	}
	kv("Usage", fmtPct(r.CPU.Usage)+"   threads "+threads+"   clock "+fmtClock(r.CPU.ClockMHz))
	temp := t.fmtTemp(r.CPU.Temp)
	if r.CPU.TempSource != nil && *r.CPU.TempSource == "acpi" {
		temp += t.dim.Render(" (ACPI thermal zone)")
	}
	kv("Temp", temp)
	out = append(out, m.coreGrid(r.CPU.Cores, w)...)

	section("Memory")
	memPct := pctOf(r.Memory.Used, r.Memory.Total)
	out = append(out, m.meter("RAM", memPct, fmtUsedTotal(r.Memory.Used, r.Memory.Total), w))

	section("GPUs")
	if len(r.GPUs) == 0 {
		out = append(out, t.dim.Render("  none detected"))
	}
	for _, g := range r.GPUs {
		name := str(g.Name)
		if g.Vendor != nil {
			name += t.dim.Render(" (" + *g.Vendor + ")")
		}
		out = append(out, "  "+name)
		out = append(out, m.meter("load", g.Usage, "", w))
		out = append(out, m.meter("vram", pctOf(g.MemUsed, g.MemTotal), fmtUsedTotal(g.MemUsed, g.MemTotal), w))
		out = append(out, fmt.Sprintf("  %s %s   %s %s / %s   %s %s   %s %s",
			t.label.Render("temp"), t.fmtTemp(g.Temp),
			t.label.Render("power"), fmtWatts(g.Power), fmtWatts(g.PowerLimit),
			t.label.Render("fan"), fmtPct(g.Fan),
			t.label.Render("clock"), fmtClock(g.ClockMHz)))
	}

	section("Drives")
	if len(r.Disks) == 0 {
		out = append(out, t.dim.Render("  none reported"))
	}
	for _, d := range r.Disks {
		name := str(d.Name)
		if d.Label != nil && *d.Label != "" {
			name += " " + *d.Label
		}
		out = append(out, m.meter(name, pctOf(d.Used, d.Total), fmtUsedTotal(d.Used, d.Total), w))
	}
	if r.DiskIO != nil {
		kv("I/O", "read "+fmtRate(r.DiskIO.Read)+"   write "+fmtRate(r.DiskIO.Write))
	}

	section("Network")
	if r.Net != nil {
		kv("Total", "↓ "+fmtRate(r.Net.RX)+"   ↑ "+fmtRate(r.Net.TX))
	}
	for _, n := range r.NetIfaces {
		kv(str(n.Name), "↓ "+fmtRate(n.RX)+"   ↑ "+fmtRate(n.TX))
	}
	if r.Net == nil && len(r.NetIfaces) == 0 {
		out = append(out, t.dim.Render("  "+none))
	}

	section("Sensors")
	if len(r.Temps) == 0 && len(r.Fans) == 0 {
		out = append(out, t.dim.Render("  none reported"))
	}
	for _, s := range r.Temps {
		name := str(s.Name)
		if s.Parent != nil {
			name = *s.Parent + " " + name
		}
		out = append(out, "  "+pad(name, 32)+" "+t.fmtTemp(s.Value))
	}
	for _, f := range r.Fans {
		rpm := none
		if f.RPM != nil {
			rpm = fmt.Sprintf("%.0f rpm", *f.RPM)
		}
		out = append(out, "  "+pad(str(f.Name), 32)+" "+rpm)
	}

	section("AI models")
	switch {
	case r.AI == nil:
		out = append(out, t.dim.Render("  not checked"))
	case len(r.AI) == 0:
		out = append(out, t.dim.Render("  none loaded"))
	}
	for _, g := range r.AI {
		head := "  " + t.label.Render(str(g.App))
		if g.Mem != nil {
			head += t.dim.Render("  " + fmtBytes(*g.Mem) + " GPU memory")
		}
		out = append(out, head)
		for _, mod := range g.Models {
			line := "    " + str(mod.Name)
			if mod.Kind != nil {
				line += t.dim.Render(" [" + *mod.Kind + "]")
			}
			if mod.Size != nil {
				line += t.dim.Render("  " + fmtBytes(*mod.Size))
			}
			out = append(out, line)
		}
	}

	section("Power")
	out = append(out, m.powerLines(r)...)
	return out
}

// powerLines is the estimate breakdown, saying which parts are measured.
func (m model) powerLines(r *report.Report) []string {
	t := m.theme
	b := power.Estimate(r)
	if b == nil {
		return []string{t.dim.Render("  no CPU power or load to estimate from")}
	}
	how := func(measured bool) string {
		if measured {
			return "measured"
		}
		return "estimated"
	}
	gpuHow := "none"
	switch {
	case b.GPUMeasured && b.GPUEstimated:
		gpuHow = "measured + estimated"
	case b.GPUMeasured:
		gpuHow = "measured"
	case b.GPUEstimated:
		gpuHow = "estimated"
	}
	row := func(k string, w float64, note string) string {
		return "  " + t.label.Render(pad(k, labelWidth)) + padLeft(fmt.Sprintf("%.0f W", w), 7) + "  " + t.dim.Render(note)
	}
	return []string{
		row("At the wall", b.Watts, "estimate"),
		row("CPU", b.CPU, how(b.CPUMeasured)),
		row("GPU", b.GPU, gpuHow),
		row("Rest", b.Rest, "board, drives, fans"),
		row("PSU losses", b.Losses, "power supply efficiency"),
	}
}

// labelWidth is the Device view's label column.
const labelWidth = 14

// meter is a labelled bar with a percentage and a note. Every meter in a frame has the same
// bar width so they line up; the note may be clipped on a narrow terminal.
func (m model) meter(label string, p *float64, note string, w int) string {
	const noteW = 24
	bw := max(10, min(50, w-2-labelWidth-1-5-2-noteW))
	return "  " + m.theme.label.Render(pad(label, labelWidth)) + m.theme.bar(p, bw) + " " + padLeft(fmtPct(p), 5) + "  " + note
}

// coreGrid lays out one small bar per logical core in as many columns as fit.
func (m model) coreGrid(cores []*float64, w int) []string {
	if len(cores) == 0 {
		return []string{m.theme.dim.Render("  per-core load not reported")}
	}
	const cellW = 20 // "  12 ██████████  45%"
	ncol := max(1, (w-2)/cellW)
	nrow := (len(cores) + ncol - 1) / ncol
	idxW := len(fmt.Sprint(len(cores) - 1))
	lines := make([]string, nrow)
	for i, c := range cores {
		row := i % nrow // fill down the columns, like htop
		cell := " " + padLeft(fmt.Sprint(i), idxW) + " " + m.theme.bar(c, cellW-idxW-8) + padLeft(fmtPct(c), 5)
		lines[row] += pad(cell, cellW)
	}
	for i := range lines {
		lines[i] = " " + strings.TrimRight(lines[i], " ")
	}
	return lines
}
