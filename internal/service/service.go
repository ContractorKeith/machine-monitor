// Package service installs machine-monitor as a user-level autostart: a systemd user unit
// on Linux, a LaunchAgent on macOS and a Task Scheduler logon task on Windows (PLAN §3 row
// 32, M8). Nothing needs admin rights.
//
// A Manager does the work for one OS, chosen by its OS field rather than build tags, so
// every OS's files and service-manager calls are tested on every OS. All service-manager
// commands go through the Runner seam; the default one has a timeout and a stripped
// environment. A Manager writes only the unit/plist file, and under the data directory a
// log directory (macOS) or a temporary task XML (Windows).
package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/ContractorKeith/machine-monitor/internal/config"
)

// Role is what the installed service runs.
type Role string

const (
	RoleHub   Role = "hub"   // machine-monitor hub [--port N]
	RoleAgent Role = "agent" // machine-monitor agent --hub URL
)

// Roles lists every role, for status and uninstall without --role.
var Roles = []Role{RoleHub, RoleAgent}

// ParseRole checks a --role value.
func ParseRole(s string) (Role, error) {
	switch r := Role(s); r {
	case RoleHub, RoleAgent:
		return r, nil
	}
	return "", fmt.Errorf("bad role %q (want hub or agent)", s)
}

// Spec is the command a service runs.
type Spec struct {
	Role   Role
	Exe    string // absolute path of the machine-monitor binary
	Port   int    // hub only; 0 means the hub's default
	HubURL string // agent only
}

// Validate checks the spec before anything is written.
func (s Spec) Validate() error {
	if !filepath.IsAbs(s.Exe) {
		return fmt.Errorf("binary path %q is not absolute", s.Exe)
	}
	switch s.Role {
	case RoleHub:
		if s.Port < 0 || s.Port > 65535 {
			return fmt.Errorf("bad port %d", s.Port)
		}
		if s.HubURL != "" {
			return errors.New("--hub is for the agent role")
		}
	case RoleAgent:
		if s.HubURL == "" {
			return errors.New("the agent role needs --hub URL")
		}
		u, err := url.Parse(s.HubURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("bad hub URL %q (want http://host:port)", s.HubURL)
		}
		if s.Port != 0 {
			return errors.New("--port is for the hub role")
		}
	default:
		return fmt.Errorf("bad role %q", s.Role)
	}
	return nil
}

// Args is the command line after the binary.
func (s Spec) Args() []string {
	if s.Role == RoleAgent {
		return []string{"agent", "--hub", s.HubURL}
	}
	if s.Port != 0 {
		return []string{"hub", "--port", strconv.Itoa(s.Port)}
	}
	return []string{"hub"}
}

// Status is what a service manager reports for one role.
type Status struct {
	Role      Role
	Kind      string // "systemd user unit", "LaunchAgent", "Task Scheduler logon task"
	Name      string // unit, label or task name
	Path      string // unit or plist file; empty on Windows
	Installed bool
	State     string // the service manager's own words, e.g. "enabled, active"
}

// String is a short report for `service status`.
func (s Status) String() string {
	where := s.Name
	if s.Path != "" {
		where = s.Path
	}
	if !s.Installed {
		return fmt.Sprintf("%s: not installed (%s %s)", s.Role, s.Kind, where)
	}
	return fmt.Sprintf("%s: installed, %s (%s %s)", s.Role, s.State, s.Kind, where)
}

// Manager installs, removes and inspects services for one OS.
type Manager struct {
	OS         string // "linux", "darwin" or "windows"
	Home       string // user home; LaunchAgents live under it
	ConfigHome string // os.UserConfigDir(); systemd user units live under it
	DataDir    string // machine-monitor's data dir (config.Paths.Dir)
	DataDirEnv string // MACHINE_MONITOR_DIR at install time, passed to the service; "" if unset
	UID        int    // launchctl's gui/<uid> domain
	User       string // Windows DOMAIN\user that the logon task runs as
	Run        Runner
}

// Default returns a Manager for this machine and user.
func Default() (*Manager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	cfgHome, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	paths, err := config.Default()
	if err != nil {
		return nil, err
	}
	// The service starts from another directory, so a relative override must be made absolute now.
	dirEnv := os.Getenv(config.DirEnv)
	if dirEnv != "" {
		if dirEnv, err = filepath.Abs(dirEnv); err != nil {
			return nil, err
		}
	}
	m := &Manager{OS: runtime.GOOS, Home: home, ConfigHome: cfgHome, DataDir: paths.Dir,
		DataDirEnv: dirEnv, UID: os.Getuid(), Run: ExecRunner}
	if u, err := user.Current(); err == nil {
		m.User = u.Username
	}
	return m, nil
}

// Executable is the absolute, symlink-resolved path of the running binary.
func Executable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	return filepath.Abs(exe)
}

// Install writes the service and starts it. Running it again replaces the service with
// the new spec and restarts it. Each step is printed to out.
func (m *Manager) Install(ctx context.Context, s Spec, out io.Writer) error {
	if err := s.Validate(); err != nil {
		return err
	}
	switch m.OS {
	case "linux":
		return m.installSystemd(ctx, s, out)
	case "darwin":
		return m.installLaunchd(ctx, s, out)
	case "windows":
		return m.installTask(ctx, s, out)
	}
	return m.unsupported()
}

// Uninstall stops and removes the service. Removing one that isn't installed is not an error.
func (m *Manager) Uninstall(ctx context.Context, r Role, out io.Writer) error {
	switch m.OS {
	case "linux":
		return m.uninstallSystemd(ctx, r, out)
	case "darwin":
		return m.uninstallLaunchd(ctx, r, out)
	case "windows":
		return m.uninstallTask(ctx, r, out)
	}
	return m.unsupported()
}

// Status reports whether the role's service is installed and what the manager says of it.
func (m *Manager) Status(ctx context.Context, r Role) (Status, error) {
	switch m.OS {
	case "linux":
		return m.statusSystemd(ctx, r), nil
	case "darwin":
		return m.statusLaunchd(ctx, r), nil
	case "windows":
		return m.statusTask(ctx, r), nil
	}
	return Status{}, m.unsupported()
}

func (m *Manager) unsupported() error {
	return fmt.Errorf("service install is not supported on %s", m.OS)
}

// run calls the Runner and folds its output into the error.
func (m *Manager) run(ctx context.Context, name string, args ...string) (string, error) {
	out, err := m.Run(ctx, name, args...)
	s := strings.TrimSpace(string(out))
	if err != nil {
		if s != "" {
			return s, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, s)
		}
		return s, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return s, nil
}

// writeIfChanged writes data to path with config.WriteFile0600 unless it already holds
// exactly data. It reports whether the file existed before and whether it changed.
func writeIfChanged(path string, data []byte) (existed, changed bool, err error) {
	old, err := os.ReadFile(path)
	if err == nil && string(old) == string(data) {
		return true, false, nil
	}
	existed = err == nil
	return existed, true, config.WriteFile0600(path, data)
}

// fileExists reports whether path is a regular file.
func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}
