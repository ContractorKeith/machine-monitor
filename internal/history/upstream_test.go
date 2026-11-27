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
		var first line
		if err := json.Unmarshal(bytes.SplitN(wb, []byte("\n"), 2)[0], &first); err != nil || first.ID == nil {
			t.Fatalf("fixture ID: %v", err)
		}
		gb, err := os.ReadFile(s.file(*first.ID))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(gb, wb) {
			t.Errorf("%s differs from upstream's", filepath.Base(w))
		}
	}
}

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
	end := ops[len(ops)-1].Now
	s, err := Open(dir, func() time.Time { return time.UnixMilli(end) })
	if err != nil {
		t.Fatal(err)
	}
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
	if r := s.Query("host b/weird:id", last.From, last.To, 1200); len(r.Points) == 0 {
		t.Fatal("no points for host b/weird:id")
	}
	for _, w := range want {
		wb, _ := os.ReadFile(w)
		var first line
		if err := json.Unmarshal(bytes.SplitN(wb, []byte("\n"), 2)[0], &first); err != nil || first.ID == nil {
			t.Fatalf("fixture ID: %v", err)
		}
		gb, _ := os.ReadFile(s.file(*first.ID))
		if !bytes.Equal(gb, wb) {
			t.Errorf("load rewrote %s", filepath.Base(w))
		}
	}
}
