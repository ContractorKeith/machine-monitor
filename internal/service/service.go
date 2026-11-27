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
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/config"
)

type Role string

const (
	RoleHub   Role = "hub"
	RoleAgent Role = "agent"
)

var Roles = []Role{RoleHub, RoleAgent}

func ParseRole(s string) (Role, error) {
	switch r := Role(s); r {
	case RoleHub, RoleAgent:
		return r, nil
	}
	return "", fmt.Errorf("bad role %q (want hub or agent)", s)
}

type Spec struct {
	Role   Role
	Exe    string
	Port   int
	HubURL string
}

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

func (s Spec) Args() []string {
	if s.Role == RoleAgent {
		return []string{"agent", "--hub", s.HubURL}
	}
	if s.Port != 0 {
		return []string{"hub", "--port", strconv.Itoa(s.Port)}
	}
	return []string{"hub"}
}

type Status struct {
	Role      Role
	Kind      string
	Name      string
	Path      string
	Installed bool
	State     string
}

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

type Manager struct {
	OS         string
	Home       string
	ConfigHome string
	DataDir    string
	DataDirEnv string
	UID        int
	User       string
	Run        Runner
}

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

type backup struct {
	data  []byte
	mode  os.FileMode
	mtime time.Time
	link  string
}

func writeIfChanged(path string, data []byte) (old backup, existed, changed bool, err error) {
	b, err := os.ReadFile(path)
	if err == nil && string(b) == string(data) {
		return backup{}, true, false, nil
	}
	existed = err == nil
	if existed {
		old.data = b
		if fi, err := os.Stat(path); err == nil {
			old.mode, old.mtime = fi.Mode().Perm(), fi.ModTime()
		}
		if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			old.link, _ = os.Readlink(path)
		}
	}
	return old, existed, true, config.WriteFile0600(path, data)
}

func restoreFile(path string, old backup) error {
	if old.link != "" {
		if err := os.Remove(path); err != nil {
			return err
		}
		return os.Symlink(old.link, path)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	_, err = f.Write(old.data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if old.mode != 0 {
		if err := os.Chmod(path, old.mode); err != nil {
			return err
		}
	}
	if !old.mtime.IsZero() {
		return os.Chtimes(path, old.mtime, old.mtime)
	}
	return nil
}

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}
