package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ContractorKeith/machine-monitor/internal/collect"
	"github.com/ContractorKeith/machine-monitor/internal/report"
)

const (
	fakeComputeApps = "101, 4096\n102, 100\n103, 8000\n104, [N/A]\n105, 2048\n106, 3000\n"
	fakeLMS         = `[{"identifier":"qwen3-8b","displayName":"Qwen3 8B","sizeBytes":5000000000},{"modelKey":"gemma-3"}]`
	fakeHistory     = `{"abc":{"prompt":[1,"abc",{
		"10":{"class_type":"LoraLoader","inputs":{"lora_name":"style.safetensors","model":["4",0],"strength_model":1}},
		"4":{"class_type":"CheckpointLoaderSimple","inputs":{"ckpt_name":"sd.safetensors"}},
		"5":{"class_type":"VAELoader","inputs":{"vae_name":"sd.safetensors"}},
		"3":{"class_type":"KSampler","inputs":{"sampler_name":"euler"}}},{},[]]}}`
)

// sentinelEnv is set in the test process; a fake that sees it was run without the
// stripped environment and fails.
const sentinelEnv = "MACHINE_MONITOR_AI_TEST_SENTINEL"

// TestMain doubles as the fake nvidia-smi and lms: the detector runs this test binary with
// "fake-nvidia-smi" or "fake-lms" first. Arguments, not env vars, select the fake because
// the probe strips the environment.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		if os.Getenv(sentinelEnv) != "" {
			os.Exit(3) // the environment leaked through
		}
		args := strings.Join(os.Args[2:], " ")
		switch os.Args[1] {
		case "fake-nvidia-smi":
			if args != "--query-compute-apps=pid,used_memory --format=csv,noheader,nounits" {
				os.Exit(2)
			}
			fmt.Print(fakeComputeApps)
			os.Exit(0)
		case "fake-lms":
			if args != "ps --json" {
				os.Exit(2)
			}
			fmt.Print(fakeLMS)
			os.Exit(0)
		}
	}
	os.Setenv(sentinelEnv, "leaked")
	os.Exit(m.Run())
}

// fakeApps serves Ollama and ComfyUI on loopback and returns their ports.
func fakeApps(t *testing.T, ollama, comfyStats, comfyHistory string) (ollamaPort, comfyPort string) {
	t.Helper()
	serve := func(routes map[string]string) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, ok := routes[r.URL.RequestURI()]
			if !ok || body == "" {
				http.NotFound(w, r)
				return
			}
			w.Write([]byte(body))
		}))
		t.Cleanup(srv.Close)
		_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
		return port
	}
	ollamaPort = serve(map[string]string{"/api/ps": ollama})
	comfyPort = serve(map[string]string{"/system_stats": comfyStats, "/history?max_items=1": comfyHistory})
	return
}

// testDetector returns a detector wired to fakes, with nothing installed or running.
func testDetector(ollamaPort, comfyPort string) *detector {
	return &detector{
		host:       "127.0.0.1",
		ollamaPort: ollamaPort,
		comfyPort:  comfyPort,
		lmsPIDs:    func(context.Context) []int { return nil },
		proc:       func(int) ([]string, string) { return nil, "" },
	}
}

