package history

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/config"
)

const (
	rawMS    = 60 * 60 * 1000
	minuteMS = 60 * 1000
	keepMS   = 30 * 24 * 60 * 60 * 1000
	dayMS    = 24 * 60 * 60 * 1000
)

type Point struct {
	T        float64  `json:"t"`
	CPU      *float64 `json:"cpu"`
	GPU      *float64 `json:"gpu"`
	Mem      *float64 `json:"mem"`
	CPUTemp  *float64 `json:"cpuTemp"`
	GPUTemp  *float64 `json:"gpuTemp"`
	GPUPower *float64 `json:"gpuPower"`
	Power    *float64 `json:"power"`
	RX       *float64 `json:"rx"`
	TX       *float64 `json:"tx"`
	DR       *float64 `json:"dr"`
	DW       *float64 `json:"dw"`

	CPUMax     *float64 `json:"cpuMax"`
	GPUMax     *float64 `json:"gpuMax"`
	CPUTempMax *float64 `json:"cpuTempMax"`
	GPUTempMax *float64 `json:"gpuTempMax"`
	PowerMax   *float64 `json:"powerMax"`

	N int `json:"n,omitempty"`
}

func (p *Point) fields() [11]**float64 {
	return [11]**float64{&p.CPU, &p.GPU, &p.Mem, &p.CPUTemp, &p.GPUTemp, &p.GPUPower, &p.Power, &p.RX, &p.TX, &p.DR, &p.DW}
}

func (p *Point) peaks() [5][2]**float64 {
	return [5][2]**float64{{&p.CPUMax, &p.CPU}, {&p.GPUMax, &p.GPU}, {&p.CPUTempMax, &p.CPUTemp}, {&p.GPUTempMax, &p.GPUTemp}, {&p.PowerMax, &p.Power}}
}

type Result struct {
	From       float64 `json:"from"`
	To         float64 `json:"to"`
	Resolution float64 `json:"resolution"`
	Points     []Point `json:"points"`
}

const (
	maxTries     = 5
	maxUnwritten = 60
)

type retry struct {
	pts   []Point
	tries int
}

type pending struct {
	minute  float64
	samples []Point
}

type Store struct {
	dir string
	now func() time.Time

	mu      sync.RWMutex
	raw     map[string][]Point
	minutes map[string][]Point
	pending map[string]*pending

	unwritten      map[string]*retry
	write          func(f *os.File, b []byte) (int, error)
	failKind       string
	warnings       []string
	needsRewrite   map[string]bool
	migrationPeers map[string][]string
	sources        map[string]bool
}

func Open(dir string, now func() time.Time) (*Store, error) {
	if now == nil {
		now = time.Now
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, now: now, raw: map[string][]Point{}, minutes: map[string][]Point{}, pending: map[string]*pending{}, unwritten: map[string]*retry{},
		write: func(f *os.File, b []byte) (int, error) { return f.Write(b) }}
	return s, s.load()
}

func (s *Store) nowMS() float64 { return float64(s.now().UnixMilli()) }

func safeName(id string) string {
	var b strings.Builder
	for i := 0; i < len(id); i++ {
		c := id[i]
		if c == '_' || c == '-' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "~%02x", c)
		}
	}
	name := b.String()
	reserved := name == "con" || name == "prn" || name == "aux" || name == "nul" || len(name) == 4 && (strings.HasPrefix(name, "com") || strings.HasPrefix(name, "lpt")) && name[3] >= '1' && name[3] <= '9'
	if reserved {
		return fmt.Sprintf("~%02x%s", name[0], name[1:])
	}
	return name
}

func (s *Store) file(id string) string { return filepath.Join(s.dir, safeName(id)+".ndjson") }

type line struct {
	ID *string `json:"id"`
	Point
}

