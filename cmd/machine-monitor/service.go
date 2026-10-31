package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ContractorKeith/machine-monitor/internal/service"
)

const serviceUsage = `Usage:
  machine-monitor service install [--role hub] [--port 7777]   start the hub at login
  machine-monitor service install --hub URL                    start an agent at login
  machine-monitor service uninstall [--role hub|agent]         stop and remove (both roles by default)
  machine-monitor service status [--role hub|agent]            show the install state
`

// Seams for tests.
var (
	newServiceManager = service.Default
	serviceExecutable = service.Executable
)

// runService is `machine-monitor service install|uninstall|status`: a user-level autostart
// (systemd user unit, LaunchAgent or logon task). No admin rights needed.
func runService(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprint(stderr, "machine-monitor service: missing action\n\n"+serviceUsage)
		return 2
	}
	action := args[0]
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
