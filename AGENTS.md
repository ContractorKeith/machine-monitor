# machine-monitor

A terminal (TUI) app that monitors CPU, GPU, memory, temperatures, disks, network,
processes and power for one machine or every machine on your network, on Windows, macOS
and Linux. It is a clean-room Go reimplementation of the ideas in
[Forward-Future/device-monitor](https://github.com/Forward-Future/device-monitor)
(an Electron app, MIT) as a single binary with no runtime dependencies. "Done" for v1 is the
feature-parity matrix in `docs/PLAN.md` shipped on all three OSes.

## Status
planning — no code yet. Last touched 2026-10-31.

## Pointers
- `docs/PLAN.md` — the build plan: architecture, feature matrix, milestones, acceptance.
- Upstream reference: clone `Forward-Future/device-monitor` to study behavior; port logic, not code style.

## Commands
```bash
# Not built yet. Planned (see docs/PLAN.md §9):
# go build ./cmd/machine-monitor
# go test ./...
# go vet ./... && staticcheck ./...
```

## Conventions & Gotchas
- Go, `CGO_ENABLED=0` on every target. Shelling out to vendor tools (`nvidia-smi`,
  `ioreg`, `smctemp`, `tailscale`) is allowed; cgo is not.
- Every value from another machine is untrusted: normalize and clamp it at the hub
  (port of upstream `security.js normalizeReport`).
- Any metric can be missing on any OS. Collectors return `nil`, never 0, for "unknown".

## Out of Scope
- Browser dashboard, Electron/tray app, PWA.
- Kernel drivers or admin-only installs. Everything runs as a normal user.
