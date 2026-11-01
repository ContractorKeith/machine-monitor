# machine-monitor — Build Plan

Status: M0–M8 built; three review fix rounds merged and a fourth repair round addresses #75–#81 (2026-11-01). Native macOS/Windows release acceptance and the known open items remain. Written 2026-10-31 from a full read of
[Forward-Future/device-monitor](https://github.com/Forward-Future/device-monitor) at commit
`c3a4c41` (v1.2.29).

## 1. Goal

A single binary, `machine-monitor`, that runs in a terminal on Windows, macOS and Linux and:

1. Monitors **this machine** with no setup (`machine-monitor`).
2. Acts as a **hub** that collects metrics from every machine on the LAN or tailnet, keeps
   30 days of history, evaluates alerts and estimates electricity cost (`machine-monitor hub`).
3. Runs as a lightweight **agent** that reports to a hub (`machine-monitor agent`).
4. Shows **all devices** in a TUI connected to a hub, from any terminal, including over SSH.

v1 is done when every row marked **Keep** or **Adapt** in the feature matrix (§3) works on
all three OSes and passes the acceptance checks in §8.

## 2. What device-monitor is (reverse-engineering summary)

An Electron app (Windows/macOS) plus a Node hub and three shell agents, about 5k lines.

| Piece | Upstream file | What it does |
|---|---|---|
| Hub | `server.js` | HTTP API on TCP 7777, SSE live stream, device registry, command queue, pairing, installer scripts |
| Report validation | `security.js` | Rebuilds every agent report from known fields with clamped types; Host-header, LAN-only, CSRF, CSP checks |
| History | `history.js` | Raw ~2 s samples for 1 h in memory; 1-min averages (+ peaks) for 30 days in `history/<id>.ndjson` |
| Power | `power.js` | Watts-at-the-wall estimate: measured GPU/CPU power where available, load-based CPU/GPU estimate otherwise, fixed "rest of system", PSU efficiency |
| Alerts | `alerts.js` | Rules (offline, CPU/GPU temp, CPU/GPU load, memory, disk, power, daily cost) with duration and hysteresis; delivery to desktop, ntfy, webhook (Discord/Slack) |
| Discovery | `discovery.js` | UDP 7778 broadcast probe `device-monitor:discover`; Wake-on-LAN magic packets |
| Remote | `tailscale.js` | Reads `tailscale status --json`; optionally turns on `tailscale serve` HTTPS |
| Agents | `agent/agent.sh`, `agent-macos.sh`, `agent.ps1` | Collect and POST a JSON report every 2 s |
| Desktop shell | `main.js`, `updater.js`, `app/` | Tray, setup wizard, firewall rules, start at login, signed self-updates |
| Dashboard | `public/index.html` | Device cards with gauges and charts, details, history, energy/cost, settings, add device |

**Gaps found in the published source.** The hub has APIs for alerts, the processes panel and
remote power actions (sleep/restart/shutdown), but the published agents never send
`processes`, `features` or `actions`, and the dashboard loads `js/alerts.js`, `js/tools.js`
and `sw.js`, which are not in the repo. Those features are designed fresh here from the hub's
API contract rather than ported.

### 2.1 Metric collection by OS (what we must replicate natively)

| Metric | Linux (upstream) | macOS (upstream) | Windows (upstream) |
|---|---|---|---|
| Device id | `/etc/machine-id` | `IOPlatformUUID` (ioreg) | `HKLM\...\Cryptography\MachineGuid` |
| CPU usage, per core | `/proc/stat` deltas | `top -l 2` (total only) | `Win32_PerfRawData_PerfOS_Processor` deltas |
| CPU clock | `/proc/cpuinfo`, cpufreq | none | `PercentProcessorPerformance` × max clock |
| CPU temp | hwmon `coretemp`/`k10temp`/`zenpower`, thermal zones | `smctemp -c` (optional brew) | LibreHardwareMonitor WMI, ACPI thermal zone fallback |
| CPU power | Grace `power_meter`, RAPL `energy_uj` | none | LHM "CPU Package" power sensor |
| Memory | `/proc/meminfo` (total − available) | `vm_stat` active + wired + compressed | `Win32_OperatingSystem` |
| NVIDIA GPU | `nvidia-smi --query-gpu=…` | — | `nvidia-smi` |
| AMD GPU | sysfs `gpu_busy_percent`, `mem_info_vram_*`, hwmon | — | perf counters (generic) |
| Apple / Intel / generic GPU | — | `ioreg IOAccelerator PerformanceStatistics` | `GPUEngine` + `GPUAdapterMemory` perf counters |
| Disks | `df` minus pseudo filesystems | `df`, APFS container math | `Win32_LogicalDisk` |
| Disk I/O, network | `/proc/diskstats`, `/proc/net/dev` | `ioreg IOBlockStorageDriver`, `netstat -ibn` | perf raw counters |
| Sensors, fans | — | — | LibreHardwareMonitor / OpenHardwareMonitor WMI |
| Form factor | DMI `chassis_type` | model name contains "Book" | `Win32_SystemEnclosure.ChassisTypes` |
| MACs (for WoL) | report field only | report field only | report field only |
| Loaded AI models | Ollama `:11434/api/ps`, `lms ps --json`, ComfyUI `:8188/history`, nvidia-smi compute apps + cmdline (vLLM, SGLang, llama.cpp) | same | Ollama, LM Studio, ComfyUI |

## 3. Feature matrix

**Keep** = same behavior. **Adapt** = same goal, TUI-shaped. **Later** = after v1.
**Drop** = not applicable to a TUI.

| # | Feature | Decision | Notes |
|---|---|---|---|
| 1 | CPU, per-core, clock, temp | Keep | All OSes; per-core on macOS too (gopsutil gives it) |
| 2 | Memory | Keep | macOS uses the Activity Monitor formula |
| 3 | GPUs (NVIDIA, AMD, Apple, generic) | Keep | Biggest-VRAM GPU first, as upstream |
| 4 | Disks and disk I/O | Keep | Same pseudo-fs filters |
| 5 | Network throughput | Keep | Plus per-interface breakdown in device detail (new) |
| 6 | Temperature sensors and fans | Keep | Windows via LHM WMI; Linux via hwmon (all sensors, new); macOS via `smctemp` |
| 7 | Uptime, OS, model, form factor | Keep | |
| 8 | Loaded AI models | Keep | Ollama, LM Studio, ComfyUI, vLLM, SGLang, llama.cpp; checked every 5th sample |
| 9 | Processes panel | Adapt | Full sortable/filterable table, like `top`; remote list sent only while a viewer has it open (upstream `procWatch`) |
| 10 | Kill process | Later | Local only, confirm dialog |
| 11 | Multi-device dashboard | Adapt | Grid of device cards, responsive to terminal width |
| 12 | Reorder devices | Adapt | `J`/`K` move card; order saved on hub |
| 13 | Rename / remove device | Keep | |
| 14 | Live charts 10 m / 60 m / 6 h | Adapt | Braille sparklines on cards |
| 15 | History 1 h – 30 d, zoom | Adapt | Full-screen braille line charts; `[`/`]` range, `+`/`-` zoom, `←`/`→` pan, cursor readout |
| 16 | History storage (raw 1 h + 1-min 30 d) | Keep | Same NDJSON format and merge rules |
| 17 | Power estimate (watts at wall) | Keep | Port `power.js` exactly, with tests against its numbers. One deviation: an AMD APU's GPU is marked integrated by the agent, so no discrete-GPU estimate is added on top of package power |
| 18 | Electricity cost, regions, TOU | Keep | Port the region price table; default region from time zone. Deviation: overlapping TOU periods are refused, so the daily and monthly figures cannot count an hour twice |
| 19 | Usage totals (online time, avg/peak, bytes) | Keep | Advanced view |
| 20 | Alerts: rules, hysteresis, log | Keep | Port `alerts.js` |
| 21 | Alert delivery: ntfy, webhook | Keep | Same SSRF guard |
| 22 | Alert delivery: desktop | Adapt | In-TUI banner + terminal bell; OS notification Later |
| 23 | LAN discovery (UDP 7778) | Keep | Same probe string, so it finds upstream hubs too |
| 24 | Wake-on-LAN | Keep | |
| 25 | Remote sleep / restart / shutdown | Keep | Opt-in per device (`--allow-power-actions`); HMAC-signed commands, 60 s TTL |
| 26 | Pairing code (6 digits, 10 min, 5 tries) | Keep | `machine-monitor connect <hub>` prompts for it |
| 27 | Hub proof before sending key (HMAC challenge) | Keep | |
| 28 | Security hardening (LAN-only, Host check, CSRF, rate limits, validation) | Keep | |
| 29 | Tailscale status / addresses | Keep | Shown in Settings; used for connect fallback |
| 30 | `tailscale serve` HTTPS toggle | Later | |
| 31 | °C / °F | Keep | |
| 32 | Start at login / run in background | Adapt | `machine-monitor service install` (systemd user unit, LaunchAgent, Windows logon task). No admin |
| 33 | One-line installs for agents | Adapt | Hub serves an install script that downloads the matching release binary and installs the service |
| 34 | Upstream agent compatibility | New | Hub accepts upstream's `/api/report` JSON, so existing device-monitor agents can report to it |
| 35 | Self-update | Adapt | `machine-monitor update` from GitHub Releases with checksum + signature check |
| 36 | Firewall rules | Drop | Document the two ports instead; hub prints a hint when unreachable |
| 37 | Browser dashboard, PWA, phone view | Drop | Out of scope; the API stays open for one later |
| 38 | Tray icon, Electron window, setup wizard | Drop | Replaced by first-run prompt in the TUI |
| 39 | Export history to CSV/JSON | New | `machine-monitor export <device> --range 7d` |
| 40 | Headless snapshot | New | `machine-monitor status --json` prints one report (scripts, CI, SSH) |

## 4. Stack

| Choice | Why |
|---|---|
| **Go**, `CGO_ENABLED=0` | One static binary per OS/arch, trivial cross-compiling, good stdlib HTTP/UDP |
| **gopsutil v4** | CPU, per-core, memory, disks, I/O, network, host info, processes and Linux sensors on all three OSes. Replaces most of the three shell agents |
| **yusufpapurcu/wmi** (already a gopsutil dependency) | Windows LibreHardwareMonitor sensors and GPU perf counters |
| **Bubble Tea v2 + Bubbles + Lip Gloss** | Mature TUI framework, works on Windows Terminal, handles resize and mouse |
| **NimbleMarkets/ntcharts** | Braille line charts and sparklines for Bubble Tea; saves writing a chart renderer |
| stdlib `net/http`, `encoding/json`, `crypto/*` | Hub, SSE, HMAC; no web framework needed |

Vendor tools we shell out to when present (never required): `nvidia-smi`, `ioreg`,
`smctemp`, `tailscale`, `lms`. Every exec has a timeout and a stripped environment.

Rejected: Rust/ratatui (slower to iterate, sysinfo has weaker Windows GPU coverage); NVML
bindings (need cgo); kardianos/service (installs system services that need admin).

## 5. Architecture

```
cmd/machine-monitor/      main, subcommands (cobra-free: stdlib flag + small dispatcher)
internal/
  collect/                Collector interface + per-OS files (collect_linux.go, _darwin.go, _windows.go)
    gpu/                  nvidia (nvidia-smi), amd (sysfs), apple (ioreg), wingeneric (perf counters)
    sensors/              hwmon, LHM WMI, smctemp
    ai/                   Ollama, LM Studio, ComfyUI, GPU-process detection
  report/                 Report schema (upstream-compatible JSON) + Normalize() (port of security.js)
  hub/                    HTTP API, SSE, registry, pairing, command queue, rate limit, security
  history/                Store: raw ring + minute NDJSON (port of history.js)
  power/                  Estimate (port of power.js) + price regions + TOU
  alerts/                 Rules, evaluation, ntfy/webhook delivery (port of alerts.js)
  discovery/              UDP probe/respond, Wake-on-LAN
  tailscale/              status reader
  client/                 Hub client for TUI and agent (report, SSE, history, actions)
  service/                Per-OS user-level autostart install/uninstall
  tui/                    Bubble Tea models: dashboard, device, processes, history, energy, alerts, settings, add-device
  config/                 Paths and settings files
```

**Data flow**

- *Local mode* (`machine-monitor`): TUI → in-process `collect` every 2 s. History kept in
  memory for the session (and on disk if a local hub is running).
- *Hub mode*: agents `POST /api/report` every 2 s → `report.Normalize` → registry, history,
  power, alerts → SSE fan-out to connected TUIs. The hub runs its own collector for its own
  machine, as upstream does.
- *Connected TUI* (`machine-monitor --hub http://host:7777`): snapshot + SSE stream; history,
  energy, alerts and actions through the REST API.

**Data directory**: `os.UserConfigDir()/machine-monitor` (`%APPDATA%`, `~/Library/Application
Support`, `~/.config`), files mode 0600 on Unix: `config.json` (token, energy, allowedHosts),
`devices.json`, `alerts.json`, `alerts-log.json`, `history/<id>.ndjson`, `client.json` (hub URL
and key for agents/TUI).

## 6. Wire protocol

Keep upstream's API paths and JSON shapes so both sides interoperate. Changes are additive.

| Endpoint | Auth | Purpose |
|---|---|---|
| `POST /api/report` | token in body | Agent report; reply may carry `procs: true` and signed `commands` |
| `GET /api/info` | open (proof only) / key | Hub identity; HMAC proof of `nonce` when unauthenticated |
| `POST /api/pair` · `POST /api/pair/code` | open · key | Pairing code exchange |
| `GET /api/devices` · `GET /api/stream` | key | Snapshot · SSE (`snapshot`, `report`, `status`, `order`, `device`, `removed`, `alert`, `actionResult`) |
| `GET /api/history/:id?range=&from=&to=&points=` | key | Downsampled history |
| `GET /api/energy?bounds=` · `GET/PUT /api/energy/settings` | key | Energy summaries and price settings |
| `GET/PUT /api/alerts` · `POST /api/alerts/test` · `DELETE /api/alerts/log` | key | Alerts |
| `GET /api/devices/:id/processes` · `POST /api/devices/:id/action` | key | Processes; wake/sleep/restart/shutdown. Commands are HMAC-signed with the shared hub key: this stops injection by anyone without the key, not forgery by another key holder (per-device keys: #43) |
| `PATCH/DELETE /api/devices/:id` · `PUT /api/order` | key | Rename, remove, reorder |
| `GET /install.sh` · `GET /install.ps1` | key | Agent installer (downloads release binary, pins SHA-256) |

Additive fields in reports: `agent: "machine-monitor/<version>"`, `netIfaces[]`,
`temps[]` on Linux/macOS. Upstream hubs ignore unknown fields; upstream agents omit them.

Discovery: same UDP 7778 probe and `{"app":"device-monitor",...}` reply, plus
`"impl":"machine-monitor"`.

## 7. TUI design

Global keys: `1`–`6` switch views, `tab` cycle, `/` filter, `?` help, `q` quit, `,` settings,
mouse click and scroll supported.

| View | Contents |
|---|---|
| **1 Dashboard** | Header: device count online/offline, total watts, cost today, active alerts. Grid of cards (columns follow width; one column under 80 cols): name, OS, uptime, CPU / GPU / memory gauges, temps with Normal / Warm / Hot colors (<70, <85, ≥85 °C as upstream), sparkline, net and disk rates. Offline devices collapse into a footer row with Wake |
| **2 Device** | Everything about one device: per-core bars, every GPU, drives, sensors, fans, network interfaces, loaded AI models, power breakdown (measured vs estimated) |
| **3 Processes** | Sortable table (CPU, mem, GPU, GPU mem, pid, user, name, cmd), filter, tree toggle Later |
| **4 History** | Device picker, range 1 h / 6 h / 24 h / 7 d / 30 d, charts for utilization, temperatures, power, network, disk; zoom/pan, cursor readout of avg and peak |
| **5 Energy** | 24 h / 7 d / 30 d: per-device now/average watts, kWh, cost, projected monthly; bar chart per hour/day; usage totals table |
| **6 Alerts** | Active alerts, rule editor (type, device or all, threshold, minutes, enabled), channels (ntfy, webhook), test button, event log |
| **Settings** | °C/°F, price region / rate / TOU, role (local, hub, connected), hub link, Tailscale status, service install state, version and update |
| **Add device** | Pairing code with countdown; copyable one-line installs for Linux/macOS (`curl … \| sh`) and Windows (`irm … \| iex`) |

Color theme follows the terminal's 16-color palette by default (respects `NO_COLOR`), with
a truecolor theme when supported.

## 8. Milestones and acceptance

Each milestone ends with tests green on the CI matrix (ubuntu, macos, windows; amd64 + arm64
builds) and a short demo GIF via `vhs` in the PR.

**M0 — Skeleton.** Go module, CI matrix, goreleaser config, lint (`go vet`, `staticcheck`).
*Accept:* `machine-monitor --version` builds and runs on all six targets.

**M1 — Local collectors.** `collect` for CPU, memory, disks, I/O, network, host, form factor,
device id on all OSes; NVIDIA via nvidia-smi; AMD sysfs; Apple ioreg; Windows GPU perf
counters; Linux hwmon temps, RAPL/Grace CPU power; LHM WMI; smctemp. `machine-monitor status
--json` prints a normalized report.
*Accept:* golden tests per OS using recorded fixtures (upstream's `build/test/macos/bin` mock
approach: fake `nvidia-smi`, `ioreg`, `smctemp` on PATH; sysfs/procfs fixture trees);
manual check that values match Task Manager / Activity Monitor / `top` within 5 points.

**M2 — Local TUI.** Dashboard (single card), Device and Processes views from the in-process
collector. *Accept:* `teatest` golden snapshots at 80×24 and 160×48; resize and `NO_COLOR` work;
no frame takes over 16 ms on a 64-thread machine.

**M3 — Hub core.** HTTP server, `report.Normalize` (table tests ported from every clamp in
`security.js`), registry, history store (tests for minute merge across restart, 30-day prune,
downsampling), SSE, Host/LAN/CSRF checks, rate limits, token generation.
*Accept:* an **upstream device-monitor agent** (`agent.sh` run unmodified) reports to our hub
and shows up; fuzz test on `Normalize` runs 10 min without panic.

**M4 — Agent + connected TUI.** `machine-monitor agent`, pairing and hub proof, discovery,
`connect` flow, multi-device dashboard, rename/remove/reorder, offline handling.
*Accept:* three machines (one per OS) on one hub; unplugging one marks it offline within 10 s.

**M5 — History, power, energy.** History view with zoom; port `power.js` and price table;
Energy view; usage totals; CSV/JSON export.
*Accept:* power estimates equal upstream's for a fixed set of reports (table test); cost math
checked against hand calculation for flat and TOU rates.

**M6 — Alerts.** Port rules and hysteresis, ntfy and webhook delivery with SSRF guard, in-TUI
banner and bell, rule editor and log.
*Accept:* each rule type fires and resolves in tests with a fake clock; webhook to
`127.0.0.1` and to a name resolving to `169.254.169.254` is refused.

**M7 — Actions.** Wake-on-LAN; processes on demand; signed power actions opt-in per device.
*Accept:* an unsigned or replayed command is ignored; actions off by default.

**M8 — Install and ship.** `service install/uninstall` per OS (systemd user unit, LaunchAgent
plist, Windows Task Scheduler logon task), hub-served one-line installers, GitHub Releases
via goreleaser with checksums and signatures, `update` command, Homebrew tap and Scoop
manifest, README with screenshots.
*Accept:* fresh VM per OS goes from one-line install to reporting in under 2 minutes with no
admin prompt.

Later: kill process, OS desktop notifications, `tailscale serve` toggle, process tree,
optional web view.

## 9. Planned commands

```bash
go build ./cmd/machine-monitor             # build for this OS
go test ./...                              # unit + golden tests
go test -fuzz=FuzzNormalize ./internal/report
goreleaser release --snapshot --clean      # all six targets locally

machine-monitor                            # local TUI
machine-monitor hub [--port 7777]          # headless hub (also monitors itself)
machine-monitor agent --hub URL            # report to a hub
machine-monitor connect URL                # pair this machine and open the TUI
machine-monitor --hub URL                  # TUI against a hub
machine-monitor status --json              # one report to stdout
machine-monitor export ID --range 7d --format csv
machine-monitor service install|uninstall [--role hub|agent]
machine-monitor update
```

## 10. Risks

| Risk | Mitigation |
|---|---|
| Windows CPU temps need LibreHardwareMonitor | Same as upstream: detect LHM WMI, ACPI fallback, and say so in the Device view |
| macOS temps need `smctemp`; no cgo for IOKit SMC | Optional dependency, documented; revisit a pure-Go SMC reader after v1 |
| Apple GPU stats change across macOS versions | Parse both key names upstream handles; fixture per macOS version |
| Windows perf counter names are localized | Use WMI classes (not PDH counter paths), as upstream does |
| Terminal differences (Windows conhost, tmux, SSH) | Test in Windows Terminal, conhost, tmux, and over SSH; ASCII fallback for braille |
| Plain HTTP on the LAN | Same stance as upstream: LAN/tailnet only, HMAC-signed commands, recommend Tailscale |
| Upstream protocol drift | Pin the compatibility test to a recorded upstream report; treat compat as best effort |

## 11. Licensing

Upstream is MIT (© 2026 Matt). Ported logic (`power.js` model, price table, alert wording and
thresholds, `normalizeReport` clamps, history aggregation) carries a header comment
crediting the upstream file and its MIT notice. The rest is written fresh.
