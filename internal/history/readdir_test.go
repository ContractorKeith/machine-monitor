package history

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"
)

func snapshotDir(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = string(b)
	}
	return out
}

func TestReadDirLeavesFilesAlone(t *testing.T) {
	const base = 1791158400000
	now := func() time.Time { return time.UnixMilli(base + 3600e3) }
	old := base - 31*dayMS
	files := map[string]string{
		"dup.ndjson": `{"id":"dup","t":` + itoa(old) + `,"cpu":1}` + "\n" +
			`{"id":"dup","t":` + itoa(base) + `,"cpu":10,"n":1}` + "\n" +
			`{"id":"dup","t":` + itoa(base+minuteMS) + `,"cpu":20,"n":1}` + "\n" +
			`{"id":"dup","t":` + itoa(base+minuteMS) + `,"cpu":40,"n":1}` + "\n",
		"part.ndjson": `{"id":"part","t":` + itoa(base) + `,"cpu":5,"n":1}` + "\n" + `{"id":"part","t":17911`,
		"gone.ndjson": `{"id":"gone","t":` + itoa(old) + `,"cpu":1}` + "\n",
		"anon.ndjson": `{"t":` + itoa(base) + `,"cpu":7}` + "\n",
		"notes.txt":   "not history\n",
	}
	readOnly, readWrite := t.TempDir(), t.TempDir()
	for _, dir := range []string{readOnly, readWrite} {
		for name, body := range files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}

	before := snapshotDir(t, readOnly)
	snap, err := ReadDir(readOnly, now)
	if err != nil {
		t.Fatal(err)
	}
	if after := snapshotDir(t, readOnly); !reflect.DeepEqual(after, before) {
		t.Errorf("ReadDir changed the directory:\nbefore %q\nafter  %q", before, after)
	}

	st, err := Open(readWrite, now)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"anon", "dup", "part"}; !slices.Equal(snap.IDs(), want) {
		t.Errorf("IDs() = %q, want %q", snap.IDs(), want)
	}
	for _, id := range []string{"anon", "dup", "part", "gone"} {
		got, want := snap.MinutePoints(id, 0, 1e18), st.MinutePoints(id, 0, 1e18)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: ReadDir minutes %+v, Open minutes %+v", id, got, want)
		}
		if snap.Has(id) != (len(want) > 0) {
			t.Errorf("%s: Has = %v with %d minutes", id, snap.Has(id), len(want))
		}
	}
	if n := len(snap.MinutePoints("dup", 0, 1e18)); n != 2 {
		t.Errorf("dup: %d minutes, want 2 (expired row dropped, duplicate merged)", n)
	}
	if reflect.DeepEqual(snapshotDir(t, readWrite), before) {
		t.Error("Open left its copy unchanged; the fixture no longer exercises rewrites")
	}
}

func TestReadDirMissing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "history")
	snap, err := ReadDir(dir, nil)
	if err != nil || len(snap.IDs()) != 0 {
		t.Fatalf("ReadDir(missing) = %v, %v; want an empty snapshot", snap.IDs(), err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("ReadDir created %s", dir)
	}
}

func itoa(ms int) string { return strconv.Itoa(ms) }
