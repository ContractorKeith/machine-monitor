// Normalize rebuilds an untrusted agent report from known fields only, with every value
// coerced to the expected type and clamped, so the hub only ever stores and streams
// strings where it expects strings and finite numbers where it expects numbers.
//
// Ported from Forward-Future/device-monitor security.js (normalizeReport,
// normalizeProcesses, isValidId, cleanText) at c3a4c41.
// MIT License, Copyright (c) 2026 Matt: https://github.com/Forward-Future/device-monitor
//
// Matching JavaScript, on purpose:
//   - Numbers accept JSON numbers and numeric strings as JS Number() parses them
//     ("12.5", " 7 ", "0x1A", "1e3"); anything else, or a non-finite result, is nil.
//   - Strings accept any JSON value as JS String() renders it (numbers, booleans,
//     arrays, "[object Object]"), drop U+0000–U+001F and U+007F, trim JS whitespace,
//     and are cut at upstream's limits counted in UTF-16 code units, as JS slice does.
//     Where a cut would split a surrogate pair, the half pair is dropped, so the result
//     is always valid UTF-8 and never longer than upstream's.
package report

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Errors from Normalize.
var (
	ErrNotObject    = errors.New("report: not a JSON object")
	ErrInvalidID    = errors.New("report: invalid id")
	ErrTrailingData = errors.New("report: data after the JSON value")
)

var (
	idRE         = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)
	macRE        = regexp.MustCompile(`^(?i)([0-9a-f]{2}[:-]){5}[0-9a-f]{2}$`)
	zeroMACRE    = regexp.MustCompile(`^(00[:-]){5}00$`)
	actionIDRE   = regexp.MustCompile(`^[a-f0-9]{12}$`)
	appVersionRE = regexp.MustCompile(`^\d{1,4}\.\d{1,4}\.\d{1,6}$`)
	jsDecimalRE  = regexp.MustCompile(`^[+-]?(Infinity|\d+\.?\d*([eE][+-]?\d+)?|\.\d+([eE][+-]?\d+)?)$`)
)

// ValidID reports whether id is an acceptable device id: machine GUID, machine-id or
// IOPlatformUUID style, 1–64 chars of [A-Za-z0-9._:-] starting with a letter or digit.
func ValidID(id string) bool { return idRE.MatchString(id) }

// ValidAction reports whether a is a remote action the hub may queue.
func ValidAction(a string) bool { return a == "sleep" || a == "restart" || a == "shutdown" }

// Normalize decodes a raw report body and rebuilds it from known fields (normalizeReport).
// It fails if the body isn't a JSON object or its id isn't ValidID, as upstream's
// handleReport does. Token is dropped. Processes is normalized separately
// (normalizeProcesses): nil when the agent didn't send an array, so the hub should move
// it out before storing the report.
func Normalize(raw []byte) (Report, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // keep 1e999 decodable; num() turns it into nil as JS does with Infinity
	var v any
	if err := dec.Decode(&v); err != nil {
		return Report{}, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return Report{}, ErrTrailingData // JSON.parse rejects anything after the value
	}
	b, ok := v.(map[string]any)
	if !ok {
		return Report{}, ErrNotObject
	}
	id, _ := b["id"].(string)
	if !ValidID(id) {
		return Report{}, ErrInvalidID
	}
	return normalizeReport(id, b), nil
}

