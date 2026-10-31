package tui

import (
	"context"
	"os/user"
	"strconv"
	"sync"

	"github.com/shirou/gopsutil/v4/process"

	"github.com/ContractorKeith/machine-monitor/internal/report"
)

// SystemProcesses returns the ProcessLister for this machine, backed by gopsutil. CPU is
// percent of one core since the previous call (as top), so a process seen for the first
// time has nil CPU. GPU columns stay nil: no OS reports them per process without vendor
// tools.
func SystemProcesses() ProcessLister {
	return &sysProcs{seen: map[int32]*process.Process{}, users: map[uint32]*string{}}
}

type sysProcs struct {
	mu    sync.Mutex
	seen  map[int32]*process.Process // kept between calls for CPU deltas
	users map[uint32]*string         // uid → name; os/user lookups are slow without cgo
}

// Processes lists every process this user can see. Per-process read errors (a process
// that exited, or one owned by another user) leave that field nil rather than failing.
func (s *sysProcs) Processes(ctx context.Context) ([]report.Process, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list, err := process.ProcessesWithContext(ctx)
	if err != nil {
		return nil, err
	}
	next := make(map[int32]*process.Process, len(list))
	out := make([]report.Process, 0, len(list))
	for _, p := range list {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		prev, ok := s.seen[p.Pid]
		if ok {
			p = prev
		}
		next[p.Pid] = p
		row := report.Process{PID: report.Ptr(float64(p.Pid))}
		if c, err := p.PercentWithContext(ctx, 0); err == nil && ok {
			row.CPU = report.Ptr(c)
		}
		if n, err := p.NameWithContext(ctx); err == nil && n != "" {
			row.Name = report.Ptr(n)
		}
		if c, err := p.CmdlineWithContext(ctx); err == nil && c != "" {
			row.Cmd = report.Ptr(c)
		}
		if mi, err := p.MemoryInfoWithContext(ctx); err == nil && mi != nil {
			row.Mem = report.Ptr(float64(mi.RSS))
		}
		row.User = s.username(ctx, p)
		out = append(out, row)
	}
	s.seen = next
	return out, nil
}

// username resolves a process owner, caching by uid where the OS has uids.
func (s *sysProcs) username(ctx context.Context, p *process.Process) *string {
	uids, err := p.UidsWithContext(ctx)
	if err != nil || len(uids) == 0 {
		if n, err := p.UsernameWithContext(ctx); err == nil && n != "" {
			return report.Ptr(n)
		}
		return nil
	}
	uid := uids[0]
	if name, ok := s.users[uid]; ok {
		return name
	}
	id := strconv.FormatUint(uint64(uid), 10)
	name := report.Ptr(id)
	if u, err := user.LookupId(id); err == nil {
		name = report.Ptr(u.Username)
	}
	s.users[uid] = name
	return name
}
