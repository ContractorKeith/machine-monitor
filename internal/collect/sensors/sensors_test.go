package sensors

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/report"
)

// asJSON renders v the way it goes on the wire, so expectations read like a report.
func asJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// writeTree builds a sysfs-like tree in a temp dir from path → contents.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for p, body := range files {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

var fixture = filepath.Join("testdata", "sys")

func TestReadHwmon(t *testing.T) {
	temps, fans, cpu := readHwmon(fixture)
	wantTemps := `[{"name":"Temp 1","parent":"acpitz","value":27.8},` +
		`{"name":"Tctl","parent":"k10temp","value":61.3},{"name":"Tccd1","parent":"k10temp","value":55},` +
		`{"name":"Composite","parent":"nvme","value":38.9},{"name":"Composite","parent":"nvme #2","value":41.9},` +
		`{"name":"Temp 1","parent":"nct6798","value":33}]`
	if got := asJSON(t, temps); got != wantTemps {
		t.Errorf("temps\ngot  %s\nwant %s", got, wantTemps)
	}
	// fan2 is stopped and left out; fan10 sorts after fan1 numerically.
	wantFans := `[{"name":"CPU Fan","rpm":812},{"name":"Fan 10","rpm":1200}]`
	if got := asJSON(t, fans); got != wantFans {
		t.Errorf("fans\ngot  %s\nwant %s", got, wantFans)
	}
	if cpu == nil || *cpu != 61.3 {
		t.Errorf("cpu temp = %v, want 61.3 from k10temp temp1", cpu)
	}
}

func TestThermalZoneFallback(t *testing.T) {
	// No CPU hwmon chip: the hottest CPU/ACPI zone wins.
	temp, source := thermalZoneTemp(fixture)
	if temp == nil || *temp != 64 || source != nil {
		t.Errorf("zones = %v %v, want 64 from x86_pkg_temp", temp, source)
	}
	onlyACPI := writeTree(t, map[string]string{
		"class/thermal/thermal_zone0/type": "acpitz", "class/thermal/thermal_zone0/temp": "48500",
		"class/thermal/thermal_zone1/type": "iwlwifi_1", "class/thermal/thermal_zone1/temp": "70000",
	})
	s := readLinux(onlyACPI, &raplMeter{}, time.Now())
	if s.cpuTemp == nil || *s.cpuTemp != 48.5 || s.tempSource == nil || *s.tempSource != "acpi" {
		t.Errorf("acpitz fallback = %s", asJSON(t, map[string]any{"temp": s.cpuTemp, "source": s.tempSource}))
	}
}

func TestGracePower(t *testing.T) {
	// Two Grace sockets summed; the non-Grace power_meter is ignored.
	if p := gracePower(fixture); p == nil || *p != 270.5 {
		t.Errorf("grace = %v, want 270.5", p)
	}
	if p := gracePower(t.TempDir()); p != nil {
		t.Errorf("no meters = %v, want nil", *p)
	}
}

