//go:build linux

package collect

import "os"

func newOS() Collector { return newCore(newSource(), gopsutil{}) }

func newSource() source {
	return linuxSource{fsys: os.DirFS("/"), ps: gopsutil{}, hostname: os.Hostname}
}
