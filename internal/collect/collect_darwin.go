//go:build darwin

package collect

import (
	"context"
	"os"
	"os/exec"
	"time"
)

func newOS() Collector { return newCore(newSource(), gopsutil{}) }

func newSource() source { return macSource{ps: gopsutil{}, run: runCmd, hostname: os.Hostname} }

const execTimeout = 3 * time.Second

func runCmd(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LC_ALL=C"}
	cmd.WaitDelay = time.Second
	return cmd.Output()
}
