package hub

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/update"
)

var InstallVersion = "dev"

var InstallSource = update.GitHub()

//go:embed install.sh
var installSh string

//go:embed install.ps1
var installPs1 string

var installSums struct {
	sync.Mutex
	byURL map[string]map[string]string
}

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
	if !h.authorized(r) {
		fail(http.StatusUnauthorized, "the hub key is missing or wrong; copy the install command from the hub again")
		return
	}
	hubURL := h.hubURLFor(r)
	if !installHost(r.Host) {
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

var (
	installNameRE = regexp.MustCompile(`^[A-Za-z0-9.\-]+$`)
	zoneRE        = regexp.MustCompile(`^[A-Za-z0-9._~\-]+$`)
)

func installHost(host string) bool {
	name, port := host, ""
	if i := strings.LastIndex(host, ":"); i >= 0 && !strings.HasSuffix(host, "]") {
		name, port = host[:i], host[i+1:]
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 || port[0] == '0' {
			return false
		}
	}
	if inner, ok := strings.CutPrefix(name, "["); ok {
		inner, ok = strings.CutSuffix(inner, "]")
		if !ok {
			return false
		}
		addr, zone, hasZone := strings.Cut(inner, "%25")
		if hasZone && !zoneRE.MatchString(zone) || strings.Contains(addr, "%") {
			return false
		}
		a, err := netip.ParseAddr(addr)
		return err == nil && a.Is6()
	}
	return installNameRE.MatchString(name)
}

func (h *Hub) hubURLFor(r *http.Request) string {
	proto := "http"
	if r.Header.Get("X-Forwarded-Proto") == "https" && h.trustedProxy(r.RemoteAddr) {
		proto = "https"
	}
	return proto + "://" + r.Host
}

func (h *Hub) trustedProxy(remoteAddr string) bool {
	ip, ok := remoteIP(remoteAddr)
	return ok && h.trustedProxies().contains(ip)
}

type proxyList []netip.Prefix

func (h *Hub) trustedProxies() proxyList {
	h.mu.Lock()
	raw := h.cfg.extra["trustedProxies"]
	h.mu.Unlock()
	var list []string
	if raw == nil || json.Unmarshal(raw, &list) != nil {
		return nil
	}
	var out proxyList
	for _, s := range list {
		if p, err := netip.ParsePrefix(s); err == nil {
			out = append(out, p.Masked())
		} else if a, err := netip.ParseAddr(s); err == nil {
			a = a.Unmap().WithZone("")
			out = append(out, netip.PrefixFrom(a, a.BitLen()))
		}
	}
	return out
}

func (l proxyList) contains(ip netip.Addr) bool {
	ip = ip.Unmap().WithZone("")
	for _, p := range l {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

func fillTemplate(tmpl string, vars map[string]string, quote func(string) string) string {
	var pairs []string
	for k, v := range vars {
		pairs = append(pairs, "__"+k+"__", quote(v))
	}
	return strings.NewReplacer(pairs...).Replace(tmpl)
}

func shQuote(s string) string { return strings.ReplaceAll(s, "'", `'\''`) }

func psQuote(s string) string {
	for _, q := range []string{"'", "\u2018", "\u2019", "\u201a", "\u201b"} {
		s = strings.ReplaceAll(s, q, q+q)
	}
	return s
}
