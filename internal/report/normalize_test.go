package report

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestParity runs every case upstream security.js was run on (testdata/parity/gen.js) and
// checks Normalize produces the same report and processes, key for key.
func TestParity(t *testing.T) {
	in, err := os.ReadFile(filepath.Join("testdata", "parity", "cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name string
		Body string
		Want struct {
			Error     string
			Report    json.RawMessage
			Processes json.RawMessage
		}
	}
	if err := json.Unmarshal(in, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 100 {
		t.Fatalf("only %d parity cases", len(cases))
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			r, err := Normalize([]byte(c.Body))
			if c.Want.Error != "" {
				if err == nil {
					t.Fatalf("upstream rejects (%s), Normalize accepted", c.Want.Error)
				}
				return
			}
			if err != nil {
				t.Fatalf("Normalize: %v", err)
			}
			procs := r.Processes
			r.Processes, r.Agent, r.NetIfaces = nil, "", nil // not in upstream's report
			gotProcs, err := json.Marshal(procs)
			if err != nil {
				t.Fatal(err)
			}
			gotReport, err := json.Marshal(&r)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(plain(t, c.Want.Report), plain(t, gotReport)) {
				t.Errorf("report differs\nbody: %s\nwant: %s\n got: %s", c.Body, c.Want.Report, gotReport)
			}
			if !reflect.DeepEqual(plain(t, c.Want.Processes), plain(t, gotProcs)) {
				t.Errorf("processes differ\nwant: %s\n got: %s", c.Want.Processes, gotProcs)
			}
		})
	}
}

func plain(t *testing.T, b []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	return v
}

