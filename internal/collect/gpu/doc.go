// Package gpu registers collect probes for graphics adapters: NVIDIA (nvidia-smi), AMD
// (sysfs), Apple (ioreg IOAccelerator) and Windows generic (GPU perf counters). Each fills
// report.Report.GPUs. See package collect for the probe contract.
//
// There is one probe per OS (gpu/linux, gpu/macos, gpu/windows) rather than one per vendor,
// so it can order every GPU it finds biggest-VRAM first and, on Windows, use the generic
// perf counters only when nvidia-smi found nothing, as upstream's agent does.
//
// GPU logic ported from Forward-Future/device-monitor agent/agent.sh, agent/agent-macos.sh,
// agent/agent.ps1 and server.js (MIT, © 2026 Matt): https://github.com/Forward-Future/device-monitor
package gpu
