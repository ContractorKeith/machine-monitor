package tui

import (
	"context"
	"fmt"

	"github.com/ContractorKeith/machine-monitor/internal/report"
)

// fakeCollector returns the same report on every call.
type fakeCollector struct{ r *report.Report }

func (f fakeCollector) Collect(ctx context.Context) (*report.Report, error) {
	c := *f.r
	return &c, nil
}

// fakeProcs returns a fixed process list.
type fakeProcs []report.Process

func (f fakeProcs) Processes(ctx context.Context) ([]report.Process, error) {
	return append([]report.Process(nil), f...), nil
}

const (
	kib = 1 << 10
	mib = 1 << 20
	gib = 1 << 30
)

var p = report.Ptr[float64]

// workstation is a fixed report with a temperature in each band and every section filled.
func workstation(cores int) *report.Report {
	cs := make([]*float64, cores)
	for i := range cs {
		cs[i] = p(float64((i*37 + 11) % 101))
	}
	return &report.Report{
		ID:         "4c4c4544-0042-3510-8051-b2c04f4e3732",
		Hostname:   report.Ptr("ws-01"),
		Platform:   report.PlatformLinux,
		OS:         report.Ptr("Ubuntu 24.04.1 LTS · Precision 7960"),
		Uptime:     p(3*86400 + 4*3600 + 12*60),
		FormFactor: report.Ptr(report.FormFactorDesktop),
		Power:      &report.Power{CPU: p(142.5)},
		CPU: report.CPU{
			Name:     report.Ptr("AMD Ryzen Threadripper PRO 7975WX"),
			Usage:    p(46.2),
			Cores:    cs,
			Threads:  p(float64(cores)),
			ClockMHz: p(4350),
			Temp:     p(72.4),
		},
		Memory: report.Memory{Total: p(128 * gib), Used: p(47.3 * gib)},
		GPUs: []report.GPU{{
			Name: report.Ptr("NVIDIA RTX 6000 Ada"), Vendor: report.Ptr("nvidia"),
			Usage: p(91), Temp: p(86), MemUsed: p(31.5 * gib), MemTotal: p(48 * gib),
			Power: p(268), PowerLimit: p(300), Fan: p(64), ClockMHz: p(2505),
		}},
		Disks: []report.Disk{
			{Name: report.Ptr("/"), Total: p(1863 * gib), Used: p(912 * gib)},
			{Name: report.Ptr("/data"), Total: p(7452 * gib), Used: p(6890 * gib)},
		},
		Net:    &report.Net{RX: p(12.4 * mib), TX: p(860 * kib)},
		DiskIO: &report.DiskIO{Read: p(54 * mib), Write: p(3.2 * mib)},
		Temps: []report.Temp{
			{Name: report.Ptr("Tctl"), Parent: report.Ptr("k10temp"), Value: p(72.4)},
			{Name: report.Ptr("Composite"), Parent: report.Ptr("nvme0"), Value: p(45)},
			{Name: report.Ptr("edge"), Parent: report.Ptr("amdgpu"), Value: p(90.5)},
		},
		Fans: []report.Fan{{Name: report.Ptr("cpu_fan"), RPM: p(1420)}},
		AI: []report.AIGroup{{
			App: report.Ptr("Ollama"), Mem: p(18.6 * gib),
			Models: []report.AIModel{{Name: report.Ptr("llama3.1:70b-instruct-q4_K_M"), Size: p(42.5 * gib)}},
		}},
		NetIfaces: []report.NetIface{
			{Name: report.Ptr("enp5s0"), RX: p(12.4 * mib), TX: p(860 * kib)},
			{Name: report.Ptr("tailscale0"), RX: p(2048), TX: p(1024)},
		},
	}
}

// procList is n processes with varied, deterministic values.
func procList(n int) fakeProcs {
	names := []string{"postgres", "ollama", "code", "firefox", "Xorg", "pipewire", "sshd", "go"}
	users := []string{"keith", "root", "postgres"}
	out := make(fakeProcs, n)
	for i := range out {
		name := names[i%len(names)]
		out[i] = report.Process{
			PID:  p(float64(1000 + i*7)),
			Name: report.Ptr(name),
			Cmd:  report.Ptr(fmt.Sprintf("/usr/bin/%s --worker %d --config /etc/%s.conf", name, i, name)),
			User: report.Ptr(users[i%len(users)]),
			CPU:  p(float64((i*53)%997) / 10),
			Mem:  p(float64((i*7919)%4096) * (1 << 20)),
		}
		if name == "ollama" {
			out[i].GPU, out[i].GPUMem = p(78), p(18.6*gib)
		}
	}
	return out
}
