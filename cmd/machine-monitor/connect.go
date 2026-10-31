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
	"strings"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/agent"
	"github.com/ContractorKeith/machine-monitor/internal/config"
)

// stdin is where connect reads the pairing code; tests replace it.
var stdin io.Reader = os.Stdin

// stdinIsTerminal reports whether a person can answer the pairing prompt; tests replace it.
var stdinIsTerminal = func() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// connectAgent starts the agent after a successful connect; tests replace it.
var connectAgent = startAgent

// runConnect is `machine-monitor connect [--code CODE] [--key-stdin] [--no-agent] URL`: pair
// this machine with a hub, save client.json, then report to the hub. URL may be a dashboard
// link with ?key=, or --key-stdin reads the key from stdin (keeping it out of argv, as the
// hub's installers do); either skips the pairing code. Either way the hub must prove it
// holds the key before the key is saved or sent.
func runConnect(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("connect", flag.ContinueOnError)
	fs.SetOutput(stderr)
	codeFlag := fs.String("code", "", "pairing code from the hub (prompted for when omitted)")
	noAgent := fs.Bool("no-agent", false, "save the pairing and exit instead of starting the agent")
	keyStdin := fs.Bool("key-stdin", false, "read the hub key from the first line of stdin")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: machine-monitor connect [--code CODE] [--key-stdin] [--no-agent] URL")
		return 2
	}
	fail := func(code int, format string, a ...any) int {
		fmt.Fprintf(stderr, "machine-monitor connect: "+format+"\n", a...)
		return code
	}
	hubURL, key, err := agent.NormalizeURL(fs.Arg(0))
	if err != nil {
		return fail(2, "%v", err)
	}
	if *keyStdin {
		if key != "" || *codeFlag != "" {
			return fail(2, "--key-stdin can't be combined with --code or a link with ?key=")
		}
		line, err := bufio.NewReader(stdin).ReadString('\n')
		if key = strings.TrimSpace(line); key == "" {
			return fail(2, "--key-stdin: no key on stdin (%v)", err)
		}
	}
	if key == "" && *codeFlag == "" && !stdinIsTerminal() { // nobody to answer the prompt
		return fail(2, "no terminal to ask for the pairing code: pass --code CODE (usage: machine-monitor connect [--code CODE] [--no-agent] URL)")
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
		if key, err = pairLoop(ctx, c, hubURL, *codeFlag, stdout); err != nil {
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
	if *noAgent {
		fmt.Fprintln(stdout, "Next: run `machine-monitor agent` to report this machine, or `machine-monitor service install --role agent` to start it at login.")
		return 0
	}
	fmt.Fprintln(stdout, "Reporting this machine to the hub. Press Ctrl+C to stop.")
	return connectAgent(hubURL, key, stderr)
}

// pairLoop pairs with code, or prompts for one until it works, the hub kills the code, or
// stdin ends. A code given with --code is tried once.
func pairLoop(ctx context.Context, c *http.Client, hubURL, code string, stdout io.Writer) (string, error) {
	in := bufio.NewReader(stdin)
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
	fmt.Fprint(stdout, "Pairing code (on the hub: Add device): ")
	line, err := in.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" && err != nil {
		return "", errors.New("no pairing code entered")
	}
	return line, nil
}
