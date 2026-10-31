package tui

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/ContractorKeith/machine-monitor/internal/report"
)

// sortCol is a Processes table column the list can be sorted by.
type sortCol int

const (
	sortCPU sortCol = iota
	sortMem
	sortGPU
	sortGPUMem
	sortPID
	sortUser
	sortName
	sortCmd
	sortCount
)

var sortNames = [sortCount]string{"CPU", "MEM", "GPU", "GPU MEM", "PID", "USER", "NAME", "CMD"}

// numeric columns sort largest first by default; text columns A–Z.
func (c sortCol) numeric() bool { return c <= sortPID }

// procKey handles keys in the Processes view.
func (m model) procKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	k := m.keys
	rows := m.procRows()
	switch {
	case key.Matches(msg, k.Filter):
		m.filter.SetValue(m.filterValue)
		m.filter.CursorEnd()
		return m, m.filter.Focus()
	case key.Matches(msg, k.Clear):
		m.filter.SetValue("")
		m.filterValue = ""
		m.refreshProcs()
	case key.Matches(msg, k.Sort):
		m.procSort = (m.procSort + 1) % sortCount
		m.procRev = false
		m.refreshProcs()
	case key.Matches(msg, k.Reverse):
		m.procRev = !m.procRev
		m.refreshProcs()
	case key.Matches(msg, k.Up):
		m.procCursor--
	case key.Matches(msg, k.Down):
		m.procCursor++
	case key.Matches(msg, k.PageUp):
		m.procCursor -= rows
	case key.Matches(msg, k.PageDown):
		m.procCursor += rows
	case key.Matches(msg, k.Top):
		m.procCursor = 0
	case key.Matches(msg, k.Bottom):
		m.procCursor = 1 << 30
	}
	m.clampScroll()
	return m, nil
}

// procRows is how many table rows fit under the column headings.
func (m model) procRows() int { return max(1, m.bodyHeight()-1) }

// visibleProcs is the process list filtered and sorted for display.
func (m model) visibleProcs() []report.Process { return m.visible }

// refreshProcs recomputes the visible list; call it after the list, filter or sort changes.
func (m *model) refreshProcs() { m.visible = m.filterSort() }

// filterSort filters and sorts the process list.
func (m model) filterSort() []report.Process {
	q := strings.ToLower(m.filterValue)
	out := make([]report.Process, 0, len(m.procs))
	for _, p := range m.procs {
		if q == "" || procMatches(p, q) {
			out = append(out, p)
		}
	}
	col, rev := m.procSort, m.procRev
	slices.SortStableFunc(out, func(a, b report.Process) int {
		var c int
		if col.numeric() {
			c = cmpNum(procNum(b, col), procNum(a, col)) // largest first
		} else {
			c = strings.Compare(strings.ToLower(procText(a, col)), strings.ToLower(procText(b, col)))
		}
		if rev {
			c = -c
		}
		if c == 0 { // stable, readable tie-break
			c = cmpNum(procNum(a, sortPID), procNum(b, sortPID))
		}
		return c
	})
	return out
}

// cmpNum orders optional numbers with unknown values below every known one.
func cmpNum(a, b *float64) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return -1
	case b == nil:
		return 1
	}
	return cmp.Compare(*a, *b)
}

func procNum(p report.Process, c sortCol) *float64 {
	switch c {
	case sortCPU:
		return p.CPU
	case sortMem:
		return p.Mem
	case sortGPU:
		return p.GPU
	case sortGPUMem:
		return p.GPUMem
	default:
		return p.PID
	}
}

