package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/config"
	"github.com/ContractorKeith/machine-monitor/internal/service"
)

const serviceUsage = `Usage:
  machine-monitor service install [--role hub] [--port 7777]   start the hub at login
  machine-monitor service install --hub URL                    start an agent at login
  machine-monitor service uninstall [--role hub|agent]         stop and remove (both roles by default)
  machine-monitor service status [--role hub|agent]            show the install state
  machine-monitor service run -- hub|agent [flags]             run a role detached, logging to a file
                                                               (what the Windows logon task starts)
`

// Seams for tests.
var (
	newServiceManager = service.Default
	serviceExecutable = service.Executable
)

// agentBuilt reports whether this binary's agent command is real rather than the stub.
// It asks the dispatcher with -h, which a real command answers with usage and no side
// effects. Set in init: the commands table refers to runService, so a package-level
// initializer would be a cycle.
var agentBuilt func() bool

// roleCommand is the dispatcher entry for a role; set in init for the same reason.
var roleCommand func(r service.Role) func(args []string, stdout, stderr io.Writer) int

func init() {
	agentBuilt = func() bool {
		var errOut bytes.Buffer
		commands["agent"]([]string{"-h"}, io.Discard, &errOut)
		return !strings.Contains(errOut.String(), "not built yet")
	}
	roleCommand = func(r service.Role) func(args []string, stdout, stderr io.Writer) int {
		return commands[string(r)]
	}
}

// redirectOutput points the whole process's output at f and, on Windows, detaches from
// the console so no window stays open. Tests replace it.
var redirectOutput = func(f *os.File) {
	os.Stdout, os.Stderr = f, f
	log.SetOutput(f)
	detachConsole()
}

// runServiceRun is `service run -- hub|agent [flags]`: the role's command with its output
// appended to <data dir>/logs/<role>.log.
func runServiceRun(args []string, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		fmt.Fprint(stderr, "machine-monitor service run: missing role\n\n"+serviceUsage)
		return 2
	}
	role, err := service.ParseRole(args[0])
	if err != nil {
		fmt.Fprintf(stderr, "machine-monitor service run: %v\n", err)
		return 2
	}
	paths, err := config.Default()
	if err != nil {
		fmt.Fprintf(stderr, "machine-monitor service run: %v\n", err)
		return 1
	}
	logPath := service.LogFile(paths.Dir, role)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		fmt.Fprintf(stderr, "machine-monitor service run: %v\n", err)
		return 1
	}
	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		fmt.Fprintf(stderr, "machine-monitor service run: %v\n", err)
		return 1
	}
	defer f.Close()
	redirectOutput(f)
	fmt.Fprintf(f, "%s machine-monitor service run %s\n", time.Now().Format(time.RFC3339), strings.Join(args, " "))
	code := roleCommand(role)(args[1:], f, f)
	fmt.Fprintf(f, "%s exited %d\n", time.Now().Format(time.RFC3339), code)
	return code
}

// runService is `machine-monitor service install|uninstall|status`: a user-level autostart
// (systemd user unit, LaunchAgent or logon task). No admin rights needed.
func runService(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprint(stderr, "machine-monitor service: missing action\n\n"+serviceUsage)
		return 2
	}
	action := args[0]
	if action == "run" {
		return runServiceRun(args[1:], stderr)
	}
	fs := flag.NewFlagSet("service", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, serviceUsage) }
	roleFlag := fs.String("role", "", "hub or agent")
	hubURL := fs.String("hub", "", "hub URL the agent reports to (install --role agent)")
	port := fs.Int("port", 0, "hub port (install --role hub); default 7777")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "machine-monitor service: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	var roles []service.Role
	if *roleFlag != "" {
		r, err := service.ParseRole(*roleFlag)
		if err != nil {
			fmt.Fprintf(stderr, "machine-monitor service: %v\n", err)
			return 2
		}
		roles = []service.Role{r}
	}
	if action != "install" && (*hubURL != "" || *port != 0) {
		fmt.Fprintf(stderr, "machine-monitor service %s: --hub and --port are for install\n", action)
		return 2
	}

	m, err := newServiceManager()
	if err != nil {
		fmt.Fprintf(stderr, "machine-monitor service: %v\n", err)
		return 1
	}
	ctx := context.Background()
	switch action {
	case "install":
		role := service.RoleHub
		if *hubURL != "" {
			role = service.RoleAgent
		}
		if len(roles) == 1 {
			role = roles[0]
		}
		exe, err := serviceExecutable()
		if err != nil {
			fmt.Fprintf(stderr, "machine-monitor service: can't find this binary: %v\n", err)
			return 1
		}
		spec := service.Spec{Role: role, Exe: exe, Port: *port, HubURL: *hubURL}
		if err := spec.Validate(); err != nil {
			fmt.Fprintf(stderr, "machine-monitor service install: %v\n", err)
			return 2
		}
		if role == service.RoleAgent && !agentBuilt() {
			fmt.Fprintln(stderr, "warning: this build's agent command isn't built yet, so the service will exit and restart until machine-monitor is updated")
		}
		if inTempDir(exe) {
			fmt.Fprintf(stderr, "warning: %s is in a temp directory (go run?); the service will break when it's cleaned up\n", exe)
		}
		if err := m.Install(ctx, spec, stdout); err != nil {
			fmt.Fprintf(stderr, "machine-monitor service install: %v\n", err)
			return 1
		}
	case "uninstall":
		if roles == nil {
			roles = service.Roles
		}
		for _, r := range roles {
			if err := m.Uninstall(ctx, r, stdout); err != nil {
				fmt.Fprintf(stderr, "machine-monitor service uninstall: %v\n", err)
				return 1
			}
		}
	case "status":
		if roles == nil {
			roles = service.Roles
		}
		for _, r := range roles {
			st, err := m.Status(ctx, r)
			if err != nil {
				fmt.Fprintf(stderr, "machine-monitor service status: %v\n", err)
				return 1
			}
			fmt.Fprintln(stdout, st)
		}
	default:
		fmt.Fprintf(stderr, "machine-monitor service: unknown action %q\n\n%s", action, serviceUsage)
		return 2
	}
	return 0
}

// inTempDir reports whether path is under the OS temp directory.
func inTempDir(path string) bool {
	tmp, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		tmp = os.TempDir()
	}
	rel, err := filepath.Rel(tmp, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
