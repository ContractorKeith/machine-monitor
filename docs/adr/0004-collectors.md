# 0004. Metric collectors and probes

Status: Accepted (2026-11-27)

## Context

These records hold the non-obvious reasons behind the code in this area. They were collected
from the source comments removed under [ADR 0001](0001-no-code-comments.md). Each section is
one decision. When code here changes, update or retire the section that covers it.

## Decisions

### Probe contract and collector

**Context:** Probes (GPU, sensors, AI) are optional, slow sources that must never stall or break the 2 s sample; Windows WMI calls take no context and some reads (statfs on hung NFS) ignore ctx.

**Decision:** Probes register from init and are run concurrently by one probeRunner; Run returns an Apply, applied in name order on the collector's goroutine. Every N means Run on every Nth sample and the last Apply is reapplied in between (as upstream keeps $AI). A probe that errors, times out or panics contributes nothing until its next success; one still running past its deadline is not restarted (busy flag cleared before the send so the next sample never sees stale busy). Apply appends to slices and fills scalars only when nil; AI probes make r.AI non-nil first because nil means 'not checked'. Core reads share one deadline below the 2 s cadence, run via async/await so a stuck read yields nil; WMI queries run on their own goroutine and a late result is written to a result nobody reads (a wmi type mismatch panic is recovered).

**Consequences:** Do not 'simplify' away the goroutines, panic recovery, busy flag or deadline budgets; each prevents a stuck command from freezing the monitor.

**Where:** `internal/collect/collect.go`, `internal/collect/collect_windows.go`, `internal/collect/core.go`, `internal/collect/oswindows.go`, `internal/collect/probes.go`

### Exec hygiene and loopback-only HTTP

**Context:** Collectors run vendor tools and talk to local apps on behalf of untrusted environment/PATH state.

**Decision:** Every exec has a ctx deadline, a stripped environment (PATH, SystemRoot, fixed locale so numbers parse; never a nil Env, which os/exec treats as 'inherit everything') and WaitDelay so a killed child's open pipes don't hang. HTTP to Ollama/ComfyUI goes only to loopback with no proxy and no redirects; the host is never configurable. The tailscale CLI is found only at fixed paths, never via PATH, and its stdout is kept on non-zero exit (the JSON decides). Homebrew smctemp locations are listed explicitly because launchd agents get a PATH without them. Tests run the test binary as the fake tool and select the fake by argument, because the probe strips the environment; the fakes fail if the environment leaks.

**Consequences:** Removing the env strip or the loopback pin lets user environment change what runs or sends requests elsewhere.

**Where:** `internal/collect/ai/ai.go`, `internal/collect/ai/ai_test.go`, `internal/collect/ai/sources.go`, `internal/collect/collect_darwin.go`, `internal/collect/gpu/util.go`, `internal/collect/sensors/sensors_darwin.go`, `internal/collect/sensors/util.go`, `internal/tailscale/tailscale.go`, `internal/tailscale/tailscale_test.go`

### WMI worker and Windows perf-counter quirks

**Context:** yusufpapurcu/wmi holds a package-global lock and a locked OS thread for the whole of a query and cannot be cancelled. Windows also has class and counter quirks.

**Decision:** All probe WMI queries go through one wmiq worker goroutine, one at a time; callers that give up only stop waiting (buffered result so the worker never blocks), and a caller hands over a query only when the worker is idle, so abandoned queries can't pile up and block all WMI (gopsutil included). After a ctx error from Do, fn may still be running or never have run, so callers must not read what it writes. WMI is slow (GPUEngine can take seconds) so Windows probes run every 5th sample. WMI class names are not localized (PDH paths are); Win32_VideoController AdapterRAM is capped at 4 GiB; PowerShell -match is case-insensitive so the Go regexes are too; main.py command-line parsing handles quoted vs unquoted paths; Windows has no cmdline matching for GPU processes, so GPU memory is read by pid from perf counters. Memory is ullTotalPhys-ullAvailPhys (a deliberate difference from upstream, firmware-reserved totals differ) and clock is max clock scaled by % Processor Performance as Task Manager shows.

**Consequences:** Keep a single shared worker; do not give each caller its own goroutine.

**Where:** `internal/collect/ai/ai.go`, `internal/collect/ai/gpumem.go`, `internal/collect/gpu/generic.go`, `internal/collect/gpu/gpu_windows.go`, `internal/collect/oswindows.go`, `internal/collect/sensors/lhm.go`, `internal/collect/sensors/sensors_windows.go`, `internal/collect/wmiq/query_windows.go`, `internal/collect/wmiq/queue.go`

### Upstream parity in core metrics

**Context:** The report must match what upstream's agents produce, with a few deliberate fixes.

