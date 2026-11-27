package hub

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"strings"

	"github.com/ContractorKeith/machine-monitor/internal/alerts"
	"github.com/ContractorKeith/machine-monitor/internal/power"
	"github.com/ContractorKeith/machine-monitor/internal/report"
)

var openPaths = map[string]bool{"/api/info": true, "/api/pair": true, "/install.sh": true, "/install.ps1": true}

func (h *Hub) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/report", h.handleReport)
	mux.HandleFunc("GET /api/info", h.handleInfo)
	mux.HandleFunc("POST /api/pair", h.handlePair)
	mux.HandleFunc("GET /install.sh", h.handleInstall)
	mux.HandleFunc("GET /install.ps1", h.handleInstall)

	key := func(pattern string, f http.HandlerFunc) { mux.Handle(pattern, h.requireKey(f)) }
	key("POST /api/pair/code", h.handlePairCode)
	key("DELETE /api/pair/code", h.handleCancelPairCode)
	key("GET /api/devices", h.handleDevices)
	key("GET /api/stream", h.handleStream)
	key("GET /api/history/{id}", h.handleHistory)
	key("GET /api/energy", h.handleEnergy)
	key("GET /api/energy/settings", h.handleGetEnergySettings)
	key("PUT /api/energy/settings", h.handlePutEnergySettings)
	key("GET /api/alerts", h.handleGetAlerts)
	key("PUT /api/alerts", h.handlePutAlerts)
	key("POST /api/alerts/test", h.handleTestAlerts)
	key("DELETE /api/alerts/log", h.handleClearAlertLog)
	key("PUT /api/order", h.handleOrder)
	key("PATCH /api/devices/{id}", h.handleRename)
	key("DELETE /api/devices/{id}", h.handleRemove)
	key("GET /api/devices/{id}/processes", h.handleProcesses)
	key("POST /api/devices/{id}/action", h.handleAction)
	key("GET /api/remote", h.handleRemote)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		sendJSON(w, http.StatusNotFound, errorBody("not found"))
	})
	return h.guard(mux)
}

func (h *Hub) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range baseHeaders {
			w.Header().Set(k, v)
		}
		h.mu.Lock()
		extra := append(h.remoteHosts(), h.cfg.AllowedHosts...)
		h.mu.Unlock()
		if !hostAllowed(r.Host, h.opt.Hostname, extra) {
			sendText(w, http.StatusMisdirectedRequest, "This hub only answers requests addressed to it by IP address or its own name.")
			return
		}
		if !isLocalAddress(r.RemoteAddr) {
			sendText(w, http.StatusForbidden, "This hub only accepts connections from your own network and Tailscale.")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !sameOrigin(r) {
			sendJSON(w, http.StatusForbidden, errorBody("cross-site request refused"))
			return
		}
		if openPaths[r.URL.Path] && !h.authorized(r) {
			if client, socket := h.clientAddr(r); !h.rate.allow(client, socket, RateMax) {
				sendJSON(w, http.StatusTooManyRequests, errorBody("too many requests"))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Hub) keyOK(k string) bool { return safeEqual(k, h.Token()) }

func (h *Hub) authorized(r *http.Request) bool { return h.keyOK(requestKey(r)) }

func (h *Hub) requireKey(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !h.authorized(r) {
			sendJSON(w, http.StatusUnauthorized, errorBody("This hub is locked. Connect with the hub's key."))
			return
		}
		next(w, r)
	})
}

func errorBody(msg string) map[string]string { return map[string]string{"error": msg} }

