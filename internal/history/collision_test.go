package history

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDistinctDeviceFilesSurviveRestartAndRemoval(t *testing.T) {
	dir, c := t.TempDir(), &fakeClock{}
	s := open(t, dir, c)
	ids := []string{"host.a", "host:a", "host_a"}
	for i, id := range ids {
		add(t, s, c, id, t0+1000, float64(10+i*40))
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = open(t, dir, c)
	for i, id := range ids {
		ps := s.MinutePoints(id, 0, t0+minuteMS-1)
		if len(ps) != 1 || *ps[0].CPU != float64(10+i*40) || ps[0].N != 1 {
			t.Fatalf("%s: %+v", id, ps)
		}
	}
	if err := s.Remove(ids[0]); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = open(t, dir, c)
	defer s.Close()
	if len(s.MinutePoints(ids[0], 0, t0+minuteMS)) != 0 {
		t.Fatal("removed device returned")
	}
	for _, id := range ids[1:] {
		if len(s.MinutePoints(id, 0, t0+minuteMS-1)) != 1 {
			t.Fatalf("removal lost %s", id)
		}
	}
}

func TestDeviceFilenamesPortableAndDistinct(t *testing.T) {
	seen := map[string]string{}
	for _, id := range []string{"host.a", "host:a", "host_a", "host~a", "Host", "host", "con", "CON", "aux", "nul", "prn", "com1", "lpt9", "../x"} {
		name := strings.ToLower(safeName(id))
		if old, ok := seen[name]; ok {
			t.Fatalf("%q and %q share %q", old, id, name)
		}
		seen[name] = id
		if strings.ContainsAny(name, "/\\:") || name == "con" || name == "aux" || name == "nul" || name == "prn" || name == "com1" || name == "lpt9" {
			t.Fatalf("not portable: %q", name)
		}
	}
}

// Interrupted migration must leave the mixed source intact until other IDs are safe.
func TestCollisionMigrationFailureIsRetryable(t *testing.T) {
	for _, live := range []bool{false, true} {
		t.Run(fmt.Sprintf("continue-reports=%v", live), func(t *testing.T) { testCollisionMigrationRetry(t, live) })
	}
}

func testCollisionMigrationRetry(t *testing.T, live bool) {
	dir, c := t.TempDir(), &fakeClock{}
	c.set(t0 + 1000)
	legacy := filepath.Join(dir, "host_a.ndjson")
	data := append(encodeLine("host_a", Point{T: t0, CPU: ptr(50), N: 1}), encodeLine("host.a", Point{T: t0, CPU: ptr(10), N: 1})...)
	data = append(data, encodeLine("host:a", Point{T: t0, CPU: ptr(90), N: 1})...)
	if err := os.WriteFile(legacy, data, 0o600); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(dir, safeName("host:a")+".ndjson")
	if err := os.Mkdir(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir, c.now)
	if s == nil || err == nil {
		t.Fatal("expected partial migration error")
	}
	if b, _ := os.ReadFile(legacy); string(b) != string(data) {
		t.Fatal("failed migration destroyed source")
	}
	if live {
		if err := s.Add("host_a", Point{T: t0 + minuteMS, CPU: ptr(20)}); err != nil {
			t.Fatal(err)
		}
		if err := s.Add("host_a", Point{T: t0 + 2*minuteMS, CPU: ptr(30)}); err == nil {
			t.Fatal("expected blocked migration error")
		}
		if b, _ := os.ReadFile(legacy); string(b) != string(data) {
			t.Fatal("continued reports destroyed a peer's source")
		}
	}
	// Do not Close: simulate the process ending immediately after the failed migration.
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	if live {
		add(t, s, c, "host_a", t0+3*minuteMS, 40)
		add(t, s, c, "host:a", t0+minuteMS, 20)
		add(t, s, c, "host:a", t0+2*minuteMS, 30)
		snap, err := ReadDir(dir, c.now)
		if err != nil {
			t.Fatal(err)
		}
		ps := snap.MinutePoints("host:a", 0, t0+3*minuteMS)
		if len(ps) != 2 || *ps[0].CPU != 90 || *ps[1].CPU != 20 {
			t.Fatalf("migration retry lost loaded history: %+v", ps)
		}
	}
	s = open(t, dir, c)
	defer s.Close()
	for id, want := range map[string]float64{"host_a": 50, "host.a": 10, "host:a": 90} {
		ps := s.MinutePoints(id, 0, t0+minuteMS-1)
		if len(ps) != 1 || *ps[0].CPU != want || ps[0].N != 1 {
			t.Fatalf("retried %s: %+v", id, ps)
		}
	}
}

func TestRemoveAfterFailedMigrationDoesNotResurrectDevice(t *testing.T) {
	dir, c := t.TempDir(), &fakeClock{}
	c.set(t0 + 1000)
	legacy := filepath.Join(dir, "host_a.ndjson")
	data := append(encodeLine("host.a", Point{T: t0, CPU: ptr(10), N: 1}), encodeLine("host:a", Point{T: t0, CPU: ptr(90), N: 1})...)
	if err := os.WriteFile(legacy, data, 0o600); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(dir, safeName("host:a")+".ndjson")
	if err := os.Mkdir(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir, c.now)
	if err == nil || s == nil {
		t.Fatal("expected migration failure")
	}
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove("host.a"); err != nil {
		t.Fatal(err)
	}
	s = open(t, dir, c)
	defer s.Close()
	if len(s.MinutePoints("host.a", 0, t0+minuteMS)) != 0 {
		t.Fatal("removed device resurrected")
	}
	ps := s.MinutePoints("host:a", 0, t0+minuteMS)
	if len(ps) != 1 || *ps[0].CPU != 90 {
		t.Fatal("removal lost peer history")
	}
}

func TestLegacyCollisionFileMigratesWithoutMixingDevices(t *testing.T) {
	dir, c := t.TempDir(), &fakeClock{}
	c.set(t0 + 1000)
	path := filepath.Join(dir, "host_a.ndjson")
	data := append(encodeLine("host.a", Point{T: t0, CPU: ptr(10), N: 1}), encodeLine("host:a", Point{T: t0, CPU: ptr(90), N: 1})...)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	snap, err := ReadDir(dir, c.now)
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]float64{"host.a": 10, "host:a": 90} {
		ps := snap.MinutePoints(id, 0, t0+minuteMS)
		if len(ps) != 1 || *ps[0].CPU != want {
			t.Fatalf("read-only %s: %+v", id, ps)
		}
	}
	if b, _ := os.ReadFile(path); string(b) != string(data) {
		t.Fatal("snapshot changed legacy file")
	}
	for range 2 {
		s := open(t, dir, c)
		for id, want := range map[string]float64{"host.a": 10, "host:a": 90} {
			ps := s.MinutePoints(id, 0, t0+minuteMS-1)
			if len(ps) != 1 || *ps[0].CPU != want || ps[0].N != 1 {
				t.Fatalf("migrated %s: %+v", id, ps)
			}
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
