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

	// Space returns: the next row lands on its own line.
	s.write = func(f *os.File, b []byte) (int, error) { return f.Write(b) }
	if err := s.Add("m", Point{T: t0 + 6*minuteMS, CPU: ptr(40.0)}); err != nil {
		t.Fatal(err)
	}
	if got := len(lines(t, path)); got != 2 {
		t.Fatalf("rows = %d, want 2", got)
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
