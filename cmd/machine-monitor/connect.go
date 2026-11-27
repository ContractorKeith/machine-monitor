package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/x/term"

	"github.com/ContractorKeith/machine-monitor/internal/agent"
	"github.com/ContractorKeith/machine-monitor/internal/collect"
	"github.com/ContractorKeith/machine-monitor/internal/config"
	"github.com/ContractorKeith/machine-monitor/internal/discovery"
	"github.com/ContractorKeith/machine-monitor/internal/tui"
)

var stdin io.Reader = os.Stdin

var stdinIsTerminal = func() bool { return term.IsTerminal(os.Stdin.Fd()) }

var stdoutIsTerminal = func(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(f.Fd())
}

var connectTUI = func(hubURL, key string, stderr io.Writer) int {
	return runConnected(hubURL, key, tuiOptions(tui.Options{}), stderr)
}

var discoverHubs = func() ([]discovery.Hub, error) { return discovery.Discover(1500 * time.Millisecond) }

const nextHint = "Next: this machine can watch the hub but doesn't report to it yet. Run `machine-monitor agent` to report, or `machine-monitor service install --role agent` to start reporting at login."

const connectUsage = "usage: machine-monitor connect [--code CODE] [--key-stdin] [--no-tui] [URL]"

func runConnect(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("connect", flag.ContinueOnError)
	fs.SetOutput(stderr)
	codeFlag := fs.String("code", "", "pairing code from the hub (prompted for when omitted)")
	noTUI := fs.Bool("no-tui", false, "save the pairing and exit instead of opening the TUI")
	fs.BoolVar(noTUI, "no-agent", false, "same as --no-tui")
	keyStdin := fs.Bool("key-stdin", false, "read the hub key from the first line of stdin")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 1 {
		if slices.ContainsFunc(fs.Args()[1:], func(a string) bool { return strings.HasPrefix(a, "-") }) {
			fmt.Fprintf(stderr, "machine-monitor connect: flags go before the URL, not after it (Go stops reading flags at the first argument): machine-monitor connect [flags] %s\n", fs.Arg(0))
			return 2
		}
		fmt.Fprintf(stderr, "machine-monitor connect: one hub at a time, got %d arguments\n%s\n", fs.NArg(), connectUsage)
		return 2
	}
	fail := func(code int, format string, a ...any) int {
		fmt.Fprintf(stderr, "machine-monitor connect: "+format+"\n", a...)
		return code
	}
	in := bufio.NewReader(stdin)
	target := fs.Arg(0)
	if target == "" {
		if *keyStdin {
			return fail(2, "--key-stdin reads the key from stdin, so name the hub: %s", connectUsage)
		}
		var code int
		if target, code = chooseHub(in, stdout, stderr); code != 0 {
			return code
		}
	}
	hubURL, key, err := agent.NormalizeURL(target)
	if err != nil {
		return fail(2, "%v", err)
	}
	if *keyStdin {
		if key != "" || *codeFlag != "" {
			return fail(2, "--key-stdin can't be combined with --code or a link with ?key=")
		}
		line, err := in.ReadString('\n')
		if key = strings.TrimSpace(line); key == "" {
			return fail(2, "--key-stdin: no key on stdin (%v)", err)
		}
	}
	if key == "" && *codeFlag == "" && !stdinIsTerminal() {
		return fail(2, "no terminal to ask for the pairing code: pass --code CODE (%s)", connectUsage)
	}
	paths, err := config.Default()
	if err != nil {
		return fail(1, "%v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	c := agent.NewHTTPClient()
	enrolled := &agent.DeviceCredentials{}
	if key != "" {
		if !agent.ValidKey(key) {
			return fail(1, "that link carries an unusable key")
		}
		pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err = agent.Prove(pctx, c, hubURL, key)
		cancel()
		if err != nil {
			return fail(1, "%s: %v", hubURL, err)
		}
	} else {
		if key, err = pairLoop(ctx, in, c, hubURL, *codeFlag, stdout, enrolled); err != nil {
			return fail(1, "%v", err)
		}
	}

	if old, err := agent.LoadClient(paths.Client); err == nil && old.HubURL == hubURL && enrolled.DeviceKey == "" {
		enrolled.DeviceKey = old.DeviceKey
	}
	if old, err := agent.LoadClient(paths.Client); err == nil && old.HubURL != "" && old.HubURL != hubURL {
		fmt.Fprintf(stdout, "Replacing the pairing with %s.\n", old.HubURL)
	}
	if err := agent.SaveClient(paths.Client, agent.Client{HubURL: hubURL, Key: key, DeviceKey: enrolled.DeviceKey}); err != nil {
		return fail(1, "save %s: %v", paths.Client, err)
	}
	fmt.Fprintf(stdout, "Paired with %s (saved in %s).\n", hubURL, paths.Client)

	if *noTUI || !stdoutIsTerminal(stdout) || (*keyStdin && !stdinIsTerminal()) {
		fmt.Fprintln(stdout, nextHint)
		return 0
	}

	fmt.Fprintln(stdout, nextHint)
	code := connectTUI(hubURL, key, stderr)
	fmt.Fprintln(stdout, nextHint)
	return code
}

func chooseHub(in *bufio.Reader, stdout, stderr io.Writer) (string, int) {
	fmt.Fprintln(stdout, "Looking for hubs on this network...")
	hubs, err := discoverHubs()
	if err != nil {
		fmt.Fprintf(stderr, "machine-monitor connect: can't search the network: %v. Name the hub: %s\n", err, connectUsage)
		return "", 1
	}
	if len(hubs) == 0 {
		fmt.Fprintf(stderr, "machine-monitor connect: no hubs answered. Is one running (machine-monitor hub)? Or name it: %s\n", connectUsage)
		return "", 1
	}
	for i, h := range hubs {
		fmt.Fprintf(stdout, "  %d. %s  %s\n", i+1, h.Name, h.URL)
	}
	if !stdinIsTerminal() {
		fmt.Fprintf(stderr, "machine-monitor connect: found %d hub(s) but there is no terminal to choose one: pass its URL (%s)\n", len(hubs), connectUsage)
		return "", 2
	}
	for {
		fmt.Fprintf(stdout, "Connect to which hub? [1-%d, Enter for 1, q to quit]: ", len(hubs))
		line, err := in.ReadString('\n')
		line = strings.TrimSpace(line)
		if line == "" && err != nil || strings.EqualFold(line, "q") {
			fmt.Fprintln(stderr, "machine-monitor connect: no hub chosen")
			return "", 1
		}
		n := 1
		if line != "" {
			if n, err = strconv.Atoi(line); err != nil || n < 1 || n > len(hubs) {
				fmt.Fprintf(stdout, "Enter a number from 1 to %d.\n", len(hubs))
				continue
			}
		}
		return hubs[n-1].URL, 0
	}
}

func pairLoop(ctx context.Context, in *bufio.Reader, c *http.Client, hubURL, code string, stdout io.Writer, enrollment *agent.DeviceCredentials) (string, error) {
	if enrollment != nil {
		id, err := collect.DeviceID(ctx)
		if err != nil {
			return "", err
		}
		enrollment.DeviceID = id
	}
	prompt := code == ""
	for {
		if prompt {
			var err error
			if code, err = readCode(in, stdout); err != nil {
				return "", err
			}
		}
		pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		key, err := agent.Connect(pctx, c, hubURL, code, enrollment)
		cancel()
		var pe *agent.PairError
		if err == nil || !prompt || !errors.As(err, &pe) || !pe.Retry {
			return key, err
		}
		fmt.Fprintln(stdout, pe.Msg)
	}
}

func readCode(in *bufio.Reader, stdout io.Writer) (string, error) {
	fmt.Fprint(stdout, `Pairing code (run "machine-monitor pair" on the hub, or Add device in its TUI): `)
	line, err := in.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" && err != nil {
		return "", errors.New("no pairing code entered")
	}
	return line, nil
}
