package history

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A crash mid-append leaves a partial last line. Loading must cut it, so the next append
// starts on a fresh line instead of running into it and losing that minute.
func TestLoadCutsPartialLastLine(t *testing.T) {
	dir := t.TempDir()
	line := `{"id":"m","t":1791158400000,"cpu":40,"n":1}` + "\n"
	path := filepath.Join(dir, "m.ndjson")
	if err := os.WriteFile(path, []byte(line+`{"id":"m","t":17911`), 0o600); err != nil {
		t.Fatal(err)
	}
	now := func() time.Time { return time.UnixMilli(1791158400000 + 3600e3) }
	s, err := Open(dir, now)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(got), "\n") || strings.Contains(string(got), `"t":17911`+"`") {
		t.Fatalf("partial last line kept: %q", got)
	}
	if n := len(s.MinutePoints("m", 0, 1e18)); n != 1 {
		t.Fatalf("want the 1 complete minute kept, got %d", n)
	}
}

// A short write (disk full) must leave no partial row, even when the truncate that undoes it
// also fails, and the failure is reported once per kind, not once per minute (B6).
func TestShortWriteLeavesNoPartialRow(t *testing.T) {
	dir := t.TempDir()
	c := &fakeClock{}
	s := open(t, dir, c)
	defer s.Close()
	add(t, s, c, "m", t0, 10)
	add(t, s, c, "m", t0+minuteMS, 20) // commits minute 0
	path := filepath.Join(dir, "m.ndjson")

	s.write = func(f *os.File, b []byte) (int, error) {
		n, _ := f.Write(b[:len(b)/2])
		return n, syscall.ENOSPC
	}
	c.set(t0 + 2*minuteMS)
	if err := s.Add("m", Point{T: t0 + 2*minuteMS, CPU: ptr(30.0)}); err == nil {
		t.Fatal("first failed write returned no error")
	}
	for i := 3; i < 6; i++ {
		if err := s.Add("m", Point{T: t0 + float64(i)*minuteMS, CPU: ptr(30.0)}); err != nil {
			t.Fatalf("repeat of the same failure returned %v, want it suppressed", err)
		}
	}
	if b, _ := os.ReadFile(path); !strings.HasSuffix(string(b), "\n") || strings.Count(string(b), "\n") != 1 {
		t.Fatalf("partial row left behind: %q", b)
	}

	// Space returns: the failed minutes are written first (N17), each on its own line.
	s.write = func(f *os.File, b []byte) (int, error) { return f.Write(b) }
	if err := s.Add("m", Point{T: t0 + 6*minuteMS, CPU: ptr(40.0)}); err != nil {
		t.Fatal(err)
	}
	if got := len(lines(t, path)); got != 6 {
		t.Fatalf("rows = %d, want minute 0, the 4 failed minutes and the new one", got)
	}
}

// A minute that failed to write is retried on the next commit, so it survives a restart; one
// that keeps failing is given up on after maxTries (skeptic pass 3, N17).
func TestFailedMinuteIsRetried(t *testing.T) {
	dir := t.TempDir()
	c := &fakeClock{}
	s := open(t, dir, c)
	add(t, s, c, "m", t0, 10)
	failing := true
	s.write = func(f *os.File, b []byte) (int, error) {
		if failing {
			return 0, syscall.EIO
		}
		return f.Write(b)
	}
	c.set(t0 + minuteMS)
	if err := s.Add("m", Point{T: t0 + minuteMS, CPU: ptr(20.0)}); err == nil { // commits minute 0: fails
		t.Fatal("the failed write returned no error")
	}
	failing = false
	add(t, s, c, "m", t0+2*minuteMS, 30) // commits minute 1: writes 0 and 1
	if err := s.Close(); err != nil {    // commits minute 2
		t.Fatal(err)
	}
	s2 := open(t, dir, c)
	defer s2.Close()
	if n := len(s2.MinutePoints("m", 0, 1e18)); n != 3 {
		t.Fatalf("minutes after restart = %d, want 3", n)
	}

	// Bounded: the queue never outgrows maxUnwritten.
	s3 := open(t, t.TempDir(), c)
	defer s3.Close()
	s3.write = func(*os.File, []byte) (int, error) { return 0, syscall.EIO }
	for i := 0; i < 2*maxUnwritten; i++ {
		s3.Add("n", Point{T: t0 + float64(i)*minuteMS, CPU: ptr(1.0)})
	}
	if q := s3.unwritten["n"]; q == nil || len(q.pts) != maxUnwritten {
		t.Fatalf("unwritten queue not capped at %d", maxUnwritten)
	}
}

// A partial row with the next row glued on (left by older versions) loses only the partial one.
func TestLoadResyncsAfterGluedRow(t *testing.T) {
	dir := t.TempDir()
	a := `{"id":"m","t":1791158400000,"cpu":40,"n":1}` + "\n"
	glued := `{"id":"m","t":1791158460000,"cpuMax":` + `{"id":"m","t":1791158520000,"cpu":50,"n":1}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "m.ndjson"), []byte(a+glued), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir, func() time.Time { return time.UnixMilli(1791158400000 + 3600e3) })
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if n := len(s.MinutePoints("m", 0, 1e18)); n != 2 {
		t.Fatalf("points = %d, want 2 (the partial row dropped, the glued one kept)", n)
	}
}

// Appends land at the end of the file even when another writer got there between this one's
// open and its write, so two stores never overwrite each other's rows (skeptic pass 3, N14).
func TestAppendLandsAtEndOfFile(t *testing.T) {
	dir := t.TempDir()
	c := &fakeClock{}
	a, b := open(t, dir, c), open(t, dir, c)
	a.write = func(f *os.File, p []byte) (int, error) {
		if err := b.appendLine("m", Point{T: t0 + minuteMS, CPU: ptr(2.0)}); err != nil {
			t.Error(err)
		}
		return f.Write(p)
	}
	if err := a.appendLine("m", Point{T: t0, CPU: ptr(1.0)}); err != nil {
		t.Fatal(err)
	}
	if got := lines(t, filepath.Join(dir, "m.ndjson")); len(got) != 2 {
		t.Fatalf("rows = %q, want both writers' rows", got)
	}
}