func normalizeReport(id string, b map[string]any) Report {
	cpu := obj(b["cpu"])
	mem := obj(b["memory"])
	r := Report{
		ID:       id,
		Hostname: str(b["hostname"], 64),
		Platform: PlatformUnknown,
		OS:       str(b["os"], 120),
		Uptime:   num(b["uptime"], 0, 1e10),
		Features: []string{},
		Actions:  []string{},
		MACs:     []string{},
		CPU: CPU{
			Name:     str(cpu["name"], 120),
			Usage:    num(cpu["usage"], 0, 100),
			Cores:    []*float64{},
			Threads:  num(cpu["threads"], 0, 1e5),
			ClockMHz: num(cpu["clockMHz"], 0, 1e5),
			Temp:     num(cpu["temp"], -60, 250),
		},
		Memory: Memory{Total: num(mem["total"], 0, 1e18), Used: num(mem["used"], 0, 1e18)},
		GPUs:   []GPU{},
		Disks:  []Disk{},
		Temps:  []Temp{},
		Fans:   []Fan{},
	}
	if p, ok := b["platform"].(string); ok && (p == PlatformLinux || p == PlatformMacOS || p == PlatformWindows) {
		r.Platform = p
	}
	if f, ok := b["formFactor"].(string); ok && (f == FormFactorLaptop || f == FormFactorDesktop) {
		r.FormFactor = Ptr(f)
	}
	if p, ok := b["power"].(map[string]any); ok {
		r.Power = &Power{CPU: num(p["cpu"], 0, 5000)}
	}
	for _, v := range list(b["features"], 8) {
		if f, ok := v.(string); ok && (f == "procs" || f == "actions") {
			r.Features = append(r.Features, f)
		}
	}
	for _, v := range list(b["actions"], 8) {
		if a, ok := v.(string); ok && ValidAction(a) {
			r.Actions = append(r.Actions, a)
		}
	}
	for _, v := range list(b["macs"], 8) {
		if m, ok := v.(string); ok && macRE.MatchString(m) && !zeroMACRE.MatchString(m) {
			r.MACs = append(r.MACs, strings.ReplaceAll(strings.ToLower(m), "-", ":"))
		}
	}
	if a, ok := b["actionResult"].(map[string]any); ok {
		if aid, ok := a["id"].(string); ok && actionIDRE.MatchString(aid) {
			r.ActionResult = &ActionResult{ID: aid, OK: a["ok"] == true, Message: str(a["message"], 200)}
		}
	}
	if v, ok := b["appVersion"].(string); ok && appVersionRE.MatchString(v) {
		r.AppVersion = Ptr(v)
	}
	for _, v := range list(cpu["cores"], 1024) {
		r.CPU.Cores = append(r.CPU.Cores, num(v, 0, 100))
	}
	if cpu["tempSource"] == "acpi" {
		r.CPU.TempSource = Ptr("acpi")
	}
	for _, g := range objs(b["gpus"], 32) {
		r.GPUs = append(r.GPUs, GPU{
			Name: str(g["name"], 120), Vendor: str(g["vendor"], 20),
			Usage: num(g["usage"], 0, 100), Temp: num(g["temp"], -60, 250),
			MemUsed: num(g["memUsed"], 0, 1e18), MemTotal: num(g["memTotal"], 0, 1e18),
			Power: num(g["power"], 0, 1e5), PowerLimit: num(g["powerLimit"], 0, 1e5),
			Fan: num(g["fan"], 0, 100), ClockMHz: num(g["clockMHz"], 0, 1e5),
		})
	}
	for _, d := range objs(b["disks"], 64) {
		r.Disks = append(r.Disks, Disk{
			Name: str(d["name"], 80), Label: str(d["label"], 80),
			Total: num(d["total"], 0, 1e18), Used: num(d["used"], 0, 1e18),
		})
	}
	if n, ok := b["net"].(map[string]any); ok {
		r.Net = &Net{RX: num(n["rx"], 0, 1e18), TX: num(n["tx"], 0, 1e18)}
	}
	if d, ok := b["diskIO"].(map[string]any); ok {
		r.DiskIO = &DiskIO{Read: num(d["read"], 0, 1e18), Write: num(d["write"], 0, 1e18)}
	}
	for _, t := range objs(b["temps"], 256) {
		r.Temps = append(r.Temps, Temp{
			Name: str(t["name"], 80), Parent: str(t["parent"], 120), Value: num(t["value"], -60, 250),
		})
	}
	for _, f := range objs(b["fans"], 64) {
		r.Fans = append(r.Fans, Fan{Name: str(f["name"], 80), RPM: num(f["rpm"], 0, 1e5)})
	}
	// AI: nil means the agent doesn't check, empty means nothing is loaded.
	if _, ok := b["ai"].([]any); ok {
		r.AI = []AIGroup{}
		for _, g := range objs(b["ai"], 8) {
			grp := AIGroup{App: str(g["app"], 40), Mem: num(g["mem"], 0, 1e18), Models: []AIModel{}}
			for _, m := range objs(g["models"], 16) {
				model := AIModel{Name: str(m["name"], 200), Size: num(m["size"], 0, 1e18), Kind: str(m["kind"], 30)}
				if model.Name != nil {
					grp.Models = append(grp.Models, model)
				}
			}
			if grp.App != nil {
				r.AI = append(r.AI, grp)
			}
		}
	}
	r.Processes = normalizeProcesses(b["processes"])

	// Additive fields, which upstream's hub drops. Same rules as their upstream siblings.
	if a := str(b["agent"], 64); a != nil {
		r.Agent = *a
	}
	for _, n := range objs(b["netIfaces"], 64) {
		r.NetIfaces = append(r.NetIfaces, NetIface{
			Name: str(n["name"], 80), RX: num(n["rx"], 0, 1e18), TX: num(n["tx"], 0, 1e18),
		})
	}
	return r
}

// normalizeProcesses rebuilds the processes list, sent only while someone has the
// Processes panel open. nil when v isn't an array; rows without a pid or name are dropped.
func normalizeProcesses(v any) []Process {
	if _, ok := v.([]any); !ok {
		return nil
	}
	out := []Process{}
	for _, p := range objs(v, 120) {
		row := Process{
			PID: num(p["pid"], 0, 1e9), Name: str(p["name"], 80),
			Cmd: str(p["cmd"], 160), User: str(p["user"], 40),
			CPU: num(p["cpu"], 0, 100), Mem: num(p["mem"], 0, 1e18),
			GPU: num(p["gpu"], 0, 100), GPUMem: num(p["gpuMem"], 0, 1e18),
		}
		if row.PID != nil && row.Name != nil {
			out = append(out, row)
		}
	}
	return out
}

// CleanText is upstream's cleanText: v as JS String() renders it, without control
// characters (and, beyond upstream, bidi override and format characters), trimmed, cut to limit UTF-16 code units. nil for null or an empty result.
// v is a value from encoding/json (decoded with or without UseNumber).
func CleanText(v any, limit int) *string { return str(v, limit) }

