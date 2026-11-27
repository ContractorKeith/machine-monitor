# 0007. CLI commands, discovery and export

Status: Accepted (2026-11-27)

## Context

These records hold the non-obvious reasons behind the code in this area. They were collected
from the source comments removed under [ADR 0001](0001-no-code-comments.md). Each section is
one decision. When code here changes, update or retire the section that covers it.

## Decisions

### Hub key handling on the command line

**Context:** Keys on argv are visible to every local user via `ps`; a `?key=` in a URL ends up in shell history and unit files; client.json is written in several field spellings.

**Decision:** The key comes from `--key` (which warns), else `MACHINE_MONITOR_KEY`. `connect --key-stdin` keeps it out of argv, as the hub's installers do. A `?key=` URL is accepted by connect only as a dashboard link and refused elsewhere (agent, service, export) so it never reaches a unit file. `loadPairing` and `export.KeyFor` read client.json through `client.LoadConfig`, so both `{"hub","key"}` and `{"hubUrl","token"}` (what connect and upstream write) work for TUI, agent, service and export. A saved key is sent only to the hub it was saved for; `localhost` and the loopback addresses count as the same hub; `HubURL` normalizes scheme and trailing slash so comparison works. `--no-agent` stays as the old name of `--no-tui` because the hub's installers still pass it.

**Consequences:** Do not "simplify" to one spelling or to unconditional key sending. A wrong key sent to a locked hub is reported as wrong, not missing (`lockedWithKey`).

**Where:** `cmd/machine-monitor/pairing.go`, `connect.go`, `export.go`, `internal/export/source.go`, `export_test.go`

### connect: hub proof, terminals and output

**Context:** connect must not hand a key to an impostor, a script must not connect to a guess, and `/dev/null` is a character device but not a terminal.

**Decision:** The hub must prove it holds the key before the key is saved or sent. With no terminal to ask on, `chooseHub` lists what it found and fails. A single `bufio.Reader` is shared by hub choice and code prompt so neither loses the other's input. The TUI is opened only when stdout is a real terminal and stdin is a keyboard (not the piped key). The "make this machine report" hint is printed before the TUI (the alternate screen leaves it in scrollback) and after it, since pairing only makes a viewer and a device appears on the hub only once it reports.

**Consequences:** Terminal detection must reject `/dev/null`; hub choice must stay non-interactive off a TTY.

**Where:** `connect.go`

### Headless hub lifecycle

**Context:** A headless hub runs under a service manager and holds SSE streams open.

**Decision:** SIGHUP is ignored so a closed terminal cannot kill the hub or abandon pending history; settings and pairings are kept in place and config changes need a restart. The HTTP server has no WriteTimeout because SSE streams stay open; the stream handler sets a per-write deadline instead (`hub.StreamWriteTimeout`). The full key prints only to a terminal; under systemd/launchd stdout lands in a log others may read, so it is masked there.

**Consequences:** Adding a server-wide WriteTimeout would break live streams; printing the key to non-terminals leaks it to logs.

**Where:** `hub.go`

### pair: proving the hub and cancelling the code

**Context:** The port pair talks to can be squatted by another user's program that relays the plain hub proof; an abandoned pair must not hold the single live pairing code.

**Decision:** `localHubClient` proves every new connection itself with `GET /api/info?nonce=` and checks `connProof`, an HMAC of the nonce and both ends of that connection, before the key goes over it; a relay's connection has other ends. Proxies are never used. Nothing may follow the proof reply because the connection is handed on. A pre-connProof hub (still running after `update`) only offers the plain proof (`hex(HMAC-SHA256(key,"device-monitor-hub:"+nonce))`, upstream-compatible) and gets no key (`errOldHub`: restart the hub). The stream is opened before the code is requested so the "paired" event cannot be missed, owned by this run so the hub ends the code if the run dies. SIGHUP and a failed stdout write (`pair | head -1`, EPIPE with SIGPIPE caught) end the run and cancel the code; before this the next pair was refused for up to 10 minutes. Only one code is live at a time; `--replace` takes it over and the loser says so. If the hub restarts its in-memory code is gone. Over-long stream lines (the snapshot) are skipped.

**Consequences:** Keep the per-connection proof, the stream-before-request order and the write-failure quit.

**Where:** `pair.go`, `pair_test.go`

### service install/run

**Context:** User-level autostart units can crash-loop or leak keys.