func procText(p report.Process, c sortCol) string {
	switch c {
	case sortUser:
		return deref(p.User)
	case sortName:
		return deref(p.Name)
	default:
		return deref(p.Cmd)
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// procMatches reports whether a lower-cased query matches the name, command, user or pid.
func procMatches(p report.Process, q string) bool {
	if p.PID != nil && strconv.FormatFloat(*p.PID, 'f', 0, 64) == q {
		return true
	}
	for _, s := range []*string{p.Name, p.Cmd, p.User} {
		if s != nil && strings.Contains(strings.ToLower(*s), q) {
			return true
		}
	}
	return false
}

// procColumn is one table column: heading, width and how a cell renders.
type procColumn struct {
	col   sortCol
	width int
	right bool
	cell  func(p report.Process) string
}

// procColumns returns the columns for a terminal width; CMD takes whatever is left.
func procColumns(w int) []procColumn {
	num := func(v *float64, f string) string {
		if v == nil {
			return none
		}
		return fmt.Sprintf(f, *v)
	}
	cols := []procColumn{
		{sortPID, 7, true, func(p report.Process) string { return num(p.PID, "%.0f") }},
		{sortUser, 10, false, func(p report.Process) string { return str(p.User) }},
		{sortCPU, 6, true, func(p report.Process) string { return num(p.CPU, "%.1f") }},
		{sortMem, 9, true, func(p report.Process) string { return fmtBytesP(p.Mem) }},
		{sortGPU, 5, true, func(p report.Process) string { return num(p.GPU, "%.0f") }},
		{sortGPUMem, 9, true, func(p report.Process) string { return fmtBytesP(p.GPUMem) }},
		{sortName, 16, false, func(p report.Process) string { return str(p.Name) }},
	}
	used := 0
	for _, c := range cols {
		used += c.width + 1
	}
	switch {
	case w >= 120: // room for longer names
		cols[6].width = 24
		used += 8
	case w < 100: // leave CMD a useful width
		cols[1].width, cols[6].width = 8, 12
		used -= 6
	}
	cmdW := max(8, w-used)
	return append(cols, procColumn{sortCmd, cmdW, false, func(p report.Process) string { return deref(p.Cmd) }})
}

// processes is view 3: a sortable, filterable table.
func (m model) processes() string {
	t := m.theme
	switch {
	case m.multi() && m.procs == nil:
		return m.remoteProcsNote()
	case !m.multi() && m.opts.Procs == nil:
		return t.dim.Render("The process list is not available on this build.")
	case m.procs == nil && m.procsErr != nil:
		return t.err.Render("process list: " + m.procsErr.Error())
	case m.procs == nil:
		return t.dim.Render("Listing processes…")
	}
	cols := procColumns(m.width)
	arrow := "↓"
	if m.procSort.numeric() == m.procRev {
		arrow = "↑"
	}
	head := make([]string, len(cols))
	for i, c := range cols {
		h := sortNames[c.col]
		if c.col == m.procSort {
			h += arrow
		}
		if c.right {
			head[i] = padLeft(h, c.width)
		} else {
			head[i] = pad(h, c.width)
		}
	}
	lines := []string{t.label.Render(strings.Join(head, " "))}

	rows := m.visibleProcs()
	end := min(len(rows), m.procOffset+m.procRows())
	for i := m.procOffset; i < end; i++ {
		cells := make([]string, len(cols))
		for j, c := range cols {
			if c.right {
				cells[j] = padLeft(c.cell(rows[i]), c.width)
			} else {
				cells[j] = pad(c.cell(rows[i]), c.width)
			}
		}
		line := pad(strings.Join(cells, " "), m.width)
		if i == m.procCursor {
			line = t.selected.Render(line)
		}
		lines = append(lines, line)
	}
	if len(rows) == 0 {
		lines = append(lines, t.dim.Render("no process matches the filter"))
	}
	return strings.Join(lines, "\n")
}

// procFooter shows the filter input, or counts, sort and filter state.
func (m model) procFooter() string {
	if m.filter.Focused() {
		return m.filter.View() + "   enter apply · esc clear"
	}
	if m.procs == nil {
		return "tab next view · ? help"
	}
	order := "↓"
	if m.procSort.numeric() == m.procRev {
		order = "↑"
	}
	s := fmt.Sprintf("%d of %d processes · sort %s %s", len(m.visibleProcs()), len(m.procs), sortNames[m.procSort], order)
	if m.filterValue != "" {
		s += " · filter \"" + m.filterValue + "\""
	}
	return s + " · / filter · s sort · r reverse"
}

// deviceFooter shows the scroll position.
func (m model) deviceFooter(total int) string {
	end := min(total, m.deviceScroll+m.bodyHeight())
	return fmt.Sprintf("lines %d–%d of %d · ↑/↓ pgup/pgdn scroll · ? help", m.deviceScroll+1, end, total)
}
