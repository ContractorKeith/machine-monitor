//go:build linux

package collect

import "os"

func newOS() Collector {
	return newCore(linuxSource{fsys: os.DirFS("/"), ps: gopsutil{}, hostname: os.Hostname}, gopsutil{})
}
