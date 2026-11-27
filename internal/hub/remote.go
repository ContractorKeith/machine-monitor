package hub

import (
	"context"
	"net/http"
	"runtime"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/tailscale"
)

const (
	remoteRefresh  = time.Minute
	remoteCacheTTL = 30 * time.Second
)

func (h *Hub) remoteStatus() tailscale.Status {
	h.rmu.Lock()
	defer h.rmu.Unlock()
	return h.remote
}

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

func (h *Hub) remoteHosts() []string {
	if st := h.remoteStatus(); st.DNSName != nil {
		return []string{*st.DNSName}
	}
	return nil
}

type remoteReply struct {
	tailscale.Status
	Port         int    `json:"port"`
	CanConfigure bool   `json:"canConfigure"`
	Platform     string `json:"platform"`
}

func (h *Hub) handleRemote(w http.ResponseWriter, r *http.Request) {
	st := h.readRemote(r.Context(), r.URL.Query().Has("fresh"))
	platform := runtime.GOOS
	if platform == "windows" {
		platform = "win32"
	}
	sendJSON(w, http.StatusOK, remoteReply{Status: st, Port: h.opt.Port, Platform: platform})
}
