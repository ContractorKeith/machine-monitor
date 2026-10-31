package hub

// Agent installers (PLAN §3 row 33): GET /install.sh and GET /install.ps1, key required and
// rate-limited, as upstream. Every refusal is itself a script that prints why and exits 1. Each returns a script that downloads the release this hub runs from GitHub,
// checks it against a SHA-256 the hub pins into the script, installs it under the user's
// home, pairs (saves the key in client.json) and installs the agent service.
//
// The hub pins hashes it has verified itself: it fetches the release's checksums.txt once
// and checks its Sigstore signature (internal/update) before serving any installer.
//
// Ported from Forward-Future/device-monitor server.js (template, hubUrlFor) and
// agent/install.sh @ c3a4c41 (MIT, © 2026 Matt): https://github.com/Forward-Future/device-monitor

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/update"
)

// InstallVersion is the release the installers download: the version this binary was built
// as. main sets it from its -ldflags version; a "dev" build can't serve installers.
var InstallVersion = "dev"

// InstallSource is where installers and the hub's checksum lookup get releases. Tests point
// it at a fake release server.
var InstallSource = update.GitHub()

//go:embed install.sh
var installSh string

//go:embed install.ps1
var installPs1 string

// installSums caches verified checksums.txt per release URL; failures aren't cached.
var installSums struct {
	sync.Mutex
	byURL map[string]map[string]string
}

// handleInstall serves install.sh or install.ps1 for the request's path.
func (h *Hub) handleInstall(w http.ResponseWriter, r *http.Request) {
	ps1 := strings.HasSuffix(r.URL.Path, ".ps1")
	fail := func(status int, msg string) {
		h.logf("hub: %s: %s", r.URL.Path, msg)
		if ps1 {
			sendText(w, status, "throw '"+psQuote("machine-monitor install: "+msg)+"'\n")
		} else {
			sendText(w, status, "echo '"+shQuote("machine-monitor install: "+msg)+"' >&2; exit 1\n")
		}
	}
	// Key required, as upstream; refused with a script so `| sh` and `| iex` fail loudly.
	if !h.authorized(r) {
		fail(http.StatusUnauthorized, "the hub key is missing or wrong; copy the install command from the hub again")
		return
	}
	hubURL := h.hubURLFor(r)
	if !installHostRE.MatchString(r.Host) {
		fail(http.StatusBadRequest, "unusable Host header")
		return
	}
	tag := "v" + InstallVersion
	if !update.ValidTag(tag) {
		fail(http.StatusServiceUnavailable, "this hub is a development build ("+InstallVersion+"), so it has no release to install; install machine-monitor by hand")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	sums, err := verifiedSums(ctx, tag)
	if err != nil {
		fail(http.StatusBadGateway, fmt.Sprintf("the hub couldn't get verified checksums for %s: %v", tag, err))
		return
	}

	goos, quote, tmpl := "linux darwin", shQuote, installSh
	if ps1 {
		goos, quote, tmpl = "windows", psQuote, installPs1
	}
	// Only the archives this script can install, in a stable order.
	var lines []string
	for name, sum := range sums {
		for _, o := range strings.Fields(goos) {
			if strings.HasPrefix(name, "machine-monitor_"+InstallVersion+"_"+o+"_") {
				lines = append(lines, sum+"  "+name)
			}
		}
	}
	sort.Strings(lines)
	vars := map[string]string{
		"HUB":     hubURL,
		"KEY":     h.Token(),
		"VERSION": InstallVersion,
		"BASE":    InstallSource.DownloadBase(tag),
		"SUMS":    strings.Join(lines, "\n"),
	}
	sendText(w, http.StatusOK, fillTemplate(tmpl, vars, quote))
}

// verifiedSums is the signature-checked checksums.txt of tag, cached after the first success.
func verifiedSums(ctx context.Context, tag string) (map[string]string, error) {
	src := InstallSource
	key := src.DownloadBase(tag)
	installSums.Lock()
	defer installSums.Unlock()
	if s, ok := installSums.byURL[key]; ok {
		return s, nil
	}
	s, err := src.Checksums(ctx, tag)
	if err != nil {
		return nil, err
	}
	if installSums.byURL == nil {
		installSums.byURL = map[string]map[string]string{}
	}
	installSums.byURL[key] = s
	return s, nil
}

// installHostRE is what a Host may hold to be pasted into a script: a name or IP and port.
var installHostRE = regexp.MustCompile(`^[A-Za-z0-9.\-]+(:[0-9]+)?$|^\[[0-9A-Fa-f:.%]+\](:[0-9]+)?$`)

// hubURLFor is the hub origin as the client addressed it (the guard already checked Host).
// Behind `tailscale serve` or another HTTPS proxy the request arrives with
// X-Forwarded-Proto: https; that header is honoured only from a proxy listed in
// config.json "trustedProxies", since anyone else could send it.
func (h *Hub) hubURLFor(r *http.Request) string {
	proto := "http"
	if r.Header.Get("X-Forwarded-Proto") == "https" && h.trustedProxy(r.RemoteAddr) {
		proto = "https"
	}
	return proto + "://" + r.Host
}

// trustedProxy reports whether remoteAddr matches an address or CIDR prefix in config.json
// "trustedProxies" (e.g. ["127.0.0.1", "::1"] for tailscale serve). None by default.
func (h *Hub) trustedProxy(remoteAddr string) bool {
	h.mu.Lock()
	raw := h.cfg.extra["trustedProxies"]
	h.mu.Unlock()
	var list []string
	if raw == nil || json.Unmarshal(raw, &list) != nil {
		return false
	}
	ip, ok := remoteIP(remoteAddr)
	if !ok {
		return false
	}
	for _, s := range list {
		if p, err := netip.ParsePrefix(s); err == nil && p.Contains(ip) {
			return true
		}
		if a, err := netip.ParseAddr(s); err == nil && a.Unmap() == ip {
			return true
		}
	}
	return false
}

// fillTemplate replaces each __NAME__ in tmpl with its value, quoted for the single-quoted
// string it lands in, so no value can end the string early. It is one pass, so a value is
// never itself searched for placeholders.
func fillTemplate(tmpl string, vars map[string]string, quote func(string) string) string {
	var pairs []string
	for k, v := range vars {
		pairs = append(pairs, "__"+k+"__", quote(v))
	}
	return strings.NewReplacer(pairs...).Replace(tmpl)
}

// shQuote escapes s for a POSIX single-quoted string.
func shQuote(s string) string { return strings.ReplaceAll(s, "'", `'\''`) }

// psQuote escapes s for a PowerShell single-quoted string. PowerShell also ends such a
// string at the typographic single quotes, so those are doubled too.
func psQuote(s string) string {
	for _, q := range []string{"'", "\u2018", "\u2019", "\u201a", "\u201b"} {
		s = strings.ReplaceAll(s, q, q+q)
	}
	return s
}
