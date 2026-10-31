package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Runner runs a service-manager command ("systemctl", "launchctl" or "schtasks") and
// returns its combined output. Tests replace it with a fake.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

const (
	runTimeout = 20 * time.Second // systemctl restart waits for the old process to stop
	maxOutput  = 64 << 10
)

// ExecRunner runs the real command from a fixed path, with a timeout and a stripped
// environment.
func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	exe, err := lookTool(name)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Env = strippedEnv()
	cmd.WaitDelay = time.Second
	var out bytes.Buffer
	lw := &limitWriter{w: &out, n: maxOutput}
	cmd.Stdout, cmd.Stderr = lw, lw
	err = cmd.Run()
	return out.Bytes(), err
}

// toolPaths are where each tool lives; PATH is never searched.
func toolPaths(name string) []string {
	switch name {
	case "systemctl":
		return []string{"/usr/bin/systemctl", "/bin/systemctl", "/run/current-system/sw/bin/systemctl"}
	case "launchctl":
		return []string{"/bin/launchctl"}
	case "schtasks":
		root := os.Getenv("SystemRoot")
		if root == "" {
			root = `C:\Windows`
		}
		return []string{filepath.Join(root, "System32", "schtasks.exe")}
	}
	return nil
}

// lookTool returns the first existing path for name.
func lookTool(name string) (string, error) {
	for _, p := range toolPaths(name) {
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			return p, nil
		}
	}
	return "", errors.New(name + " not found")
}

// allowedEnv is the parent environment the tools may see. systemctl --user needs
// XDG_RUNTIME_DIR and DBUS_SESSION_BUS_ADDRESS to reach the user manager.
var allowedEnv = []string{"PATH", "HOME", "USER", "LOGNAME", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS",
	"USERPROFILE", "USERNAME", "USERDOMAIN", "APPDATA", "LOCALAPPDATA", "SystemRoot", "SystemDrive", "TEMP", "TMP", "TMPDIR"}

// strippedEnv keeps only allowedEnv, plus LC_ALL=C so output parses the same everywhere.
// Never nil: os/exec treats a nil Env as "inherit everything".
func strippedEnv() []string {
	env := []string{"LC_ALL=C"}
	for _, k := range allowedEnv {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// limitWriter drops output past n bytes.
type limitWriter struct {
	w io.Writer
	n int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if l.n > 0 {
		k := min(len(p), l.n)
		l.n -= k
		l.w.Write(p[:k])
	}
	return len(p), nil
}
