// Package gpu registers collect probes for graphics adapters: NVIDIA (nvidia-smi), AMD
// (sysfs), Apple (ioreg IOAccelerator) and Windows generic (GPU perf counters). Each fills
// report.Report.GPUs. See package collect for the probe contract.
package gpu
