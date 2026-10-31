// Package history keeps metric history per device:
//
//   - raw samples (every ~2 s) for the last hour, in memory;
//   - 1-minute averages, plus peaks, for the last 30 days, in memory and appended to
//     <dir>/<id>.ndjson.
//
// The NDJSON lines, the minute aggregation, the same-minute merge after a restart (upstream
// v1.2.29), the 30-day prune and the downsampling behind GET /api/history/:id all match
// upstream, so history files move freely between a machine-monitor hub and a device-monitor
// hub. Times are Unix milliseconds as float64, like upstream's JSON: samples and minutes are
// whole milliseconds, downsampled buckets may not be.
//
// A Store is safe for concurrent use: the hub adds samples while connected TUIs query.
//
// Ported from Forward-Future/device-monitor history.js @ c3a4c41
// (MIT, © 2026 Matt): https://github.com/Forward-Future/device-monitor
package history

import (
	"bytes"
	"encoding/json"
	"errors"
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
	rawMS    = 60 * 60 * 1000           // raw samples kept for an hour
	minuteMS = 60 * 1000                // minute bucket width
	keepMS   = 30 * 24 * 60 * 60 * 1000 // minute points kept for 30 days
	dayMS    = 24 * 60 * 60 * 1000
)

// Point is one raw sample, one stored minute, or one downsampled bucket. A nil field is
// unknown. Raw samples leave the *Max fields nil; minutes and buckets set them to the peak in
// the bucket. N counts the raw samples behind a stored minute (0 for rows from before upstream
// stored counts, and for raw samples and buckets).
//
// Points returned by a Store share their *float64 values with the store: read them, never
// write through them.
type Point struct {
	T        float64  `json:"t"`
	CPU      *float64 `json:"cpu"`      // %
	GPU      *float64 `json:"gpu"`      // % of the main GPU
	Mem      *float64 `json:"mem"`      // % used
	CPUTemp  *float64 `json:"cpuTemp"`  // °C
	GPUTemp  *float64 `json:"gpuTemp"`  // °C
	GPUPower *float64 `json:"gpuPower"` // W
	Power    *float64 `json:"power"`    // W, estimated whole-machine draw
	RX       *float64 `json:"rx"`       // bytes/s
	TX       *float64 `json:"tx"`       // bytes/s
	DR       *float64 `json:"dr"`       // disk read bytes/s
	DW       *float64 `json:"dw"`       // disk write bytes/s

	CPUMax     *float64 `json:"cpuMax"`
	GPUMax     *float64 `json:"gpuMax"`
	CPUTempMax *float64 `json:"cpuTempMax"`
	GPUTempMax *float64 `json:"gpuTempMax"`
	PowerMax   *float64 `json:"powerMax"`

	N int `json:"n,omitempty"`
}

// fields returns the averaged fields in upstream's FIELDS order.
func (p *Point) fields() [11]**float64 {
	return [11]**float64{&p.CPU, &p.GPU, &p.Mem, &p.CPUTemp, &p.GPUTemp, &p.GPUPower, &p.Power, &p.RX, &p.TX, &p.DR, &p.DW}
}

// peaks returns, in upstream's MAX_FIELDS order, each peak field and the field it peaks.
func (p *Point) peaks() [5][2]**float64 {
	return [5][2]**float64{{&p.CPUMax, &p.CPU}, {&p.GPUMax, &p.GPU}, {&p.CPUTempMax, &p.CPUTemp}, {&p.GPUTempMax, &p.GPUTemp}, {&p.PowerMax, &p.Power}}
}

// Result is the body of GET /api/history/:id. Resolution is the bucket width in ms.
type Result struct {
	From       float64 `json:"from"`
	To         float64 `json:"to"`
	Resolution float64 `json:"resolution"`
	Points     []Point `json:"points"`
}

type pending struct {
	minute  float64
	samples []Point
}

// Store holds history for every device. Create one with Open.
type Store struct {
	dir string
	now func() time.Time

	mu      sync.RWMutex
	raw     map[string][]Point // id -> samples (last hour)
	minutes map[string][]Point // id -> 1-minute points (last 30 days)
	pending map[string]*pending
}

// Open loads every <id>.ndjson in dir (config.Paths.History), creating dir if needed. It
// drops rows older than 30 days, merges duplicate minutes and rewrites files that needed
// either. now is the clock for prunes and Recent; nil means time.Now.
func Open(dir string, now func() time.Time) (*Store, error) {
	if now == nil {
		now = time.Now
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, now: now, raw: map[string][]Point{}, minutes: map[string][]Point{}, pending: map[string]*pending{}}
	return s, s.load()
}

