package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/config"
	"github.com/ContractorKeith/machine-monitor/internal/hub"
)

// Clock and tick for pair's countdown; tests replace them.
var (
	pairNow  = time.Now
	pairTick = time.Second
)

// runPair is `machine-monitor pair [--port 7777] [--replace]`, run on the hub's machine: it
// asks the hub running here for a one-time pairing code (with the key in this machine's
// config.json), prints it and counts down until a device uses it or it expires. On a
// terminal the countdown updates in place; otherwise it prints a plain line each minute. It
// exits 1 when the hub goes away, since the code goes with it. Only one code is live at a
// time: while another is, pair says so, unless --replace.
func runPair(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pair", flag.ContinueOnError)
	fs.SetOutput(stderr)
	port := fs.Int("port", hub.DefaultPort, "port the hub on this machine listens on")
	replace := fs.Bool("replace", false, "end a pairing code that is already active and show a new one")
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
	addr := fmt.Sprintf("127.0.0.1:%d", *port)
	hubURL := "http://" + addr
	hc := localHubClient(key)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Follow the stream before asking for the code, so its "paired" event can't be missed.
	// Every connection proves it reaches the hub itself before the key goes over it.
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	events, err := pairStream(sctx, hc, hubURL, key)
	if err != nil {
		hint := "is machine-monitor hub running here?"
		if *port == hub.DefaultPort {
			hint += " If it runs on another port, pass --port."
		}
		return fail("no hub with this machine's key answers at %s: %v (%s)", hubURL, err, hint)
	}
	code, expires, id, err := requestPairCode(ctx, hc, hubURL, key, *replace)
	var active *codeActiveError
	if errors.As(err, &active) {
		fmt.Fprintf(stderr, "machine-monitor pair: a pairing code is already active (from another machine-monitor pair, or the TUI's Add device); it expires in %s.\n", clockText(active.expires.Sub(pairNow()).Round(time.Second)))
		fmt.Fprintln(stderr, "Use that code, or run machine-monitor pair --replace to end it and show a new one.")
		return 1
	}
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
			// End this run's code, so the next pair needn't wait it out.
			cctx, ccancel := context.WithTimeout(context.Background(), 3*time.Second)
			cancelPairCode(cctx, hc, hubURL, key, id)
			ccancel()
			return 1
		case ev, ok := <-events:
			switch {
			case !ok:
				// The hub stopped or restarted, and an active code lives only in its memory.
				clearLine()
				fmt.Fprintln(stdout, "Lost the hub, and the code with it. Run machine-monitor pair again once the hub is back.")
				return 1
			case ev.ID != id:
				// another code's event
			case ev.Type == "paired":
				clearLine()
				fmt.Fprintln(stdout, "Paired: a device used the code.")
				return 0
			case ev.Type == "pairEnded":
				clearLine()
				fmt.Fprintln(stdout, "This code no longer works: a new one was asked for (pair --replace, or the TUI's Add device).")
				return 1
			}
		case <-t.C:
			if !render() {
				return 1
			}
		}
	}
}

// errNotHub is a program on the port that can't prove, for this very connection, that it is
// the hub holding this machine's key.
var errNotHub = errors.New("the program on this port can't prove it is the hub with this machine's key, so the key was not sent")

// localHubClient talks to the hub on this machine. Each new connection first proves it
// reaches the hub itself (proveHub), and only then carries the key. A program squatting on
// the port (say another user's, when the hub runs on another port) can relay the plain hub
// proof but not this one, so it never sees the key.
func localHubClient(key string) *http.Client {
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		if err := proveHub(conn, addr, key); err != nil {
			conn.Close()
			return nil, err
		}
		return conn, nil
	}
	return &http.Client{Transport: &http.Transport{
		Proxy:                 nil, // never send the key to a proxy
		DialContext:           dial,
		ResponseHeaderTimeout: 15 * time.Second,
		MaxIdleConnsPerHost:   2,
	}}
}

