package hub

// Remote access through Tailscale: GET /api/remote (key) reports this machine's Tailscale
// status, as upstream. The hub refreshes it every minute; its MagicDNS name joins the Host
// check, its tailnet URLs go in /api/info and a `tailscale serve` HTTPS address becomes the
// link in alert notifications. Turning on `tailscale serve` (upstream's POST
// /api/remote/https) is not implemented (PLAN feature 30, Later), so canConfigure is false.
//
// Ported from Forward-Future/device-monitor server.js (refreshRemote, /api/remote) and
// tailscale.js (status cache) @ c3a4c41 (MIT, © 2026 Matt):
// https://github.com/Forward-Future/device-monitor

import (
	"context"
	"net/http"
	"runtime"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/tailscale"
)

// Tailscale status timings, as upstream.
const (
	remoteRefresh  = time.Minute      // background refresh
	remoteCacheTTL = 30 * time.Second // GET /api/remote reuses a read this fresh unless ?fresh
)

// remoteStatus is the cached Tailscale status.
func (h *Hub) remoteStatus() tailscale.Status {
	h.rmu.Lock()
	defer h.rmu.Unlock()
	return h.remote
}

// readRemote returns the Tailscale status, reading it again unless the cached one is newer
// than remoteCacheTTL and fresh is false.
func (h *Hub) readRemote(ctx context.Context, fresh bool) tailscale.Status {
	if h.opt.Tailscale == nil {
		return h.remoteStatus()
	}
	h.remoteReads.Lock()
	defer h.remoteReads.Unlock()
	h.rmu.Lock()
	cached, at := h.remote, h.remoteAt
	h.rmu.Unlock()
	if !fresh && !at.IsZero() && time.Since(at) < remoteCacheTTL {
		return cached
	}
	st := h.opt.Tailscale(ctx, h.opt.Port)
	if st.URLs == nil {
		st.URLs = []string{}
	}
	h.rmu.Lock()
	h.remote, h.remoteAt = st, time.Now()
	h.rmu.Unlock()
	return st
}

// remoteLoop refreshes the Tailscale status now and every remoteRefresh until ctx is done.
func (h *Hub) remoteLoop(ctx context.Context) {
	t := time.NewTicker(remoteRefresh)
	defer t.Stop()
	for {
		h.readRemote(ctx, true)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// remoteHosts is the extra Host names the guard accepts: the MagicDNS name, as upstream.
func (h *Hub) remoteHosts() []string {
	if st := h.remoteStatus(); st.DNSName != nil {
		return []string{*st.DNSName}
	}
	return nil
}

// remoteReply is GET /api/remote: the status plus what upstream adds to it.
type remoteReply struct {
	tailscale.Status
	Port         int    `json:"port"`
	CanConfigure bool   `json:"canConfigure"` // never: there is no POST /api/remote/https
	Platform     string `json:"platform"`     // Node's process.platform names, as upstream
}

func (h *Hub) handleRemote(w http.ResponseWriter, r *http.Request) {
	st := h.readRemote(r.Context(), r.URL.Query().Has("fresh"))
	platform := runtime.GOOS
	if platform == "windows" {
		platform = "win32"
	}
	sendJSON(w, http.StatusOK, remoteReply{Status: st, Port: h.opt.Port, Platform: platform})
}
