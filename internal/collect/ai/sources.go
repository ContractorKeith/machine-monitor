package ai

// Ported from Forward-Future/device-monitor agent/agent.sh (AI_PY) and agent/agent.ps1
// (Get-AiModels) at c3a4c41. MIT License, Copyright (c) 2026 Matt.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/report"
)

const maxBody = 8 << 20 // cap on any HTTP or exec output we parse

// client talks only to loopback apps: no proxy, no redirects.
var client = &http.Client{
	Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true},
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("redirects not followed")
	},
}

// getJSON fetches http://host:port/path into v with upstream's 1.5 s timeout.
func (d *detector) getJSON(ctx context.Context, port, path string, v any) error {
	ctx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(d.host, port)+path, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New(resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(v)
}

// runCmd runs argv with ctx's deadline and a stripped environment and returns stdout, or
// "" on any failure.
func runCmd(ctx context.Context, argv ...string) string {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = strippedEnv()
	cmd.WaitDelay = 500 * time.Millisecond
	var out bytes.Buffer
	cmd.Stdout = &limitWriter{w: &out, n: maxBody}
	if err := cmd.Run(); err != nil {
		return ""
	}
	return out.String()
}

// strippedEnv keeps only what the vendor tools need to find their binaries and config.
func strippedEnv() []string {
	var env []string
	for _, k := range []string{"PATH", "HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "SystemRoot", "TEMP", "TMP", "TMPDIR"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// limitWriter drops output past n bytes.
type limitWriter struct {
	w io.Writer
	n int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if l.n > 0 {
		k := min(len(p), l.n)
		l.n -= k
		l.w.Write(p[:k])
	}
	return len(p), nil
}

// lookPath finds a tool on PATH.
func lookPath(name string) (string, error) { return exec.LookPath(name) }

// gpuProcs maps pid to GPU memory in bytes from nvidia-smi's compute apps; nil when
// nvidia-smi is missing or fails.
func (d *detector) gpuProcs(ctx context.Context) map[int]float64 {
	if len(d.nvidiaSMI) == 0 {
		return nil
	}
	argv := append(append([]string{}, d.nvidiaSMI...), "--query-compute-apps=pid,used_memory", "--format=csv,noheader,nounits")
	return parseComputeApps(runCmd(ctx, argv...))
}

// parseComputeApps parses "pid, used_memory_MiB" lines, summing a pid seen on several GPUs.
func parseComputeApps(out string) map[int]float64 {
	gpu := map[int]float64{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, ",")
		if len(f) != 2 {
			continue
		}
		pid, err1 := strconv.Atoi(strings.TrimSpace(f[0]))
		mib, err2 := strconv.ParseFloat(strings.TrimSpace(f[1]), 64)
		if err1 != nil || err2 != nil || pid <= 0 || !(mib >= 0) || math.IsInf(mib, 0) {
			continue // "[N/A]", "[Insufficient Permissions]", ...
		}
		gpu[pid] += mib * 1048576
	}
	return gpu
}

// ollama reads the models Ollama has in memory from /api/ps; nil when it isn't running or
// has nothing loaded.
func (d *detector) ollama(ctx context.Context) *report.AIGroup {
	var j struct {
		Models []struct {
			Name     *string  `json:"name"`
			Size     *float64 `json:"size"`
			SizeVRAM *float64 `json:"size_vram"`
		} `json:"models"`
	}
	if d.getJSON(ctx, d.ollamaPort, "/api/ps", &j) != nil || len(j.Models) == 0 {
		return nil
	}
	g := &report.AIGroup{App: report.Ptr("Ollama"), Models: []report.AIModel{}}
	var vram float64
	for _, m := range j.Models {
		if m.SizeVRAM != nil {
			vram += *m.SizeVRAM
		}
		g.Models = append(g.Models, report.AIModel{Name: m.Name, Size: m.Size})
	}
	if vram > 0 {
		g.Mem = report.Ptr(vram)
	}
	return g
}

// lmsModel is one entry of `lms ps --json`.
type lmsModel struct {
	DisplayName *string  `json:"displayName"`
	Identifier  *string  `json:"identifier"`
	ModelKey    *string  `json:"modelKey"`
	SizeBytes   *float64 `json:"sizeBytes"`
}

// name is the first of displayName, identifier and modelKey that is set.
func (m lmsModel) name() *string {
	for _, s := range []*string{m.DisplayName, m.Identifier, m.ModelKey} {
		if s != nil && *s != "" {
			return s
		}
	}
	return nil
}

// lmsModels asks the LM Studio CLI what is loaded. Only when LM Studio is already running:
// `lms` starts its server otherwise.
func (d *detector) lmsModels(ctx context.Context) []lmsModel {
	if len(d.lms) == 0 {
		return nil
	}
	if _, err := os.Stat(d.lms[0]); err != nil || !d.lmsRunning(ctx) {
		return nil
	}
	argv := append(append([]string{}, d.lms...), "ps", "--json")
	var arr []lmsModel
	if json.Unmarshal([]byte(runCmd(ctx, argv...)), &arr) != nil {
		return nil
	}
	return arr
}

// lmsRunning reports whether an LM Studio app or llmster daemon process exists.
func lmsRunning(ctx context.Context) bool {
	if runtime.GOOS == "windows" {
		out := strings.ToLower(runCmd(ctx, "tasklist", "/FO", "CSV", "/NH"))
		return strings.Contains(out, `"lm studio.exe"`) || strings.Contains(out, `"llmster.exe"`)
	}
	return strings.TrimSpace(runCmd(ctx, "pgrep", "-f", `llmster|LM Studio|lmstudio`)) != ""
}

// comfyRunning reports whether a ComfyUI server answers on the default port.
func (d *detector) comfyRunning(ctx context.Context) bool {
	var j struct {
		System struct {
			Version string `json:"comfyui_version"`
		} `json:"system"`
	}
	return d.getJSON(ctx, d.comfyPort, "/system_stats", &j) == nil && j.System.Version != ""
}

// comfy lists the models ComfyUI loaded. ComfyUI doesn't say what it holds in memory, so
// this uses the loader nodes of the last workflow it ran. home is the server's working
// directory (its models live in home/models); file sizes are looked up there and in
// d.comfyRoots.
func (d *detector) comfy(ctx context.Context, port, home string, mem *float64) report.AIGroup {
	g := report.AIGroup{App: report.Ptr("ComfyUI"), Mem: mem, Models: []report.AIModel{}}
	var hist map[string]struct {
		Prompt []json.RawMessage `json:"prompt"`
	}
	if d.getJSON(ctx, port, "/history?max_items=1", &hist) != nil {
		return g
	}
	roots := d.comfyRoots
	if home != "" {
		roots = append([]string{filepath.Join(home, "models")}, roots...)
	}
	seen := map[string]bool{}
	for _, k := range sortedKeys(hist) {
		item := hist[k]
		if len(item.Prompt) < 3 {
			continue
		}
		var nodes map[string]struct {
			ClassType any                        `json:"class_type"`
			Inputs    map[string]json.RawMessage `json:"inputs"`
		}
		if json.Unmarshal(item.Prompt[2], &nodes) != nil {
			continue
		}
		for _, id := range sortedKeys(nodes) {
			node := nodes[id]
			if ct, _ := node.ClassType.(string); !strings.Contains(ct, "Loader") {
				continue
			}
			for _, k := range sortedKeys(node.Inputs) {
				var v string
				if !strings.HasSuffix(k, "_name") || json.Unmarshal(node.Inputs[k], &v) != nil || seen[v] {
					continue
				}
				seen[v] = true
				g.Models = append(g.Models, report.AIModel{
					Name: report.Ptr(v),
					Size: modelSize(roots, v),
					Kind: report.Ptr(strings.TrimSuffix(k, "_name")),
				})
			}
		}
	}
	return g
}

// modelSize finds name in any subfolder of roots (checkpoints/, loras/, ...) and returns its
// size, or nil. Names that could escape the root are not looked up.
func modelSize(roots []string, name string) *float64 {
	if name == "" || filepath.IsAbs(name) || strings.Contains(name, "..") {
		return nil
	}
	for _, root := range roots {
		subs, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, sub := range subs {
			if !sub.IsDir() {
				continue
			}
			if fi, err := os.Stat(filepath.Join(root, sub.Name(), name)); err == nil && fi.Mode().IsRegular() {
				return report.Ptr(float64(fi.Size()))
			}
		}
	}
	return nil
}

// sortedKeys returns m's keys in a stable order: numeric ids ascending, then the rest.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, errA := strconv.Atoi(keys[i])
		b, errB := strconv.Atoi(keys[j])
		switch {
		case errA == nil && errB == nil:
			return a < b
		case (errA == nil) != (errB == nil):
			return errA == nil
		}
		return keys[i] < keys[j]
	})
	return keys
}