// proveHub sends GET /api/info?nonce= on conn itself and checks the hub's connProof: an HMAC
// of the nonce and conn's own local address under the key (hub.ConnProof). A relay's
// connection to the real hub has another address, so its answer can't match.
func proveHub(conn net.Conn, addr, key string) error {
	local, ok := conn.LocalAddr().(*net.TCPAddr)
	if !ok {
		return errNotHub
	}
	n := make([]byte, 16)
	if _, err := rand.Read(n); err != nil {
		return err
	}
	nonce := hex.EncodeToString(n)
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	defer conn.SetDeadline(time.Time{})
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/api/info?nonce="+nonce, nil)
	if err != nil {
		return err
	}
	if err := req.Write(conn); err != nil {
		return err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		return err
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d from /api/info", resp.StatusCode)
	}
	var info struct {
		ConnProof string `json:"connProof"`
	}
	want := hub.ConnProof(key, nonce, local.AddrPort())
	// Nothing may follow the reply: the connection is handed on for the next request.
	if json.Unmarshal(b, &info) != nil || resp.Close || br.Buffered() > 0 ||
		!hmac.Equal([]byte(info.ConnProof), []byte(want)) {
		return errNotHub
	}
	return nil
}

// pairEvent is the part of a stream event pair follows.
type pairEvent struct {
	Type string `json:"type"`
	ID   int64  `json:"id"`
}

// pairStream opens the hub's live stream and returns its "paired" and "pairEnded" events.
// The channel closes when the stream ends. The hub registers a stream before it sends the
// reply headers, so once this returns no event can be missed.
func pairStream(ctx context.Context, hc *http.Client, hubURL, key string) (<-chan pairEvent, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, hubURL+"/api/stream", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := hc.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // the URL is already in the caller's message
		}
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("the hub's live stream didn't open: HTTP %d", resp.StatusCode)
	}
	out := make(chan pairEvent, 16)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		// Lines too long for the buffer (the snapshot) are skipped: pair's events are small.
		br := bufio.NewReaderSize(resp.Body, 64<<10)
		long := false
		for {
			line, err := br.ReadSlice('\n')
			if errors.Is(err, bufio.ErrBufferFull) {
				long = true
				continue
			}
			if err != nil {
				return
			}
			if long {
				long = false
				continue
			}
			var ev pairEvent
			data, ok := bytes.CutPrefix(line, []byte("data: "))
			if !ok || json.Unmarshal(data, &ev) != nil || (ev.Type != "paired" && ev.Type != "pairEnded") {
				continue
			}
			select {
			case out <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// codeActiveError is the hub refusing a new code because another is live.
type codeActiveError struct{ expires time.Time }

func (e *codeActiveError) Error() string { return "a pairing code is already active" }

// requestPairCode asks the hub for a new pairing code. Unless replace, the hub refuses while
// another code is live (codeActiveError).
func requestPairCode(ctx context.Context, hc *http.Client, hubURL, key string, replace bool) (code string, expires time.Time, id int64, err error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	body := fmt.Sprintf(`{"replace":%t}`, replace)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hubURL+"/api/pair/code", strings.NewReader(body))
	if err != nil {
		return "", time.Time{}, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return "", time.Time{}, 0, err
	}
	defer resp.Body.Close()
	var v struct {
		Code    string `json:"code"`
		Expires int64  `json:"expires"` // Unix ms
		ID      int64  `json:"id"`
		Active  bool   `json:"active"`
		Error   string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&v); err != nil {
		return "", time.Time{}, 0, fmt.Errorf("the hub sent an unreadable reply (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode == http.StatusConflict && v.Active {
		return "", time.Time{}, 0, &codeActiveError{expires: time.UnixMilli(v.Expires)}
	}
	if resp.StatusCode != http.StatusOK {
		if v.Error != "" {
			return "", time.Time{}, 0, errors.New(v.Error)
		}
		return "", time.Time{}, 0, fmt.Errorf("the hub refused: HTTP %d", resp.StatusCode)
	}
	if len(v.Code) != 6 || strings.Trim(v.Code, "0123456789") != "" {
		return "", time.Time{}, 0, errors.New("the hub sent an unusable code")
	}
	return v.Code, time.UnixMilli(v.Expires), v.ID, nil
}

// cancelPairCode ends code id on the hub, if it is still the live one. Best effort.
func cancelPairCode(ctx context.Context, hc *http.Client, hubURL, key string, id int64) {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, fmt.Sprintf("%s/api/pair/code?id=%d", hubURL, id), nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+key)
	if resp, err := hc.Do(req); err == nil {
		resp.Body.Close()
	}
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
