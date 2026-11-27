# Notice

machine-monitor ports logic from [Forward-Future/device-monitor](https://github.com/Forward-Future/device-monitor),
pinned at `c3a4c41`, under the MIT License below. The files that carry ported logic, and the
upstream source each one draws on, are listed after it.

```text
MIT License

Copyright (c) 2026 Matt

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```

## Ported files

| File | Upstream source (at c3a4c41) |
|---|---|
| `internal/agent/hubapi.go` | main.js (fetchInfo, pairWithHub, hubAnswers, the connect flow) and server.js (hubProof) |
| `internal/alerts/deliver.go` | alerts.js (SSRF guard) |
| `internal/alerts/engine.go` | alerts.js |
| `internal/alerts/settings.go` | alerts.js |
| `internal/alerts/text.go` | alerts.js |
| `internal/collect/ai/ai.go` | agent/agent.sh (AI_PY), agent/agent-macos.sh, agent/agent.ps1 (Get-AiModels) |
| `internal/collect/ai/gpumem.go` | agent/agent.ps1 (Get-GpuProcessMemory) |
| `internal/collect/ai/sources.go` | agent/agent.sh (AI_PY), agent/agent.ps1 (Get-AiModels) |
| `internal/collect/ai/win_windows.go` | agent/agent.ps1 (Get-GpuProcessMemory, GetActiveTcpListeners, Get-NetTCPConnection, Win32_Process) |
| `internal/collect/gpu/doc.go` | agent/agent.sh, agent/agent-macos.sh, agent/agent.ps1, server.js |
| `internal/collect/oslinux.go` | agent/agent.sh |
| `internal/collect/osmacos.go` | agent/agent-macos.sh |
| `internal/collect/oswindows.go` | agent/agent.ps1 |
| `internal/collect/sensors/doc.go` | agent/agent.sh, agent/agent-macos.sh, agent/agent.ps1 |
| `internal/discovery/discovery.go` | discovery.js (respond, broadcastAddresses, discover, wake) and security.js (isPrivateAddress) |
| `internal/discovery/wake.go` | discovery.js (wake) |
| `internal/history/aggregate.go` | history.js |
| `internal/history/history.go` | history.js |
| `internal/history/params.go` | server.js |
| `internal/hub/commands.go` | server.js (commands, /api/devices/:id/action) |
| `internal/hub/energy.go` | server.js (usageSummary, costToday) |
| `internal/hub/handlers.go` | server.js |
| `internal/hub/hub.go` | server.js |
| `internal/hub/install.go` | server.js (template, hubUrlFor) and agent/install.sh |
| `internal/hub/pair.go` | server.js |
| `internal/hub/procs.go` | server.js (procWatch, /api/devices/:id/processes) |
| `internal/hub/registry.go` | server.js (handleReport, deviceView) |
| `internal/hub/remote.go` | server.js (refreshRemote, /api/remote) and tailscale.js (status cache) |
| `internal/hub/security.go` | security.js and server.js |
| `internal/power/power.go` | power.js, energy part of server.js, price tables in public/index.html |
| `internal/report/normalize.go` | security.js (normalizeReport, normalizeProcesses, isValidId, cleanText) |
| `internal/report/report.go` | security.js and agent/* (agent.sh, agent-macos.sh, agent.ps1) |
| `internal/tailscale/tailscale.go` | tailscale.js (cli, run, readStatus, peerUrls) |
| `internal/tui/energy.go` | public/index.html renderAdv, advBounds, drawAdvChart (Advanced page tiles, price wording, projections, tables) |
| `internal/tui/history.go` | public/index.html (history page chart set, ranges, axis scaling) |
| `internal/tui/local.go` | server.js (usageSummary), mirrored via internal/hub energy.go and registry.go |
