package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/ContractorKeith/machine-monitor/internal/power"
	"github.com/ContractorKeith/machine-monitor/internal/report"
)

// maxCardWidth caps the card so it stays readable on a wide terminal.
const maxCardWidth = 100

// dashboard is view 1: a summary line and this machine's card.
func (m model) dashboard() string {
	r := m.last
	if r == nil {
		return m.theme.dim.Render("Collecting the first sample…")
	}
	watts := none
	if b := power.Estimate(r); b != nil {
		watts = "~" + fmtWatts(&b.Watts)
	}
	summary := fmt.Sprintf("1 device · 1 online · %s at the wall", watts)
	return m.theme.dim.Render(summary) + "\n" + m.card(r, min(m.width, maxCardWidth))
}

// card draws one device: identity, gauges, temperatures, sparklines and I/O rates.
func (m model) card(r *report.Report, w int) string {
	t := m.theme
	iw := max(10, w-4) // inside the border and one space of padding each side
	var lines []string

	name := t.title.Render(str(r.Hostname))
	lines = append(lines, spread(name, t.dim.Render(r.Platform), iw))
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

	lines = append(lines, m.sparkRow("cpu", m.cpu, iw)...)
	if len(r.GPUs) > 0 {
		lines = append(lines, m.sparkRow("gpu", m.gpu, iw)...)
	} else {
		lines = append(lines, m.sparkRow("mem", m.mem, iw)...)
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

	box := lipgloss.NewStyle().Border(t.border).BorderForeground(t.borderStyle.GetForeground()).Padding(0, 1)
	return box.Render(strings.Join(lines, "\n"))
}

// gauge is one card row: a label, a bar filling the free width, and right-hand figures.
func (m model) gauge(label string, p *float64, right string, w int) string {
	const labelW = 5
	bw := max(4, w-labelW-1-lipgloss.Width(right))
	return pad(m.theme.label.Render(pad(label, labelW))+m.theme.bar(p, bw)+" "+right, w)
}

// sparkRow is a labelled sparkline sparkHeight rows tall.
func (m model) sparkRow(label string, s series, w int) []string {
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