**Decision:** CPU usage is 100 x (1 - didle/dtotal) with iowait counted as idle and guest excluded (Linux already counts it in user); byte rates clamp counter resets to 0; MACs for Wake-on-LAN are globally administered unicast only (locally administered ones belong to bridges, containers, VPNs, randomized Wi-Fi), deduplicated, max 8. Linux CPU name falls back to gopsutil then device-tree for ARM; disk filters mirror upstream's df -x list and mount drops, and additionally dedupe repeat mounts of one /dev/ block device (shortest mount) but not non-/dev sources. macOS: no current clock (null), memory = active+wired+compressed like Activity Monitor, APFS used = total - available, only en* count toward the network total, netstat columns are located from the header and counted from the end because rows without an Address are one field short.

**Consequences:** Changing these changes numbers hubs compare against upstream reports.

**Where:** `internal/collect/core.go`, `internal/collect/oslinux.go`, `internal/collect/osmacos.go`

### AMD APU package power

**Context:** On an APU the amdgpu hwmon power label is PPT: whole-package power including CPU cores.

**Decision:** The GPU probe reports no GPU power for it (upstream reads only power1_average, so null) and flags the GPU integrated so the hub adds no discrete-GPU estimate; the sensors probe reports it as CPU package power, used after Grace and RAPL.

**Consequences:** Do not move PPT into GPU power.

**Where:** `internal/collect/gpu/amd.go`, `internal/collect/sensors/linux.go`

### CPU power meters (RAPL, Grace)

**Context:** RAPL energy_uj is root-only on most kernels since 2020; counters wrap; psys overlaps packages.

**Decision:** Power is nil (never 0) when unreadable and the hub estimates from load. Only package zones count (psys on some laptops would double count the CPU; upstream sums every zone). A wrap with no readable max_energy_range_uj is skipped rather than reporting negative watts. RAPL is read every Run even while Grace answers so its baseline stays fresh.

**Consequences:** Priority is Grace, then RAPL, then APU PPT.

**Where:** `internal/collect/sensors/linux.go`

### Sensor readings and GPU parity

**Context:** Sensors lie: disconnected ones read 0, -273 or 255.

**Decision:** temp() rejects implausible readings and returns nil; smctemp output of 0 or nothing means unsupported; fans count only if spinning (agent.ps1); hwmon sysfs power may be power1_average (older) or power1_input (newer), in microwatts; Apple GPU usage uses Device Utilization % on Apple Silicon and GPU Activity(%) on older Macs; nvidia memory of 0/missing is unknown; GPUs sort biggest-VRAM first, unknown last, ties stable (upstream server.js); thermal zones are top-level only (not intel-rapl sub-zones).

**Consequences:** Preserve nil-for-unknown.

**Where:** `internal/collect/gpu/amd.go`, `internal/collect/gpu/apple.go`, `internal/collect/gpu/nvidia.go`, `internal/collect/gpu/util.go`, `internal/collect/sensors/linux.go`, `internal/collect/sensors/smctemp.go`, `internal/collect/sensors/util.go`

### One probe per OS

**Context:** A vendor-per-probe layout cannot order GPUs across vendors or decide fallbacks.

**Decision:** There is one gpu probe and one sensors probe per OS. GPU can sort every adapter biggest-VRAM first and on Windows use generic perf counters only when nvidia-smi found nothing. The sensors probe name sorts after gpu so its Apply runs later and fills GPU temperatures the GPU probe left nil (LHM GPU Core, smctemp -g).

**Consequences:** Do not split probes per vendor.

**Where:** `internal/collect/gpu/doc.go`, `internal/collect/sensors/doc.go`

### AI probe detection details

**Context:** Ported from upstream's AI detection with some local safety changes.

**Decision:** lms is a Node CLI (~1 s) and starts LM Studio's server, so it is only asked when LM Studio is already running; run must set r.AI to an empty non-nil slice on success (partial results would look like 'nothing loaded'); GPU compute processes need a readable cmdline/cwd, which only Linux gives without deps; 'python -m vllm' is classified as the module, skipping -m (upstream reads it as 'vllm...'); ComfyUI doesn't report what it holds, so loader nodes of the last workflow are used; model names that could escape the models root are never stat'ed.

**Consequences:** Keep the lms gate and path-escape guard.

**Where:** `internal/collect/ai/ai.go`, `internal/collect/ai/ai_test.go`, `internal/collect/ai/sources.go`

### Data directory and atomic file writes

**Context:** Data files hold secrets (hub token, keys).

**Decision:** All files live under os.UserConfigDir()/machine-monitor or MACHINE_MONITOR_DIR (tests, second instance) and are written via WriteFile0600: temp file in the same dir, mode 0600, fsync, rename, parents 0700, so a crash never leaves a half-written file. On Windows mode bits are ignored and the profile ACL applies.

**Consequences:** Never write data files with os.WriteFile.

**Where:** `internal/config/config.go`

### Tailscale status shape and CLI lookup

**Context:** The hub and clients read Tailscale state in upstream's JSON shape.

**Decision:** Status keys are always present (null for unknown, [] for no URLs, never nil). Candidates for the CLI include the normal installs plus a Windows user-mode install in %LOCALAPPDATA%\dm-tailscale with its own pipe; the candidate list is a var so tests replace it.