func (s *Store) nowMS() float64 { return float64(s.now().UnixMilli()) }

// safeName maps an id to its file name stem: anything outside [a-zA-Z0-9_-] becomes "_".
func safeName(id string) string {
	return strings.Map(func(r rune) rune {
		if r == '_' || r == '-' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			return r
		}
		return '_'
	}, id)
}

func (s *Store) file(id string) string { return filepath.Join(s.dir, safeName(id)+".ndjson") }

// line is one NDJSON row: the device id, then the point.
type line struct {
	ID *string `json:"id"`
	Point
}

func (s *Store) load() error {
	files, err := readFiles(s.dir, s.nowMS()-keepMS)
	for _, f := range files {
		if len(f.pts) == 0 {
			if rerr := os.Remove(f.path); rerr != nil {
				err = errors.Join(err, rerr)
			}
			continue
		}
		s.minutes[f.id] = f.pts
		if f.dirty {
			if werr := s.rewrite(f.id); werr != nil {
				err = errors.Join(err, werr)
			}
		}
	}
	return err
}

// file is one parsed <id>.ndjson: the minutes still within 30 days, merged, and whether the
// file on disk differs from them (expired lines, duplicate minutes or a partial last line).
type file struct {
	path  string
	id    string
	pts   []Point
	dirty bool
}

// readFiles parses every <id>.ndjson in dir, dropping rows before cutoff, without changing
// anything on disk. Unreadable files are skipped and their errors joined.
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
		f := parseFile(data, cutoff)
		f.path = path
		if f.id == "" {
			f.id = strings.TrimSuffix(name, ".ndjson") // no id in the rows: use the file name
		}
		files = append(files, f)
	}
	return files, errors.Join(errs...)
}

// parseFile reads one file's rows, as upstream's loader does: rows before cutoff and bad
// JSON are skipped, and a row with the same minute as the one before is merged into it. id is
// the id of the last point's row, or "" when it has none.
func parseFile(data []byte, cutoff float64) file {
	lines := bytes.Split(data, []byte("\n"))
	var pts []Point
	var lastID *string // id of the last kept point, as upstream tracks it
	merged := false
	for _, b := range lines {
		if len(b) == 0 {
			continue
		}
		var l line
		if json.Unmarshal(b, &l) != nil || !(l.T >= cutoff) {
			continue
		}
		if n := len(pts); n > 0 && pts[n-1].T == l.T {
			pts[n-1] = merge(pts[n-1], l.Point)
			merged = true
			if l.ID != nil {
				lastID = l.ID
			}
		} else {
			pts = append(pts, l.Point)
			lastID = l.ID
		}
	}
	f := file{pts: pts}
	if lastID != nil {
		f.id = *lastID
	}
	// A partial last line is left by a crash mid-append; the next append would run into it.
	partial := len(data) > 0 && data[len(data)-1] != '\n'
	f.dirty = merged || partial || len(pts) < len(lines)-1
	return f
}

// Snapshot is a read-only copy of a history directory's stored minutes, for tools such as
// export that run beside a live hub. Create one with ReadDir.
type Snapshot struct {
	minutes map[string][]Point
}

// ReadDir loads every <id>.ndjson in dir with the same parsing, merging and 30-day cutoff as
// Open, but never writes, renames, removes or creates anything, so it is safe while a hub
// appends to the same files. A missing dir is an empty snapshot. Unreadable files are skipped
// and their errors returned with the snapshot. now is the clock for the cutoff; nil means
// time.Now.
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
		if len(f.pts) > 0 {
			snap.minutes[f.id] = f.pts
		}
	}
	return snap, err
}

