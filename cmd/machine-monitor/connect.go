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
	"strconv"
	"strings"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/agent"
	"github.com/ContractorKeith/machine-monitor/internal/config"
	"github.com/ContractorKeith/machine-monitor/internal/discovery"
	"github.com/ContractorKeith/machine-monitor/internal/tui"
)

// stdin is where connect reads the pairing code; tests replace it.
var stdin io.Reader = os.Stdin

// stdinIsTerminal reports whether a person can answer the pairing prompt; tests replace it.
var stdinIsTerminal = func() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// stdoutIsTerminal reports whether w is a terminal the TUI can draw on; tests replace it.
var stdoutIsTerminal = func(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// connectTUI opens the TUI against the hub after a successful connect; tests replace it.
var connectTUI = func(hubURL, key string, stderr io.Writer) int {
	return runConnected(hubURL, key, tui.OptionsFromEnv(tui.Options{}), stderr)
}

// discoverHubs finds hubs on the LAN (1.5 s, as upstream); tests replace it.
var discoverHubs = func() ([]discovery.Hub, error) { return discovery.Discover(1500 * time.Millisecond) }

const connectUsage = "usage: machine-monitor connect [--code CODE] [--key-stdin] [--no-tui] [URL]"

// runConnect is `machine-monitor connect [--code CODE] [--key-stdin] [--no-tui] [URL]`: pair
// this machine with a hub, save client.json, then open the TUI on the hub (when stdout is a
// terminal; otherwise it prints what to run next). Without a URL it looks for hubs on the
// LAN and offers them. URL may be a dashboard link with ?key=, or --key-stdin reads the key
// from stdin (keeping it out of argv, as the hub's installers do); either skips the pairing
// code. Either way the hub must prove it holds the key before the key is saved or sent.
// --no-agent is the old name of --no-tui, which the hub's installers still pass.
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
		fmt.Fprintln(stderr, connectUsage)
		return 2
	}
	fail := func(code int, format string, a ...any) int {
		fmt.Fprintf(stderr, "machine-monitor connect: "+format+"\n", a...)
		return code
	}
	in := bufio.NewReader(stdin) // one reader, so the hub choice and the code don't lose each other's input
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
	if key == "" && *codeFlag == "" && !stdinIsTerminal() { // nobody to answer the prompt
		return fail(2, "no terminal to ask for the pairing code: pass --code CODE (%s)", connectUsage)
	}
	paths, err := config.Default()
	if err != nil {
		return fail(1, "%v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	c := agent.NewHTTPClient()
	if key != "" { // dashboard link
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
		if key, err = pairLoop(ctx, in, c, hubURL, *codeFlag, stdout); err != nil {
			return fail(1, "%v", err)
		}
	}

	if old, err := agent.LoadClient(paths.Client); err == nil && old.HubURL != "" && old.HubURL != hubURL {
		fmt.Fprintf(stdout, "Replacing the pairing with %s.\n", old.HubURL)
	}
	if err := agent.SaveClient(paths.Client, agent.Client{HubURL: hubURL, Key: key}); err != nil {
		return fail(1, "save %s: %v", paths.Client, err)
	}
	fmt.Fprintf(stdout, "Paired with %s (saved in %s).\n", hubURL, paths.Client)
	if *noTUI || !stdoutIsTerminal(stdout) {
		fmt.Fprintln(stdout, "Next: run `machine-monitor agent` to report this machine to the hub, or `machine-monitor service install --role agent` to start it at login.")
		return 0
	}
	return connectTUI(hubURL, key, stderr)
}

// chooseHub looks for hubs on the LAN and returns the address of the one to connect to. With
// no terminal to ask on it lists what it found and fails, so a script never connects to a
// guess.
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

// pairLoop pairs with code, or prompts for one until it works, the hub kills the code, or
// stdin ends. A code given with --code is tried once.
func pairLoop(ctx context.Context, in *bufio.Reader, c *http.Client, hubURL, code string, stdout io.Writer) (string, error) {
	prompt := code == ""
	for {
		if prompt {
			var err error
			if code, err = readCode(in, stdout); err != nil {
				return "", err
			}
		}
		pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		key, err := agent.Connect(pctx, c, hubURL, code)
		cancel()
		var pe *agent.PairError
		if err == nil || !prompt || !errors.As(err, &pe) || !pe.Retry {
			return key, err
		}
		fmt.Fprintln(stdout, pe.Msg)
	}
}

// readCode prompts for a pairing code on stdout and reads one line from in.
func readCode(in *bufio.Reader, stdout io.Writer) (string, error) {
	fmt.Fprint(stdout, "Pairing code from the hub: ")
	line, err := in.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" && err != nil {
		return "", errors.New("no pairing code entered")
	}
	return line, nil
}