func self(t *testing.T) string {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

func jsonOf(t *testing.T, v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDetectAll(t *testing.T) {
	ollama := `{"models":[{"name":"llama3.2:3b","size":3000000000,"size_vram":2000000000},{"name":"nomic-embed","size":300000000,"size_vram":0}]}`
	ollamaPort, comfyPort := fakeApps(t, ollama, "", fakeHistory)

	comfyHome := filepath.Join(t.TempDir(), "ComfyUI")
	os.MkdirAll(filepath.Join(comfyHome, "models", "checkpoints"), 0o755)
	os.WriteFile(filepath.Join(comfyHome, "models", "checkpoints", "sd.safetensors"), make([]byte, 1234), 0o644)

	procs := map[int][]string{
		101: {"python", "main.py", "--port", comfyPort},
		102: {"python", "tiny.py"},
		103: {"/usr/bin/python3", "/venv/bin/vllm", "serve", "Qwen/Qwen3-8B", "--port", "8000"},
		105: {"/opt/lmstudio/llmster"},
		106: {"/usr/local/bin/ollama", "runner"},
	}
	d := testDetector(ollamaPort, comfyPort)
	d.nvidiaSMI = []string{self(t), "fake-nvidia-smi"}
	d.lms = []string{self(t), "fake-lms"}
	d.lmsPIDs = func(context.Context) []int { return []int{105} }
	d.proc = func(pid int) ([]string, string) {
		if pid == 101 {
			return procs[pid], comfyHome
		}
		return procs[pid], "/"
	}

	got := jsonOf(t, d.detect(context.Background()))
	const mib = 1048576
	want := jsonOf(t, []report.AIGroup{
		{App: report.Ptr("Ollama"), Mem: report.Ptr(2e9), Models: []report.AIModel{
			{Name: report.Ptr("llama3.2:3b"), Size: report.Ptr(3e9)},
			{Name: report.Ptr("nomic-embed"), Size: report.Ptr(3e8)},
		}},
		{App: report.Ptr("LM Studio"), Mem: report.Ptr(2048.0 * mib), Models: []report.AIModel{
			{Name: report.Ptr("Qwen3 8B"), Size: report.Ptr(5e9)},
			{Name: report.Ptr("gemma-3")},
		}},
		{App: report.Ptr("vLLM"), Mem: report.Ptr(8000.0 * mib), Models: []report.AIModel{
			{Name: report.Ptr("Qwen/Qwen3-8B")},
		}},
		{App: report.Ptr("ComfyUI"), Mem: report.Ptr(4096.0 * mib), Models: []report.AIModel{
			{Name: report.Ptr("sd.safetensors"), Size: report.Ptr(1234.0), Kind: report.Ptr("ckpt")},
			{Name: report.Ptr("style.safetensors"), Kind: report.Ptr("lora")},
		}},
	})
	if got != want {
		t.Errorf("detect\n got %s\nwant %s", got, want)
	}
}

// ComfyUI with no GPU process (macOS, Windows, AMD) is found through /system_stats.
func TestComfyFallback(t *testing.T) {
	_, comfyPort := fakeApps(t, "", `{"system":{"comfyui_version":"0.3.40"}}`, fakeHistory)
	d := testDetector("1", comfyPort)
	got := d.detect(context.Background())
	if len(got) != 1 || *got[0].App != "ComfyUI" || got[0].Mem != nil || len(got[0].Models) != 2 {
		t.Fatalf("got %s", jsonOf(t, got))
	}

	// A ComfyUI that never ran a workflow adds no group.
	_, comfyPort = fakeApps(t, "", `{"system":{"comfyui_version":"0.3.40"}}`, `{}`)
	if got := testDetector("1", comfyPort).detect(context.Background()); len(got) != 0 {
		t.Errorf("idle ComfyUI: got %s", jsonOf(t, got))
	}
}

// LM Studio is only asked when its process is running, since `lms` would start it.
func TestLMStudioNotRunning(t *testing.T) {
	d := testDetector("1", "1")
	d.lms = []string{self(t), "fake-lms"}
	if got := d.detect(context.Background()); len(got) != 0 {
		t.Errorf("got %s", jsonOf(t, got))
	}
}

// run reports an empty, non-nil AI when it checked and found nothing, and appends to
// groups another probe may have added.
func TestRunNothingLoaded(t *testing.T) {
	ollamaPort, comfyPort := fakeApps(t, `{"models":[]}`, "", "")
	apply, err := testDetector(ollamaPort, comfyPort).run(context.Background())
	if err != nil || apply == nil {
		t.Fatalf("run = %v, %v", apply, err)
	}
	r := &report.Report{}
	apply(r)
	if r.AI == nil || len(r.AI) != 0 {
		t.Errorf("AI = %#v, want empty non-nil", r.AI)
	}
	if got := jsonOf(t, r.AI); got != "[]" {
		t.Errorf("AI encodes as %s, want []", got)
	}
}

func TestRunCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := testDetector("1", "1").run(ctx); err == nil {
		t.Error("want error from a canceled run")
	}
}

// Only loopback answers count: a redirect elsewhere is not followed.
func TestNoRedirects(t *testing.T) {
	srv := httptest.NewServer(http.RedirectHandler("http://example.com/api/ps", http.StatusFound))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	if g := testDetector(port, "1").ollama(context.Background()); g != nil {
		t.Errorf("got %s", jsonOf(t, g))
	}
}

func TestClassify(t *testing.T) {
	dir := t.TempDir()
	gguf := filepath.Join(dir, "qwen3-8b-q4.gguf")
	os.WriteFile(gguf, nil, 0o644)
	os.WriteFile(filepath.Join(dir, "rel.gguf"), nil, 0o644)

	for _, tc := range []struct {
		argv       []string
		cwd        string
		app, model string
	}{
		{[]string{"/venv/bin/python3", "/venv/bin/vllm", "serve", "Qwen/Qwen3-8B"}, "", "vLLM", "Qwen/Qwen3-8B"},
		{[]string{"python", "-m", "vllm.entrypoints.openai.api_server", "--model", "meta-llama/Llama-3.1-8B"}, "", "vLLM", "meta-llama/Llama-3.1-8B"},
		{[]string{"vllm", "serve", "--port", "8000"}, "", "vLLM", ""},
		{[]string{"python3", "-m", "sglang.launch_server", "--model-path", "deepseek-ai/DeepSeek-V3"}, "", "SGLang", "deepseek-ai/DeepSeek-V3"},
		{[]string{"python3", "-m", "sglang.launch_server", "--model-path=" + dir + "/"}, "", "SGLang", filepath.Base(dir)},
		{[]string{"/opt/llama.cpp/llama-server", "-m", gguf, "-ngl", "99"}, "", "llama.cpp", "qwen3-8b-q4.gguf"},
		{[]string{"./llama-server", "--model=rel.gguf"}, dir, "llama.cpp", "rel.gguf"},
		{[]string{"./llama-server", "-m", "missing/x.gguf"}, dir, "llama.cpp", "missing/x.gguf"},
		{[]string{"/usr/bin/python3", "train.py"}, "", "python3", ""},
		{[]string{"/opt/app/bin/tabbyAPI", "-m"}, "", "tabbyAPI", ""},
	} {
		t.Run(strings.Join(tc.argv, " "), func(t *testing.T) {
			app, model := classify(tc.argv, tc.cwd)
			if app != tc.app || model != tc.model {
				t.Errorf("classify = %q, %q; want %q, %q", app, model, tc.app, tc.model)
			}
		})
	}
}

func TestParseComputeApps(t *testing.T) {
	got := parseComputeApps("12, 100\n12, 50\n13, [N/A]\nNo running processes found\n\n -1, 5\n14,0\n")
	if len(got) != 2 || got[12] != 150*1048576 || got[14] != 0 {
		t.Errorf("got %v", got)
	}
}

func TestModelSizeStaysInRoot(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "loras"), 0o755)
	os.WriteFile(filepath.Join(root, "secret"), []byte("x"), 0o644)
	for _, name := range []string{"../secret", "", filepath.Join(root, "secret")} {
		if s := modelSize([]string{root}, name); s != nil {
			t.Errorf("modelSize(%q) = %v, want nil", name, *s)
		}
	}
}

