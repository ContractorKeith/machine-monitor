package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/ContractorKeith/machine-monitor/internal/power"
)

// maxCardWidth caps the card so it stays readable on a wide terminal.
const maxCardWidth = 100

// dashboard is view 1: a summary line and this machine's card.
func (m model) dashboard() string {
	d := m.cur()
	if d == nil || d.last == nil {
		return m.theme.dim.Render("Collecting the first sample…")
	}
	r := d.last
	watts := none
	if b := power.Estimate(r); b != nil {
		watts = "~" + fmtWatts(&b.Watts)
	}
	summary := fmt.Sprintf("1 device · 1 online · %s at the wall", watts)
	return m.theme.dim.Render(summary) + "\n" + m.card(*d, min(m.width, maxCardWidth), false)
}

// card draws one device: identity, gauges, temperatures, sparklines and I/O rates. A
// selected card (hub mode) gets a heavier border and a marker before its name.
func (m model) card(d device, w int, selected bool) string {
	t := m.theme
	iw := max(10, w-4) // inside the border and one space of padding each side
	var lines []string
	box := lipgloss.NewStyle().Border(t.border).BorderForeground(t.borderStyle.GetForeground()).Padding(0, 1)
	name := t.title.Render(d.name)
	if selected {
		marker := "▶ "
		if t.ascii {
			marker = "> "
		} else {
			box = box.Border(lipgloss.ThickBorder()).BorderForeground(t.title.GetForeground())
		}
		name = t.title.Render(marker + d.name)
	}
	r := d.last
	if r == nil { // on the hub's list, but no report since the hub started
		lines = append(lines, pad(name, iw), pad(t.dim.Render("no report yet"), iw))
		return box.Render(strings.Join(lines, "\n"))
	}
	lines = append(lines, spread(name, t.dim.Render(orNone(r.Platform)), iw))
	lines = append(lines, spread(t.dim.Render(str(r.OS)), t.dim.Render("up "+fmtUptime(r.Uptime)), iw))

	cpuRight := cols(fmtPct(r.CPU.Usage), 5, fmtClock(r.CPU.ClockMHz), 9, t.fmtTemp(r.CPU.Temp), 5)
	lines = append(lines, m.gauge("CPU", r.CPU.Usage, cpuRight, iw))
	for i, g := range r.GPUs {
		label := "GPU"
		if len(r.GPUs) > 1 {
			label = fmt.Sprintf("GPU%d", i)
		}
		right := cols(fmtPct(g.Usage), 5, shortBytes(g.MemUsed, g.MemTotal), 9, t.fmtTemp(g.Temp), 5)
		lines = append(lines, m.gauge(label, g.Usage, right, iw))
	}
	memPct := pctOf(r.Memory.Used, r.Memory.Total)
	memRight := cols(fmtPct(memPct), 5, fmtUsedTotal(r.Memory.Used, r.Memory.Total), 21)
	lines = append(lines, m.gauge("MEM", memPct, memRight, iw))

	lines = append(lines, m.sparkRow("cpu", d.cpu, iw)...)
	if len(r.GPUs) > 0 {
		lines = append(lines, m.sparkRow("gpu", d.gpu, iw)...)
	} else {
		lines = append(lines, m.sparkRow("mem", d.mem, iw)...)
	}

	var rx, tx, rd, wr *float64
	if r.Net != nil {
		rx, tx = r.Net.RX, r.Net.TX
	}
	if r.DiskIO != nil {
		rd, wr = r.DiskIO.Read, r.DiskIO.Write
	}
	lines = append(lines, pad(fmt.Sprintf("%s ↓ %-11s ↑ %-11s  %s r %-11s w %s",
		t.label.Render("NET"), fmtRate(rx), fmtRate(tx), t.label.Render("DISK"), fmtRate(rd), fmtRate(wr)), iw))

	return box.Render(strings.Join(lines, "\n"))
}

// gauge is one card row: a label, a bar filling the free width, and right-hand figures.
func (m model) gauge(label string, p *float64, right string, w int) string {
	const labelW = 5
	bw := max(4, w-labelW-1-lipgloss.Width(right))
	return pad(m.theme.label.Render(pad(label, labelW))+m.theme.bar(p, bw)+" "+right, w)
}

// sparkRow is a labelled sparkline sparkHeight rows tall, or nothing when no sample is known.
func (m model) sparkRow(label string, s series, w int) []string {
	if !s.known() {
		return nil
	}
	const labelW = 5
	chart := strings.Split(m.theme.spark(s, w-labelW), "\n")
	out := make([]string, len(chart))
	for i, c := range chart {
		l := ""
		if i == len(chart)-1 {
			l = label
		}
		out[i] = pad(m.theme.dim.Render(pad(l, labelW))+c, w)
	}
	return out
}

// shortBytes is "8.0/24 GiB" for GPU memory, which has little room on the card.
func shortBytes(used, total *float64) string {
	if used == nil || total == nil {
		return fmtBytesP(used)
	}
	const gib = 1 << 30
	return fmt.Sprintf("%.1f/%.0fG", *used/gib, *total/gib)
}
