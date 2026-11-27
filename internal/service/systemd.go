package service

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func unitName(r Role) string { return "machine-monitor-" + string(r) + ".service" }

func (m *Manager) unitPath(r Role) string {
	return filepath.Join(m.ConfigHome, "systemd", "user", unitName(r))
}

func (m *Manager) SystemdUnit(s Spec) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Written by `machine-monitor service install`; remove with `machine-monitor service uninstall`.\n")
	fmt.Fprintf(&b, "[Unit]\nDescription=machine-monitor %s\n\n", s.Role)
	b.WriteString("[Service]\n")
	b.WriteString("ExecStart=" + systemdQuote(s.Exe))
	for _, a := range s.Args() {
		b.WriteString(" " + systemdQuote(a))
	}
	b.WriteString("\n")
	if m.DataDirEnv != "" {
		b.WriteString("Environment=" + systemdEnvQuote("MACHINE_MONITOR_DIR="+m.DataDirEnv) + "\n")
	}
	b.WriteString("Restart=always\nRestartSec=10\n\n")
	b.WriteString("[Install]\nWantedBy=default.target\n")
	return b.String()
}

func systemdQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "$", "$$", "\n", `\n`)
	return `"` + r.Replace(s) + `"`
}

func systemdEnvQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "\n", `\n`)
	return `"` + r.Replace(s) + `"`
}

func (m *Manager) installSystemd(ctx context.Context, s Spec, out io.Writer) error {
	path, name := m.unitPath(s.Role), unitName(s.Role)

	wasDisabled := false
	if fileExists(path) {
		state, _ := m.run(ctx, "systemctl", "--user", "is-enabled", name)
		wasDisabled = firstLine(state, "") == "disabled"
	}
	old, existed, changed, err := writeIfChanged(path, []byte(m.SystemdUnit(s)))
	if err != nil {
		return err
	}
	switch {
	case !changed:
		fmt.Fprintf(out, "unit %s is up to date\n", path)
	case existed:
		fmt.Fprintf(out, "updated %s\n", path)
	default:
		fmt.Fprintf(out, "wrote %s\n", path)
	}
	steps := [][]string{{"--user", "daemon-reload"}, {"--user", "enable", "--now", name}}
	if existed && changed {
		steps = append(steps, []string{"--user", "restart", name})
	}
	for _, args := range steps {
		if _, err := m.run(ctx, "systemctl", args...); err != nil {
			if existed && changed {
				if rerr := restoreFile(path, old); rerr != nil {
					return fmt.Errorf("%w (and the old unit could not be put back: %v)", err, rerr)
				}
				_, _ = m.run(ctx, "systemctl", "--user", "daemon-reload")
			}
			if wasDisabled {
				_, _ = m.run(ctx, "systemctl", "--user", "disable", "--now", name)
			}
			return err
		}
		fmt.Fprintf(out, "ran systemctl %s\n", strings.Join(args, " "))
	}
	fmt.Fprintf(out, "machine-monitor %s starts at login (logs: journalctl --user -u %s)\n", s.Role, name)
	fmt.Fprintf(out, "To keep it running while you are logged out: loginctl enable-linger %s\n", m.userName())
	return nil
}

func (m *Manager) uninstallSystemd(ctx context.Context, r Role, out io.Writer) error {
	path, name := m.unitPath(r), unitName(r)
	if !fileExists(path) {
		fmt.Fprintf(out, "%s: not installed (%s)\n", r, path)
		return nil
	}

	if _, err := m.run(ctx, "systemctl", "--user", "disable", "--now", name); err != nil {
		fmt.Fprintf(out, "warning: %v\n", err)
	} else {
		fmt.Fprintf(out, "ran systemctl --user disable --now %s\n", name)
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	fmt.Fprintf(out, "removed %s\n", path)
	if _, err := m.run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	fmt.Fprintf(out, "ran systemctl --user daemon-reload\n")
	return nil
}

func (m *Manager) statusSystemd(ctx context.Context, r Role) Status {
	st := Status{Role: r, Kind: "systemd user unit", Name: unitName(r), Path: m.unitPath(r)}
	st.Installed = fileExists(st.Path)
	if !st.Installed {
		return st
	}

	enabled, _ := m.run(ctx, "systemctl", "--user", "is-enabled", st.Name)
	active, _ := m.run(ctx, "systemctl", "--user", "is-active", st.Name)
	st.State = firstLine(enabled, "unknown") + ", " + firstLine(active, "unknown")
	return st
}

func (m *Manager) userName() string {
	if m.User != "" {
		return m.User
	}
	return "$USER"
}

func firstLine(s, def string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if s == "" {
		return def
	}
	return strings.TrimSpace(s)
}
