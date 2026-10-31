// Package sensors registers collect probes for temperatures, fans and CPU power: Linux
// hwmon, thermal zones, RAPL and Grace power meters; Windows LibreHardwareMonitor WMI and
// ACPI thermal zones; macOS smctemp. Each fills CPU.Temp, Temps, Fans and Power. See package
// collect for the probe contract.
package sensors
