// Package tailscale reads this machine's Tailscale state (`tailscale status --json`) so the
// hub can show its tailnet addresses and clients away from home can find a hub. It only
// reads: turning on `tailscale serve` is not implemented (PLAN feature 30, Later).
//
// Ported from Forward-Future/device-monitor tailscale.js (cli, run, readStatus, peerUrls)
// at c3a4c41.
// MIT License, Copyright (c) 2026 Matt: https://github.com/Forward-Future/device-monitor
package tailscale

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	runTimeout = 8 * time.Second // per command, as upstream
	maxOutput  = 4 << 20
	maxPeers   = 40 // PeerURLs cap, as upstream
)

// Status is this machine's Tailscale state, in upstream's JSON shape. Fields other than
// Installed and Running are omitted when unknown.
type Status struct {
	Installed bool     `json:"installed"`
	Running   bool     `json:"running"`
	State     *string  `json:"state,omitempty"`    // BackendState: Running, NeedsLogin, Stopped, NotRunning...
	IP        *string  `json:"ip,omitempty"`       // first tailnet IPv4
	DNSName   *string  `json:"dnsName,omitempty"`  // MagicDNS name without the trailing dot
	HTTPSURL  *string  `json:"httpsUrl,omitempty"` // https://<name> when `tailscale serve` already proxies to the hub
	URLs      []string `json:"urls,omitempty"`     // where the hub is reachable on the tailnet, best first
	Tailnet   *string  `json:"tailnet,omitempty"`
}

// rawStatus is the part of `tailscale status --json` we read.
type rawStatus struct {
	BackendState   string
	Self           *rawNode
	Peer           map[string]rawNode
	CurrentTailnet *struct{ Name string }
}

type rawNode struct {
	DNSName      string
	TailscaleIPs []string
	Online       bool
}

// rawServe is the part of `tailscale serve status --json` we read.
type rawServe struct {
	Web map[string]struct {
		Handlers map[string]struct{ Proxy string }
	}
}

// Installed reports whether a tailscale CLI is present.
func Installed() bool { _, _, ok := cli(); return ok }

// Read returns the current status for a hub listening on hubPort. A missing CLI gives
// {installed:false}; a CLI whose daemon isn't answering gives state "NotRunning".
func Read(ctx context.Context, hubPort int) Status {
	if !Installed() {
		return Status{}
	}
	out := run(ctx, "status", "--json")
	var serve []byte
	var s rawStatus
	if json.Unmarshal(out, &s) == nil && s.BackendState == "Running" {
		serve = run(ctx, "serve", "status", "--json")
	}
	return parseStatus(out, serve, hubPort)
}

// PeerURLs lists addresses where a hub might be on this machine's online peers: https://<dns>
// and http://<ipv4>:<hubPort> for each, at most 40.
func PeerURLs(ctx context.Context, hubPort int) []string {
	if !Installed() {
		return nil
	}
	return parsePeers(run(ctx, "status", "--json"), hubPort)
}

// parseStatus builds Status from `status --json` output and, when running, `serve status --json`.
func parseStatus(statusJSON, serveJSON []byte, hubPort int) Status {
	var j rawStatus
	if json.Unmarshal(statusJSON, &j) != nil {
		return Status{Installed: true, State: ptr("NotRunning")}
	}
	st := Status{Installed: true, Running: j.BackendState == "Running"}
	if j.BackendState != "" {
		st.State = ptr(j.BackendState)
	}
	if j.Self != nil {
		if ip := firstIPv4(j.Self.TailscaleIPs); ip != "" {
			st.IP = ptr(ip)
		}
		if dns := strings.TrimSuffix(j.Self.DNSName, "."); dns != "" {
			st.DNSName = ptr(dns)
		}
	}
	if j.CurrentTailnet != nil && j.CurrentTailnet.Name != "" {
		st.Tailnet = ptr(j.CurrentTailnet.Name)
	}
	if !st.Running {
		return st
	}
	st.HTTPSURL = serveURL(serveJSON, hubPort)
	if st.HTTPSURL != nil {
		st.URLs = append(st.URLs, *st.HTTPSURL)
	}
	port := strconv.Itoa(hubPort)
	if st.IP != nil {
		st.URLs = append(st.URLs, "http://"+*st.IP+":"+port)
	}
	if st.DNSName != nil {
		st.URLs = append(st.URLs, "http://"+*st.DNSName+":"+port)
	}
	return st
}

