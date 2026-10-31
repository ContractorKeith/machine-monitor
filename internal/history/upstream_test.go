package history

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// The fixtures in testdata/upstream were written by upstream's history.js running
// testdata/gen-upstream.js (see the header there for how to regenerate them).

// op is one line of testdata/upstream/ops.ndjson.
type op struct {
	Op     string          `json:"op"`
	Now    int64           `json:"now"`
	ID     string          `json:"id"`
	Sample Point           `json:"sample"`
	From   float64         `json:"from"`
	To     float64         `json:"to"`
	Points float64         `json:"points"`
	MS     int64           `json:"ms"`
	Want   json.RawMessage `json:"want"`
}

func readOps(t *testing.T) []op {
	t.Helper()
	f, err := os.Open("testdata/upstream/ops.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var ops []op
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<24)
	for sc.Scan() {
		var o op
		if err := json.Unmarshal(sc.Bytes(), &o); err != nil {
			t.Fatal(err)
		}
		ops = append(ops, o)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return ops
}

// sameJSON compares two JSON documents, treating a null and a missing key as equal (upstream
// raw samples have no *Max keys; ours encode them as null).
func sameJSON(t *testing.T, got any, want json.RawMessage) bool {
	t.Helper()
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var g, w any
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatal(err)
	}
	return reflect.DeepEqual(dropNulls(g), dropNulls(w))
}

func dropNulls(v any) any {
	switch v := v.(type) {
	case map[string]any:
		for k, x := range v {
			if x == nil {
				delete(v, k)
			} else {
				v[k] = dropNulls(x)
			}
		}
	case []any:
		for i := range v {
			v[i] = dropNulls(v[i])
		}
	}
	return v
}

// TestMatchesUpstream replays every call upstream's history.js saw, checks each query
// answer against upstream's, then checks the files left on disk are byte-identical.
func TestMatchesUpstream(t *testing.T) {
	dir := t.TempDir()
	var now int64
	clock := func() time.Time { return time.UnixMilli(now) }
	var s *Store
	queries := 0
	for i, o := range readOps(t) {
		now = o.Now
		var got any
		switch o.Op {
		case "open":
			var err error
			if s, err = Open(dir, clock); err != nil {
				t.Fatal(err)
			}
		case "close":
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
		case "add":
			if err := s.Add(o.ID, o.Sample); err != nil {
				t.Fatal(err)
			}
		case "remove":
			if err := s.Remove(o.ID); err != nil {
				t.Fatal(err)
			}
		case "query":
			got = s.Query(o.ID, o.From, o.To, o.Points)
		case "minutePoints":
			got = s.MinutePoints(o.ID, o.From, o.To)
		case "recent":
			got = s.Recent(o.ID, time.Duration(o.MS)*time.Millisecond)
		default:
			t.Fatalf("op %d: unknown op %q", i, o.Op)
		}
		if o.Want != nil {
			queries++
			if got == nil || reflect.ValueOf(got).Kind() == reflect.Slice && reflect.ValueOf(got).Len() == 0 {
				got = []Point{}
			}
			if !sameJSON(t, got, o.Want) {
				b, _ := json.Marshal(got)
				t.Fatalf("op %d %s(%s, %v..%v, %v):\n got %.600s\nwant %.600s", i, o.Op, o.ID, o.From, o.To, o.Points, b, o.Want)
			}
		}
	}
	if queries < 20 {
		t.Fatalf("only %d answers checked", queries)
	}

	want, _ := filepath.Glob("testdata/upstream/history/*.ndjson")
	got, _ := filepath.Glob(filepath.Join(dir, "*.ndjson"))
	if len(want) == 0 || len(got) != len(want) {
		t.Fatalf("files: got %v, want %v", got, want)
	}
	for _, w := range want {
		wb, _ := os.ReadFile(w)
		gb, err := os.ReadFile(filepath.Join(dir, filepath.Base(w)))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(gb, wb) {
			t.Errorf("%s differs from upstream's", filepath.Base(w))
		}
	}
}

// TestReadsUpstreamFiles opens the directory upstream wrote and serves the same history.
func TestReadsUpstreamFiles(t *testing.T) {
	dir := t.TempDir()
	want, _ := filepath.Glob("testdata/upstream/history/*.ndjson")
	for _, w := range want {
		b, _ := os.ReadFile(w)
		if err := os.WriteFile(filepath.Join(dir, filepath.Base(w)), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ops := readOps(t)
	end := ops[len(ops)-1].Now // upstream's final close
	s, err := Open(dir, func() time.Time { return time.UnixMilli(end) })
	if err != nil {
		t.Fatal(err)
	}
	// Upstream's last query covered the last 30 days of machine-a just before that close.
	var last op
	for _, o := range ops {
		if o.Op == "query" && o.ID == "machine-a" {
			last = o
		}
	}
	got := s.Query("machine-a", last.From, last.To, last.Points)
	var w Result
	if err := json.Unmarshal(last.Want, &w); err != nil {
		t.Fatal(err)
	}
	// The live, unfinished minute was flushed at close, so it now comes from the file with a
	// count; everything else must match exactly.
	if len(got.Points) != len(w.Points) || len(got.Points) < 20 {
		t.Fatalf("got %d points, want %d", len(got.Points), len(w.Points))
	}
	for i := range w.Points {
		g := got.Points[i]
		g.N = w.Points[i].N
		if !reflect.DeepEqual(g, w.Points[i]) {
			t.Fatalf("point %d: got %+v, want %+v", i, got.Points[i], w.Points[i])
		}
	}
	// The odd id survives the trip through its safe file name.
	if r := s.Query("host b/weird:id", last.From, last.To, 1200); len(r.Points) == 0 {
		t.Fatal("no points for host b/weird:id")
	}
	// Nothing was expired or duplicated, so load left the files alone.
	for _, w := range want {
		wb, _ := os.ReadFile(w)
		gb, _ := os.ReadFile(filepath.Join(dir, filepath.Base(w)))
		if !bytes.Equal(gb, wb) {
			t.Errorf("load rewrote %s", filepath.Base(w))
		}
	}
}