**Consequences:** Keep the JSON shape stable.

**Where:** `internal/tailscale/tailscale.go`

### Text cleaning policy

**Context:** Text from other machines reaches terminals, logs and alerts (skeptic pass 3, N15/N18).

**Decision:** One policy in textclean, used by report.CleanText, the TUI and alerts: strip escape sequences (ESC/C1 CSI, OSC incl. OSC 8, DCS, APC, PM, SOS), control chars, bidi controls, invisible/zero-width characters, tag characters, invalid UTF-8; Clean spaces tabs/line breaks, Strip (for report.CleanText, matching upstream cleanText) deletes them. ZWJ/ZWNJ/U+FE0F are kept only where legitimate (emoji sequences, joining scripts, after marks, keycaps); joiners are judged after unconditional removals so they see final neighbors. Clean returns the input uncopied when Needs is false.

**Consequences:** Do not weaken or fork the policy per caller.

**Where:** `internal/textclean/textclean.go`

### Empty versus missing AI data

**Context:** Collectors return nil for "unknown" (AGENTS.md). The AI probe can also finish and find nothing loaded.

**Decision:** The AI probe always returns an Apply on success, setting `r.AI` to an empty, non-nil slice. On context cancellation it returns an error instead of partial groups.

**Consequences:** Consumers can tell "checked, nothing loaded" from "unknown". Partial results would look like "nothing loaded", so do not return them. Do not collapse empty to nil.

**Where:** `internal/collect/ai/ai.go`, `internal/collect/ai/ai_test.go`

### Probe ordering and Windows/mac/Linux GPU quirks

**Context:** GPU data comes from different tools per OS, and some fields are filled by other probes.

**Decision:** macOS GPU temperature is not read by the GPU probe; the sensors probe (smctemp -g) fills it after the GPU Apply, so ordering matters. Apple GPUs have no memory total (unified memory) and an absent name falls back to "<CPU name> GPU" or "Apple GPU". On Linux, AMD GPUs are those DRM cards exposing `gpu_busy_percent`, which only amdgpu provides; NVIDIA and AMD reads fail independently, so a failing nvidia-smi does not hide AMD cards. On Windows, nvidia-smi is used when it finds GPUs, otherwise the generic perf counters via WMI, as upstream agent.ps1; adapters are filtered as agent.ps1 does (Basic Display Adapter and Copy engines ignored, idle adapters with no memory skipped, idle adapters holding memory kept). Win32_VideoController names and memory totals are applied by position only when the controller count equals the adapter count; otherwise every adapter stays "GPU" with no total, since the perf-counter (LUID) order and the WMI order are unrelated (#90). Windows-only AI hooks (GPU memory per pid, port listener, command line) and the ComfyUI check without /system_stats mirror agent.ps1.

**Consequences:** Reordering probes or "simplifying" detection breaks temperature, names, or upstream parity.

**Where:** `internal/collect/gpu/gpu_darwin.go`, `internal/collect/gpu/apple.go`, `internal/collect/gpu/amd.go`, `internal/collect/gpu/gpu_linux.go`, `internal/collect/gpu/gpu_windows.go`, `internal/collect/gpu/gpu_test.go`, `internal/collect/ai/ai.go`, `internal/collect/ai/ai_test.go`

### Collect deadline and device identity

**Context:** Reads can hang (statfs, WMI).

**Decision:** Core reads run concurrently under one deadline, with probes running alongside and applied last; a source ignoring ctx must not block collection. DeviceID reads the same identity as reports without running probes (macOS needs only ioreg; fall back to hostname when no machine-id, as upstream).

**Consequences:** Identity must stay consistent between DeviceID and reports, or devices duplicate on the hub.

**Where:** `internal/collect/core.go`, `internal/collect/core_test.go`, `internal/collect/collect.go`, `internal/collect/oslinux_test.go`

### Platform parsing, units and gaps

**Context:** netstat layout varies by macOS version; RAPL counters wrap; devices go off.

**Decision:** netstat byte columns come from the header; a RAPL wrap with unreadable range skips the zone (never negative watts); energy gaps add nothing and are not interpolated.

**Consequences:** Hard-coded columns or interpolation yield wrong numbers.

**Where:** `internal/collect/osmacos_test.go`, `internal/collect/sensors/sensors_test.go`, `internal/power/energy_test.go`

### Linux network total keeps upstream's interface sum

**Context:** Upstream `agent.sh` sums every interface except `lo`, so traffic over `docker0`, `tailscale0` or a bridge is counted on the virtual link and again on the physical one.

**Decision:** The Linux collector keeps upstream's sum for report parity. The per-interface rates in the report show each link on its own.

**Consequences:** The Linux Net total can read high on machines with VPNs or containers. Changing it is a deliberate break from upstream parity and needs this record updated.

**Where:** `internal/collect/oslinux.go`