func (s *Store) load() error {
	files, err := readFiles(s.dir, s.nowMS()-keepMS)
	rewrite := map[string]bool{}
	s.needsRewrite = rewrite
	originals := map[string]bool{}
	s.sources = originals
	sourceGroups := map[string]int{}
	for _, f := range files {
		sourceGroups[f.path]++
	}
	for _, f := range files {
		originals[f.path] = true
		if f.skipped > 0 {
			s.warnings = append(s.warnings, fmt.Sprintf("history: %s: skipped %d corrupt or truncated record(s)", f.path, f.skipped))
		}
		if f.shadowed || len(f.pts) == 0 {
			continue
		}
		s.minutes[f.id] = mergeMinutes(append(s.minutes[f.id], f.pts...))
		if f.dirty || f.path != s.file(f.id) {
			rewrite[f.id] = true
		}
	}
	s.migrationPeers = map[string][]string{}
	for _, f := range files {
		if sourceGroups[f.path] > 1 && f.path == s.file(f.id) {
			for _, peer := range files {
				if peer.path == f.path && peer.id != f.id {
					s.migrationPeers[f.id] = append(s.migrationPeers[f.id], peer.id)
				}
			}
		}
	}
	failed := false
	for _, protectSource := range []bool{false, true} {
		if failed {
			break
		}
		for id := range rewrite {
			if (sourceGroups[s.file(id)] > 1) != protectSource {
				continue
			}
			if werr := s.rewrite(id); werr != nil {
				err = errors.Join(err, werr)
				failed = true
			}
		}
	}
	if !failed {
		err = errors.Join(err, s.cleanupSources())
	}
	return err
}

func (s *Store) cleanupSources() error {
	for id := range s.minutes {
		delete(s.sources, s.file(id))
	}
	var errs []error
	for path := range s.sources {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		} else {
			delete(s.sources, path)
		}
	}
	return errors.Join(errs...)
}

func (s *Store) Warnings() []string { return slices.Clone(s.warnings) }

type file struct {
	path     string
	id       string
	pts      []Point
	dirty    bool
	skipped  int
	shadowed bool
}

func readFiles(dir string, cutoff float64) ([]file, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []file
	var errs []error
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".ndjson") || e.IsDir() {
			continue
		}
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, f := range parseFiles(data, cutoff, strings.TrimSuffix(name, ".ndjson")) {
			f.path = path
			files = append(files, f)
		}
	}
	canonical := map[string]bool{}
	for _, f := range files {
		if len(f.pts) > 0 && filepath.Base(f.path) == safeName(f.id)+".ndjson" {
			canonical[f.id] = true
		}
	}
	for i := range files {
		f := &files[i]
		f.shadowed = canonical[f.id] && filepath.Base(f.path) != safeName(f.id)+".ndjson"
	}
	return files, errors.Join(errs...)
}

