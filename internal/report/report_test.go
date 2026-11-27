package report

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestFixturesRoundTrip(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "*.json"))
	if err != nil || len(files) < 4 {
		t.Fatalf("want 4+ fixtures, got %d (%v)", len(files), err)
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			in, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			var r Report
			dec := json.NewDecoder(bytes.NewReader(in))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&r); err != nil {
				t.Fatalf("decode: %v", err)
			}
			out, err := json.Marshal(&r)
			if err != nil {
				t.Fatal(err)
			}
			want, got := generic(t, in), generic(t, out)
			if !reflect.DeepEqual(want, got) {
				t.Errorf("round trip changed the report\n in: %s\nout: %s", in, out)
			}
		})
	}
}

func TestNullStaysNil(t *testing.T) {
	in, err := os.ReadFile(filepath.Join("testdata", "machine-monitor.json"))
	if err != nil {
		t.Fatal(err)
	}
	var r Report
	if err := json.Unmarshal(in, &r); err != nil {
		t.Fatal(err)
	}
	if r.Memory.Used != nil || r.CPU.Cores[1] != nil || r.Power.CPU != nil || r.Net != nil {
		t.Errorf("null decoded to a value: used=%v core1=%v power=%v net=%v",
			r.Memory.Used, r.CPU.Cores[1], r.Power.CPU, r.Net)
	}
	if r.Memory.Total == nil || *r.Memory.Total != 33285996544 {
		t.Errorf("memory.total = %v", r.Memory.Total)
	}
}

func TestZeroReportEncodesNull(t *testing.T) {
	out, err := json.Marshal(Report{ID: "x", Platform: PlatformLinux})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{`"uptime":null`, `"usage":null`, `"temp":null`, `"total":null`, `"ai":null`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s in %s", want, s)
		}
	}
	for _, absent := range []string{`"token"`, `"processes"`, `"agent"`, `"netIfaces"`} {
		if strings.Contains(s, absent) {
			t.Errorf("unexpected %s in %s", absent, s)
		}
	}
	if Ptr(1.5) == nil || *Ptr("a") != "a" {
		t.Error("Ptr")
	}
}

func generic(t *testing.T, b []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return dropNullKeys(v)
}

func dropNullKeys(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if e == nil {
				delete(x, k)
			} else {
				x[k] = dropNullKeys(e)
			}
		}
	case []any:
		for i, e := range x {
			x[i] = dropNullKeys(e)
		}
	}
	return v
}
