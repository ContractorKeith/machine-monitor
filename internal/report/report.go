// Package report defines the device report: the JSON an agent POSTs to a hub's /api/report
// every 2 s, and the shape the hub stores and streams after validating it.
//
// The field set and JSON names match upstream device-monitor (security.js normalizeReport and
// normalizeProcesses, and the agents agent.sh, agent-macos.sh and agent.ps1) so either side
// can talk to the other. Fields this project adds are marked "additive"; upstream hubs ignore
// them.
//
// Conventions every producer and consumer follows:
//   - A metric that is unknown is nil and encodes as null, never 0. Use Ptr to set one.
//   - null and a missing key mean the same thing. Normalize emits every key, agents may not.
//   - All numbers are *float64, as in upstream's JSON, including counts like threads and pid.
//   - Bytes are bytes, rates are bytes per second, temperatures are °C, power is watts,
//     clocks are MHz, percentages are 0–100.
//
// A Report decoded straight from the network is untrusted. Only the hub's Normalize (M3)
// turns one into something safe to store and display.
//
// Field set ported from Forward-Future/device-monitor security.js and agent/*
// (MIT, © 2026 Matt): https://github.com/Forward-Future/device-monitor
package report

// Platform values. Normalize maps anything else to PlatformUnknown.
const (
	PlatformLinux   = "linux"
	PlatformMacOS   = "macos"
	PlatformWindows = "windows"
	PlatformUnknown = "unknown"
)

// FormFactor values. Normalize maps anything else to nil.
const (
	FormFactorLaptop  = "laptop"
	FormFactorDesktop = "desktop"
)

// Report is one sample from one device.
type Report struct {
	ID           string        `json:"id"`              // machine GUID / machine-id / IOPlatformUUID
	Token        string        `json:"token,omitempty"` // hub token; agent → hub only, Normalize drops it
	Hostname     *string       `json:"hostname"`
	Platform     string        `json:"platform"` // one of the Platform constants
	OS           *string       `json:"os"`       // pretty OS name, e.g. "Ubuntu 24.04.1 LTS · ThinkPad X1"
	Uptime       *float64      `json:"uptime"`   // seconds
	FormFactor   *string       `json:"formFactor"`
	Power        *Power        `json:"power"`
	Features     []string      `json:"features"`     // "procs", "actions": what the agent supports
	Actions      []string      `json:"actions"`      // "sleep", "restart", "shutdown": allowed remote actions
	MACs         []string      `json:"macs"`         // lower-case aa:bb:cc:dd:ee:ff, for Wake-on-LAN
	ActionResult *ActionResult `json:"actionResult"` // outcome of the last remote action
	AppVersion   *string       `json:"appVersion"`   // upstream desktop app version, "1.2.29"
	CPU          CPU           `json:"cpu"`
	Memory       Memory        `json:"memory"`
	GPUs         []GPU         `json:"gpus"`
	Disks        []Disk        `json:"disks"`
	Net          *Net          `json:"net"`
	DiskIO       *DiskIO       `json:"diskIO"`
	Temps        []Temp        `json:"temps"`
	Fans         []Fan         `json:"fans"`
	// AI is nil when the agent doesn't check and empty when nothing is loaded.
	AI []AIGroup `json:"ai"`

	// Processes is sent only while the hub asks for it (reply "procs": true). The hub
	// normalizes it separately (normalizeProcesses) and keeps it out of the stored report.
	Processes []Process `json:"processes,omitempty"`

	// Additive fields (machine-monitor only).
	Agent     string     `json:"agent,omitempty"` // "machine-monitor/<version>"
	NetIfaces []NetIface `json:"netIfaces,omitempty"`
}

// Power holds measured power draw. CPU is package power from RAPL, Grace power meters or LHM.
type Power struct {
	CPU *float64 `json:"cpu"`
}

