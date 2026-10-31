package gpu

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

// fakeTools puts testdata/bin first on PATH. The fakes are shell scripts, so tests that
// exec them skip on Windows.
func fakeTools(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake tools are shell scripts")
	}
	bin, err := filepath.Abs(filepath.Join("testdata", "bin"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

const (
	rtx3060 = `{"name":"NVIDIA GeForce RTX 3060","vendor":"nvidia","usage":7,"temp":41,"memUsed":1073741824,"memTotal":12884901888,"power":18.52,"powerLimit":170,"fan":30,"clockMHz":210}`
	rtx4090 = `{"name":"NVIDIA GeForce RTX 4090","vendor":"nvidia","usage":97,"temp":72,"memUsed":21474836480,"memTotal":25757220864,"power":412.33,"powerLimit":450,"fan":null,"clockMHz":2520}`
)

func TestParseNvidia(t *testing.T) {
	out := "NVIDIA GeForce RTX 3060, 7, 41, 1024, 12288, 18.52, 170.00, 30, 210\r\n" +
		"Tesla T4, [N/A], 35, 0, [N/A], [Not Supported], 70.00, [N/A], 585\r\n" +
		"garbage line\n\n"
	got := asJSON(t, parseNvidia([]byte(out)))
	want := "[" + rtx3060 + `,{"name":"Tesla T4","vendor":"nvidia","usage":null,"temp":35,"memUsed":null,"memTotal":null,"power":null,"powerLimit":70,"fan":null,"clockMHz":585}]`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestQueryNvidiaFakeTool(t *testing.T) {
	fakeTools(t)
	smi := findTool("nvidia-smi")
	if smi == "" {
		t.Fatal("fake nvidia-smi not found on PATH")
	}
	gpus, err := queryNvidia(context.Background(), smi)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := asJSON(t, gpus), "["+rtx3060+","+rtx4090+"]"; got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestQueryNvidiaMissing(t *testing.T) {
	gpus, err := queryNvidia(context.Background(), "")
	if gpus != nil || err != nil {
		t.Errorf("missing nvidia-smi = %v, %v; want nil, nil", gpus, err)
	}
	if p := findTool("no-such-tool-machine-monitor", filepath.Join(t.TempDir(), "nope")); p != "" {
		t.Errorf("findTool found %q", p)
	}
}

func TestCommandStripsEnvironment(t *testing.T) {
	fakeTools(t)
	t.Setenv("MACHINE_MONITOR_SECRET", "leak")
	out, err := command(context.Background(), findTool("envdump"))
	if err != nil {
		t.Fatal(err)
	}
	env := string(out)
	if strings.Contains(env, "MACHINE_MONITOR_SECRET") || !strings.Contains(env, "LC_ALL=C") || !strings.Contains(env, "PATH=") {
		t.Errorf("environment not stripped:\n%s", env)
	}
}

func TestCommandTimeout(t *testing.T) {
	fakeTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := command(ctx, findTool("slow")); err == nil {
		t.Error("want an error from a tool that outlives its deadline")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("command took %v after a 100ms deadline", d)
	}
}

func TestReadAMD(t *testing.T) {
	got := asJSON(t, readAMD(filepath.Join("testdata", "sys")))
	want := `[{"name":"AMD GPU","vendor":"amd","usage":45,"temp":52,"memUsed":2147483648,"memTotal":17163091968,"power":87,"powerLimit":null,"fan":null,"clockMHz":null},` +
		`{"name":"AMD GPU","vendor":"amd","usage":3,"temp":null,"memUsed":null,"memTotal":4294967296,"power":31,"powerLimit":null,"fan":null,"clockMHz":null}]`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	if gpus := readAMD(t.TempDir()); gpus != nil {
		t.Errorf("empty sysfs = %v, want nil", gpus)
	}
}

func TestParseIoreg(t *testing.T) {
	out, err := os.ReadFile(filepath.Join("testdata", "bin", "ioreg"))
	if err != nil {
		t.Fatal(err)
	}
	gpus := parseIoreg(out)
	appleNames(gpus, report.Ptr("Apple M3 Max"))
	got := asJSON(t, gpus)
	want := `[{"name":"Apple M3 Max","vendor":"apple","usage":33,"temp":null,"memUsed":2147483648,"memTotal":null,"power":null,"powerLimit":null,"fan":null,"clockMHz":null},` +
		`{"name":"Apple M3 Max GPU","vendor":"apple","usage":5,"temp":null,"memUsed":104857600,"memTotal":null,"power":null,"powerLimit":null,"fan":null,"clockMHz":null}]`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}

	gpus = parseIoreg(out)
	appleNames(gpus, nil)
	if *gpus[1].Name != "Apple GPU" {
		t.Errorf("fallback name = %q, want Apple GPU", *gpus[1].Name)
	}
	if parseIoreg(nil) != nil {
		t.Error("no IOAccelerator should give no GPUs")
	}
}

// Rows in the shape Windows publishes for a Radeon dGPU plus Intel iGPU, with a Basic Display
// Adapter, a Copy engine and an idle adapter to exercise agent.ps1's filters.
var (
	recEngines = []gpuEngine{
		{Name: "pid_4120_luid_0x00000000_0x0000D1A4_phys_0_eng_0_engtype_3D", UtilizationPercentage: 21},
		{Name: "pid_7788_luid_0x00000000_0x0000D1A4_phys_0_eng_0_engtype_3D", UtilizationPercentage: 17},
		{Name: "pid_4120_luid_0x00000000_0x0000D1A4_phys_0_eng_1_engtype_Copy", UtilizationPercentage: 60},
		{Name: "pid_912_luid_0x00000000_0x0000E2B0_phys_0_eng_0_engtype_3D", UtilizationPercentage: 0},
		{Name: "pid_912_luid_0x00000000_0x0000F001_phys_0_eng_0_engtype_3D", UtilizationPercentage: 0},
	}
	recMems = []gpuAdapterMemory{
		{Name: "luid_0x00000000_0x0000D1A4_phys_0", DedicatedUsage: 3221225472},
		{Name: "luid_0x00000000_0x0000E2B0_phys_0", DedicatedUsage: 0},
		{Name: "luid_0x00000000_0x0000F001_phys_0", DedicatedUsage: 268435456},
	}
	recControllers = []videoController{
		{Name: "Microsoft Basic Display Adapter"},
		{Name: "AMD Radeon RX 7800 XT", AdapterRAM: 4293918720},
		{Name: "Intel(R) UHD Graphics 770", AdapterRAM: 1073741824},
	}
)

func TestGenericGPUs(t *testing.T) {
	got := asJSON(t, genericGPUs(recEngines, recMems, recControllers))
	// D1A4: 3D engines 21+17, Copy ignored. E2B0: idle with no memory, skipped.
	// F001: idle but holding memory, kept; named by position like agent.ps1.
	want := `[{"name":"AMD Radeon RX 7800 XT","vendor":"generic","usage":38,"temp":null,"memUsed":3221225472,"memTotal":4293918720,"power":null,"powerLimit":null,"fan":null,"clockMHz":null},` +
		`{"name":"Intel(R) UHD Graphics 770","vendor":"generic","usage":0,"temp":null,"memUsed":268435456,"memTotal":1073741824,"power":null,"powerLimit":null,"fan":null,"clockMHz":null}]`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}

	none := genericGPUs(recEngines, recMems, nil)
	if len(none) != 2 || *none[0].Name != "GPU" || none[0].MemTotal != nil {
		t.Errorf("without video controllers = %s", asJSON(t, none))
	}
	if genericGPUs(nil, nil, recControllers) != nil {
		t.Error("no engines should give no GPUs")
	}
}

func TestAppendGPUsSortsByVRAM(t *testing.T) {
	gpus := []report.GPU{
		{Name: report.Ptr("small"), MemTotal: report.Ptr(8.0)},
		{Name: report.Ptr("unknown")},
		{Name: report.Ptr("big"), MemTotal: report.Ptr(24.0)},
		{Name: report.Ptr("small2"), MemTotal: report.Ptr(8.0)},
	}
	r := &report.Report{GPUs: []report.GPU{{Name: report.Ptr("existing")}}}
	appendGPUs(gpus)(r)
	var names []string
	for _, g := range r.GPUs {
		names = append(names, *g.Name)
	}
	if got := strings.Join(names, ","); got != "existing,big,small,small2,unknown" {
		t.Errorf("order = %s", got)
	}
	if appendGPUs(nil) != nil {
		t.Error("no GPUs should give a nil Apply")
	}
}

// An APU's amdgpu hwmon labels its power PPT (the whole package). It is not GPU power.
func TestReadAMDAPUPowerIsNotGPUPower(t *testing.T) {
	gpus := readAMD(filepath.Join("testdata", "apu", "sys"))
	if len(gpus) != 1 || gpus[0].Usage == nil || *gpus[0].Usage != 6 {
		t.Fatalf("APU GPUs = %s", asJSON(t, gpus))
	}
	if gpus[0].Power != nil {
		t.Errorf("APU GPU power = %v, want nil (PPT is package power)", *gpus[0].Power)
	}
	if gpus[0].Temp == nil || *gpus[0].Temp != 41 {
		t.Errorf("APU GPU temp = %v, want 41", gpus[0].Temp)
	}
}
