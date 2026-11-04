package agent

// Signed power actions (PLAN §3 row 25, M7). Off unless the agent runs with
// --allow-power-actions; then each report advertises the actions this OS supports
// (features "actions") and the hub may put commands in its reply. A command runs only if
// it passes verify: a known action this agent allows, a valid HMAC from the device key (hub key before enrollment) over
// this device's id, the command id, the action and the time it was queued, queued at most
// CommandTTL ago (and at most maxSkew ahead of this machine's clock), and an id not seen
// before. Accepted ids are saved (SeenFile, 0600) until they would expire, so restarting
// the agent doesn't make a captured command run again. Anything else is logged and
// ignored, so someone on the network path without the hub key can't inject or replay one.
//
// Enrolled agents verify only with their device key, with no shared-token fallback.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/config"
	"github.com/ContractorKeith/machine-monitor/internal/report"
)

// PowerActions are the remote actions an agent can allow.
var PowerActions = []string{"sleep", "restart", "shutdown"}

// CommandTTL is how long after it was queued a command may run, matching the hub's TTL.
const CommandTTL = 60 * time.Second

// maxSkew is how far in this machine's future a command's queue time may be: a hub clock
// slightly ahead of ours.
const maxSkew = 5 * time.Second

// maxSeen caps remembered command ids. Only the key holder can add one, and ids leave
// after CommandTTL, so the cap is never reached in use; past it commands are refused.
const maxSeen = 256

// execTimeout bounds one power command.
const execTimeout = 15 * time.Second

// Executor runs one verified power action. SystemExecutor is the real one.
type Executor func(ctx context.Context, action string) error

var cmdIDRE = regexp.MustCompile(`^[a-f0-9]{12}$`)

// Command is a signed power action in the hub's report reply.
type Command struct {
	ID     string `json:"id"`
	Action string `json:"action"`
	T      int64  `json:"t"` // when the hub queued it, Unix ms
	Sig    string `json:"sig"`
}

// CommandSig is hex(HMAC-SHA256(key, "machine-monitor-cmd:<device>:<id>:<action>:<t>")),
// the signature the hub puts on a command (hub.SignCommand).
func CommandSig(key, device, id, action string, t int64) string {
	m := hmac.New(sha256.New, []byte(key))
	fmt.Fprintf(m, "machine-monitor-cmd:%s:%s:%s:%d", device, id, action, t)
	return hex.EncodeToString(m.Sum(nil))
}

// guard verifies commands and remembers the ids it has accepted.
type guard struct {
	key   string
	allow bool
	path  string               // where seen ids are saved; "" keeps them in memory only
	seen  map[string]time.Time // command id -> when it may be forgotten
}

