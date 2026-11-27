package report

const (
	PlatformLinux   = "linux"
	PlatformMacOS   = "macos"
	PlatformWindows = "windows"
	PlatformUnknown = "unknown"
)

const (
	FormFactorLaptop  = "laptop"
	FormFactorDesktop = "desktop"
)

type Report struct {
	ID           string        `json:"id"`
	Token        string        `json:"token,omitempty"`
	Hostname     *string       `json:"hostname"`
	Platform     string        `json:"platform"`
	OS           *string       `json:"os"`
	Uptime       *float64      `json:"uptime"`
	FormFactor   *string       `json:"formFactor"`
	Power        *Power        `json:"power"`
	Features     []string      `json:"features"`
	Actions      []string      `json:"actions"`
	MACs         []string      `json:"macs"`
	ActionResult *ActionResult `json:"actionResult"`
	AppVersion   *string       `json:"appVersion"`
	CPU          CPU           `json:"cpu"`
	Memory       Memory        `json:"memory"`
	GPUs         []GPU         `json:"gpus"`
	Disks        []Disk        `json:"disks"`
	Net          *Net          `json:"net"`
	DiskIO       *DiskIO       `json:"diskIO"`
	Temps        []Temp        `json:"temps"`
	Fans         []Fan         `json:"fans"`
	AI           []AIGroup     `json:"ai"`

	Processes []Process `json:"processes,omitempty"`

	Agent     string     `json:"agent,omitempty"`
	NetIfaces []NetIface `json:"netIfaces,omitempty"`
}

type Power struct {
	CPU *float64 `json:"cpu"`
}

type ActionResult struct {
	ID      string  `json:"id"`
	OK      bool    `json:"ok"`
	Message *string `json:"message"`
}

type CPU struct {
	Name       *string    `json:"name"`
	Usage      *float64   `json:"usage"`
	Cores      []*float64 `json:"cores"`
	Threads    *float64   `json:"threads"`
	ClockMHz   *float64   `json:"clockMHz"`
	Temp       *float64   `json:"temp"`
	TempSource *string    `json:"tempSource"`
}

type Memory struct {
	Total *float64 `json:"total"`
	Used  *float64 `json:"used"`
}

type GPU struct {
	Name       *string  `json:"name"`
	Vendor     *string  `json:"vendor"`
	Usage      *float64 `json:"usage"`
	Temp       *float64 `json:"temp"`
	MemUsed    *float64 `json:"memUsed"`
	MemTotal   *float64 `json:"memTotal"`
	Power      *float64 `json:"power"`
	PowerLimit *float64 `json:"powerLimit"`
	Fan        *float64 `json:"fan"`
	ClockMHz   *float64 `json:"clockMHz"`
	Integrated bool     `json:"integrated,omitempty"`
}

type Disk struct {
	Name  *string  `json:"name"`
	Label *string  `json:"label"`
	Total *float64 `json:"total"`
	Used  *float64 `json:"used"`
}

type Net struct {
	RX *float64 `json:"rx"`
	TX *float64 `json:"tx"`
}

type DiskIO struct {
	Read  *float64 `json:"read"`
	Write *float64 `json:"write"`
}

type Temp struct {
	Name   *string  `json:"name"`
	Parent *string  `json:"parent"`
	Value  *float64 `json:"value"`
}

type Fan struct {
	Name *string  `json:"name"`
	RPM  *float64 `json:"rpm"`
}

type AIGroup struct {
	App    *string   `json:"app"`
	Mem    *float64  `json:"mem"`
	Models []AIModel `json:"models"`
}

type AIModel struct {
	Name *string  `json:"name"`
	Size *float64 `json:"size"`
	Kind *string  `json:"kind"`
}

type Process struct {
	PID    *float64 `json:"pid"`
	Name   *string  `json:"name"`
	Cmd    *string  `json:"cmd"`
	User   *string  `json:"user"`
	CPU    *float64 `json:"cpu"`
	Mem    *float64 `json:"mem"`
	GPU    *float64 `json:"gpu"`
	GPUMem *float64 `json:"gpuMem"`
}

type NetIface struct {
	Name *string  `json:"name"`
	RX   *float64 `json:"rx"`
	TX   *float64 `json:"tx"`
}

func Ptr[T any](v T) *T { return &v }
