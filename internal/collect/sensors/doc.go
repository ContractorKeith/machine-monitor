// Package sensors registers collect probes for temperatures, fans and CPU power: Linux
// hwmon, thermal zones, RAPL and Grace power meters; Windows LibreHardwareMonitor WMI and
// ACPI thermal zones; macOS smctemp. Each fills CPU.Temp, Temps, Fans and Power. See package
// collect for the probe contract.
//
// There is one probe per OS (sensors/linux, sensors/macos, sensors/windows). Its name sorts
// after the gpu probes, so its Apply runs later and fills GPU temperatures the GPU probe
// left nil (LHM "GPU Core" on Windows, smctemp -g on macOS), as upstream's agents do.
//
// Sensor logic ported from Forward-Future/device-monitor agent/agent.sh,
// agent/agent-macos.sh and agent/agent.ps1 (MIT, © 2026 Matt):
// https://github.com/Forward-Future/device-monitor
package sensors