// load reads the saved ids, dropping expired ones. A missing file is no ids; a damaged
// one is reported and treated as none (anyone who can write it already owns the machine).
func (g *guard) load(now time.Time) error {
	g.seen = map[string]time.Time{}
	if g.path == "" {
		return nil
	}
	b, err := os.ReadFile(g.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var saved map[string]int64 // id -> forget after, Unix ms
	if err := json.Unmarshal(b, &saved); err != nil {
		return fmt.Errorf("%s: %w", g.path, err)
	}
	for id, ms := range saved {
		if until := time.UnixMilli(ms); cmdIDRE.MatchString(id) && now.Before(until) && len(g.seen) < maxSeen {
			g.seen[id] = until
		}
	}
	return nil
}

// save writes the seen ids.
func (g *guard) save() error {
	if g.path == "" {
		return nil
	}
	out := make(map[string]int64, len(g.seen))
	for id, until := range g.seen {
		out[id] = until.UnixMilli()
	}
	b, err := json.Marshal(out)
	if err != nil {
		return err
	}
	return config.WriteFile0600(g.path, b)
}

// verify checks one command for device at now and, on success, records its id (saved
// before verify returns, so the command can't run twice even across a restart).
func (g *guard) verify(c Command, device string, now time.Time) error {
	for id, until := range g.seen {
		if now.After(until) {
			delete(g.seen, id)
		}
	}
	switch {
	case !g.allow:
		return errors.New("power actions are off on this machine (--allow-power-actions)")
	case !slices.Contains(SupportedActions(), c.Action) || !cmdIDRE.MatchString(c.ID):
		return errors.New("unknown command")
	case !hmac.Equal([]byte(c.Sig), []byte(CommandSig(g.key, device, c.ID, c.Action, c.T))):
		return errors.New("bad signature")
	}
	queued := time.UnixMilli(c.T)
	if d := now.Sub(queued); d > CommandTTL {
		return errors.New("expired")
	} else if d < -maxSkew {
		return errors.New("queued in the future (hub clock ahead?)")
	}
	if _, dup := g.seen[c.ID]; dup {
		return errors.New("already run (replayed)")
	}
	if len(g.seen) >= maxSeen {
		return errors.New("too many commands")
	}
	if g.seen == nil {
		g.seen = map[string]time.Time{}
	}
	// Past this the TTL check refuses it anyway: queued <= now+maxSkew.
	g.seen[c.ID] = now.Add(CommandTTL + maxSkew)
	if err := g.save(); err != nil {
		delete(g.seen, c.ID)
		return fmt.Errorf("can't record the command id: %w", err)
	}
	return nil
}

// SupportedActions are the PowerActions this OS has a command for.
func SupportedActions() []string {
	var out []string
	for _, a := range PowerActions {
		if _, err := powerCommand(runtime.GOOS, a); err == nil {
			out = append(out, a)
		}
	}
	return out
}

// runCommands verifies and runs each command, queueing one actionResult per command run.
func (a *runner) runCommands(ctx context.Context, device string, cmds []Command) {
	for _, c := range cmds {
		if err := a.guard.verify(c, device, a.opt.Now()); err != nil {
			a.logf("agent: ignored a remote %q command: %v", cleanMsg(c.Action), err)
			continue
		}
		a.logf("agent: running remote %s (command %s)", c.Action, c.ID)
		res := report.ActionResult{ID: c.ID, OK: true, Message: report.Ptr(actionDone[c.Action])}
		if err := a.opt.Exec(ctx, c.Action); err != nil {
			a.logf("agent: %s failed: %v", c.Action, err)
			res.OK, res.Message = false, report.Ptr(cleanMsg(c.Action+" failed: "+err.Error()))
		}
		a.results = append(a.results, res)
	}
}

var actionDone = map[string]string{
	"sleep":    "Going to sleep.",
	"restart":  "Restarting.",
	"shutdown": "Shutting down.",
}

// SystemExecutor runs the OS command for a power action as the current user: systemctl
// on Linux (logind/polkit allow it for the active local session), pmset and System Events
// on macOS, shutdown.exe and powrprof on Windows. Each runs from a fixed path with a
// timeout and a stripped environment.
func SystemExecutor(ctx context.Context, action string) error {
	argv, err := powerCommand(runtime.GOOS, action)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = strippedEnv()
	cmd.WaitDelay = time.Second
	if out, err := cmd.CombinedOutput(); err != nil {
		if len(out) > 0 {
			return fmt.Errorf("%w: %s", err, cleanMsg(string(out)))
		}
		return err
	}
	return nil
}

// powerCommand is the argv for action on goos.
func powerCommand(goos, action string) ([]string, error) {
	if !slices.Contains(PowerActions, action) {
		return nil, fmt.Errorf("unknown action %q", action)
	}
	switch goos {
	case "linux":
		verb := map[string]string{"sleep": "suspend", "restart": "reboot", "shutdown": "poweroff"}[action]
		return []string{systemctlPath(), verb}, nil
	case "darwin":
		switch action {
		case "sleep":
			return []string{"/usr/bin/pmset", "sleepnow"}, nil
		case "restart":
			return []string{"/usr/bin/osascript", "-e", `tell application "System Events" to restart`}, nil
		default:
			return []string{"/usr/bin/osascript", "-e", `tell application "System Events" to shut down`}, nil
		}
	case "windows":
		sys := filepath.Join(systemRoot(), "System32")
		switch action {
		case "sleep":
			return []string{filepath.Join(sys, "rundll32.exe"), "powrprof.dll,SetSuspendState", "0,1,0"}, nil
		case "restart":
			return []string{filepath.Join(sys, "shutdown.exe"), "/r", "/t", "0"}, nil
		default:
			return []string{filepath.Join(sys, "shutdown.exe"), "/s", "/t", "0"}, nil
		}
	}
	return nil, fmt.Errorf("power actions are not supported on %s", goos)
}

// systemctlPath is the first systemctl that exists; PATH is never searched.
func systemctlPath() string {
	for _, p := range []string{"/usr/bin/systemctl", "/bin/systemctl", "/run/current-system/sw/bin/systemctl"} {
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			return p
		}
	}
	return "/usr/bin/systemctl"
}

func systemRoot() string {
	if r := os.Getenv("SystemRoot"); r != "" {
		return r
	}
	return `C:\Windows`
}

// strippedEnv is what a power command sees: a fixed locale and only the variables it needs
// to start. Never nil: os/exec treats a nil Env as "inherit everything".
func strippedEnv() []string {
	env := []string{"LC_ALL=C"}
	for _, k := range []string{"PATH", "HOME", "SystemRoot", "SystemDrive"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}