func parseFiles(data []byte, cutoff float64, fallback string) []file {
	groups := map[string][]Point{}
	skipped := 0
	dirty := len(data) > 0 && data[len(data)-1] != '\n'
	for _, b := range bytes.Split(data, []byte("\n")) {
		if len(b) == 0 {
			continue
		}
		valid := json.Valid(b)
		if !valid {
			skipped++
			dirty = true
		}
		l, ok := parseLine(b)
		if !ok {
			if valid {
				skipped++
				dirty = true
			}
			continue
		}
		if !(l.T >= cutoff) {
			dirty = true
			continue
		}
		id := fallback
		if l.ID != nil && *l.ID != "" {
			id = *l.ID
		}
		groups[id] = append(groups[id], l.Point)
	}
	if len(groups) == 0 {
		return []file{{id: fallback, dirty: dirty, skipped: skipped}}
	}
	ids := make([]string, 0, len(groups))
	for id := range groups {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	var out []file
	for _, id := range ids {
		pts := mergeMinutes(groups[id])
		out = append(out, file{id: id, pts: pts, dirty: dirty || len(groups) > 1 || len(pts) != len(groups[id])})
	}
	out[0].skipped = skipped
	return out
}

func mergeMinutes(pts []Point) []Point {
	slices.SortStableFunc(pts, func(a, b Point) int {
		if a.T < b.T {
			return -1
		}
		if a.T > b.T {
			return 1
		}
		return 0
	})
	out := pts[:0]
	for _, p := range pts {
		if n := len(out); n > 0 && out[n-1].T == p.T {
			out[n-1] = merge(out[n-1], p)
		} else {
			out = append(out, p)
		}
	}
	return out
}

var rowStart = []byte(`{"id":"`)

func parseLine(b []byte) (l line, ok bool) {
	for {
		l = line{}
		if json.Unmarshal(b, &l) == nil {
			return l, true
		}
		if len(b) == 0 {
			return line{}, false
		}
		i := bytes.Index(b[1:], rowStart)
		if i < 0 {
			return line{}, false
		}
		b = b[1+i:]
	}
}

type Snapshot struct {
	minutes map[string][]Point
}

func ReadDir(dir string, now func() time.Time) (*Snapshot, error) {
	if now == nil {
		now = time.Now
	}
	snap := &Snapshot{minutes: map[string][]Point{}}
	files, err := readFiles(dir, float64(now().UnixMilli())-keepMS)
	if errors.Is(err, fs.ErrNotExist) && files == nil {
		return snap, nil
	}
	for _, f := range files {
		if len(f.pts) > 0 && !f.shadowed {
			snap.minutes[f.id] = mergeMinutes(append(snap.minutes[f.id], f.pts...))
		}
	}
	return snap, err
}

func (s *Snapshot) IDs() []string {
	ids := make([]string, 0, len(s.minutes))
	for id := range s.minutes {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func (s *Snapshot) Has(id string) bool { return len(s.minutes[id]) > 0 }

func (s *Snapshot) MinutePoints(id string, from, to float64) []Point {
	return filter(s.minutes[id], func(p Point) bool { return p.T >= from && p.T <= to })
}

func encodeLine(id string, p Point) []byte {
	b, _ := json.Marshal(line{ID: &id, Point: p})
	return append(b, '\n')
}

func (s *Store) rewrite(id string) error {
	for _, peer := range s.migrationPeers[id] {
		if s.needsRewrite[peer] {
			if len(s.migrationPeers[peer]) > 0 {
				return fmt.Errorf("history: overlapping legacy migration sources for %s and %s", id, peer)
			}
			if err := s.writeMinutes(peer); err != nil {
				return err
			}
		}
	}
	return s.writeMinutes(id)
}

func (s *Store) writeMinutes(id string) error {
	var buf bytes.Buffer
	for _, p := range s.minutes[id] {
		buf.Write(encodeLine(id, p))
	}
	err := config.WriteFile0600(s.file(id), buf.Bytes())
	if err == nil {
		delete(s.unwritten, id)
		delete(s.needsRewrite, id)
	}
	return err
}

func (s *Store) appendLine(id string, p Point) error {
	path := s.file(id)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	b := encodeLine(id, p)
	if size := fi.Size(); size > 0 {
		var last [1]byte
		if _, err := f.ReadAt(last[:], size-1); err == nil && last[0] != '\n' {
			b = append([]byte{'\n'}, b...)
		}
	}
	n, err := s.write(f, b)
	if err == nil && n < len(b) {
		err = io.ErrShortWrite
	}
	if err != nil {
		truncate(path, fi.Size())
		return err
	}
	return nil
}

func truncate(path string, size int64) {
	if f, err := os.OpenFile(path, os.O_WRONLY, 0); err == nil {
		_ = f.Truncate(size)
		f.Close()
	}
}

func (s *Store) reportErr(err error) error {
	if err == nil {
		s.failKind = ""
		return nil
	}
	kind := err.Error()
	var pe *os.PathError
	if errors.As(err, &pe) {
		kind = pe.Err.Error()
	}
	if kind == s.failKind {
		return nil
	}
	s.failKind = kind
	return err
}

func (s *Store) Add(id string, p Point) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw := append(s.raw[id], p)
	i := 0
	for i < len(raw) && raw[i].T < p.T-rawMS {
		i++
	}
	s.raw[id] = raw[i:]

	minute := math.Floor(p.T/minuteMS) * minuteMS
	var err error
	pend := s.pending[id]
	if pend != nil && pend.minute != minute {
		err = s.reportErr(s.commit(id, pend))
		pend = nil
	}
	if pend == nil {
		pend = &pending{minute: minute}
		s.pending[id] = pend
	}
	pend.samples = append(pend.samples, p)
	return err
}

func (s *Store) commit(id string, pend *pending) error {
	agg := aggregate(pend.samples, minuteMS, pend.minute)
	if len(agg) == 0 {
		return nil
	}
	p := agg[0]
	p.N = len(pend.samples)
	mins := s.minutes[id]
	if n := len(mins); n > 0 && mins[n-1].T == p.T {
		mins[n-1] = merge(mins[n-1], p)
		return s.rewrite(id)
	}
	mins = append(mins, p)
	s.minutes[id] = mins
	cutoff := s.nowMS() - keepMS
	if mins[0].T < cutoff-dayMS {
		i := 0
		for i < len(mins) && mins[i].T < cutoff {
			i++
		}
		s.minutes[id] = append([]Point(nil), mins[i:]...)
		return s.rewrite(id)
	}
	if s.needsRewrite[id] {
		return s.rewrite(id)
	}
	return s.appendRetrying(id, p)
}

func (s *Store) appendRetrying(id string, p Point) error {
	q := s.unwritten[id]
	if q == nil {
		q = &retry{}
	}
	q.pts = append(q.pts, p)
	for len(q.pts) > 0 {
		if err := s.appendLine(id, q.pts[0]); err != nil {
			if q.tries++; q.tries >= maxTries {
				q.pts, q.tries = q.pts[1:], 0
			}
			if len(q.pts) > maxUnwritten {
				q.pts = q.pts[len(q.pts)-maxUnwritten:]
			}
			if len(q.pts) > 0 {
				s.unwritten[id] = q
			} else {
				delete(s.unwritten, id)
			}
			return err
		}
		q.pts, q.tries = q.pts[1:], 0
	}
	delete(s.unwritten, id)
	return nil
}

func (s *Store) Recent(id string, d time.Duration) []Point {
	since := s.nowMS() - float64(d.Milliseconds())
	s.mu.RLock()
	defer s.mu.RUnlock()
	return filter(s.raw[id], func(p Point) bool { return p.T >= since })
}

func (s *Store) MinutePoints(id string, from, to float64) []Point {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.minutePoints(id, from, to)
}

func (s *Store) minutePoints(id string, from, to float64) []Point {
	mins := filter(s.minutes[id], func(p Point) bool { return p.T >= from && p.T <= to })
	lastMin := math.Inf(-1)
	if len(mins) > 0 {
		lastMin = mins[len(mins)-1].T
	}
	tail := filter(s.raw[id], func(p Point) bool { return p.T >= lastMin+minuteMS && p.T >= from && p.T <= to })
	return append(mins, aggregate(tail, minuteMS, math.Floor(from/minuteMS)*minuteMS)...)
}

func (s *Store) Query(id string, from, to, maxPoints float64) Result {
	s.mu.RLock()
	defer s.mu.RUnlock()
	span := math.Max(1, to-from)
	width := math.Max(1, span/maxPoints)
	raw := filter(s.raw[id], func(p Point) bool { return p.T >= from && p.T <= to })
	var points []Point
	var resolution float64
	if width < minuteMS {
		rawStart := math.Inf(1)
		if len(raw) > 0 {
			rawStart = raw[0].T
		}
		older := filter(s.minutes[id], func(p Point) bool { return p.T >= from && p.T+minuteMS <= rawStart })
		fine := raw
		if width > 2000 {
			fine = aggregate(raw, width, from)
		}
		points = append(older, fine...)
		resolution = math.Max(2000, width)
		if len(older) > 0 {
			resolution = minuteMS
		}
	} else {
		points = s.minutePoints(id, from, to)
		if width > minuteMS {
			points = aggregate(points, width, from)
		}
		resolution = math.Max(minuteMS, width)
	}
	if points == nil {
		points = []Point{}
	}
	return Result{From: from, To: to, Resolution: resolution, Points: points}
}

func (s *Store) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for pendingID := range s.needsRewrite {
		if err := s.rewrite(pendingID); err != nil {
			return err
		}
	}
	if err := s.cleanupSources(); err != nil {
		return err
	}
	delete(s.raw, id)
	delete(s.minutes, id)
	delete(s.pending, id)
	delete(s.needsRewrite, id)
	delete(s.migrationPeers, id)
	if err := os.Remove(s.file(id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var errs []error
	for id, pend := range s.pending {
		errs = append(errs, s.commit(id, pend))
	}
	clear(s.pending)
	return errors.Join(errs...)
}

func filter(ps []Point, keep func(Point) bool) []Point {
	var out []Point
	for _, p := range ps {
		if keep(p) {
			out = append(out, p)
		}
	}
	return out
}
