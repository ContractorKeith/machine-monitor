package service

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// bootstrapRetry is the wait between bootstrap attempts after a bootout; tests shorten it.
var bootstrapRetry = 500 * time.Millisecond

// launchdLabel is the LaunchAgent label for a role.
func launchdLabel(r Role) string { return "io.github.contractorkeith.machine-monitor." + string(r) }

// plistPath is ~/Library/LaunchAgents/<label>.plist.
func (m *Manager) plistPath(r Role) string {
	return filepath.Join(m.Home, "Library", "LaunchAgents", launchdLabel(r)+".plist")
}

// logPath is where launchd sends the service's stdout and stderr.
func (m *Manager) logPath(r Role) string {
	return filepath.Join(m.DataDir, "logs", string(r)+".log")
}

// domain is launchctl's per-user GUI domain.
func (m *Manager) domain() string { return "gui/" + strconv.Itoa(m.UID) }

// LaunchdPlist renders the LaunchAgent for s: start at login, restart if it exits.
func (m *Manager) LaunchdPlist(s Spec) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
`)
	key := func(k string) { b.WriteString("\t<key>" + xmlEscape(k) + "</key>\n") }
	str := func(indent, v string) { b.WriteString(indent + "<string>" + xmlEscape(v) + "</string>\n") }
	key("Label")
	str("\t", launchdLabel(s.Role))
	key("ProgramArguments")
	b.WriteString("\t<array>\n")
	for _, a := range append([]string{s.Exe}, s.Args()...) {
		str("\t\t", a)
	}
	b.WriteString("\t</array>\n")
	if m.DataDirEnv != "" {
		key("EnvironmentVariables")
		b.WriteString("\t<dict>\n\t\t<key>MACHINE_MONITOR_DIR</key>\n")
		str("\t\t", m.DataDirEnv)
		b.WriteString("\t</dict>\n")
	}
	key("RunAtLoad")
	b.WriteString("\t<true/>\n")
	key("KeepAlive")
	b.WriteString("\t<true/>\n")
	key("ThrottleInterval")
	b.WriteString("\t<integer>10</integer>\n")
	key("ProcessType")
	str("\t", "Background")
	key("StandardOutPath")
	str("\t", m.logPath(s.Role))
	key("StandardErrorPath")
	str("\t", m.logPath(s.Role))
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

// xmlEscape escapes text for an XML element. Quotes need no escaping there.
var xmlEscape = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace

func (m *Manager) installLaunchd(ctx context.Context, s Spec, out io.Writer) error {
	path, label := m.plistPath(s.Role), launchdLabel(s.Role)
	// launchd opens the log file but won't create its directory.
	if err := os.MkdirAll(filepath.Dir(m.logPath(s.Role)), 0o700); err != nil {
		return err
	}
	existed, changed, err := writeIfChanged(path, []byte(m.LaunchdPlist(s)))
	if err != nil {
		return err
	}
	switch {
	case !changed:
		fmt.Fprintf(out, "%s is up to date\n", path)
	case existed:
		fmt.Fprintf(out, "updated %s\n", path)
	default:
		fmt.Fprintf(out, "wrote %s\n", path)
	}
	// Unload any previous copy so the new plist takes effect; failing means none was loaded.
	booted := false
	if _, err := m.run(ctx, "launchctl", "bootout", m.domain()+"/"+label); err == nil {
		booted = true
		fmt.Fprintf(out, "ran launchctl bootout %s/%s\n", m.domain(), label)
	}
	// bootout returns before the old job is gone, and bootstrap fails until it is: retry.
	for try := 0; ; try++ {
		_, err := m.run(ctx, "launchctl", "bootstrap", m.domain(), path)
		if err == nil {
			break
		}
		if !booted || try == 4 {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(bootstrapRetry):
		}
	}
	fmt.Fprintf(out, "ran launchctl bootstrap %s %s\n", m.domain(), path)
	fmt.Fprintf(out, "machine-monitor %s starts at login (logs: %s)\n", s.Role, m.logPath(s.Role))
	return nil
}

func (m *Manager) uninstallLaunchd(ctx context.Context, r Role, out io.Writer) error {
	path, label := m.plistPath(r), launchdLabel(r)
	if !fileExists(path) {
		fmt.Fprintf(out, "%s: not installed (%s)\n", r, path)
		return nil
	}
	if _, err := m.run(ctx, "launchctl", "bootout", m.domain()+"/"+label); err == nil {
		fmt.Fprintf(out, "ran launchctl bootout %s/%s\n", m.domain(), label)
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	fmt.Fprintf(out, "removed %s\n", path)
	return nil
}

func (m *Manager) statusLaunchd(ctx context.Context, r Role) Status {
	st := Status{Role: r, Kind: "LaunchAgent", Name: launchdLabel(r), Path: m.plistPath(r)}
	st.Installed = fileExists(st.Path)
	if !st.Installed {
		return st
	}
	out, err := m.run(ctx, "launchctl", "print", m.domain()+"/"+st.Name)
	if err != nil {
		st.State = "not loaded"
		return st
	}
	st.State = "loaded"
	for line := range strings.SplitSeq(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), " = "); ok && k == "state" {
			st.State = "loaded, " + strings.TrimSpace(v)
			break
		}
	}
	return st
}