// norm normalizes a report body built from fields, with a valid id added.
func norm(t *testing.T, fields map[string]any) Report {
	t.Helper()
	body := map[string]any{"id": "dev1"}
	for k, v := range fields {
		body[k] = v
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Normalize(b)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestNormalizeRejects(t *testing.T) {
	for _, c := range []struct {
		body string
		want error
	}{
		{`null`, ErrNotObject},
		{`[]`, ErrNotObject},
		{`"x"`, ErrNotObject},
		{`{}`, ErrInvalidID},
		{`{"id":5}`, ErrInvalidID},
		{`{"id":"-x"}`, ErrInvalidID},
		{`{"id":"x y"}`, ErrInvalidID},
		{`{"id":"` + strings.Repeat("a", 65) + `"}`, ErrInvalidID},
		{`{"id":"x"} {}`, ErrTrailingData},
		{`{"id":"x"}]`, ErrTrailingData},
		{`{"id":"x"`, nil}, // syntax error, not a sentinel
		{``, nil},
	} {
		_, err := Normalize([]byte(c.body))
		if err == nil || (c.want != nil && !errors.Is(err, c.want)) {
			t.Errorf("Normalize(%q) err = %v, want %v", c.body, err, c.want)
		}
	}
}

func TestValidID(t *testing.T) {
	for id, want := range map[string]bool{
		"a": true, "4c4c4544-004e-3510-804a-b4c04f4d4e32": true, "A1:b.c_d-e": true,
		strings.Repeat("a", 64): true, strings.Repeat("a", 65): false,
		"": false, ".a": false, "_a": false, "a b": false, "a\n": false, "a/b": false, "é": false,
	} {
		if got := ValidID(id); got != want {
			t.Errorf("ValidID(%q) = %v", id, got)
		}
	}
}

func TestValidAction(t *testing.T) {
	for a, want := range map[string]bool{"sleep": true, "restart": true, "shutdown": true, "Sleep": false, "hibernate": false, "": false} {
		if ValidAction(a) != want {
			t.Errorf("ValidAction(%q) != %v", a, want)
		}
	}
}

// numField maps a field path to how to read it back, its upstream range and a body for it.
type numField struct {
	name   string
	lo, hi float64
	body   func(v any) map[string]any
	get    func(Report) *float64
}

func numFields() []numField {
	in := func(k string, sub string) func(v any) map[string]any {
		return func(v any) map[string]any { return map[string]any{k: map[string]any{sub: v}} }
	}
	row := func(k, sub string) func(v any) map[string]any {
		return func(v any) map[string]any {
			m := map[string]any{"name": "n", "pid": 1, "app": "a"}
			m[sub] = v
			return map[string]any{k: []any{m}}
		}
	}
	// processes.pid is in TestProcesses: a nil pid drops the row.
	return []numField{
		{"uptime", 0, 1e10, func(v any) map[string]any { return map[string]any{"uptime": v} }, func(r Report) *float64 { return r.Uptime }},
		{"power.cpu", 0, 5000, in("power", "cpu"), func(r Report) *float64 { return r.Power.CPU }},
		{"cpu.usage", 0, 100, in("cpu", "usage"), func(r Report) *float64 { return r.CPU.Usage }},
		{"cpu.threads", 0, 1e5, in("cpu", "threads"), func(r Report) *float64 { return r.CPU.Threads }},
		{"cpu.clockMHz", 0, 1e5, in("cpu", "clockMHz"), func(r Report) *float64 { return r.CPU.ClockMHz }},
		{"cpu.temp", -60, 250, in("cpu", "temp"), func(r Report) *float64 { return r.CPU.Temp }},
		{"cpu.cores[0]", 0, 100, func(v any) map[string]any { return map[string]any{"cpu": map[string]any{"cores": []any{v}}} }, func(r Report) *float64 { return r.CPU.Cores[0] }},
		{"memory.total", 0, 1e18, in("memory", "total"), func(r Report) *float64 { return r.Memory.Total }},
		{"memory.used", 0, 1e18, in("memory", "used"), func(r Report) *float64 { return r.Memory.Used }},
		{"gpus.usage", 0, 100, row("gpus", "usage"), func(r Report) *float64 { return r.GPUs[0].Usage }},
		{"gpus.temp", -60, 250, row("gpus", "temp"), func(r Report) *float64 { return r.GPUs[0].Temp }},
		{"gpus.memUsed", 0, 1e18, row("gpus", "memUsed"), func(r Report) *float64 { return r.GPUs[0].MemUsed }},
		{"gpus.memTotal", 0, 1e18, row("gpus", "memTotal"), func(r Report) *float64 { return r.GPUs[0].MemTotal }},
		{"gpus.power", 0, 1e5, row("gpus", "power"), func(r Report) *float64 { return r.GPUs[0].Power }},
		{"gpus.powerLimit", 0, 1e5, row("gpus", "powerLimit"), func(r Report) *float64 { return r.GPUs[0].PowerLimit }},
		{"gpus.fan", 0, 100, row("gpus", "fan"), func(r Report) *float64 { return r.GPUs[0].Fan }},
		{"gpus.clockMHz", 0, 1e5, row("gpus", "clockMHz"), func(r Report) *float64 { return r.GPUs[0].ClockMHz }},
		{"disks.total", 0, 1e18, row("disks", "total"), func(r Report) *float64 { return r.Disks[0].Total }},
		{"disks.used", 0, 1e18, row("disks", "used"), func(r Report) *float64 { return r.Disks[0].Used }},
		{"net.rx", 0, 1e18, in("net", "rx"), func(r Report) *float64 { return r.Net.RX }},
		{"net.tx", 0, 1e18, in("net", "tx"), func(r Report) *float64 { return r.Net.TX }},
		{"diskIO.read", 0, 1e18, in("diskIO", "read"), func(r Report) *float64 { return r.DiskIO.Read }},
		{"diskIO.write", 0, 1e18, in("diskIO", "write"), func(r Report) *float64 { return r.DiskIO.Write }},
		{"temps.value", -60, 250, row("temps", "value"), func(r Report) *float64 { return r.Temps[0].Value }},
		{"fans.rpm", 0, 1e5, row("fans", "rpm"), func(r Report) *float64 { return r.Fans[0].RPM }},
		{"ai.mem", 0, 1e18, row("ai", "mem"), func(r Report) *float64 { return r.AI[0].Mem }},
		{"ai.models.size", 0, 1e18, func(v any) map[string]any {
			return map[string]any{"ai": []any{map[string]any{"app": "a", "models": []any{map[string]any{"name": "m", "size": v}}}}}
		}, func(r Report) *float64 { return r.AI[0].Models[0].Size }},
		{"processes.cpu", 0, 100, row("processes", "cpu"), func(r Report) *float64 { return r.Processes[0].CPU }},
		{"processes.mem", 0, 1e18, row("processes", "mem"), func(r Report) *float64 { return r.Processes[0].Mem }},
		{"processes.gpu", 0, 100, row("processes", "gpu"), func(r Report) *float64 { return r.Processes[0].GPU }},
		{"processes.gpuMem", 0, 1e18, row("processes", "gpuMem"), func(r Report) *float64 { return r.Processes[0].GPUMem }},
		{"netIfaces.rx", 0, 1e18, row("netIfaces", "rx"), func(r Report) *float64 { return r.NetIfaces[0].RX }},
		{"netIfaces.tx", 0, 1e18, row("netIfaces", "tx"), func(r Report) *float64 { return r.NetIfaces[0].TX }},
	}
}

// Every numeric field clamps to its upstream range, accepts numeric strings and turns
// anything non-numeric into nil.
func TestNumberClamps(t *testing.T) {
	for _, f := range numFields() {
		mid := (f.lo + math.Min(f.hi, 100)) / 2
		for _, c := range []struct {
			in   any
			want *float64
		}{
			{mid, Ptr(mid)},
			{f.lo - 1, Ptr(f.lo)},
			{f.hi * 10, Ptr(f.hi)},
			{f.lo, Ptr(f.lo)},
			{f.hi, Ptr(f.hi)},
			{"50", Ptr(math.Min(f.hi, 50))},
			{" 1e1 ", Ptr(10.0)},
			{nil, nil},
			{"", nil},
			{"abc", nil},
			{true, nil},
			{[]any{5}, nil},
		} {
			r := norm(t, f.body(c.in))
			got := f.get(r)
			if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
				t.Errorf("%s(%#v) = %v, want %v", f.name, c.in, fmtp(got), fmtp(c.want))
			}
		}
	}
}

func fmtp[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

// JS Number() parsing of strings, through num.
func TestNumberStrings(t *testing.T) {
	nan := math.NaN()
	for s, want := range map[string]float64{
		"42": 42, " 42 ": 42, "\u00a042\ufeff": 42, "\u2003 7 \u3000": 7, "+3": 3, "-2": -2,
		".5": 0.5, "5.": 5, "1e3": 1000, "1E+3": 1000, "1e-400": 0, "00012": 12,
		"0x1A": 26, "0X1a": 26, "0b101": 5, "0B11": 3, "0o17": 15, "0O7": 7,
		"Infinity": math.Inf(1), "-Infinity": math.Inf(-1), "1e400": math.Inf(1),
		"0x": nan, "-0x1": nan, "+0x1": nan, "0x1_0": nan, "1_0": nan, "0x1.8p1": nan, "0b2": nan,
		"inf": nan, "NaN": nan, "nan": nan, "1e": nan, "e1": nan, ".": nan, "1.2.3": nan, "12px": nan,
		"\u008512": nan, "١٢": nan, "1 2": nan,
	} {
		got := jsNumber(s)
		if !(got == want || math.IsNaN(got) && math.IsNaN(want)) {
			t.Errorf("jsNumber(%q) = %v, want %v", s, got, want)
		}
	}
	if f := jsNumber("0x" + strings.Repeat("f", 300)); !math.IsInf(f, 1) {
		t.Errorf("huge hex = %v", f)
	}
	if f := jsNumber("0x" + strings.Repeat("f", 16)); f != 18446744073709551615 {
		t.Errorf("64-bit hex = %v", f)
	}
}

// Non-finite JSON numbers (1e999) are nil, and -0 encodes as 0 like JSON.stringify.
func TestNumberEdges(t *testing.T) {
	r, err := Normalize([]byte(`{"id":"d","uptime":1e999,"cpu":{"temp":-0,"usage":-1e999},"memory":{"total":1e-999}}`))
	if err != nil {
		t.Fatal(err)
	}
	if r.Uptime != nil || r.CPU.Usage != nil {
		t.Errorf("non-finite kept: uptime=%v usage=%v", fmtp(r.Uptime), fmtp(r.CPU.Usage))
	}
	if r.Memory.Total == nil || *r.Memory.Total != 0 {
		t.Errorf("underflow = %v", fmtp(r.Memory.Total))
	}
	out, _ := json.Marshal(r.CPU)
	if !strings.Contains(string(out), `"temp":0,`) {
		t.Errorf("-0 encoding: %s", out)
	}
}

// strField is one string field, its upstream limit, a body for it and how to read it back.
type strField struct {
	name  string
	limit int
	body  func(v any) map[string]any
	get   func(Report) *string
}

func strFields() []strField {
	in := func(k, sub string) func(v any) map[string]any {
		return func(v any) map[string]any { return map[string]any{k: map[string]any{sub: v, "id": "0a1b2c3d4e5f"}} }
	}
	top := func(k string) func(v any) map[string]any {
		return func(v any) map[string]any { return map[string]any{k: v} }
	}
	row := func(k, sub string) func(v any) map[string]any {
		return func(v any) map[string]any {
			m := map[string]any{"name": "n", "pid": 1, "app": "a"}
			m[sub] = v
			return map[string]any{k: []any{m}}
		}
	}
	return []strField{
		{"hostname", 64, top("hostname"), func(r Report) *string { return r.Hostname }},
		{"os", 120, top("os"), func(r Report) *string { return r.OS }},
		{"actionResult.message", 200, in("actionResult", "message"), func(r Report) *string { return r.ActionResult.Message }},
		{"cpu.name", 120, in("cpu", "name"), func(r Report) *string { return r.CPU.Name }},
		{"gpus.name", 120, row("gpus", "name"), func(r Report) *string { return r.GPUs[0].Name }},
		{"gpus.vendor", 20, row("gpus", "vendor"), func(r Report) *string { return r.GPUs[0].Vendor }},
		{"disks.name", 80, row("disks", "name"), func(r Report) *string { return r.Disks[0].Name }},
		{"disks.label", 80, row("disks", "label"), func(r Report) *string { return r.Disks[0].Label }},
		{"temps.name", 80, row("temps", "name"), func(r Report) *string { return r.Temps[0].Name }},
		{"temps.parent", 120, row("temps", "parent"), func(r Report) *string { return r.Temps[0].Parent }},
		{"fans.name", 80, row("fans", "name"), func(r Report) *string { return r.Fans[0].Name }},
		{"ai.app", 40, row("ai", "app"), func(r Report) *string { return r.AI[0].App }},
		{"ai.models.name", 200, func(v any) map[string]any {
			return map[string]any{"ai": []any{map[string]any{"app": "a", "models": []any{map[string]any{"name": v}}}}}
		}, func(r Report) *string { return r.AI[0].Models[0].Name }},
		{"ai.models.kind", 30, func(v any) map[string]any {
			return map[string]any{"ai": []any{map[string]any{"app": "a", "models": []any{map[string]any{"name": "m", "kind": v}}}}}
		}, func(r Report) *string { return r.AI[0].Models[0].Kind }},
		{"processes.name", 80, row("processes", "name"), func(r Report) *string { return r.Processes[0].Name }},
		{"processes.cmd", 160, row("processes", "cmd"), func(r Report) *string { return r.Processes[0].Cmd }},
		{"processes.user", 40, row("processes", "user"), func(r Report) *string { return r.Processes[0].User }},
		{"netIfaces.name", 80, row("netIfaces", "name"), func(r Report) *string { return r.NetIfaces[0].Name }},
	}
}

// Every string field strips control characters, trims, coerces non-strings like JS
// String() and cuts at its upstream limit. Rows that need the field (ai.app, models.name,
// processes.name) are dropped when it comes out nil, so only non-nil cases are read back.
func TestStringFields(t *testing.T) {
	for _, f := range strFields() {
		long := strings.Repeat("x", f.limit+10)
		for _, c := range []struct {
			in   any
			want string
		}{
			{"plain", "plain"},
			{"  pad\t\n", "pad"},
			{"a\x00b\x1fc\x7fd", "abcd"},
			{"\u00a0\ufeffnbsp\u3000", "nbsp"},
			{long, long[:f.limit]},
			{12.5, "12.5"},
			{true, "true"},
			{[]any{1, "a", nil}, "1,a,"},
			{map[string]any{"a": 1}, "[object Object]"},
		} {
			got := f.get(norm(t, f.body(c.in)))
			if got == nil || *got != c.want {
				t.Errorf("%s(%#v) = %v, want %q", f.name, c.in, fmtp(got), c.want)
			}
		}
		for _, empty := range []any{nil, "", " \t ", "\x00\x01"} {
			r := norm(t, f.body(empty))
			switch f.name {
			case "ai.app":
				if len(r.AI) != 0 {
					t.Errorf("%s(%q): group kept", f.name, empty)
				}
			case "ai.models.name":
				if len(r.AI[0].Models) != 0 {
					t.Errorf("%s(%q): model kept", f.name, empty)
				}
			case "processes.name":
				if len(r.Processes) != 0 {
					t.Errorf("%s(%q): process kept", f.name, empty)
				}
			default:
				if got := f.get(r); got != nil {
					t.Errorf("%s(%q) = %q, want nil", f.name, empty, *got)
				}
			}
		}
	}
}

// Limits count UTF-16 code units like JS slice; a surrogate pair is never split.
func TestStringUTF16Cut(t *testing.T) {
	for _, c := range []struct {
		in    string
		limit int
		want  string
	}{
		{strings.Repeat("é", 70), 64, strings.Repeat("é", 64)},             // BMP: one unit each
		{strings.Repeat("😀", 40), 64, strings.Repeat("😀", 32)},             // two units each
		{"a" + strings.Repeat("😀", 40), 64, "a" + strings.Repeat("😀", 31)}, // pair at the cut is dropped
		{"\xff\xfeab", 64, "\ufffd\ufffdab"},                               // invalid UTF-8 → U+FFFD
	} {
		got := str(c.in, c.limit)
		if got == nil || *got != c.want || !utf8.ValidString(*got) {
			t.Errorf("str(%q, %d) = %v, want %q", c.in, c.limit, fmtp(got), c.want)
		}
	}
	// JSON with a lone surrogate decodes to U+FFFD and stays valid.
	r, err := Normalize([]byte(`{"id":"d","hostname":"a\ud800b"}`))
	if err != nil || r.Hostname == nil || *r.Hostname != "a\ufffdb" {
		t.Errorf("lone surrogate: %v %v", fmtp(r.Hostname), err)
	}
	if CleanText(" a\x00b ", 60) == nil || *CleanText(" a\x00b ", 60) != "ab" || CleanText(nil, 60) != nil {
		t.Error("CleanText")
	}
}

func TestJSNumberString(t *testing.T) {
	for f, want := range map[float64]string{
		0: "0", math.Copysign(0, -1): "0", 1: "1", -1.5: "-1.5", 0.1: "0.1", 123.456: "123.456",
		1e21: "1e+21", 1.5e300: "1.5e+300", 1e-7: "1e-7", 0.000001: "0.000001", 1e20: "100000000000000000000",
		9007199254740994: "9007199254740994", math.Inf(1): "Infinity", math.Inf(-1): "-Infinity",
	} {
		if got := jsNumberString(f); got != want {
			t.Errorf("jsNumberString(%v) = %q, want %q", f, got, want)
		}
	}
}

func TestEnums(t *testing.T) {
	for in, want := range map[any]string{"linux": "linux", "macos": "macos", "windows": "windows", "Linux": "unknown", "darwin": "unknown", 1.0: "unknown"} {
		if r := norm(t, map[string]any{"platform": in}); r.Platform != want {
			t.Errorf("platform %v = %q", in, r.Platform)
		}
	}
	if r := norm(t, nil); r.Platform != PlatformUnknown {
		t.Errorf("missing platform = %q", r.Platform)
	}
	for in, want := range map[any]any{"laptop": "laptop", "desktop": "desktop", "Laptop": nil, "tablet": nil, true: nil} {
		if got := fmtp(norm(t, map[string]any{"formFactor": in}).FormFactor); got != want {
			t.Errorf("formFactor %v = %v", in, got)
		}
	}
	for in, want := range map[any]any{"acpi": "acpi", "ACPI": nil, "smc": nil, 1.0: nil} {
		if got := fmtp(norm(t, map[string]any{"cpu": map[string]any{"tempSource": in}}).CPU.TempSource); got != want {
			t.Errorf("tempSource %v = %v", in, got)
		}
	}
}

func TestLists(t *testing.T) {
	r := norm(t, map[string]any{
		"features": []any{"procs", "x", "actions", 1, nil, "procs", "a", "b", "actions"},
		"actions":  []any{"sleep", "reboot", "restart", "shutdown", "SLEEP", "sleep", "x", "y", "shutdown"},
	})
	if want := []string{"procs", "actions", "procs"}; !reflect.DeepEqual(r.Features, want) {
		t.Errorf("features = %q, want %q (first 8 only)", r.Features, want)
	}
	if want := []string{"sleep", "restart", "shutdown", "sleep"}; !reflect.DeepEqual(r.Actions, want) {
		t.Errorf("actions = %q, want %q", r.Actions, want)
	}
	// Non-arrays become empty lists, never nil, so they encode as [] like upstream.
	r = norm(t, map[string]any{"features": "procs", "actions": map[string]any{"0": "sleep"}, "macs": 5, "gpus": "x", "disks": nil})
	for name, l := range map[string]any{"features": r.Features, "actions": r.Actions, "macs": r.MACs, "gpus": r.GPUs, "disks": r.Disks, "temps": r.Temps, "fans": r.Fans, "cores": r.CPU.Cores} {
		if v := reflect.ValueOf(l); v.IsNil() || v.Len() != 0 {
			t.Errorf("%s = %#v, want empty non-nil", name, l)
		}
	}
	// Length limits apply before filtering non-objects.
	for _, c := range []struct {
		key   string
		limit int
		count func(Report) int
	}{
		{"gpus", 32, func(r Report) int { return len(r.GPUs) }},
		{"disks", 64, func(r Report) int { return len(r.Disks) }},
		{"temps", 256, func(r Report) int { return len(r.Temps) }},
		{"fans", 64, func(r Report) int { return len(r.Fans) }},
		{"ai", 8, func(r Report) int { return len(r.AI) }},
		{"processes", 120, func(r Report) int { return len(r.Processes) }},
		{"netIfaces", 64, func(r Report) int { return len(r.NetIfaces) }},
	} {
		rows := []any{"not an object"}
		for i := 0; i < c.limit+10; i++ {
			rows = append(rows, map[string]any{"name": "n", "app": "a", "pid": i})
		}
		if got := c.count(norm(t, map[string]any{c.key: rows})); got != c.limit-1 {
			t.Errorf("%s: %d rows, want %d", c.key, got, c.limit-1)
		}
	}
	cores := make([]any, 1100)
	cores[3] = 50
	r = norm(t, map[string]any{"cpu": map[string]any{"cores": cores}})
	if len(r.CPU.Cores) != 1024 || r.CPU.Cores[0] != nil || *r.CPU.Cores[3] != 50 {
		t.Errorf("cores: len %d", len(r.CPU.Cores))
	}
	models := []any{}
	for i := 0; i < 20; i++ {
		models = append(models, map[string]any{"name": "m"})
	}
	if r = norm(t, map[string]any{"ai": []any{map[string]any{"app": "a", "models": models}}}); len(r.AI[0].Models) != 16 {
		t.Errorf("models: %d", len(r.AI[0].Models))
	}
}

func TestMACs(t *testing.T) {
	r := norm(t, map[string]any{"macs": []any{
		"AA:BB:CC:DD:EE:FF", "aa-bb-cc-dd-ee-01", "aa:bb-cc:dd-ee:02", "00:00:00:00:00:00", "00-00:00-00:00-00",
		"aa:bb:cc:dd:ee", "gg:bb:cc:dd:ee:ff", 7.0, " aa:bb:cc:dd:ee:03", "aa:bb:cc:dd:ee:04",
	}})
	want := []string{"aa:bb:cc:dd:ee:ff", "aa:bb:cc:dd:ee:01", "aa:bb:cc:dd:ee:02"}
	if !reflect.DeepEqual(r.MACs, want) {
		t.Errorf("macs = %q, want %q (first 8 only)", r.MACs, want)
	}
}

func TestActionResult(t *testing.T) {
	for _, c := range []struct {
		in   any
		want *ActionResult
	}{
		{map[string]any{"id": "0a1b2c3d4e5f", "ok": true, "message": " done "}, &ActionResult{ID: "0a1b2c3d4e5f", OK: true, Message: Ptr("done")}},
		{map[string]any{"id": "0a1b2c3d4e5f", "ok": "true", "message": 404}, &ActionResult{ID: "0a1b2c3d4e5f", Message: Ptr("404")}},
		{map[string]any{"id": "0a1b2c3d4e5f", "ok": 1}, &ActionResult{ID: "0a1b2c3d4e5f"}},
		{map[string]any{"id": "0A1B2C3D4E5F", "ok": true}, nil},
		{map[string]any{"id": "0a1b2c3d4e5", "ok": true}, nil},
		{map[string]any{"id": "0a1b2c3d4e5f0", "ok": true}, nil},
		{map[string]any{"ok": true}, nil},
		{[]any{"0a1b2c3d4e5f"}, nil},
		{"0a1b2c3d4e5f", nil},
	} {
		if got := norm(t, map[string]any{"actionResult": c.in}).ActionResult; !reflect.DeepEqual(got, c.want) {
			t.Errorf("actionResult(%v) = %+v, want %+v", c.in, got, c.want)
		}
	}
	long := strings.Repeat("m", 300)
	got := norm(t, map[string]any{"actionResult": map[string]any{"id": "0a1b2c3d4e5f", "message": long}}).ActionResult
	if got == nil || *got.Message != long[:200] {
		t.Error("actionResult.message not cut to 200")
	}
}

func TestAppVersion(t *testing.T) {
	for in, want := range map[any]any{
		"1.2.29": "1.2.29", "0.0.0": "0.0.0", "9999.9999.999999": "9999.9999.999999",
		"1.2": nil, "12345.1.1": nil, "1.1.1234567": nil, "v1.2.3": nil, "1.2.3-beta": nil,
		"1.2.3\n": nil, " 1.2.3": nil, "١.٢.٣": nil, "": nil, 1.2: nil,
	} {
		if got := fmtp(norm(t, map[string]any{"appVersion": in}).AppVersion); got != want {
			t.Errorf("appVersion(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestObjects(t *testing.T) {
	r := norm(t, map[string]any{"power": []any{1}, "net": "x", "diskIO": 5, "cpu": []any{1}, "memory": "lots"})
	if r.Power != nil || r.Net != nil || r.DiskIO != nil {
		t.Errorf("non-objects kept: %+v %+v %+v", r.Power, r.Net, r.DiskIO)
	}
	if r.CPU.Name != nil || r.Memory.Total != nil {
		t.Error("cpu/memory from non-object")
	}
	r = norm(t, map[string]any{"power": map[string]any{}, "net": map[string]any{}, "diskIO": map[string]any{}})
	if r.Power == nil || r.Power.CPU != nil || r.Net == nil || r.DiskIO == nil {
		t.Error("empty objects should give non-nil structs with nil fields")
	}
}

func TestAI(t *testing.T) {
	if r := norm(t, nil); r.AI != nil {
		t.Error("missing ai should be nil")
	}
	if r := norm(t, map[string]any{"ai": map[string]any{"app": "x"}}); r.AI != nil {
		t.Error("non-array ai should be nil")
	}
	if r := norm(t, map[string]any{"ai": []any{}}); r.AI == nil || len(r.AI) != 0 {
		t.Error("empty ai should be empty, not nil")
	}
	r := norm(t, map[string]any{"ai": []any{
		map[string]any{"app": "", "mem": 1},
		map[string]any{"app": "Ollama", "mem": "5e9", "models": []any{map[string]any{"name": ""}, map[string]any{"name": "llama3", "size": "4e9"}, "x"}},
		map[string]any{"app": "ComfyUI", "models": "none"},
		nil,
	}})
	if len(r.AI) != 2 || *r.AI[0].App != "Ollama" || *r.AI[0].Mem != 5e9 || len(r.AI[0].Models) != 1 || *r.AI[0].Models[0].Size != 4e9 {
		t.Errorf("ai = %+v", r.AI)
	}
	if r.AI[1].Models == nil || len(r.AI[1].Models) != 0 {
		t.Error("models should be empty, not nil")
	}
}

func TestProcesses(t *testing.T) {
	if r := norm(t, nil); r.Processes != nil {
		t.Error("missing processes should be nil")
	}
	if r := norm(t, map[string]any{"processes": map[string]any{"pid": 1, "name": "x"}}); r.Processes != nil {
		t.Error("non-array processes should be nil")
	}
	r := norm(t, map[string]any{"processes": []any{
		map[string]any{"pid": 1}, map[string]any{"name": "x"}, map[string]any{"pid": "a", "name": "y"}, nil,
	}})
	if r.Processes == nil || len(r.Processes) != 0 {
		t.Errorf("all-invalid processes = %#v, want empty non-nil", r.Processes)
	}
	r = norm(t, map[string]any{"processes": []any{map[string]any{"pid": 0, "name": "idle"}, map[string]any{"pid": -5, "name": "neg"}}})
	if len(r.Processes) != 2 || *r.Processes[1].PID != 0 {
		t.Errorf("pid 0 and clamped pids are kept: %+v", r.Processes)
	}
	r = norm(t, map[string]any{"processes": []any{
		map[string]any{"pid": 2e9, "name": "a"}, map[string]any{"pid": " 0x10 ", "name": "b"}, map[string]any{"pid": true, "name": "c"},
	}})
	if len(r.Processes) != 2 || *r.Processes[0].PID != 1e9 || *r.Processes[1].PID != 16 {
		t.Errorf("pid clamp and numeric string: %+v", r.Processes)
	}
}

// Token and unknown keys are dropped; the additive agent field is cleaned like hostname.
func TestDropsAndAdditive(t *testing.T) {
	r := norm(t, map[string]any{"token": "secret", "receivedAt": 5, "agent": " machine-monitor/0.1.0\x00 "})
	if r.Token != "" {
		t.Error("token kept")
	}
	if r.Agent != "machine-monitor/0.1.0" {
		t.Errorf("agent = %q", r.Agent)
	}
	if r = norm(t, map[string]any{"agent": strings.Repeat("a", 100)}); len(r.Agent) != 64 {
		t.Errorf("agent len %d", len(r.Agent))
	}
	if r = norm(t, map[string]any{"agent": 5}); r.Agent != "5" {
		t.Errorf("agent number = %q", r.Agent)
	}
	out, _ := json.Marshal(r)
	if strings.Contains(string(out), "secret") || strings.Contains(string(out), "receivedAt") {
		t.Errorf("leaked: %s", out)
	}
}

// FuzzNormalize checks Normalize never panics and its output always holds the invariants
// the hub relies on, whatever bytes arrive. Seeds: the M0 fixtures and the parity bodies.
func FuzzNormalize(f *testing.F) {
	files, _ := filepath.Glob(filepath.Join("testdata", "*.json"))
	for _, p := range files {
		b, err := os.ReadFile(p)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	if b, err := os.ReadFile(filepath.Join("testdata", "parity", "cases.json")); err == nil {
		var cases []struct{ Body string }
		if json.Unmarshal(b, &cases) == nil {
			for _, c := range cases {
				f.Add([]byte(c.Body))
			}
		}
	}
	f.Add([]byte(`{"id":"d","cpu":{"usage":"0x` + strings.Repeat("f", 400) + `"}}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		r, err := Normalize(b)
		if err != nil {
			return
		}
		if !ValidID(r.ID) || r.Token != "" {
			t.Fatalf("id %q token %q", r.ID, r.Token)
		}
		out, err := json.Marshal(&r)
		if err != nil {
			t.Fatalf("marshal: %v", err) // a NaN or Inf would fail here
		}
		if !utf8.Valid(out) {
			t.Fatal("invalid UTF-8 in output")
		}
		checkLimits(t, r)
		// Its own output is always accepted again. (Not byte-identical: like upstream, a
		// cut can leave trailing space that a second pass trims.)
		if _, err := Normalize(out); err != nil {
			t.Fatalf("renormalize: %v", err)
		}
	})
}

// checkLimits asserts list lengths and that no string carries control characters.
func checkLimits(t *testing.T, r Report) {
	t.Helper()
	if len(r.Features) > 8 || len(r.Actions) > 8 || len(r.MACs) > 8 || len(r.CPU.Cores) > 1024 ||
		len(r.GPUs) > 32 || len(r.Disks) > 64 || len(r.Temps) > 256 || len(r.Fans) > 64 ||
		len(r.AI) > 8 || len(r.Processes) > 120 || len(r.NetIfaces) > 64 {
		t.Fatal("list over its limit")
	}
	for _, s := range []*string{r.Hostname, r.OS, r.CPU.Name} {
		if s != nil && (*s == "" || strings.ContainsFunc(*s, func(c rune) bool { return c < 0x20 || c == 0x7f })) {
			t.Fatalf("bad string %q", *s)
		}
	}
	if r.Uptime != nil && (*r.Uptime < 0 || *r.Uptime > 1e10) || r.CPU.Temp != nil && (*r.CPU.Temp < -60 || *r.CPU.Temp > 250) {
		t.Fatal("number out of range")
	}
}

// U+202E and the other bidi controls are stripped; upstream's cleanText keeps them.
func TestCleanTextStripsBidi(t *testing.T) {
	in := "a\u202ecod.exe\u200e\u200f\u202a\u202b\u202c\u202d\u2066\u2067\u2068\u2069b"
	if got := CleanText(in, 60); got == nil || *got != "acod.exeb" {
		t.Errorf("CleanText = %v, want acod.exeb", got)
	}
	if got := CleanText("\u202e\u2069", 60); got != nil {
		t.Errorf("only bidi controls = %q, want nil", *got)
	}
	if got := CleanText("\u2065\u202f\u2061\u00a1\u00a0x\u061b", 60); got == nil || *got != "\u2065\u202f\u2061\u00a1\u00a0x\u061b" {
		t.Errorf("neighbouring code points must stay, got %v", got)
	}
}

// The Arabic letter mark, zero-width characters and C1 controls go too (skeptic pass 2, B16).
func TestCleanTextStripsInvisibles(t *testing.T) {
	in := "omar\u200bchy\u200c\u200d\u2060\ufeff\u061c\u0080\u009b41m\u009f!"
	if got := CleanText(in, 60); got == nil || *got != "omarchy41m!" {
		t.Errorf("CleanText = %v, want omarchy41m!", got)
	}
}
