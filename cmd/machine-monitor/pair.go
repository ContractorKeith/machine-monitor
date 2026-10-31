package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/client"
	"github.com/ContractorKeith/machine-monitor/internal/config"
	"github.com/ContractorKeith/machine-monitor/internal/hub"
)

// Clock and tick for pair's countdown; tests replace them.
var (
	pairNow  = time.Now
	pairTick = time.Second
)

// runPair is `machine-monitor pair [--port 7777]`, run on the hub's machine: it asks the hub
// running here for a one-time pairing code (with the key in this machine's config.json),
// prints it and counts down until a device uses it or it expires. On a terminal the
// countdown updates in place; otherwise it prints a plain line each minute.
func runPair(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pair", flag.ContinueOnError)
	fs.SetOutput(stderr)
	port := fs.Int("port", hub.DefaultPort, "port the hub on this machine listens on")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "machine-monitor pair: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if *port < 1 || *port > 65535 {
		fmt.Fprintf(stderr, "machine-monitor pair: bad port %d\n", *port)
		return 2
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "machine-monitor pair: "+format+"\n", a...)
		return 1
	}
	paths, err := config.Default()
	if err != nil {
		return fail("%v", err)
	}
	key, err := hub.LocalKey(paths)
	if err != nil {
		return fail("%v", err)
	}
	hubURL := fmt.Sprintf("http://127.0.0.1:%d", *port)
	c, err := client.New(hubURL, key)
	if err != nil {
		return fail("%v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// The hub proves it holds the key before the key is sent, so another program squatting
	// on the port can't collect it.
	if err := c.Verify(ctx); err != nil {
		return fail("no hub with this machine's key answers at %s: %v (is machine-monitor hub running here?)", hubURL, err)
	}
	// Follow the stream before asking for the code, so its "paired" event can't be missed.
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	events := c.Stream(sctx)
	if err := waitSnapshot(ctx, events); err != nil {
		return fail("%v", err)
	}
	code, expires, err := requestPairCode(ctx, hubURL, key)
	if err != nil {
		return fail("%v", err)
	}

	fmt.Fprintf(stdout, "Pairing code: %s %s\n", code[:3], code[3:])
	fmt.Fprintf(stdout, "On the new device, run: machine-monitor connect %s\n", hubAddress(*port))
	tty := isTerminal(stdout)
	clearLine := func() {
		if tty {
			fmt.Fprint(stdout, "\r\033[K")
		}
	}
	// render shows the time left and reports whether any is.
	lastMinute := int64(-1)
	render := func() bool {
		left := expires.Sub(pairNow()).Round(time.Second)
		switch {
		case left <= 0:
			clearLine()
			fmt.Fprintln(stdout, "The code expired. Run machine-monitor pair for a new one.")
			return false
		case tty:
			fmt.Fprintf(stdout, "\r\033[KExpires in %s   (Ctrl+C to stop)", clockText(left))
		case int64(left/time.Minute) != lastMinute: // a plain line on each new minute
			lastMinute = int64(left / time.Minute)
			fmt.Fprintf(stdout, "Expires in %s\n", clockText(left))
		}
		return true
	}
	t := time.NewTicker(pairTick)
	defer t.Stop()
	if !render() {
		return 1
	}
	for {
		select {
		case <-ctx.Done():
			if tty {
				fmt.Fprintln(stdout)
			}
			return 1
		case ev, ok := <-events:
			if !ok { // the stream gave up (the key changed); keep counting down
				events = nil
			} else if ev.Type == "paired" {
				clearLine()
				fmt.Fprintln(stdout, "Paired: a device used the code.")
				return 0
			}
		case <-t.C:
			if !render() {
				return 1
			}
		}
	}
}

// waitSnapshot waits until the stream is open (its snapshot has arrived), for up to 15 s.
func waitSnapshot(ctx context.Context, events <-chan client.Event) error {
	timeout := time.NewTimer(15 * time.Second)
	defer timeout.Stop()
	var last error
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout.C:
			if last != nil {
				return fmt.Errorf("the hub's live stream didn't open: %v", last)
			}
			return errors.New("the hub's live stream didn't open")
		case ev, ok := <-events:
			if !ok {
				return fmt.Errorf("the hub's live stream closed: %v", last)
			}
			switch ev.Type {
			case client.EventSnapshot:
				return nil
			case client.EventDisconnected:
				last = ev.Err
			}
		}
	}
}

// requestPairCode asks the hub for a new pairing code, replacing any earlier one.
func requestPairCode(ctx context.Context, hubURL, key string) (code string, expires time.Time, err error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hubURL+"/api/pair/code", nil)
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	hc := &http.Client{Transport: &http.Transport{Proxy: nil}} // never send the key to a proxy
	resp, err := hc.Do(req)
	if err != nil {
		return "", time.Time{}, err
	}
	defer resp.Body.Close()
	var v struct {
		Code    string `json:"code"`
		Expires int64  `json:"expires"` // Unix ms
		Error   string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&v); err != nil {
		return "", time.Time{}, fmt.Errorf("the hub sent an unreadable reply (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}, fmt.Errorf("the hub refused: HTTP %d %s", resp.StatusCode, v.Error)
	}
	if len(v.Code) != 6 || strings.Trim(v.Code, "0123456789") != "" {
		return "", time.Time{}, errors.New("the hub sent an unusable code")
	}
	return v.Code, time.UnixMilli(v.Expires), nil
}

// virtualIface matches adapters other devices can't reach the hub through.
var virtualIface = regexp.MustCompile(`(?i)vEthernet|WSL|VirtualBox|VMware|docker|^br-|^veth|^virbr`)

// hubAddress is the address other devices reach this hub at: its first real LAN address.
func hubAddress(port int) string {
	addrs := hub.LANAddresses()
	for _, a := range addrs {
		if !virtualIface.MatchString(a.Name) {
			return fmt.Sprintf("http://%s:%d", a.Address, port)
		}
	}
	if len(addrs) > 0 {
		return fmt.Sprintf("http://%s:%d", addrs[0].Address, port)
	}
	return fmt.Sprintf("http://<this machine's address>:%d", port)
}

// clockText is a duration as m:ss.
func clockText(d time.Duration) string {
	s := int64(d / time.Second)
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}
