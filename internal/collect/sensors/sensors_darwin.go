//go:build darwin

package sensors

import (
	"context"

	"github.com/ContractorKeith/machine-monitor/internal/collect"
)

// smctempPaths are where Homebrew installs smctemp; launchd gives agents a PATH without them.
var smctempPaths = []string{"/opt/homebrew/bin/smctemp", "/usr/local/bin/smctemp"}

func init() {
	collect.Register(collect.Probe{Name: "sensors/macos", Run: func(ctx context.Context) (collect.Apply, error) {
		s, err := readSmctemp(ctx, findTool("smctemp", smctempPaths...))
		if err != nil {
			return nil, err
		}
		return s.apply(), nil
	}})
}
