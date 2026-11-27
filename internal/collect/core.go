package collect

import (
	"cmp"
	"context"
	"log/slog"
	"math"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"

	"github.com/ContractorKeith/machine-monitor/internal/report"
)

type source interface {
	platform() string
	deviceID(ctx context.Context) string

	static(ctx context.Context) staticInfo
	clockMHz(ctx context.Context) *float64
	memory(ctx context.Context) report.Memory
	disks(ctx context.Context) []report.Disk

	diskIO(ctx context.Context) (read, write uint64, ok bool)

	netIO(ctx context.Context) []ifaceBytes

	inNetTotal(name string) bool
}

type staticInfo struct {
	id         string
	hostname   *string
	os         *string
	formFactor *string
	cpuName    *string
}

type ifaceBytes struct {
	name   string
	rx, tx uint64
}

type counters struct {
	at          time.Time
	cpu         *cpu.TimesStat
	cores       []cpu.TimesStat
	read, write uint64
	diskOK      bool
	ifaces      []ifaceBytes
}

const (
	sampleTimeout = 1500 * time.Millisecond
	staticTimeout = 10 * time.Second
)

type core struct {
	src     source
	ps      ps
	probes  *probeRunner
	now     func() time.Time
	timeout time.Duration

	info    *staticInfo
	threads *float64
	macs    []string
	prev    *counters
}

func newCore(src source, p ps) *core {
	return &core{src: src, ps: p, probes: newProbeRunner(Probes()), now: time.Now, timeout: sampleTimeout}
}

type cpuSample struct {
	total *cpu.TimesStat
	cores []cpu.TimesStat
}

type diskSample struct {
	read, write uint64
	ok          bool
}

func (c *core) Collect(ctx context.Context) (*report.Report, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	finish := c.probes.start(ctx)

	if c.info == nil {
		sctx, cancel := context.WithTimeout(ctx, staticTimeout)
		c.loadStatic(sctx)
		cancel()
	}
	r := &report.Report{
		ID:         c.info.id,
		Hostname:   c.info.hostname,
		Platform:   c.src.platform(),
		OS:         c.info.os,
		FormFactor: c.info.formFactor,
		MACs:       c.macs,
		CPU:        report.CPU{Name: c.info.cpuName, Threads: c.threads},
	}

	sctx, cancel := context.WithTimeout(ctx, cmp.Or(c.timeout, sampleTimeout))
	defer cancel()
	at := c.now()
	uptime := async(sctx, c.uptime)
	clock := async(sctx, c.src.clockMHz)
	memory := async(sctx, c.src.memory)
	disks := async(sctx, c.src.disks)
	times := async(sctx, c.cpuTimes)
	dio := async(sctx, func(ctx context.Context) diskSample {
		rd, wr, ok := c.src.diskIO(ctx)
		return diskSample{rd, wr, ok}
	})
	nets := async(sctx, c.src.netIO)

	r.Uptime = await(sctx, uptime)
	r.CPU.ClockMHz = await(sctx, clock)
	r.Memory = await(sctx, memory)
	r.Disks = await(sctx, disks)
	t, d := await(sctx, times), await(sctx, dio)
	cur := &counters{at: at, cpu: t.total, cores: t.cores, read: d.read, write: d.write, diskOK: d.ok, ifaces: await(sctx, nets)}
	if c.prev != nil {
		c.rates(r, c.prev, cur)
	}
	c.prev = cur

	finish(r)
	return r, nil
}

func (c *core) loadStatic(ctx context.Context) {
	info := c.src.static(ctx)
	c.info = &info
	if n, err := c.ps.CPUCount(ctx); err == nil && n > 0 {
		c.threads = report.Ptr(float64(n))
	}
	c.macs = c.readMACs(ctx)
}

func (c *core) uptime(ctx context.Context) *float64 {
	if up, err := c.ps.Uptime(ctx); err == nil && up > 0 {
		return report.Ptr(float64(up))
	}
	return nil
}

func (c *core) cpuTimes(ctx context.Context) cpuSample {
	var s cpuSample
	if t, err := c.ps.CPUTimes(ctx, false); err == nil && len(t) > 0 {
		s.total = &t[0]
	} else if err != nil {
		slog.Debug("collect: cpu times", "err", err)
	}
	if t, err := c.ps.CPUTimes(ctx, true); err == nil {
		s.cores = t
	}
	return s
}

