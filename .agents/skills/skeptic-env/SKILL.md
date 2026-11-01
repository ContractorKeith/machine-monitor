---
name: skeptic-env
description: "How a skeptical reviewer builds, runs, seeds and attacks machine-monitor. Read by the skeptic-review skill; not a workflow on its own."
---

# skeptic-env — machine-monitor

A Go TUI that monitors one machine or every machine on a network: local
collectors, a hub with an HTTP API and SSE, agents that report to it, pairing,
discovery, history, alerts, signed power actions, per-user service install.
"Done" is the feature matrix in `docs/PLAN.md §3` shipped on Linux, macOS and
Windows as one static binary each. v0.1.0 is judged ready to tag but not tagged.

## Stack and commands

```bash
go build ./cmd/machine-monitor && ./machine-monitor --version
go test ./...
go test -race ./...                                    # CI runs this on Linux
test -z "$(gofmt -l .)" && go vet ./... && staticcheck ./...   # staticcheck v0.8.2
for os in linux darwin windows; do for a in amd64 arm64; do
  GOOS=$os GOARCH=$a CGO_ENABLED=0 go build ./... || echo "FAIL $os/$a"; done; done
go run github.com/goreleaser/goreleaser/v2@v2.19.0 check
go run github.com/goreleaser/goreleaser/v2@v2.19.0 release --snapshot --clean --skip=sign
go test -fuzz=FuzzNormalize -fuzztime=2m ./internal/report

# run against a scratch data dir, never the reviewer's own
export MACHINE_MONITOR_DIR=$(mktemp -d)
./machine-monitor hub --port 7797 &                    # headless hub, monitors itself
./machine-monitor status --json                        # one normalized report
./machine-monitor agent --hub http://127.0.0.1:7797    # a second instance reporting in
./machine-monitor --hub http://127.0.0.1:7797          # TUI against the hub
```

Scratch data: `MACHINE_MONITOR_DIR`. Two instances on one box need two dirs
and two ports. Discovery is UDP 7778; a second hub on the LAN will see yours.

## Platforms

| Platform | Ships | Review on | Notes |
|---|---|---|---|
| Linux | yes | Omarchy | hwmon temps, RAPL power, NVIDIA via `nvidia-smi` if present |
| macOS | yes | Mac Studio | `ioreg`, `smctemp` if installed; LaunchAgent install |
| Windows | yes | read for it | Task Scheduler install, LHM WMI, perf counters; `0600` has no equivalent |

Fixtures for other OSes: fake `nvidia-smi`, `ioreg`, `smctemp` on PATH and
sysfs/procfs trees under testdata, so an OS's collectors can be exercised
anywhere through tests.

## What to attack

- **Hub API and SSE**: a report with a value outside every clamp in
  `internal/report.Normalize`; oversized bodies; a device id that is a path;
  Host and CSRF checks from a non-LAN address; the stream on a very slow
  client; kill the agent mid-report.
- **Pairing and tokens**: a token minted for one hub used on another; replay
  of a signed power action; the unsigned path; actions off by default; a
  pairing proof after the hub restarts.
- **Discovery**: a flood of UDP announcements; an announcement naming a
  bogus address; two hubs on one LAN.
- **History**: minute merge across a restart; a corrupt or truncated ndjson
  line; 30-day prune; downsampling at range boundaries; export with a range
  larger than the data.
- **Alerts**: hysteresis with a fake clock; the webhook SSRF guard against
  `127.0.0.1`, a name resolving to `169.254.169.254`, and an IPv6 loopback.
- **TUI**: 80×24 and 160×48, resize mid-frame, `NO_COLOR`, a device name
  with control characters or lookalike blanks (#71), a frame over 16 ms.
- **Service install**: over an existing service; uninstall of a service
  that is not there; the unit or plist as written, from a fresh shell.
- **Install docs**: every README and `packaging/` command as typed. The
  installers' ShellCheck and PowerShell tests only run when those tools are
  on PATH, so a green CI may not have run them.

## Traps

- `CGO_ENABLED=0` on every target. Shelling out to vendor tools is allowed;
  cgo is not. A build that needs cgo is a Bug even if it passes locally.
- Collectors return `nil`, never 0, for unknown. `report` fields are
  pointers set with `report.Ptr(v)`. A 0 where a metric is missing is a Bug;
  a `nil` is correct.
- Every value from another machine is untrusted and is clamped at the hub.
  The wire format and UDP 7778 are kept compatible with upstream
  device-monitor agents on purpose; do not flag the compatibility.
- All data files go through `config.WriteFile0600`. On Windows that call is
  a plain write; read for what stands in for the mode.
- One owner per package (`AGENTS.md`); a finding that spans packages names
  each package.
- Ported logic carries an upstream header comment and MIT notice; that is
  required, not a leftover.
- The merge gate requires at least one completed CI check after #67 merged
  with an empty check list.

## Earlier rounds

- 2026-10-31, main `6ee69c8`: three fix rounds (#45–#49, #56–#59, #64–#66)
  and a final targeted check, 18 of 22 pass-3 items fixed, 30-minute soak
  clean. Open: #71 (lows: discovery /24 flood, textclean idempotence, blank
  lookalikes), #43 (per-device signing keys). Reports predate the worklog
  `reports/` convention; the issues carry the detail.
