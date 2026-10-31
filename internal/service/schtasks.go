package service

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"

	"github.com/ContractorKeith/machine-monitor/internal/config"
)

// taskName is the Task Scheduler task for a role, in the root folder.
func taskName(r Role) string { return "machine-monitor-" + string(r) }

// conhost runs the console binary with no window. Task Scheduler expands %SystemRoot%.
const conhost = `%SystemRoot%\System32\conhost.exe`

// TaskXML renders the logon task for s. It runs as the current user with an interactive
// token and least privilege, so creating it needs no elevation. The binary is started
// through `conhost.exe --headless` so a console window doesn't open at every login.
func (m *Manager) TaskXML(s Spec) string {
	return m.taskXML("machine-monitor "+string(s.Role), append([]string{s.Exe}, s.Args()...))
}

// taskXML renders a logon task that runs argv (argv[0] is the binary) under conhost.
func (m *Manager) taskXML(what string, argv []string) string {
	args := append([]string{"--headless"}, argv...)
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = winQuote(a)
	}
	user := xmlEscape(m.User)
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>` + xmlEscape(what) + `. Written by machine-monitor service install; remove with machine-monitor service uninstall.</Description>
  </RegistrationInfo>
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>
      <UserId>` + user + `</UserId>
    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>` + user + `</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <StartWhenAvailable>true</StartWhenAvailable>
    <IdleSettings>
      <StopOnIdleEnd>false</StopOnIdleEnd>
      <RestartOnIdle>false</RestartOnIdle>
    </IdleSettings>
    <Hidden>true</Hidden>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <RestartOnFailure>
      <Interval>PT1M</Interval>
      <Count>3</Count>
    </RestartOnFailure>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>` + conhost + `</Command>
      <Arguments>` + xmlEscape(strings.Join(quoted, " ")) + `</Arguments>
    </Exec>
  </Actions>
</Task>
`)
	return b.String()
}

// winQuote quotes one argument for a Windows command line, by the rules
// CommandLineToArgvW reverses (the same as syscall.EscapeArg, which is Windows-only).
func winQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\"") {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\':
			slashes++
		case '"':
			b.WriteString(strings.Repeat(`\`, slashes+1))
			slashes = 0
		default:
			slashes = 0
		}
		b.WriteByte(s[i])
	}
	b.WriteString(strings.Repeat(`\`, slashes)) // so the closing quote isn't escaped
	b.WriteByte('"')
	return b.String()
}

// utf16File encodes s as UTF-16LE with a byte-order mark, the encoding schtasks /XML expects.
func utf16File(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, 2, 2+2*len(u))
	b[0], b[1] = 0xFF, 0xFE
	for _, c := range u {
		b = append(b, byte(c), byte(c>>8))
	}
	return b
}

func (m *Manager) installTask(ctx context.Context, s Spec, out io.Writer) error {
	if m.User == "" {
		return fmt.Errorf("can't tell the current Windows user")
	}
	name := taskName(s.Role)
	if err := m.createTask(ctx, name, m.TaskXML(s)); err != nil {
		return err
	}
	fmt.Fprintf(out, "ran schtasks /Create /TN %s /F (logon task for %s)\n", name, m.User)
	// Stop a copy started by an earlier install so /Run starts the new command line.
	if _, err := m.run(ctx, "schtasks", "/End", "/TN", name); err == nil {
		fmt.Fprintf(out, "ran schtasks /End /TN %s\n", name)
	}
	if _, err := m.run(ctx, "schtasks", "/Run", "/TN", name); err != nil {
		return err
	}
	fmt.Fprintf(out, "ran schtasks /Run /TN %s\n", name)
	fmt.Fprintf(out, "machine-monitor %s starts at login (Task Scheduler task %s)\n", s.Role, name)
	if m.DataDirEnv != "" {
		fmt.Fprintf(out, "warning: a logon task can't set MACHINE_MONITOR_DIR; the service uses the default data dir\n")
	}
	return nil
}

// createTask creates or replaces the task name from its XML. schtasks reads the definition
// from a file, so it's kept in the data dir only while schtasks runs.
func (m *Manager) createTask(ctx context.Context, name, xml string) error {
	xmlPath := filepath.Join(m.DataDir, "service-"+name+".xml")
	if err := config.WriteFile0600(xmlPath, utf16File(xml)); err != nil {
		return err
	}
	defer os.Remove(xmlPath)
	_, err := m.run(ctx, "schtasks", "/Create", "/TN", name, "/XML", xmlPath, "/F")
	return err
}

func (m *Manager) uninstallTask(ctx context.Context, r Role, out io.Writer) error {
	name := taskName(r)
	if !m.statusTask(ctx, r).Installed {
		fmt.Fprintf(out, "%s: not installed (Task Scheduler task %s)\n", r, name)
		return nil
	}
	if _, err := m.run(ctx, "schtasks", "/End", "/TN", name); err == nil {
		fmt.Fprintf(out, "ran schtasks /End /TN %s\n", name)
	}
	if _, err := m.run(ctx, "schtasks", "/Delete", "/TN", name, "/F"); err != nil {
		return err
	}
	fmt.Fprintf(out, "ran schtasks /Delete /TN %s /F\n", name)
	return nil
}

func (m *Manager) statusTask(ctx context.Context, r Role) Status {
	st := Status{Role: r, Kind: "Task Scheduler logon task", Name: taskName(r)}
	st.Installed, st.State = m.queryTask(ctx, st.Name)
	return st
}

// queryTask reports whether the task exists and its status ("Ready", "Running", ...;
// the words are localized).
func (m *Manager) queryTask(ctx context.Context, name string) (exists bool, state string) {
	out, err := m.run(ctx, "schtasks", "/Query", "/TN", name, "/FO", "CSV", "/NH")
	if err != nil {
		return false, "" // the task doesn't exist
	}
	// One row: "\machine-monitor-hub","<next run>","<status>".
	if rec, err := csv.NewReader(strings.NewReader(out)).Read(); err == nil && len(rec) >= 3 {
		return true, strings.TrimSpace(rec[len(rec)-1])
	}
	return true, "unknown"
}