// ActionResult reports how a signed remote action (sleep, restart, shutdown) went.
type ActionResult struct {
	ID      string  `json:"id"` // 12 hex chars, the command id from the hub
	OK      bool    `json:"ok"`
	Message *string `json:"message"`
}

// CPU is processor load, clock and temperature.
type CPU struct {
	Name       *string    `json:"name"`
	Usage      *float64   `json:"usage"`      // total, percent
	Cores      []*float64 `json:"cores"`      // per logical core, percent; empty when unknown
	Threads    *float64   `json:"threads"`    // logical processors
	ClockMHz   *float64   `json:"clockMHz"`   // current average clock
	Temp       *float64   `json:"temp"`       // package temperature
	TempSource *string    `json:"tempSource"` // "acpi" when Temp is an ACPI thermal-zone fallback
}

// Memory is physical RAM in bytes.
type Memory struct {
	Total *float64 `json:"total"`
	Used  *float64 `json:"used"`
}

// GPU is one graphics adapter.
type GPU struct {
	Name       *string  `json:"name"`
	Vendor     *string  `json:"vendor"` // "nvidia", "amd", "apple", "generic"
	Usage      *float64 `json:"usage"`
	Temp       *float64 `json:"temp"`
	MemUsed    *float64 `json:"memUsed"`
	MemTotal   *float64 `json:"memTotal"`
	Power      *float64 `json:"power"`
	PowerLimit *float64 `json:"powerLimit"`
	Fan        *float64 `json:"fan"` // percent
	ClockMHz   *float64 `json:"clockMHz"`
}

// Disk is one mounted volume.
type Disk struct {
	Name  *string  `json:"name"`  // mount point, drive letter or volume name
	Label *string  `json:"label"` // volume label (Windows)
	Total *float64 `json:"total"`
	Used  *float64 `json:"used"`
}

// Net is total network throughput in bytes per second, loopback excluded.
type Net struct {
	RX *float64 `json:"rx"`
	TX *float64 `json:"tx"`
}

// DiskIO is total disk throughput in bytes per second.
type DiskIO struct {
	Read  *float64 `json:"read"`
	Write *float64 `json:"write"`
}

// Temp is one temperature sensor.
type Temp struct {
	Name   *string  `json:"name"`   // e.g. "Core (Tctl/Tdie)"
	Parent *string  `json:"parent"` // hardware it belongs to, e.g. "/amdcpu/0"
	Value  *float64 `json:"value"`
}

// Fan is one fan sensor.
type Fan struct {
	Name *string  `json:"name"`
	RPM  *float64 `json:"rpm"`
}

// AIGroup is the models one AI app has loaded.
type AIGroup struct {
	App    *string   `json:"app"` // "Ollama", "LM Studio", "ComfyUI", "vLLM", ...
	Mem    *float64  `json:"mem"` // GPU memory the app holds
	Models []AIModel `json:"models"`
}

// AIModel is one loaded model.
type AIModel struct {
	Name *string  `json:"name"`
	Size *float64 `json:"size"` // bytes on disk
	Kind *string  `json:"kind"` // ComfyUI loader kind, e.g. "ckpt", "lora"
}

// Process is one row of the processes panel.
type Process struct {
	PID    *float64 `json:"pid"`
	Name   *string  `json:"name"`
	Cmd    *string  `json:"cmd"`
	User   *string  `json:"user"`
	CPU    *float64 `json:"cpu"` // percent
	Mem    *float64 `json:"mem"` // bytes
	GPU    *float64 `json:"gpu"` // percent
	GPUMem *float64 `json:"gpuMem"`
}

// NetIface is throughput for one network interface (additive).
type NetIface struct {
	Name *string  `json:"name"`
	RX   *float64 `json:"rx"`
	TX   *float64 `json:"tx"`
}

// Ptr returns a pointer to v, for setting optional fields: r.CPU.Temp = report.Ptr(61.5).
func Ptr[T any](v T) *T { return &v }