func TestRAPL(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("RAPL zone names contain ':', which Windows paths can't")
	}
	zone := func(e string) map[string]string {
		return map[string]string{
			"class/powercap/intel-rapl:0/name": "package-0", "class/powercap/intel-rapl:0/energy_uj": e,
			"class/powercap/intel-rapl:0/max_energy_range_uj": "262143328850",
			"class/powercap/intel-rapl:0:0/name":              "core", "class/powercap/intel-rapl:0:0/energy_uj": "0",
			"class/powercap/intel-rapl:1/name": "psys", "class/powercap/intel-rapl:1/energy_uj": "0",
		}
	}
	var m raplMeter
	t0 := time.Unix(1000, 0)
	if w := m.watts(writeTree(t, zone("1000000000")), t0); w != nil {
		t.Errorf("first reading = %v, want nil", *w)
	}
	// 90 J over 2 s = 45 W. Only package-0 counts: not the core sub-zone, not psys.
	if w := m.watts(writeTree(t, zone("1090000000")), t0.Add(2*time.Second)); w == nil || *w != 45 {
		t.Errorf("watts = %v, want 45", w)
	}
	// The counter wrapped at max_energy_range_uj: 262143000000 → 50000000 is
	// 328850 + 50000000 uJ = 50.3 J over 1 s.
	m = raplMeter{}
	m.watts(writeTree(t, zone("262143000000")), t0)
	if w := m.watts(writeTree(t, zone("50000000")), t0.Add(time.Second)); w == nil || *w != 50.3 {
		t.Errorf("wrapped watts = %v, want 50.3", w)
	}
	// A wrap with an unreadable max_energy_range_uj can't be unwrapped: skip the zone, never
	// report negative watts.
	noMax := func(e string) map[string]string {
		return map[string]string{"class/powercap/intel-rapl:0/name": "package-0", "class/powercap/intel-rapl:0/energy_uj": e}
	}
	m = raplMeter{}
	m.watts(writeTree(t, noMax("262143000000")), t0)
	if w := m.watts(writeTree(t, noMax("50000000")), t0.Add(time.Second)); w != nil {
		t.Errorf("wrap without max = %v, want nil", *w)
	}
	if w := m.watts(writeTree(t, noMax("80000000")), t0.Add(2*time.Second)); w == nil || *w != 30 {
		t.Errorf("after wrap without max = %v, want 30", w)
	}
	// Unreadable energy_uj (root-only): nil, never 0.
	m = raplMeter{}
	empty := t.TempDir()
	m.watts(empty, t0)
	if w := m.watts(empty, t0.Add(time.Second)); w != nil {
		t.Errorf("no counters = %v, want nil", *w)
	}
}

func TestReadLinuxFixture(t *testing.T) {
	s := readLinux(fixture, &raplMeter{}, time.Now())
	r := &report.Report{}
	s.apply()(r)
	if *r.CPU.Temp != 61.3 || r.CPU.TempSource != nil || *r.Power.CPU != 270.5 || len(r.Temps) != 6 || len(r.Fans) != 2 {
		t.Errorf("report = %s", asJSON(t, r))
	}
}

func TestReadingApplyFillsOnlyNil(t *testing.T) {
	s := reading{cpuTemp: report.Ptr(70.0), cpuPower: report.Ptr(30.0), gpuTemp: report.Ptr(50.0),
		temps: []report.Temp{{Name: report.Ptr("a")}}, fans: []report.Fan{{Name: report.Ptr("f")}}}
	r := &report.Report{
		CPU:   report.CPU{Temp: report.Ptr(60.0)},
		Power: &report.Power{CPU: report.Ptr(10.0)},
		GPUs:  []report.GPU{{Temp: report.Ptr(40.0)}, {}},
		Temps: []report.Temp{{Name: report.Ptr("existing")}},
	}
	s.apply()(r)
	if *r.CPU.Temp != 60 || *r.Power.CPU != 10 || *r.GPUs[0].Temp != 40 || *r.GPUs[1].Temp != 50 || len(r.Temps) != 2 || len(r.Fans) != 1 {
		t.Errorf("report = %s", asJSON(t, r))
	}
	r = &report.Report{}
	s.apply()(r)
	if *r.CPU.Temp != 70 || *r.Power.CPU != 30 {
		t.Errorf("empty report = %s", asJSON(t, r))
	}
	if (reading{}).apply() != nil {
		t.Error("empty reading should give a nil Apply")
	}
}

