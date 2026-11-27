package report

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/ContractorKeith/machine-monitor/internal/textclean"
	"io"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"
)

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

func ValidID(id string) bool { return idRE.MatchString(id) }

func ValidAction(a string) bool { return a == "sleep" || a == "restart" || a == "shutdown" }

func Normalize(raw []byte) (Report, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return Report{}, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return Report{}, ErrTrailingData
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
			Integrated: g["integrated"] == true,
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

func CleanText(v any, limit int) *string { return str(v, limit) }

func obj(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func list(v any, limit int) []any {
	a, _ := v.([]any)
	if len(a) > limit {
		a = a[:limit]
	}
	return a
}

func objs(v any, limit int) []map[string]any {
	var out []map[string]any
	for _, e := range list(v, limit) {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func str(v any, limit int) *string {
	if v == nil {
		return nil
	}
	s := textclean.Strip(jsString(v))
	s = strings.TrimFunc(s, isJSSpace)
	if s == "" {
		return nil
	}
	return Ptr(cutUTF16(s, limit))
}

func cutUTF16(s string, limit int) string {
	n := 0
	for i, r := range s {
		w := utf16.RuneLen(r)
		if w < 0 {
			w = 1
		}
		if n+w > limit {
			return s[:i]
		}
		n += w
	}
	return s
}

func jsString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case json.Number:
		f, _ := strconv.ParseFloat(string(x), 64)
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
	s := strconv.FormatFloat(f, 'e', -1, 64)
	mant, exp, _ := strings.Cut(s, "e")
	sign, digits := exp[:1], strings.TrimLeft(exp[1:], "0")
	return mant + "e" + sign + digits
}

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
		n = 0
	}
	return &n
}

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
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

func isJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}
