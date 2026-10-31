package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/collect"
	_ "github.com/ContractorKeith/machine-monitor/internal/collect/all" // register probes
	"github.com/ContractorKeith/machine-monitor/internal/config"
	"github.com/ContractorKeith/machine-monitor/internal/discovery"
	"github.com/ContractorKeith/machine-monitor/internal/hub"
	"github.com/ContractorKeith/machine-monitor/internal/tailscale"
	"github.com/ContractorKeith/machine-monitor/internal/tui"
)

// runHub is `machine-monitor hub [--port 7777] [--show-key]`: a headless hub that also
// monitors this machine. --show-key prints this machine's hub key and exits.
func runHub(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hub", flag.ContinueOnError)
	fs.SetOutput(stderr)
	port := fs.Int("port", hub.DefaultPort, "TCP port to listen on")
	showKey := fs.Bool("show-key", false, "print this machine's hub key and exit")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "machine-monitor hub: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if *port < 1 || *port > 65535 {
		fmt.Fprintf(stderr, "machine-monitor hub: bad port %d\n", *port)
		return 2
	}
	paths, err := config.Default()
	if err != nil {
		fmt.Fprintf(stderr, "machine-monitor hub: %v\n", err)
		return 1
	}
	if *showKey {
		key, err := hub.LocalKey(paths)
		if err != nil {
			fmt.Fprintf(stderr, "machine-monitor hub: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, key)
		return 0
	}
	logger := log.New(stderr, "", log.LstdFlags)
	h, err := hub.New(hub.Options{Paths: paths, Port: *port, Collector: collect.New(), Agent: "machine-monitor/" + version,
		Procs: tui.SystemProcesses().Processes, Logf: logger.Printf,
		Discovery: fmt.Sprintf(":%d", discovery.Port), Tailscale: tailscale.Read})
	if err != nil {
		fmt.Fprintf(stderr, "machine-monitor hub: %v\n", err)
		return 1
	}
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", *port)) // dual-stack: IPv4 and IPv6
	if err != nil {
		h.Close()
		fmt.Fprintf(stderr, "machine-monitor hub: %v (is a hub already running on port %d?)\n", err, *port)
		return 1
	}

	fmt.Fprintf(stdout, "machine-monitor hub running on port %d (data in %s)\n", *port, paths.Dir)
	printKey(stdout, h.Token())
	fmt.Fprintf(stdout, "  this machine:  http://127.0.0.1:%d\n", *port)
	for _, a := range hub.LANAddresses() {
		fmt.Fprintf(stdout, "  other devices: http://%s:%d   (%s)\n", a.Address, *port, a.Name)
	}
	fmt.Fprintf(stdout, "  add a device:  run machine-monitor pair here, then machine-monitor connect on the device\n")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// No WriteTimeout: SSE streams stay open while a TUI is connected. The stream handler sets
	// a deadline on each write instead (hub.StreamWriteTimeout).
	srv := &http.Server{Handler: h.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
		IdleTimeout: 2 * time.Minute, ErrorLog: logger}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	go func() {
		if err := <-served; !errors.Is(err, http.ErrServerClosed) {
			logger.Printf("hub: serve: %v", err)
			stop()
		}
	}()
	h.Run(ctx) // returns once SSE streams have ended, so Shutdown doesn't wait on them

	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(sctx)
	if err := h.Close(); err != nil { // after Shutdown: no report can arrive later
		logger.Printf("hub: close: %v", err)
		return 1
	}
	return 0
}

// printKey prints the hub key line. The full key only goes to a person's terminal: under a
// service manager stdout lands in a log (the systemd journal, the launchd log) that others
// may read, so there it is masked.
func printKey(w io.Writer, key string) {
	if isTerminal(w) {
		fmt.Fprintf(w, "  key:           %s\n", key)
	} else {
		fmt.Fprintf(w, "  key:           %s   (show it with: machine-monitor hub --show-key)\n", maskKey(key))
	}
}

// isTerminal reports whether w is a character device: a terminal, not a pipe, file or log
// (the systemd journal, a launchd log). /dev/null counts too, which is harmless.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// maskKey shows only the first four characters of a key.
func maskKey(key string) string {
	if len(key) <= 4 {
		return strings.Repeat("•", len(key))
	}
	return key[:4] + strings.Repeat("•", len(key)-4)
}