func TestSmctempFakeTool(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake tools are shell scripts")
	}
	bin, err := filepath.Abs(filepath.Join("testdata", "bin"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	s, err := readSmctemp(context.Background(), findTool("smctemp"))
	if err != nil {
		t.Fatal(err)
	}
	r := &report.Report{GPUs: []report.GPU{{Name: report.Ptr("Apple M3 Max")}}}
	s.apply()(r)
	if *r.CPU.Temp != 61.4 || *r.GPUs[0].Temp != 55.2 {
		t.Errorf("report = %s", asJSON(t, r))
	}
	if got, want := asJSON(t, r.Temps), `[{"name":"CPU","parent":"smctemp","value":61.4},{"name":"GPU","parent":"smctemp","value":55.2}]`; got != want {
		t.Errorf("temps\ngot  %s\nwant %s", got, want)
	}
}

func TestSmctempMissing(t *testing.T) {
	s, err := readSmctemp(context.Background(), "")
	if err != nil || s.apply() != nil {
		t.Errorf("missing smctemp = %v, %v; want nothing", s.apply() != nil, err)
	}
	for _, out := range []string{"", "0.0\n", "garbage", "-1"} {
		if v := parseSmctemp([]byte(out)); v != nil {
			t.Errorf("parseSmctemp(%q) = %v, want nil", out, *v)
		}
	}
}

// Rows in the shape LibreHardwareMonitor publishes for a Ryzen CPU and Radeon GPU, with the
// edge cases agent.ps1 handles: a 0 °C reading, a stopped fan, non-package power.
var recLHM = []lhmSensor{
	{Name: "Core (Tctl/Tdie)", SensorType: "Temperature", Parent: "/amdcpu/0", Value: 71.625},
	{Name: "CCD1 (Tdie)", SensorType: "Temperature", Parent: "/amdcpu/0", Value: 66.5},
	{Name: "Package", SensorType: "Power", Parent: "/amdcpu/0", Value: 88.43},
	{Name: "Core #1", SensorType: "Power", Parent: "/amdcpu/0", Value: 7.2},
	{Name: "CPU Total", SensorType: "Load", Parent: "/amdcpu/0", Value: 23},
	{Name: "GPU Core", SensorType: "Temperature", Parent: "/gpu-amd/0", Value: 48},
	{Name: "GPU Hot Spot", SensorType: "Temperature", Parent: "/gpu-amd/0", Value: 61},
	{Name: "GPU Package", SensorType: "Power", Parent: "/gpu-amd/0", Value: 40},
	{Name: "Temperature", SensorType: "Temperature", Parent: "/nvme/0", Value: 0},
	{Name: "Fan #1", SensorType: "Fan", Parent: "/lpc/nct6799d/0", Value: 1033.7},
	{Name: "Fan #2", SensorType: "Fan", Parent: "/lpc/nct6799d/0", Value: 0},
}

func TestFromLHM(t *testing.T) {
	s := fromLHM(recLHM)
	got := asJSON(t, map[string]any{"cpuTemp": s.cpuTemp, "cpuPower": s.cpuPower, "gpuTemp": s.gpuTemp, "temps": s.temps, "fans": s.fans})
	want := `{"cpuPower":88.4,"cpuTemp":71.6,"fans":[{"name":"Fan #1","rpm":1034}],"gpuTemp":48,"temps":[` +
		`{"name":"Core (Tctl/Tdie)","parent":"/amdcpu/0","value":71.6},{"name":"CCD1 (Tdie)","parent":"/amdcpu/0","value":66.5},` +
		`{"name":"GPU Core","parent":"/gpu-amd/0","value":48},{"name":"GPU Hot Spot","parent":"/gpu-amd/0","value":61}]}`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	if fromLHM(nil).apply() != nil {
		t.Error("no hardware monitor should give a nil Apply")
	}
}

func TestFromACPI(t *testing.T) {
	zones := []thermalZone{{Name: `\_TZ.TZ00`, HighPrecisionTemperature: 0}, {Name: `\_TZ.TZ01`, HighPrecisionTemperature: 3132}}
	if v := fromACPI(zones); v == nil || *v != 40.1 {
		t.Errorf("acpi = %v, want 40.1 (3132 dK)", v)
	}
	if v := fromACPI(nil); v != nil {
		t.Errorf("no zones = %v, want nil", *v)
	}
}

// On an AMD APU the PPT sensor is CPU package power, used when RAPL and Grace give nothing.
func TestAPUPackagePower(t *testing.T) {
	apu := filepath.Join("testdata", "apu", "sys")
	s := readLinux(apu, &raplMeter{}, time.Now())
	r := &report.Report{}
	s.apply()(r)
	if r.Power == nil || r.Power.CPU == nil || *r.Power.CPU != 23 {
		t.Errorf("APU report = %s, want power.cpu 23", asJSON(t, r))
	}
	// A value another probe already set wins.
	r = &report.Report{Power: &report.Power{CPU: report.Ptr(10.0)}}
	s.apply()(r)
	if *r.Power.CPU != 10 {
		t.Errorf("power.cpu = %v, want 10 kept", *r.Power.CPU)
	}
	if p := apuPower(fixture); p != nil {
		t.Errorf("no PPT chip = %v, want nil", *p)
	}
}
