package alerts

// Ported from Forward-Future/device-monitor alerts.js (MIT, © 2026 Matt):
// https://github.com/Forward-Future/device-monitor

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/report"
)

// metric is a report's current value for a rule type, nil when unknown, plus detail for the
// message. watts is the hub's power estimate for the report.
func metric(typ string, r *report.Report, watts *float64) (*float64, Detail) {
	if r == nil {
		return nil, Detail{}
	}
	maxGPU := func(get func(g report.GPU) *float64) *float64 {
		var m *float64
		for _, g := range r.GPUs {
			if v := get(g); v != nil && (m == nil || *v > *m) {
				m = v
			}
		}
		return m
	}
	switch typ {
	case TypeCPUTemp:
		return r.CPU.Temp, Detail{}
	case TypeGPUTemp:
		return maxGPU(func(g report.GPU) *float64 { return g.Temp }), Detail{}
	case TypeCPU:
		return r.CPU.Usage, Detail{}
	case TypeGPU:
		return maxGPU(func(g report.GPU) *float64 { return g.Usage }), Detail{}
	case TypeMem:
		if m := r.Memory; m.Total != nil && *m.Total > 0 && m.Used != nil {
			return report.Ptr(*m.Used / *m.Total * 100), Detail{}
		}
	case TypePower:
		return watts, Detail{}
	case TypeDisk:
		// The fullest disk over 1 GB; smaller ones are boot and recovery partitions.
		var worst *float64
		var d Detail
		for _, disk := range r.Disks {
			if disk.Total == nil || *disk.Total <= 1e9 || disk.Used == nil {
				continue
			}
			if p := *disk.Used / *disk.Total * 100; worst == nil || p > *worst {
				worst = &p
				d = Detail{Free: report.Ptr(*disk.Total - *disk.Used)}
				if disk.Name != nil {
					d.Disk = *disk.Name
				}
			}
		}
		return worst, d
	}
	return nil, Detail{}
}

// text is an alert's title and message, worded as upstream.
func (e *Engine) text(r Rule, id, st string, value *float64, d Detail) (title, message string) {
	name, label := e.nameOf(id), Types[r.Type].Label
	limit := e.format(r.Type, r.Threshold, d)
	dur := ""
	if r.Minutes > 0 {
		dur = fmt.Sprintf(" for %d min", r.Minutes)
	}
	clock := func(ms int64) string {
		t := e.opt.Now()
		if ms != 0 {
			t = time.UnixMilli(ms)
		}
		return t.Local().Format("3:04 PM")
	}
	switch r.Type {
	case TypeOffline:
		if st == Firing {
			return name + " is offline", "No reports since " + clock(d.Since) + "."
		}
		if d.Since != 0 {
			return name + " is back online", "It was offline since " + clock(d.Since) + "."
		}
		return name + " is back online", "Reporting again."
	case TypeDisk:
		if st == Firing {
			free := ""
			if d.Free != nil {
				free = " · " + strconv.FormatFloat(*d.Free/1e9, 'f', 1, 64) + " GB free"
			}
			title = strings.Replace(fmt.Sprintf("%s: disk %s is %s full", name, d.Disk, e.format(TypeDisk, value, d)), "  ", " ", 1)
			return title, "Over your " + limit + " limit" + free + "."
		}
		return name + ": disk space back under " + limit, "Now " + e.format(TypeDisk, value, d) + " used."
	case TypeDailyCost:
		return name + ": electricity today is " + e.format(r.Type, value, d), "Over your " + limit + " daily limit."
	}
	if st == Firing {
		return name + ": " + label + " " + e.format(r.Type, value, d), "Above " + limit + dur + "."
	}
	return name + ": " + label + " back to normal", "Now " + e.format(r.Type, value, d) + " (limit " + limit + ")."
}

// format renders a value in its type's unit: °C/°F, W/kW, currency or percent.
func (e *Engine) format(typ string, v *float64, d Detail) string {
	if v == nil {
		return ""
	}
	x := *v
	switch typ {
	case TypeCPUTemp, TypeGPUTemp:
		if e.cfg.TempUnit == "F" {
			return fmt.Sprintf("%.0f°F", jsRound(x*9/5+32))
		}
		return fmt.Sprintf("%.0f°C", jsRound(x))
	case TypePower:
		if x >= 1000 {
			return strconv.FormatFloat(x/1000, 'f', 2, 64) + " kW"
		}
		return fmt.Sprintf("%.0f W", jsRound(x))
	case TypeDailyCost:
		return money(x, d.Currency)
	}
	return fmt.Sprintf("%.0f%%", jsRound(x))
}

// currencySymbols follows Intl.NumberFormat('en-US') for the currencies the power price
// table uses; others print as "CODE 1.23" as Intl does.
var currencySymbols = map[string]string{
	"USD": "$", "EUR": "€", "GBP": "£", "JPY": "¥", "INR": "₹", "KRW": "₩", "CNY": "CN¥",
	"CAD": "CA$", "AUD": "A$", "NZD": "NZ$", "MXN": "MX$", "BRL": "R$", "ILS": "₪", "VND": "₫",
}

// money formats an amount like Intl.NumberFormat('en-US', {style: 'currency'}): grouped
// thousands, two decimals (none for JPY and KRW).
func money(v float64, currency string) string {
	if currency == "" {
		currency = "USD"
	}
	currency = strings.ToUpper(currency)
	decimals := 2
	if currency == "JPY" || currency == "KRW" || currency == "VND" {
		decimals = 0
	}
	neg := v < 0
	if neg {
		v = -v
	}
	s := strconv.FormatFloat(v, 'f', decimals, 64)
	whole, frac, _ := strings.Cut(s, ".")
	for i := len(whole) - 3; i > 0; i -= 3 {
		whole = whole[:i] + "," + whole[i:]
	}
	if frac != "" {
		whole += "." + frac
	}
	sym, ok := currencySymbols[currency]
	if !ok {
		sym = currency + " "
	}
	if neg {
		return "-" + sym + whole
	}
	return sym + whole
}
