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
	"syscall"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/collect"
	_ "github.com/ContractorKeith/machine-monitor/internal/collect/all" // register probes
	"github.com/ContractorKeith/machine-monitor/internal/config"
	"github.com/ContractorKeith/machine-monitor/internal/hub"
)

// runHub is `machine-monitor hub [--port 7777]`: a headless hub that also monitors this machine.
func runHub(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hub", flag.ContinueOnError)
	fs.SetOutput(stderr)
	port := fs.Int("port", hub.DefaultPort, "TCP port to listen on")
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
	logger := log.New(stderr, "", log.LstdFlags)
	h, err := hub.New(hub.Options{Paths: paths, Port: *port, Collector: collect.New(), Logf: logger.Printf})
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
	fmt.Fprintf(stdout, "  key:           %s\n", h.Token())
	fmt.Fprintf(stdout, "  this machine:  http://127.0.0.1:%d\n", *port)
	for _, a := range hub.LANAddresses() {
		fmt.Fprintf(stdout, "  other devices: http://%s:%d   (%s)\n", a.Address, *port, a.Name)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// No WriteTimeout: SSE streams stay open for as long as a TUI is connected.
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