**Decision:** `agent` installs require a pairing (else it crash-loops), refused with exit 1, and `?key=` in --hub is refused with 2. A role not installed before that fails halfway is removed again; an already-installed role is left as it is because its old definition can't be restored. After a failed uninstall, `status` decides the outcome. `service_test` uses `os.TempDir()` paths, not `t.TempDir()`, because the latter follows GOTMPDIR which `os.TempDir()` ignores.

**Where:** `service.go`, `service_test.go`

### update

**Context:** Updates are a supply-chain surface.

**Decision:** Hub installers download the release this binary is (`hub.InstallVersion = version`). GitHub answers 404 for `releases/latest` before the first release; update says so and exits 0. Release notes fetched over the network are cleaned (`textclean`) before printing.

**Where:** `update.go`

### Discovery wire compatibility with upstream

**Context:** Our hubs and upstream device-monitor must find each other.

**Decision:** Probe string, UDP 7778 and reply `{"app":"device-monitor","name","port"}` are upstream's; clients ignore replies without the app value. Our additions (`impl`, `id`) are additive. Names are stripped to `[A-Za-z0-9_ .()'-]` and capped at 64 chars so replies stay under upstream's 512 bytes. Replies are validated (integer port 1-65535, string name) because anyone on the LAN can send one. Only private/loopback IPv4 senders are answered. The port is bound with address reuse so upstream hubs and second instances can share it: SO_REUSEADDR on Linux/Windows, plus SO_REUSEPORT on macOS (BSD needs both). Reuse means a broadcast probe reaches all sockets, a unicast probe only the last bound, so clients probe broadcast addresses. Windows ICMP port-unreachable resets are ignored and the responder keeps serving. A per-hub random id distinguishes one hub on several addresses (LAN, Tailscale) from two machines with one hostname; for upstream hubs the key is name+port. Addresses rank LAN > Tailscale (100.64/10) > loopback > WSL/Hyper-V 172.16/12 (virtual). Wake-on-LAN sends the 102-byte magic packet on ports 9 and 7, validating every MAC first and ignoring per-network send errors, as upstream.

**Consequences:** Changing the probe, reply shape or reuse flags breaks interop.

**Where:** `discovery.go`, `wake.go`

### Discovery reply limiting

**Context:** The responder could be abused to reflect traffic at a spoofed source, and a flood could silence the hub for legitimate probers (skeptic N2, review of #70, PR #84).

**Decision:** Per-source token bucket (replyPerSource, refilling at replySourceRate), per-network bucket (/24 IPv4, /64 IPv6, replyPerNetwork/s), a global replyTotal/s cap, plus a per-source fallback of one reply every five seconds from a bounded per-network reserve usable only by sources silent for replyQuiet. Only answered sources/networks create entries, with hard map caps and idle expiry, so rejected probes can't grow memory. A source hammering gets no claim on the reserve. A client probe reaches a hub a few times at most.

**Consequences:** Regression tests (flood, rotating flood, rotating /24 fallback, fallback reserve) pin these; relaxing any layer reintroduces a reflector or a starvation bug.

**Where:** `discovery.go`, `discovery_test.go`

### Export

**Context:** Scripts consume exports and the hub caps history replies.

**Decision:** A range with no points is an error so an empty file never looks like a quiet machine. CSV columns are fixed and later versions only append; unknown metrics are empty cells or JSON null, never 0. Local reads are read-only so safe beside a running hub (fixture is copied since opening a store may rewrite files). The hub clamps `?points=` to [10,3000] (~1.5 MB), so Hub.Points requests minute-aligned windows of at most 3000 minutes (windows share an edge minute; the hub pads past [from,to]) and joins them; a reply cut at maxBody is reported as too large. Hub transport ignores proxies/redirects and proves the hub before sending the key. User-facing errors are full sentences (`sentenceError`), which ST1005 would otherwise forbid. A device list failure falls back to using the argument as an id; names are a convenience.

**Where:** `internal/export/export.go`, `source.go`, `export_test.go`

### Command wiring and release verification

**Context:** Go init cycles; pairing codes die with the hub; update trust.

**Decision:** `agentBuilt`/`roleCommand` are set in `init()` because the commands table refers to runService (package-level init would cycle). `pair` allows one live code and exits 1 when the hub goes away. Update verification pins the certificate's OIDC issuer and release.yml tag identity.

**Consequences:** Moving to package-level vars fails to compile; loosening identity allows foreign signers.

**Where:** `cmd/machine-monitor/service.go`, `cmd/machine-monitor/pair.go`, `internal/update/verify.go`