// The exec fakes fail when they see the test process's environment, so a missing
// cmd.Env = strippedEnv() turns every exec test red.
func TestExecEnvStripped(t *testing.T) {
	if out := runCmd(context.Background(), self(t), "fake-lms", "ps", "--json"); out != fakeLMS {
		t.Fatalf("fake-lms under runCmd = %q", out)
	}
	cmd := exec.Command(self(t), "fake-lms", "ps", "--json") // inherits the environment
	if err := cmd.Run(); err == nil {
		t.Fatal("the fake did not notice the sentinel")
	}
}

// Windows, as agent.ps1: LM Studio and ComfyUI memory come from GPU process memory by pid,
// and ComfyUI is checked when its port has a listener, without /system_stats.
func TestWindowsGPUProcessMemory(t *testing.T) {
	_, comfyPort := fakeApps(t, "", "", fakeHistory)
	d := testDetector("1", comfyPort)
	d.lms = []string{self(t), "fake-lms"}
	d.lmsPIDs = func(context.Context) []int { return []int{40, 41} }
	queries := 0
	d.gpuMem = func(context.Context) map[int]float64 {
		queries++
		return map[int]float64{40: 1e9, 41: 5e8, 77: 3e9, 99: 1}
	}
	d.listenPID = func(_ context.Context, port string) int {
		if port != comfyPort {
			t.Errorf("listener asked for port %s", port)
		}
		return 77
	}
	got := d.detect(context.Background())
	if len(got) != 2 || *got[0].App != "LM Studio" || got[0].Mem == nil || *got[0].Mem != 1.5e9 ||
		*got[1].App != "ComfyUI" || got[1].Mem == nil || *got[1].Mem != 3e9 || len(got[1].Models) != 2 {
		t.Fatalf("got %s", jsonOf(t, got))
	}
	if queries != 1 {
		t.Errorf("GPU memory queried %d times, want 1", queries)
	}

	// No listener: ComfyUI isn't asked. No GPU memory for the pid: mem is null.
	d.listenPID = func(context.Context, string) int { return 0 }
	d.gpuMem = func(context.Context) map[int]float64 { return nil }
	got = d.detect(context.Background())
	if len(got) != 1 || *got[0].App != "LM Studio" || got[0].Mem != nil {
		t.Fatalf("got %s", jsonOf(t, got))
	}
}

