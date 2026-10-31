package ai

// Detection logic ported from Forward-Future/device-monitor agent/agent.sh (AI_PY),
// agent/agent-macos.sh and agent/agent.ps1 (Get-AiModels) at c3a4c41.
// MIT License, Copyright (c) 2026 Matt: https://github.com/Forward-Future/device-monitor

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/collect"
	"github.com/ContractorKeith/machine-monitor/internal/report"
)

const (
	every       = 5                       // run on every 5th sample, as upstream's agents do
	timeout     = 4 * time.Second         // whole probe; lms is a Node CLI and can take ~1 s
	httpTimeout = 1500 * time.Millisecond // per request, upstream's urlopen timeout
	minGPUMem   = 256 << 20               // ignore GPU processes holding less than this
	maxGroups   = 8                       // upstream sends at most 8 groups
)

var (
	lmsProcRe  = regexp.MustCompile(`lmstudio|llmster`)
	skipProcRe = regexp.MustCompile(`ollama|lmstudio|llmster`) // reported by their own APIs
)

func init() {
	d := newDetector()
	collect.Register(collect.Probe{Name: "ai/models", Every: every, Timeout: timeout, Run: d.run})
}

// detector finds loaded AI models. Its fields are the seams tests replace.
type detector struct {
	host       string // always loopback; never configurable from outside
	ollamaPort string
	comfyPort  string // default ComfyUI port when a process doesn't pass --port
	nvidiaSMI  []string
	lms        []string // command prefix for `lms`; lms[0] must exist
	lmsRunning func(ctx context.Context) bool
	proc       func(pid int) (argv []string, cwd string) // nil argv when unreadable
	comfyRoots []string                                  // extra ComfyUI models dirs to size files in
}

// newDetector returns a detector wired to this machine's tools.
func newDetector() *detector {
	d := &detector{
		host:       "127.0.0.1",
		ollamaPort: "11434",
		comfyPort:  "8188",
		lmsRunning: lmsRunning,
		proc:       procInfo,
	}
	home, _ := os.UserHomeDir()
	if home != "" {
		lms := filepath.Join(home, ".lmstudio", "bin", "lms")
		if runtime.GOOS == "windows" {
			lms += ".exe"
			d.comfyRoots = []string{filepath.Join(home, "Documents", "ComfyUI", "models")}
		}
		d.lms = []string{lms}
	}
	// GPU compute processes need a readable cmdline and cwd, which only Linux gives us
	// without extra dependencies (upstream's macOS agent reads /proc too, so finds none).
	if runtime.GOOS == "linux" {
		if p, err := lookPath("nvidia-smi"); err == nil {
			d.nvidiaSMI = []string{p}
		}
	}
	return d
}

// run is the probe's Run. It always returns an Apply on success, so a check that found
// nothing still sets r.AI to an empty, non-nil slice.
func (d *detector) run(ctx context.Context) (collect.Apply, error) {
	groups := d.detect(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err // partial results would look like "nothing loaded"
	}
	return func(r *report.Report) {
		if r.AI == nil {
			r.AI = []report.AIGroup{}
		}
		r.AI = append(r.AI, groups...)
	}, nil
}