func sendJSON(w http.ResponseWriter, status int, body any) {
	b, err := json.Marshal(body)
	if err != nil {
		status, b = http.StatusInternalServerError, []byte(`{"error":"internal error"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(b)
}

func sendText(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	io.WriteString(w, body)
}

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return nil, errors.New("too large")
	}
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return []byte("{}"), nil
	}
	if !json.Valid(b) {
		return nil, errors.New("invalid JSON")
	}
	return b, nil
}

func readObject(w http.ResponseWriter, r *http.Request) (map[string]any, []byte, error) {
	b, err := readBody(w, r)
	if err != nil {
		return nil, nil, err
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil || m == nil {
		m = map[string]any{}
	}
	return m, b, nil
}

func (h *Hub) deviceFromPath(r *http.Request) string {
	id := r.PathValue("id")
	if !report.ValidID(id) {
		return ""
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.registry[id] == nil {
		return ""
	}
	return id
}

const (
	reportFailMax = 60
	reportPeek    = 64 << 10
)

var reportTokenRE = regexp.MustCompile(`"token"\s*:\s*"([^"\\]{1,128})"`)

func (h *Hub) handleReport(w http.ResponseWriter, r *http.Request) {
	client, socket := h.clientAddr(r)
	fail := func() { h.reportFails.allow(client, socket, reportFailMax) }
	if !h.authorized(r) && h.reportFails.over(client, reportFailMax) {
		peek, _ := io.ReadAll(io.LimitReader(r.Body, reportPeek))
		m := reportTokenRE.FindSubmatch(peek)
		if m == nil || !h.keyOK(string(m[1])) {
			sendJSON(w, http.StatusTooManyRequests, errorBody("too many failed reports"))
			return
		}
		r.Body = readCloser{io.MultiReader(bytes.NewReader(peek), r.Body), r.Body}
	}
	raw, err := readBody(w, r)
	if err != nil {
		fail()
		sendJSON(w, http.StatusBadRequest, errorBody(err.Error()))
		return
	}
	var tok struct {
		Token string `json:"token"`
	}
	if json.Unmarshal(raw, &tok) != nil || !h.keyOK(tok.Token) {
		fail()
		sendJSON(w, http.StatusUnauthorized, errorBody("bad token"))
		return
	}
	reply, err := h.ingest(raw)
	if errors.Is(err, errDeviceKey) {
		fail()
		sendJSON(w, http.StatusUnauthorized, errorBody("bad device key"))
		return
	}
	if errors.Is(err, errTooManyDevices) {
		sendJSON(w, http.StatusBadRequest, errorBody(fullMessage))
		return
	}
	if err != nil {
		sendJSON(w, http.StatusBadRequest, errorBody(err.Error()))
		return
	}
	sendJSON(w, http.StatusOK, reply)
}

type readCloser struct {
	io.Reader
	io.Closer
}

func connEnds(r *http.Request) (client, hub netip.AddrPort, ok bool) {
	client, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return client, hub, false
	}
	local, _ := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if local == nil {
		return client, hub, false
	}
	hub, err = netip.ParseAddrPort(local.String())
	return client, hub, err == nil
}

func (h *Hub) handleInfo(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		body := map[string]any{"app": "device-monitor", "impl": "machine-monitor", "hostname": h.opt.Hostname,
			"port": h.opt.Port, "locked": true}
		if n := r.URL.Query().Get("nonce"); nonceRE.MatchString(n) {
			body["proof"] = hubProof(h.Token(), n)
			if client, hub, ok := connEnds(r); ok {
				body["connProof"] = ConnProof(h.Token(), n, client, hub)
			}
		}
		sendJSON(w, http.StatusOK, body)
		return
	}
	sendJSON(w, http.StatusOK, map[string]any{"token": h.Token(), "port": h.opt.Port, "addresses": lanAddresses(),
		"hostname": h.opt.Hostname, "remote": map[string]any{"urls": h.remoteStatus().URLs}})
}

func (h *Hub) handleDevices(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	var v any
	if wantsCompact(r) {
		v = h.compactViews()
	} else {
		v = h.views()
	}
	h.mu.Unlock()
	sendJSON(w, http.StatusOK, v)
}

func (h *Hub) handleHistory(w http.ResponseWriter, r *http.Request) {
	id := h.deviceFromPath(r)
	if id == "" {
		sendJSON(w, http.StatusNotFound, errorBody("not found"))
		return
	}
	from, to, points, err := h.history.Params(r.URL.Query())
	if err != nil {
		sendJSON(w, http.StatusBadRequest, errorBody("bad range"))
		return
	}
	sendJSON(w, http.StatusOK, h.history.Query(id, from, to, points))
}

func (h *Hub) handleOrder(w http.ResponseWriter, r *http.Request) {
	body, _, err := readObject(w, r)
	if err != nil {
		sendJSON(w, http.StatusBadRequest, errorBody(err.Error()))
		return
	}
	ids, _ := body["ids"].([]any)
	h.mu.Lock()
	i := 0
	for _, v := range ids {
		if id, ok := v.(string); ok && report.ValidID(id) && h.registry[id] != nil {
			h.registry[id].Order = i
			i++
		}
	}
	order := make(map[string]int, len(h.registry))
	for id, d := range h.registry {
		order[id] = d.Order
	}
	h.scheduleSave()
	h.mu.Unlock()
	h.broadcast(map[string]any{"type": "order", "order": order})
	sendJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *Hub) handleRename(w http.ResponseWriter, r *http.Request) {
	id := h.deviceFromPath(r)
	if id == "" {
		sendJSON(w, http.StatusNotFound, errorBody("not found"))
		return
	}
	body, _, err := readObject(w, r)
	if err != nil {
		sendJSON(w, http.StatusBadRequest, errorBody(err.Error()))
		return
	}
	h.mu.Lock()
	d := h.registry[id]
	if d == nil {
		h.mu.Unlock()
		sendJSON(w, http.StatusNotFound, errorBody("not found"))
		return
	}
	if s, ok := body["name"].(string); ok {
		if name := report.CleanText(s, 60); name != nil {
			d.Name, d.Renamed = *name, true
		}
	}
	h.scheduleSave()
	v := h.view(id)
	h.mu.Unlock()
	h.broadcast(map[string]any{"type": "device", "device": v})
	sendJSON(w, http.StatusOK, v)
}