func async[T any](ctx context.Context, f func(context.Context) T) <-chan T {
	ch := make(chan T, 1)
	go func() {
		var v T
		defer func() {
			if p := recover(); p != nil {
				slog.Debug("collect: core read panicked", "panic", p)
			}
			ch <- v
		}()
		v = f(ctx)
	}()
	return ch
}

func await[T any](ctx context.Context, ch <-chan T) T {
	select {
	case v := <-ch:
		return v
	case <-ctx.Done():
		select {
		case v := <-ch:
			return v
		default:
			var zero T
			return zero
		}
	}
}

func (c *core) rates(r *report.Report, prev, cur *counters) {
	if prev.cpu != nil && cur.cpu != nil {
		r.CPU.Usage = cpuUsage(*prev.cpu, *cur.cpu)
	}
	if len(cur.cores) > 0 && len(cur.cores) == len(prev.cores) {
		r.CPU.Cores = make([]*float64, len(cur.cores))
		for i := range cur.cores {
			r.CPU.Cores[i] = cpuUsage(prev.cores[i], cur.cores[i])
		}
	}

	sec := cur.at.Sub(prev.at).Seconds()
	if sec <= 0 {
		return
	}
	if prev.diskOK && cur.diskOK {
		r.DiskIO = &report.DiskIO{Read: rate(prev.read, cur.read, sec), Write: rate(prev.write, cur.write, sec)}
	}

	before := map[string]ifaceBytes{}
	for _, i := range prev.ifaces {
		before[i.name] = i
	}
	var rx, tx float64
	seen := false
	for _, i := range cur.ifaces {
		b, ok := before[i.name]
		if !ok {
			continue
		}
		ir, it := rate(b.rx, i.rx, sec), rate(b.tx, i.tx, sec)
		r.NetIfaces = append(r.NetIfaces, report.NetIface{Name: report.Ptr(i.name), RX: ir, TX: it})
		if c.src.inNetTotal(i.name) {
			rx, tx, seen = rx+*ir, tx+*it, true
		}
	}
	if seen {
		r.Net = &report.Net{RX: report.Ptr(rx), TX: report.Ptr(tx)}
	}
}

func cpuUsage(a, b cpu.TimesStat) *float64 {
	total := cpuTotal(b) - cpuTotal(a)
	if total <= 0 {
		return nil
	}
	idle := (b.Idle + b.Iowait) - (a.Idle + a.Iowait)
	u := 100 * (1 - idle/total)
	return report.Ptr(math.Round(math.Max(0, math.Min(100, u))*10) / 10)
}

func cpuTotal(t cpu.TimesStat) float64 {
	return t.User + t.Nice + t.System + t.Idle + t.Iowait + t.Irq + t.Softirq + t.Steal
}

func rate(prev, cur uint64, sec float64) *float64 {
	if cur < prev {
		return report.Ptr(0.0)
	}
	return report.Ptr(math.Round(float64(cur-prev) / sec))
}

func (c *core) readMACs(ctx context.Context) []string {
	ifs, err := c.ps.Interfaces(ctx)
	if err != nil {
		return nil
	}
	var out []string
	for _, i := range ifs {
		if slices.Contains(i.Flags, "loopback") {
			continue
		}
		hw, err := net.ParseMAC(i.HardwareAddr)
		if err != nil || len(hw) != 6 || hw[0]&0x03 != 0 || isZero(hw) {
			continue
		}
		m := strings.ToLower(hw.String())
		if !slices.Contains(out, m) {
			out = append(out, m)
		}
	}
	slices.Sort(out)
	if len(out) > 8 {
		out = out[:8]
	}
	return out
}

func psNetIO(ctx context.Context, p ps) []ifaceBytes {
	stats, err := p.NetIO(ctx)
	if err != nil {
		return nil
	}
	loop := map[string]bool{}
	if ifs, err := p.Interfaces(ctx); err == nil {
		for _, i := range ifs {
			if slices.Contains(i.Flags, "loopback") {
				loop[i.Name] = true
			}
		}
	}
	var out []ifaceBytes
	for _, s := range stats {
		if loop[s.Name] || s.Name == "lo" || (s.BytesRecv == 0 && s.BytesSent == 0) {
			continue
		}
		out = append(out, ifaceBytes{name: s.Name, rx: s.BytesRecv, tx: s.BytesSent})
	}
	return out
}

func isZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

func strPtr(s string) *string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return &s
}
