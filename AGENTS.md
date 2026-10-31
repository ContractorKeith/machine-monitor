# machine-monitor

A terminal (TUI) app that monitors CPU, GPU, memory, temperatures, disks, network,
processes and power for one machine or every machine on your network, on Windows, macOS
and Linux. It is a clean-room Go reimplementation of the ideas in
[Forward-Future/device-monitor](https://github.com/Forward-Future/device-monitor)
(an Electron app, MIT) as a single binary with no runtime dependencies. "Done" for v1 is the
feature-parity matrix in `docs/PLAN.md` shipped on all three OSes.

## Status
Packages for M0–M8 are merged, but the 2026-10-31 skeptic review found gaps in the assembled
product: discovery and Tailscale aren't wired in, there's no way to issue a pairing code
without curl, the Settings and Add device views are missing, and a TUI on a 250-device hub
fails. Fixes are tracked in #45–#49. No release is tagged yet. Last touched 2026-10-31.

## Pointers
- `docs/PLAN.md` — the build plan: architecture, feature matrix, milestones, acceptance.
- Upstream reference: clone `Forward-Future/device-monitor` to study behavior; port logic, not code style.

## Commands
```bash
go build ./cmd/machine-monitor && ./machine-monitor --version
go test ./...
gofmt -l . && go vet ./... && staticcheck ./...    # staticcheck v0.8.2 (go install)
for os in linux darwin windows; do for a in amd64 arm64; do
  GOOS=$os GOARCH=$a CGO_ENABLED=0 go build ./... || echo "FAIL $os/$a"; done; done
go run github.com/goreleaser/goreleaser/v2@v2.19.0 check
go run github.com/goreleaser/goreleaser/v2@v2.19.0 release --snapshot --clean --skip=sign
```
Releases: push a `v*` tag; `.github/workflows/release.yml` builds, signs `checksums.txt` with
cosign keyless and publishes. Installer tests run ShellCheck and PowerShell when on PATH.

## Package ownership
One owner per package; change another package's exported API only through its owner's task.

| Package | PLAN area |
|---|---|
| `cmd/machine-monitor` | §9 commands; each milestone swaps its stub in `commands` |
| `internal/report` | §6 report schema (shared contract) + `Normalize` (M3, port of `security.js`) |
| `internal/collect` | Collector, probe registry and runner, per-OS core metrics (M1) |
| `internal/collect/gpu`, `sensors`, `ai` | Probes (M1); register via `init`, never edit `collect` |
| `internal/config` | §5 paths and `WriteFile0600` |
| `internal/hub` | §6 HTTP API, SSE, registry, pairing, security (M3–M4, M7) |
| `internal/history`, `power`, `alerts` | M5–M6 ports of `history.js`, `power.js`, `alerts.js` |
| `internal/discovery`, `tailscale`, `client`, `service` | M4, M7, M8 |
| `internal/tui` | §7 views (M2, M4–M6) |

## Conventions & Gotchas
- Go, `CGO_ENABLED=0` on every target. Shelling out to vendor tools (`nvidia-smi`,
  `ioreg`, `smctemp`, `tailscale`) is allowed; cgo is not.
- Every value from another machine is untrusted: normalize and clamp it at the hub
  (port of upstream `security.js normalizeReport`).
- Any metric can be missing on any OS. Collectors return `nil`, never 0, for "unknown".
  `report` fields are pointers; set them with `report.Ptr(v)`.
- Probes fail soft and follow the contract in `internal/collect/collect.go`.
- All data files go through `config.WriteFile0600`. `MACHINE_MONITOR_DIR` overrides the
  data dir (tests, second instance).
- Upstream reference source is pinned at `c3a4c41`. Ported logic carries a header comment
  naming the upstream file and its MIT notice (PLAN §11).

## Out of Scope
- Browser dashboard, Electron/tray app, PWA.
- Kernel drivers or admin-only installs. Everything runs as a normal user.
