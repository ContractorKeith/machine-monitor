package ai

import (
	"os"
	"strconv"
	"strings"
)

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
