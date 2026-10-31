//go:build darwin

package collect

import (
	"context"
	"os"
	"os/exec"
	"time"
)

func newOS() Collector {
	return newCore(macSource{ps: gopsutil{}, run: runCmd, hostname: os.Hostname}, gopsutil{})
}

// execTimeout bounds every command the collector runs, even when ctx has no deadline.
const execTimeout = 3 * time.Second

// runCmd is the real runFunc: a timeout and a stripped environment, so the user's PATH,
// locale and DYLD_* variables can't change what runs or how its output looks.
func runCmd(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LC_ALL=C"}
	cmd.WaitDelay = time.Second
	return cmd.Output()
}
