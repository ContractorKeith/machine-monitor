package history

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock is a settable clock in Unix ms.
type fakeClock struct {
	mu sync.Mutex
	ms float64
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.UnixMilli(int64(c.ms))
}

func (c *fakeClock) set(ms float64) { c.mu.Lock(); c.ms = ms; c.mu.Unlock() }

const t0 float64 = 1_793_000_000_000 - 1_793_000_000_000%minuteMS // a whole minute

func open(t *testing.T, dir string, c *fakeClock) *Store {
	t.Helper()
	s, err := Open(dir, c.now)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func add(t *testing.T, s *Store, c *fakeClock, id string, ms, cpu float64) {
	t.Helper()
	c.set(ms)
	if err := s.Add(id, Point{T: ms, CPU: ptr(cpu), Power: ptr(cpu * 2)}); err != nil {
		t.Fatal(err)
	}
}

func lines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

func val(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

// A hub that stops and restarts within one minute must merge the two halves of that minute,
// weighted by sample count, instead of storing it twice (upstream v1.2.29).
func TestMinuteMergeAcrossRestart(t *testing.T) {
	dir, c := t.TempDir(), &fakeClock{}
	s := open(t, dir, c)
	add(t, s, c, "a", t0+1000, 10)
	add(t, s, c, "a", t0+3000, 20)
	add(t, s, c, "a", t0+5000, 30) // 3 samples, avg 20, peak 30
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s = open(t, dir, c)
	add(t, s, c, "a", t0+40000, 60)   // same minute, 1 sample
	add(t, s, c, "a", t0+minuteMS, 1) // next minute: commits the merged one
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	got := lines(t, filepath.Join(dir, "a.ndjson"))
	if len(got) != 2 {
		t.Fatalf("got %d lines, want 2:\n%s", len(got), strings.Join(got, "\n"))
	}
	if !strings.Contains(got[0], `"cpu":30,`) || !strings.Contains(got[0], `"cpuMax":60,`) || !strings.HasSuffix(got[0], `"n":4}`) {
		t.Errorf("merged minute = %s, want cpu 30 (=(20*3+60)/4), cpuMax 60, n 4", got[0])
	}

	// A file that already has the duplicate (written by a pre-1.2.29 hub) is merged on load.
	dup := `{"id":"b","t":` + ftoa(t0) + `,"cpu":10,"cpuMax":10}` + "\n" + `{"id":"b","t":` + ftoa(t0) + `,"cpu":20,"cpuMax":25}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "b.ndjson"), []byte(dup), 0o600); err != nil {
		t.Fatal(err)
	}
	s = open(t, dir, c)
	mp := s.MinutePoints("b", 0, t0+minuteMS)
	if len(mp) != 1 || val(mp[0].CPU) != 15.0 || val(mp[0].CPUMax) != 25.0 || mp[0].N != 2 {
		t.Fatalf("legacy duplicate minute = %+v, want cpu 15 cpuMax 25 n 2", mp)
	}
	if got := lines(t, filepath.Join(dir, "b.ndjson")); len(got) != 1 {
		t.Errorf("load left %d lines in b.ndjson, want 1", len(got))
	}
}

func ftoa(ms float64) string { return strconv.FormatFloat(ms, 'f', -1, 64) }

func TestPrune(t *testing.T) {
	dir, c := t.TempDir(), &fakeClock{}
	s := open(t, dir, c)
	for d := 0; d < 3; d++ { // one minute on each of days 0, 1, 2
		add(t, s, c, "a", t0+float64(d)*dayMS, 1)
		add(t, s, c, "a", t0+float64(d)*dayMS+minuteMS, 1)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "a.ndjson")
	if n := len(lines(t, path)); n != 6 {
		t.Fatalf("got %d lines, want 6", n)
	}

	// On load, rows older than 30 days are dropped and the file rewritten.
	c.set(t0 + keepMS + dayMS) // cutoff lands on day 1's first minute, which stays
	s = open(t, dir, c)
	if n := len(lines(t, path)); n != 4 {
		t.Fatalf("after load: %d lines, want 4 (day 0 expired)", n)
	}
	if mp := s.MinutePoints("a", 0, c.ms); len(mp) != 4 || mp[0].T != t0+dayMS {
		t.Fatalf("after load: %d points from %v", len(mp), mp[0].T)
	}

	// On commit, prune runs once the oldest row is more than a day past the cutoff.
	late := t0 + 2*dayMS + keepMS + 2*dayMS
	add(t, s, c, "a", late, 1)
	add(t, s, c, "a", late+minuteMS, 1)
	got := lines(t, path)
	if len(got) != 1 || !strings.Contains(got[0], `"t":`+ftoa(late)+`,`) {
		t.Fatalf("after prune: %v", got)
	}

	// Remove forgets the device and deletes the file.
	if err := s.Remove("a"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file still there: %v", err)
	}
	if r := s.Query("a", 0, late+2*minuteMS, 1200); len(r.Points) != 0 {
		t.Fatalf("still %d points after Remove", len(r.Points))
	}

	// A file with only expired rows is deleted on load.
	old := `{"id":"z","t":1,"cpu":1}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "z.ndjson"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	open(t, dir, c)
	if _, err := os.Stat(filepath.Join(dir, "z.ndjson")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("expired-only file not deleted")
	}
}

func TestDownsampling(t *testing.T) {
	dir, c := t.TempDir(), &fakeClock{}
	s := open(t, dir, c)
	// Two hours of 2 s samples; cpu ramps so averages are predictable, with one spike.
	end := t0 + 2*rawMS
	for ms := float64(t0); ms < end; ms += 2000 {
		cpu := float64(int(ms-t0)/minuteMS%10) * 10
		if ms == t0+rawMS+30*minuteMS {
			cpu = 99
		}
		add(t, s, c, "a", ms, cpu)
	}

	// Last 10 min at 1200 points: width 500 ms ≤ 2 s, so raw samples come back as is.
	r := s.Query("a", end-10*minuteMS, end, 1200)
	if r.Resolution != 2000 || len(r.Points) != 300 || r.Points[0].CPUMax != nil {
		t.Fatalf("10m: resolution %v, %d points", r.Resolution, len(r.Points))
	}

	// Last hour at 120 points: 30 s buckets from raw, peaks kept.
	r = s.Query("a", end-rawMS, end, 120)
	if r.Resolution != 30000 || len(r.Points) != 120 {
		t.Fatalf("1h/120: resolution %v, %d points", r.Resolution, len(r.Points))
	}
	peak := 0.0
	for _, p := range r.Points {
		if *p.CPUMax > peak {
			peak = *p.CPUMax
		}
		if (p.T-(end-rawMS))/30000 != float64(int((p.T-(end-rawMS))/30000)) {
			t.Fatalf("bucket %v not aligned to from", p.T)
		}
	}
	if peak != 99 {
		t.Errorf("peak = %v, want the 99 spike", peak)
	}

	// Two hours at 1200 points (6 s) reaches past the raw hour: minutes fill in before it.
	r = s.Query("a", t0, end, 1200)
	if r.Resolution != minuteMS || r.Points[0].T != t0 || r.Points[0].N != 30 {
		t.Fatalf("2h: resolution %v, first %+v", r.Resolution, r.Points[0])
	}

	// Two hours at 12 points: 10-minute buckets of minute points; the live minute is included.
	r = s.Query("a", t0, end, 12)
	if r.Resolution != 10*minuteMS || len(r.Points) != 12 {
		t.Fatalf("2h/12: resolution %v, %d points", r.Resolution, len(r.Points))
	}
	for i, p := range r.Points {
		if i != 9 && val(p.CPU) != 45.0 || val(p.CPUMax) == nil {
			t.Fatalf("10-min bucket %+v: want avg 45 of 0..90 (bucket 9 has the spike)", p)
		}
	}
	if val(r.Points[9].CPUMax) != 99.0 {
		t.Errorf("bucket 9 peak = %v, want 99", val(r.Points[9].CPUMax))
	}

	// A device with no data gets an empty (non-nil) list.
	if r := s.Query("nobody", t0, end, 100); r.Points == nil || len(r.Points) != 0 {
		t.Fatalf("nobody: %#v", r.Points)
	}

	// Recent is raw samples within the window, by the injected clock.
	c.set(end)
	if got := s.Recent("a", 10*time.Minute); len(got) != 300 {
		t.Fatalf("Recent: %d samples, want 300", len(got))
	}
}

func TestNilStaysNil(t *testing.T) {
	s := open(t, t.TempDir(), &fakeClock{})
	for i := range 5 {
		if err := s.Add("a", Point{T: t0 + float64(i)*2000, CPU: ptr(1)}); err != nil {
			t.Fatal(err)
		}
	}
	p := s.Query("a", t0, t0+minuteMS, 1)
	if len(p.Points) != 1 || p.Points[0].GPU != nil || p.Points[0].GPUMax != nil || val(p.Points[0].CPU) != 1.0 {
		t.Fatalf("got %+v", p.Points)
	}
}

func TestRound2(t *testing.T) {
	for _, c := range []struct{ in, want float64 }{
		{0.125, 0.13}, {2.375, 2.38}, {-0.125, -0.13}, {0.25, 0.25}, {1.005, 1}, // 1.005 is 1.00499...
		{12.344999, 12.34}, {-0.001, 0}, {1e15 + 0.125, 1e15 + 0.13}, {3, 3},
	} {
		if got := round2(c.in); got != c.want {
			t.Errorf("round2(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestParams(t *testing.T) {
	const now = 1_800_000_000_000
	s := &Store{now: func() time.Time { return time.UnixMilli(now) }}
	for _, c := range []struct {
		q                string
		from, to, points float64
		err              bool
	}{
		{"", now - rawMS, now, 1200, false},
		{"range=7d&points=5", now - 7*dayMS, now, 10, false},
		{"range=bogus&points=99999", now - rawMS, now, 3000, false},
		{"from=1000&to=5000&points=0", 1000, 5000, 1200, false},
		{"to=Infinity&from=1e999&points=NaN", now - rawMS, now, 1200, false},
		{"from=%20&to=10", 0, 10, 1200, false},
		{"from=10&to=10", 0, 0, 0, true},
	} {
		q, _ := url.ParseQuery(c.q)
		from, to, points, err := s.Params(q)
		if (err != nil) != c.err || !c.err && (from != c.from || to != c.to || points != c.points) {
			t.Errorf("%q: got %v %v %v %v", c.q, from, to, points, err)
		}
	}
}

// The hub adds while TUIs query; run with -race to check.
func TestConcurrent(t *testing.T) {
	c := &fakeClock{}
	s := open(t, t.TempDir(), c)
	var wg sync.WaitGroup
	for g := range 4 {
		wg.Go(func() {
			id := string(rune('a' + g))
			for i := range 300 {
				if err := s.Add(id, Point{T: t0 + float64(i)*2000, CPU: ptr(float64(i))}); err != nil {
					t.Error(err)
					return
				}
			}
		})
		wg.Go(func() {
			for range 300 {
				s.Query("a", t0, t0+rawMS, 100)
				s.Recent("b", time.Hour)
				s.MinutePoints("c", t0, t0+rawMS)
			}
		})
	}
	wg.Wait()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}