// serveURL finds a `tailscale serve` site whose handler proxies to the hub on localhost.
func serveURL(serveJSON []byte, hubPort int) *string {
	var s rawServe
	if json.Unmarshal(serveJSON, &s) != nil {
		return nil
	}
	toHub := regexp.MustCompile(`^(https?://)?(127\.0\.0\.1|localhost):` + strconv.Itoa(hubPort) + `/?$`)
	hosts := make([]string, 0, len(s.Web))
	for h := range s.Web {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	var url *string
	for _, h := range hosts {
		for _, hd := range s.Web[h].Handlers {
			if toHub.MatchString(hd.Proxy) {
				url = ptr("https://" + strings.TrimSuffix(h, ":443"))
				break
			}
		}
	}
	return url
}

// parsePeers lists hub candidate URLs on online peers, in peer-key order for stable output.
func parsePeers(statusJSON []byte, hubPort int) []string {
	var j rawStatus
	if json.Unmarshal(statusJSON, &j) != nil || j.BackendState != "Running" {
		return nil
	}
	keys := make([]string, 0, len(j.Peer))
	for k := range j.Peer {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []string
	for _, k := range keys {
		p := j.Peer[k]
		if !p.Online {
			continue
		}
		if dns := strings.TrimSuffix(p.DNSName, "."); dns != "" {
			out = append(out, "https://"+dns)
		}
		if ip := firstIPv4(p.TailscaleIPs); ip != "" {
			out = append(out, "http://"+ip+":"+strconv.Itoa(hubPort))
		}
	}
	if len(out) > maxPeers {
		out = out[:maxPeers]
	}
	return out
}

func firstIPv4(ips []string) string {
	for _, s := range ips {
		if a, err := netip.ParseAddr(s); err == nil && a.Is4() {
			return a.String()
		}
	}
	return ""
}

// candidates lists [path, extra args] for each place the CLI may live. Besides the normal
// installs, a Windows PC can run Tailscale in user mode (no admin rights) from
// %LOCALAPPDATA%\dm-tailscale, listening on its own pipe. A var so tests can replace it.
var candidates = func() [][]string {
	switch runtime.GOOS {
	case "windows":
		pf := os.Getenv("ProgramFiles")
		if pf == "" {
			pf = `C:\Program Files`
		}
		return [][]string{
			{filepath.Join(pf, "Tailscale", "tailscale.exe")},
			{filepath.Join(os.Getenv("LOCALAPPDATA"), "dm-tailscale", "bin", "tailscale.exe"), `--socket=\\.\pipe\dm-tailscaled`},
		}
	case "darwin":
		return [][]string{{"/Applications/Tailscale.app/Contents/MacOS/Tailscale"}, {"/opt/homebrew/bin/tailscale"}, {"/usr/local/bin/tailscale"}}
	default:
		return [][]string{{"/usr/bin/tailscale"}, {"/usr/sbin/tailscale"}, {"/usr/local/bin/tailscale"}}
	}
}

// cli returns the first candidate that is a regular file. Fixed paths only, never PATH.
func cli() (exe string, pre []string, ok bool) {
	for _, c := range candidates() {
		if fi, err := os.Stat(c[0]); err == nil && fi.Mode().IsRegular() {
			return c[0], c[1:], true
		}
	}
	return "", nil, false
}

// run executes the CLI with a timeout and a stripped environment and returns stdout, even
// when it exits non-zero (the caller decides from the JSON), or nil if it can't start.
func run(ctx context.Context, args ...string) []byte {
	exe, pre, ok := cli()
	if !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, append(pre, args...)...)
	cmd.Env = strippedEnv()
	cmd.WaitDelay = 500 * time.Millisecond
	var out bytes.Buffer
	cmd.Stdout = &limitWriter{w: &out, n: maxOutput}
	cmd.Run()
	return out.Bytes()
}

// strippedEnv keeps only what the CLI needs to find its socket and config.
func strippedEnv() []string {
	var env []string
	for _, k := range []string{"PATH", "HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "ProgramData", "SystemRoot", "TEMP", "TMP", "TMPDIR", "XDG_RUNTIME_DIR"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// limitWriter drops output past n bytes.
type limitWriter struct {
	w io.Writer
	n int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if l.n > 0 {
		k := min(len(p), l.n)
		l.n -= k
		l.w.Write(p[:k])
	}
	return len(p), nil
}

func ptr(s string) *string { return &s }