// detect runs every check and returns the groups in upstream's order: Ollama, LM Studio,
// GPU processes by memory, then ComfyUI found only through its HTTP API.
func (d *detector) detect(ctx context.Context) []report.AIGroup {
	var (
		wg     sync.WaitGroup
		gpu    map[int]float64
		ollama *report.AIGroup
		lms    []lmsModel
	)
	wg.Add(3)
	go func() { defer wg.Done(); gpu = d.gpuProcs(ctx) }()
	go func() { defer wg.Done(); ollama = d.ollama(ctx) }()
	go func() { defer wg.Done(); lms = d.lmsModels(ctx) }()
	wg.Wait()

	groups := []report.AIGroup{}
	if ollama != nil {
		groups = append(groups, *ollama)
	}
	if len(lms) > 0 {
		g := report.AIGroup{App: report.Ptr("LM Studio"), Models: []report.AIModel{}}
		var mem float64
		for pid, used := range gpu {
			if argv, _ := d.proc(pid); lmsProcRe.MatchString(strings.ToLower(strings.Join(argv, " "))) {
				mem += used
			}
		}
		if mem > 0 {
			g.Mem = report.Ptr(mem)
		}
		for _, m := range lms {
			g.Models = append(g.Models, report.AIModel{Name: m.name(), Size: m.SizeBytes})
		}
		groups = append(groups, g)
	}

	haveComfy := false
	for _, p := range byMemDesc(gpu) {
		used := gpu[p]
		argv, cwd := d.proc(p)
		cmd := strings.ToLower(strings.Join(argv, " "))
		if len(argv) == 0 || used < minGPUMem || skipProcRe.MatchString(cmd) {
			continue
		}
		if strings.Contains(cmd, "comfy") || strings.Contains(strings.ToLower(cwd), "comfy") {
			port := argValue(argv, "--port")
			if port == "" {
				port = d.comfyPort
			}
			groups = append(groups, d.comfy(ctx, port, cwd, report.Ptr(used)))
			haveComfy = true
			continue
		}
		app, model := classify(argv, cwd)
		g := report.AIGroup{App: report.Ptr(app), Mem: report.Ptr(used), Models: []report.AIModel{}}
		if model != "" {
			g.Models = append(g.Models, report.AIModel{Name: report.Ptr(model)})
		}
		groups = append(groups, g)
	}

	if !haveComfy && d.comfyRunning(ctx) {
		if g := d.comfy(ctx, d.comfyPort, "", nil); len(g.Models) > 0 {
			groups = append(groups, g)
		}
	}
	if len(groups) > maxGroups {
		groups = groups[:maxGroups]
	}
	return groups
}

// byMemDesc returns the pids sorted by GPU memory, largest first (pid breaks ties).
func byMemDesc(gpu map[int]float64) []int {
	pids := make([]int, 0, len(gpu))
	for p := range gpu {
		pids = append(pids, p)
	}
	sort.Slice(pids, func(i, j int) bool {
		if gpu[pids[i]] != gpu[pids[j]] {
			return gpu[pids[i]] > gpu[pids[j]]
		}
		return pids[i] < pids[j]
	})
	return pids
}

// classify names the app behind a GPU process from its argv, and the model it serves if
// the command line says. A model path that exists is shortened to its base name; a hub
// id like "Qwen/Qwen3-8B" is kept whole. Relative paths resolve against the process cwd.
func classify(argv []string, cwd string) (app, model string) {
	cmd := strings.ToLower(strings.Join(argv, " "))
	switch {
	case strings.Contains(cmd, "vllm"):
		app = "vLLM"
	case strings.Contains(cmd, "sglang"):
		app = "SGLang"
	case strings.Contains(cmd, "llama-server"):
		app = "llama.cpp"
	default:
		app = filepath.Base(argv[0])
	}
	// Upstream reads "python -m vllm..." as model "vllm...": skip the interpreter's -m.
	args := argv
	if len(args) > 2 && strings.HasPrefix(filepath.Base(args[0]), "python") && args[1] == "-m" {
		args = args[3:]
	}
	model = argValue(args, "--model", "-m", "--model-path")
	if model == "" {
		for i, a := range args {
			if a == "serve" {
				if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
					model = args[i+1]
				}
				break
			}
		}
	}
	if model != "" {
		p := model
		if !filepath.IsAbs(p) && cwd != "" {
			p = filepath.Join(cwd, p)
		}
		if _, err := os.Stat(p); err == nil {
			model = filepath.Base(strings.TrimRight(model, "/"))
		}
	}
	return app, model
}

// argValue returns the value of the first of names in argv, as "--x v" or "--x=v".
func argValue(argv []string, names ...string) string {
	for i, a := range argv {
		for _, n := range names {
			if a == n && i+1 < len(argv) {
				return argv[i+1]
			}
			if strings.HasPrefix(a, n+"=") {
				return a[len(n)+1:]
			}
		}
	}
	return ""
}
