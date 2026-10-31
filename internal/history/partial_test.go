package history

import (
	"os"
	"path/filepath"
	"strings"
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
