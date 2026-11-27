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
	_ "github.com/ContractorKeith/machine-monitor/internal/collect/all"
	"github.com/ContractorKeith/machine-monitor/internal/config"
	"github.com/ContractorKeith/machine-monitor/internal/discovery"
	"github.com/ContractorKeith/machine-monitor/internal/hub"
	"github.com/ContractorKeith/machine-monitor/internal/tailscale"
	"github.com/ContractorKeith/machine-monitor/internal/tui"
)

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

	hangups := make(chan os.Signal, 1)
	signal.Notify(hangups, syscall.SIGHUP)
	defer signal.Stop(hangups)
	logger := log.New(stderr, "", log.LstdFlags)
	h, err := hub.New(hub.Options{Paths: paths, Port: *port, Collector: collect.New(), Agent: "machine-monitor/" + version,
		Procs: tui.SystemProcesses().Processes, Logf: logger.Printf,
		Discovery: fmt.Sprintf(":%d", discovery.Port), Tailscale: tailscale.Read})
	if err != nil {
		fmt.Fprintf(stderr, "machine-monitor hub: %v\n", err)
		return 1
	}
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
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
	fmt.Fprintf(stdout, "  add a device:  run %s here, then machine-monitor connect on the device\n", pairCommand(*port))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
	h.Run(ctx)

	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(sctx)
	if err := h.Close(); err != nil {
		logger.Printf("hub: close: %v", err)
		return 1
	}
	return 0
}

func printKey(w io.Writer, key string) {
	if isTerminal(w) {
		fmt.Fprintf(w, "  key:           %s\n", key)
	} else {
		fmt.Fprintf(w, "  key:           %s   (show it with: machine-monitor hub --show-key)\n", maskKey(key))
	}
}

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func maskKey(key string) string {
	if len(key) <= 4 {
		return strings.Repeat("•", len(key))
	}
	return key[:4] + strings.Repeat("•", len(key)-4)
}

func pairCommand(port int) string {
	if port == hub.DefaultPort {
		return "machine-monitor pair"
	}
	return fmt.Sprintf("machine-monitor pair --port %d", port)
}