// obj returns v as a JSON object, or an empty one (upstream isObj(v) ? v : {}).
func obj(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// list returns the first limit elements of v if it's an array, else nothing.
func list(v any, limit int) []any {
	a, _ := v.([]any)
	if len(a) > limit {
		a = a[:limit]
	}
	return a
}

// objs is list(v, limit).filter(isObj): elements past limit are cut before filtering.
func objs(v any, limit int) []map[string]any {
	var out []map[string]any
	for _, e := range list(v, limit) {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// str is upstream's str(v, max).
func str(v any, limit int) *string {
	if v == nil {
		return nil
	}
	s := strings.Map(func(r rune) rune {
		if r <= 0x1f || r == 0x7f || isBidiFormat(r) {
			return -1
		}
		return r
	}, jsString(v))
	s = strings.TrimFunc(s, isJSSpace)
	if s == "" {
		return nil
	}
	return Ptr(cutUTF16(s, limit))
}

// isBidiFormat reports the invisible direction controls: LRM/RLM (U+200E, U+200F), the
// embeddings and overrides (U+202A–U+202E) and the isolates (U+2066–U+2069). Unlike the
// rest of cleanText this goes beyond upstream, which keeps them: a U+202E in a hostname
// or process name reverses the text after it on screen and can disguise one name as another.
func isBidiFormat(r rune) bool {
	return r == 0x200e || r == 0x200f || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069)
}

// cutUTF16 keeps the first limit UTF-16 code units of s, never splitting a surrogate pair.
func cutUTF16(s string, limit int) string {
	n := 0
	for i, r := range s {
		w := utf16.RuneLen(r)
		if w < 0 {
			w = 1 // invalid UTF-8 never comes out of encoding/json, but count it like U+FFFD
		}
		if n+w > limit {
			return s[:i]
		}
		n += w
	}
	return s
}

// jsString renders a decoded JSON value as JS String() does.
func jsString(v any) string {
	switch x := v.(type) {
	case nil:
		return "" // only reachable inside arrays: String([null]) is ""
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case json.Number:
		f, _ := strconv.ParseFloat(string(x), 64) // out-of-range gives ±Inf or ±0, as JS does
		return jsNumberString(f)
	case float64:
		return jsNumberString(x)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = jsString(e)
		}
		return strings.Join(parts, ",")
	default:
		return "[object Object]"
	}
}

// jsNumberString formats f as JS Number.prototype.toString() does.
func jsNumberString(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	case f == 0:
		return "0"
	}
	if a := math.Abs(f); a >= 1e-6 && a < 1e21 {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	// Go writes 1e-07 and 1e+21; JS writes 1e-7 and 1e+21.
	s := strconv.FormatFloat(f, 'e', -1, 64)
	mant, exp, _ := strings.Cut(s, "e")
	sign, digits := exp[:1], strings.TrimLeft(exp[1:], "0")
	return mant + "e" + sign + digits
}

// num is upstream's num(v, min, max): a finite number clamped to [lo, hi], or nil.
func num(v any, lo, hi float64) *float64 {
	var n float64
	switch x := v.(type) {
	case json.Number:
		n, _ = strconv.ParseFloat(string(x), 64)
	case float64:
		n = x
	case string:
		if strings.TrimFunc(x, isJSSpace) == "" {
			return nil
		}
		n = jsNumber(x)
	default:
		return nil
	}
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return nil
	}
	n = math.Min(hi, math.Max(lo, n))
	if n == 0 {
		n = 0 // -0 would encode as "-0"; JSON.stringify writes 0
	}
	return &n
}

// jsNumber converts a string as JS Number(s) does; NaN when it isn't a number.
func jsNumber(s string) float64 {
	s = strings.TrimFunc(s, isJSSpace)
	if s == "" {
		return 0
	}
	if len(s) > 2 && s[0] == '0' {
		base := 0
		switch s[1] {
		case 'x', 'X':
			base = 16
		case 'o', 'O':
			base = 8
		case 'b', 'B':
			base = 2
		}
		if base != 0 {
			digits := s[2:]
			// big.Int.SetString would accept a sign or underscores; JS doesn't.
			if strings.ContainsAny(digits, "+-_") {
				return math.NaN()
			}
			i, ok := new(big.Int).SetString(digits, base)
			if !ok {
				return math.NaN()
			}
			f, _ := new(big.Float).SetInt(i).Float64()
			return f
		}
	}
	if !jsDecimalRE.MatchString(s) {
		return math.NaN()
	}
	switch s {
	case "Infinity", "+Infinity":
		return math.Inf(1)
	case "-Infinity":
		return math.Inf(-1)
	}
	f, _ := strconv.ParseFloat(s, 64) // out-of-range gives ±Inf or ±0, as JS does
	return f
}

// isJSSpace is the set String.prototype.trim removes: WhiteSpace and LineTerminator.
// It differs from unicode.IsSpace by including U+FEFF and excluding U+0085.
func isJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}
