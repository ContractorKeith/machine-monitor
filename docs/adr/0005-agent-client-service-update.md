# 0005. Agent, client, services and self-update

Status: Accepted (2026-11-27)

## Context

These records hold the non-obvious reasons behind the code in this area. They were collected
from the source comments removed under [ADR 0001](0001-no-code-comments.md). Each section is
one decision. When code here changes, update or retire the section that covers it.

## Decisions

### Hub key proof before any keyed request

**Context.** Agents and the connected TUI hold a hub key and send it in every request. A stale address, spoofed DHCP lease or a fake hub on the LAN must never receive it.
**Decision.** Before the first keyed request on a connection (and in the agent, before every first report and after every failed one) the client sends a random nonce to the open `GET /api/info?nonce=` and checks the reply is `hex(HMAC-SHA256(key, "device-monitor-hub:"+nonce))`. A failed proof is not remembered, so the next request retries. The stream re-proves on every (re)connection because the address may now be another machine. A pairing code is the one secret sent before proof (the client doesn't know the key yet): it is single-use and short-lived, and the key it buys is proven before it is used or saved. Hubs that hand out their key openly in /api/info (upstream before 1.2.16) are refused.
**Consequences.** The proof shows only that the server holds the key; it is not bound to the connection. On plain HTTP the key travels in cleartext in every report, so confidentiality relies on a trusted LAN/tailnet or HTTPS (e.g. `tailscale serve`). The HMAC string is a wire contract with upstream and our hub.

**Where:** `internal/agent/hubapi.go`, `internal/client/client.go`, `internal/client/stream.go`

### Hub HTTP client and key hygiene

**Context.** The key rides in headers and is easy to leak via URLs, redirects, proxies and error text.
**Decision.** Hub calls use a short timeout, no proxy (a proxy would see the key) and no redirects (a redirect would re-send the key to wherever it points). A hub-supplied key must match keyRE (upstream TOKEN_RE) so it can't carry header-breaking characters. A hub URL containing `?key=` is refused wherever it would be stored or shown (ErrKeyInURL). Client errors never echo the URL because it may carry a password.
**Consequences.** Keep these when "simplifying" the transport; each guards a leak.

**Where:** `internal/agent/hubapi.go`, `internal/agent/client.go`, `internal/client/client.go`

### Signed power actions (agent)

**Context.** The hub can ask an agent to sleep/restart/shut down. A captured command must not be injectable or replayable.
**Decision.** Off unless `--allow-power-actions`. A command runs only if it names a known allowed action, carries a valid HMAC from the device key (hub key before enrollment; enrolled agents verify only with their device key, no shared-token fallback) over device id, command id, action and queue time, was queued no more than CommandTTL ago and no more than maxSkew ahead of the local clock, and has an unseen id. Accepted ids are saved (SeenFile, 0600) before verify returns, so a restart can't re-run a captured command; ids drop after the TTL. maxSeen caps the set (only the key holder can add ids, ids expire, so the cap is never reached in practice; past it commands are refused). A damaged seen file is reported and treated as empty because anyone who can write it already owns the machine. maxSkew tolerates a hub clock slightly ahead.
**Consequences.** Moving the save after the action, or loosening the skew/TTL window, reopens replay.

**Where:** `internal/agent/actions.go`

### Power command execution

**Context.** Commands run as the current user, not root.
**Decision.** systemctl on Linux (logind/polkit permit it for the active local session), pmset and System Events on macOS, shutdown.exe/powrprof on Windows. Each runs from a fixed path (PATH is never searched) with a timeout and a stripped environment: fixed locale and only needed variables. The env slice must never be nil, since os/exec treats nil Env as "inherit everything".

**Where:** `internal/agent/actions.go`

### Agent report loop

**Context.** Many agents may lose the hub at once.
**Decision.** On failure the agent doubles its wait up to MaxBackoff and re-proves the hub before the next report; waits are jittered ×(1+rand·0.2) so agents don't retry in lockstep; logs only on state change. A failing process listing sends no processes that round rather than delaying the report.

**Where:** `internal/agent/agent.go`

### Upstream compatibility of pairing (agent)
**Decision.** Pair reads `retry` only from machine-monitor hubs; for upstream hubs the only signal is the wording of server.js's "Wrong pairing code." (see excuses.md). LoadConfig accepts upstream's field names `hubUrl`/`token`; History/Energy `ms` formats whole-number Unix ms as upstream's dashboard sends; client sends `compact=1` to get our hub's compact stream (upstream hubs route by path and ignore it).

**Where:** `internal/client/config.go`, `internal/client/history.go`, `internal/client/stream.go`

### SSE stream consumption

**Context.** The stream carries large snapshots and busy hubs send a report per device every couple of seconds; a slow consumer must not stall the reader (the hub drops streams that fall behind).
**Decision.** eventQueue sits between reader and consumer and merges events that only replace state (a device's reports into its newest, status events into one), moving the merged event to the newer one's position. Connection events and snapshots are barriers nothing merges across, so a pre-reconnect report can't land after the fresh snapshot. Beyond streamBuffer (two slots per device plus a margin) the client drops the connection and reconnects for a fresh snapshot rather than block. maxSamples caps merged samples so a long-busy consumer loses old sparkline points, not memory. maxLine/maxEvent are sized for an upstream snapshot (≈20 MB for 250 busy devices); our compact snapshot is <1 MB; the initial scanner buffer must stay small since a bigger one raises the cap. A snapshot's Raw is nil to avoid doubling a multi-megabyte payload. idleTimeout counts bytes, not lines, so a big snapshot arriving slowly isn't mistaken for silence; the hub's 3 s status event makes real silence mean a dead network. Reconnect backoff doubles from the hub's `retry:` with up to 25 % jitter. Client-made events (Connected/Disconnected) never wait for the consumer. The stream http.Client has no overall timeout; the idle watchdog covers stalls. maxResponse bounds non-stream reads (full snapshot of MaxDevices with 10 min history stays well under).

**Where:** `internal/client/queue.go`, `internal/client/stream.go`, `internal/client/client.go`

### Pairing codes and install URLs
**Decision.** The client names itself (owner) on its stream and its codes so the hub ends a code once that client's stream is gone (a killed TUI); the hub refuses (409) while no stream is open. InstallURL swaps a loopback hub address for the first real LAN address because loopback only works on the hub's machine.

**Where:** `internal/client/client.go`, `internal/client/settings.go`

### Client non-JSON status errors
**Decision.** statusErr tolerates plain-text bodies because the hub's guard answers in plain text.

**Where:** `internal/client/client.go`

### Regression tests: skeptic findings
Tests encode past bugs: the idle watchdog once reset only per complete line, so a big snapshot over a slow link was cancelled every time (B1, compact_test.go); a 21 MB upstream-style snapshot over a slow link made the hub drop the stream endlessly, hence the compact stream (slowlink_test.go); a throttled reader let megabytes queue in the hub's send buffer and the stream fell minutes behind (N1, slowstream_test.go); pairing codes outlived a killed TUI for the full TTL (#70, settings_test.go); the hub hijacks compact streams so a test listener must track connections itself (client_test.go); a relative MACHINE_MONITOR_DIR must be absolutized at install (L5), a failed update must leave the old unit byte-identical, restore mode/mtime (systemd compares mtime), restore symlinks and previously disabled state (B7/N13) (service_test.go, 439, 459, 524); a replayed genuine Fulcio cert and signature with edited checksums.txt must be refused by the signature check itself (update_test.go).

**Where:** `internal/client/compact_test.go`, `internal/client/slowlink_test.go`, `internal/client/slowstream_test.go`, `internal/client/settings_test.go`, `internal/client/client_test.go`, `internal/service/service_test.go`, `internal/update/update_test.go`

### Service installation (user-level autostart)

**Context.** Nothing may need admin rights; OS differences should be testable everywhere.
**Decision.** A Manager chooses behaviour by its OS field, not build tags, so every OS's files and manager calls are tested on every OS; all manager commands go via the Runner seam with a fixed tool path (PATH never searched), a timeout and a stripped env (systemctl --user needs XDG_RUNTIME_DIR and DBUS_SESSION_BUS_ADDRESS; LC_ALL=C so output parses everywhere). A relative data-dir override is made absolute at install since the service starts elsewhere. A failed update restores the previous service file (bytes, mode, mtime, symlink; written in place to keep owner).

**Where:** `internal/service/service.go`, `internal/service/runner.go`

### launchd (macOS)
**Decision.** Log path uses path.Join (a macOS path even when rendered elsewhere); the log directory is created because launchd won't; a previous job is booted out so the new plist takes effect, and since bootout returns before the job is gone, bootstrap is retried.

**Where:** `internal/service/launchd.go`

### systemd user unit
**Decision.** ExecStart words and Environment= values are quoted for systemd's expansion rules (% specifiers and $ variables in ExecStart; only % in Environment). is-enabled/is-active print their answer and exit non-zero for disabled/inactive, so output is used and exit code ignored; a unit the user left disabled must not end up enabled by a failed install.

**Where:** `internal/service/systemd.go`

### Windows Task Scheduler
**Decision.** The task runs `service run -- <args>`, which detaches from the console and logs to a file; the binary is the task's own process so `/End` stops it (a launcher like `conhost.exe --headless` would end while the binary keeps running). The task runs as the current user with an interactive token and least privilege, so no elevation. The XML is UTF-16LE with BOM (what schtasks /XML expects). A copy started by an earlier install is stopped so /Run uses the new command line. Status words are localized, so only the row's presence is trusted.

**Where:** `internal/service/schtasks.go`

### Release updates and signature verification

**Context.** `machine-monitor update` and the hub's installers must not trust GitHub alone.
**Decision.** A release is trusted only through checksums.txt: its Sigstore bundle must verify against the Fulcio root and this repo's release.yml at exactly the release tag, and each archive must match its SHA-256 line. Verification uses the standard library only (sigstore-go would pull a large tree for one signature). It checks the cert chain (at the cert's own issue time, since Fulcio certs live ten minutes), code-signing usage, OIDC issuer (GitHub Actions; v1 raw/v2 DER extensions) and identity, and that the key signed the exact bytes. The Rekor log entry is not checked; the key is ephemeral. Malformed checksum lines are errors (file is signed, damage means trouble). Tags must match tagRE before reaching a URL or installer script. ArchiveName must match archives.name_template in .goreleaser.yaml.
**Consequences.** Replace is atomic (write beside exe, rename); on Windows the running exe is renamed aside to `.old` first and CleanOld ignores failures since it may still run.

**Where:** `internal/update/update.go`, `internal/update/verify.go`, `internal/update/apply.go`

### Key is never sent before the server proves it holds it

**Context:** Hub keys are bearer secrets. A client (agent, TUI, export, service installer) may be pointed at a wrong or hostile host.

**Decision:** Clients first demand a proof of key knowledge and only then send the key; `ErrBadProof` means the key was not sent. A key saved in client.json is used only for the same hub URL. `?key=` in a URL is refused and never written to unit files or sent.

**Consequences:** Do not simplify to "send key on first request". Streams end on bad proof. Tests TestWrongKey and TestNoKeyBeforeProof pin it.

**Where:** `internal/agent/hubapi.go`, `internal/client/client.go`, `internal/client/client_test.go`, `cmd/machine-monitor/connected.go`, `cmd/machine-monitor/service_test.go`, `cmd/machine-monitor/connect_test.go`

### Signed power commands and replay protection

**Context:** The hub can ask an agent to sleep/restart/shut down; a captured command could be replayed.

**Decision:** Commands carry hex(HMAC-SHA256(key, "machine-monitor-cmd:<device>:<id>:<action>:<t>")). The agent ignores unsigned, wrongly signed, expired and replayed ones, and persists accepted ids in SeenFile (beside client.json) until they expire; "" means memory only. Off by default (AllowPowerActions).

**Consequences:** The signed string format is a protocol contract with hub.SignCommand. Dropping persistence reopens replay after restart.

**Where:** `internal/agent/agent.go`, `internal/agent/actions.go`, `internal/agent/actions_test.go`

### Wire compatibility with upstream hubs and clients

**Context:** Our hub, TUI and agents interoperate with Forward-Future's.

**Decision:** Discovery replies keep upstream's shape and add `impl`/`id` (ignored upstream); hubs are deduplicated by identity across LAN/Tailscale replies and shared ports. Client treats missing endpoints (`/api/remote`, energy settings) as `ErrNotAvailable`; `tailscale serve` is offered only on upstream hubs. The client opts into our compact stream (no history in snapshot) and a merged report carries all samples while a plain upstream report yields one. Processes are sent only while the hub's watch is live (`procs:true`); a viewer re-calls about every report interval.

**Consequences:** Making these fields mandatory breaks mixed-hub setups.

**Where:** `internal/discovery/discovery.go`, `internal/discovery/discovery_test.go`, `internal/client/settings.go`, `internal/client/compact_test.go`, `internal/agent/agent.go`, `internal/client/client.go`

### Client queue and race-test limits

**Context:** A busy consumer must not retain memory or stall the reader; -race distorts timings.

**Decision:** Superseded reports must not leave empty slots (queue_growth_test); link-timing tests are skipped under -race.

**Consequences:** Regressing either brings back unbounded growth or false failures.

**Where:** `internal/client/queue_growth_test.go`, `internal/client/race_on_test.go`

### Windows quirks

**Context:** Windows cannot delete a running exe; schtasks reads XML from a file; WMI localizes PDH counters.

**Decision:** Updates leave exe.old, and CleanOld fails silently since it may still run; createTask writes XML to the data dir only while schtasks runs; the integration test runs a copy of the binary (with a space in the path to exercise quoting); WMI classes are used instead of PDH paths.

**Consequences:** "Fixing" the silent failure or using PDH counters breaks non-English Windows or updates.

**Where:** `internal/update/apply.go`, `internal/service/schtasks.go`, `internal/service/schtasks_windows_test.go`, `internal/collect/oswindows.go`

### Upstream pairing refusals are told apart by their wording

**Context:** An upstream hub refuses a wrong pairing code with a plain message and no machine-readable code.

**Decision:** For upstream hubs, the exact `server.js` message "Wrong pairing code." (pinned at c3a4c41) marks a refusal as retryable. Our hubs send a structured reason.

**Consequences:** If upstream rewords that message, wrong codes against upstream hubs stop being retryable. Keep the string pinned to c3a4c41.

**Where:** `internal/agent/hubapi.go`

### Pre-release versions compare equal to their release

**Context:** Releases are cut only as final tags, and `Latest` skips GitHub pre-releases.

**Decision:** `parseVersion` drops anything after `-` or `+`, so `1.2.3-rc.1` and `1.2.3` compare equal.

**Consequences:** A machine running an rc is not offered the matching final release. If pre-releases are ever published, compare pre-release identifiers per semver.

**Where:** `internal/update/update.go`