func TestParseGPUProcMem(t *testing.T) {
	got := parseGPUProcMem([]gpuProcMemRow{
		{"pid_12_luid_0x00000000_0x0000D1A4_phys_0", 100},
		{"pid_12_luid_0x00000000_0x0000E000_phys_0", 50},
		{"pid_0_luid_0x0_phys_0", 7},
		{"_Total", 999},
		{"pid_13_luid_0x0_phys_0", 0},
	})
	if len(got) != 2 || got[12] != 150 || got[13] != 0 {
		t.Errorf("got %v", got)
	}
	if m := memOf(got, 13); m != nil {
		t.Errorf("memOf zero = %v, want nil", *m)
	}
}

func TestParseTasklist(t *testing.T) {
	out := "\"LM Studio.exe\",\"4012\",\"Console\",\"1\",\"312,004 K\"\r\n" +
		"\"llmster.exe\",\"4100\",\"Console\",\"1\",\"10,000 K\"\r\n" +
		"\"chrome.exe\",\"5000\",\"Console\",\"1\",\"1 K\"\r\n" +
		"INFO: garbage \"line\r\n"
	if got := parseTasklist(out, "lm studio.exe", "llmster.exe"); !slices.Equal(got, []int{4012, 4100}) {
		t.Errorf("got %v", got)
	}
}

func TestRegistered(t *testing.T) {
	for _, p := range collect.Probes() {
		if p.Name == "ai/models" {
			if p.Every != 5 {
				t.Errorf("Every = %d, want 5", p.Every)
			}
			return
		}
	}
	t.Error("ai/models not registered")
}