func (h *Hub) handleRemove(w http.ResponseWriter, r *http.Request) {
	id := h.deviceFromPath(r)
	if id == "" {
		sendJSON(w, http.StatusNotFound, errorBody("not found"))
		return
	}
	h.mu.Lock()
	delete(h.registry, id)
	delete(h.latest, id)
	delete(h.ingestLocks, id)
	h.scheduleSave()
	h.mu.Unlock()
	if err := h.history.Remove(id); err != nil {
		h.logf("hub: remove history %s: %v", id, err)
	}
	h.alerts.RemoveDevice(id)
	h.forgetActions(id)
	h.broadcast(map[string]any{"type": "removed", "id": id})
	sendJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *Hub) handleEnergy(w http.ResponseWriter, r *http.Request) {
	bounds, ok := parseBounds(r.URL.Query().Get("bounds"))
	if !ok {
		sendJSON(w, http.StatusBadRequest, errorBody("bad bounds"))
		return
	}
	price, list := h.usageAll(bounds, alerts.AllDevices)
	sendJSON(w, http.StatusOK, map[string]any{"bounds": bounds, "settings": price, "devices": list})
}

func (h *Hub) handleGetEnergySettings(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	s := h.cfg.Energy
	h.mu.Unlock()
	sendJSON(w, http.StatusOK, s)
}

func (h *Hub) handlePutEnergySettings(w http.ResponseWriter, r *http.Request) {
	raw, err := readBody(w, r)
	if err != nil {
		sendJSON(w, http.StatusBadRequest, errorBody(err.Error()))
		return
	}
	s, err := power.ParseSettings(raw)
	if err != nil {
		sendJSON(w, http.StatusBadRequest, errorBody(settingsRefusal(err)))
		return
	}
	h.mu.Lock()
	h.cfg.Energy = &s
	err = h.saveConfig()
	h.mu.Unlock()
	if err != nil {
		h.logf("hub: save config: %v", err)
	}
	h.broadcast(map[string]any{"type": "energySettings", "settings": s})
	sendJSON(w, http.StatusOK, s)
}

func (h *Hub) handleGetAlerts(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	priceSet := h.cfg.Energy != nil
	h.mu.Unlock()
	sendJSON(w, http.StatusOK, map[string]any{"settings": h.alerts.Settings(), "active": h.alerts.Active(),
		"events": h.alerts.Events(150), "priceSet": priceSet})
}

func (h *Hub) handlePutAlerts(w http.ResponseWriter, r *http.Request) {
	raw, err := readBody(w, r)
	if err != nil {
		sendJSON(w, http.StatusBadRequest, errorBody(err.Error()))
		return
	}
	s, err := h.alerts.Update(raw)
	if err != nil {
		sendJSON(w, http.StatusBadRequest, errorBody(alertsRefusal(err)))
		return
	}
	sendJSON(w, http.StatusOK, map[string]any{"settings": s, "active": h.alerts.Active()})
}

func (h *Hub) handleTestAlerts(w http.ResponseWriter, r *http.Request) {
	sendJSON(w, http.StatusOK, h.alerts.Test(r.Context()))
}

func (h *Hub) handleClearAlertLog(w http.ResponseWriter, r *http.Request) {
	h.alerts.ClearLog()
	sendJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type Address struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

func lanAddresses() []Address {
	out := []Address{}
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipn.IP.To4()
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			out = append(out, Address{Name: ifc.Name, Address: ip.String()})
		}
	}
	return out
}

func LANAddresses() []Address { return lanAddresses() }

var virtualIfaceRE = regexp.MustCompile(`(?i)vEthernet|WSL|VirtualBox|VMware|Tailscale`)

func (h *Hub) preferredURL() string {
	if st := h.remoteStatus(); st.HTTPSURL != nil {
		return *st.HTTPSURL
	}
	for _, a := range lanAddresses() {
		if !virtualIfaceRE.MatchString(a.Name) && !strings.HasPrefix(a.Address, "100.") {
			return fmt.Sprintf("http://%s:%d", a.Address, h.opt.Port)
		}
	}
	return fmt.Sprintf("http://%s:%d", h.opt.Hostname, h.opt.Port)
}
