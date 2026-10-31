package ai

import (
	"os"
	"strconv"
	"strings"
)

// procInfo reads a process's argv and working directory from /proc. argv is nil when the
// process is gone or not ours to read.
func procInfo(pid int) (argv []string, cwd string) {
	dir := "/proc/" + strconv.Itoa(pid)
	b, err := os.ReadFile(dir + "/cmdline")
	if err != nil {
		return nil, ""
	}
	for _, a := range strings.Split(string(b), "\x00") {
		if a != "" {
			argv = append(argv, a)
		}
	}
	cwd, _ = os.Readlink(dir + "/cwd")
	return argv, cwd
}