// IDs returns the ids of every device with stored minutes, sorted.
func (s *Snapshot) IDs() []string {
	ids := make([]string, 0, len(s.minutes))
	for id := range s.minutes {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// Has reports whether id has any stored minutes.
func (s *Snapshot) Has(id string) bool { return len(s.minutes[id]) > 0 }

// MinutePoints returns id's stored minutes with from <= T <= to, oldest first.
func (s *Snapshot) MinutePoints(id string, from, to float64) []Point {
	return filter(s.minutes[id], func(p Point) bool { return p.T >= from && p.T <= to })
}

// encodeLine renders one row exactly as upstream's JSON.stringify({ id, ...p }) does.
func encodeLine(id string, p Point) []byte {
	b, _ := json.Marshal(line{ID: &id, Point: p}) // cannot fail: no maps, no NaN (inputs are finite)
	return append(b, '\n')
}

// rewrite replaces id's file with its in-memory minutes. Caller holds mu (or is load).
func (s *Store) rewrite(id string) error {
	var buf bytes.Buffer
	for _, p := range s.minutes[id] {
		buf.Write(encodeLine(id, p))
	}
	return config.WriteFile0600(s.file(id), buf.Bytes())
}

// appendLine adds one row to id's file. Appends can't go through WriteFile0600 (it replaces
// the whole file, which at 30 days is ~40k rows per minute), so the file is opened 0600.
func (s *Store) appendLine(id string, p Point) error {
	f, err := os.OpenFile(s.file(id), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(encodeLine(id, p))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// Add records one raw sample for id; p.T is when it was taken. Finishing a minute writes it to
// disk, and only a write failure returns an error; the sample is kept in memory regardless.
// Values must be finite (the hub's Normalize guarantees it).
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
		err = s.commit(id, pend)
		pend = nil
	}
	if pend == nil {
		pend = &pending{minute: minute}
		s.pending[id] = pend
	}
	pend.samples = append(pend.samples, p)
	return err
}

// commit turns a finished minute into a stored point. Caller holds mu.
func (s *Store) commit(id string, pend *pending) error {
	agg := aggregate(pend.samples, minuteMS, pend.minute)
	if len(agg) == 0 {
		return nil
	}
	p := agg[0]
	p.N = len(pend.samples)
	mins := s.minutes[id]
	if n := len(mins); n > 0 && mins[n-1].T == p.T { // same minute as before a restart: merge, don't double-count
		mins[n-1] = merge(mins[n-1], p)
		return s.rewrite(id)
	}
	mins = append(mins, p)
	s.minutes[id] = mins
	cutoff := s.nowMS() - keepMS
	if mins[0].T < cutoff-dayMS { // prune about once a day
		i := 0
		for i < len(mins) && mins[i].T < cutoff {
			i++
		}
		s.minutes[id] = append([]Point(nil), mins[i:]...)
		return s.rewrite(id)
	}
	return s.appendLine(id, p)
}

// Recent returns id's raw samples from the last d, oldest first (the live dashboard view).
func (s *Store) Recent(id string, d time.Duration) []Point {
	since := s.nowMS() - float64(d.Milliseconds())
	s.mu.RLock()
	defer s.mu.RUnlock()
	return filter(s.raw[id], func(p Point) bool { return p.T >= since })
}

// MinutePoints returns id's 1-minute points covering [from, to], including the current,
// unfinished minute built from raw samples.
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

// Query returns id's points covering [from, to], at most about maxPoints of them, using raw
// samples when the requested resolution is finer than a minute.
func (s *Store) Query(id string, from, to, maxPoints float64) Result {
	s.mu.RLock()
	defer s.mu.RUnlock()
	span := math.Max(1, to-from)
	width := math.Max(1, span/maxPoints)
	raw := filter(s.raw[id], func(p Point) bool { return p.T >= from && p.T <= to })
	var points []Point
	var resolution float64
	if width < minuteMS {
		// Fine-grained: raw samples, with minute data filling in before raw starts (e.g. right
		// after a restart).
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
		// Minute data, plus the current unfinished minute from raw so the right edge is live.
		points = s.minutePoints(id, from, to)
		if width > minuteMS {
			points = aggregate(points, width, from)
		}
		resolution = math.Max(minuteMS, width)
	}
	if points == nil {
		points = []Point{} // upstream sends [], not null
	}
	return Result{From: from, To: to, Resolution: resolution, Points: points}
}

// Remove forgets id and deletes its file.
func (s *Store) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.raw, id)
	delete(s.minutes, id)
	delete(s.pending, id)
	if err := os.Remove(s.file(id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Close writes every unfinished minute to disk. Call it on hub shutdown; a restart in the
// same minute merges with what Close wrote.
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

// filter returns a new slice of the points in ps that keep accepts.
func filter(ps []Point, keep func(Point) bool) []Point {
	var out []Point
	for _, p := range ps {
		if keep(p) {
			out = append(out, p)
		}
	}
	return out
}
