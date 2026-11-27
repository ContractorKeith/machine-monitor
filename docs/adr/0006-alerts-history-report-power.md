# 0006. Alerts, history, report validation and power

Status: Accepted (2026-11-27)

## Context

These records hold the non-obvious reasons behind the code in this area. They were collected
from the source comments removed under [ADR 0001](0001-no-code-comments.md). Each section is
one decision. When code here changes, update or retire the section that covers it.

## Decisions

### SSRF guard

**Context.** Alert delivery posts to user-supplied ntfy and webhook URLs from the hub, and a key-holder can set those URLs. Left unchecked the hub becomes a request bounce point into cloud metadata, loopback services or its own API.

**Decision.** Delivery refuses loopback, unspecified, 0.0.0.0/8, link-local (including 169.254.169.254), Tailscale's on-host service address (quad-100, IPv4 and IPv6, which net.InterfaceAddrs does not list), IPv4-mapped and IPv4-compatible IPv6 forms, and every address of the hub's own interfaces. Private LAN and tailnet ranges are refused only when BlockPrivate is set, because upstream allows a self-hosted ntfy or chat bridge on the LAN. A host that resolves to several addresses is refused if any one is blocked, since the dialer may pick any. Odd IPv4 spellings ('2130706433', '0x7f.1', '127.1') are rejected at save time rather than guessed at. The default client never follows redirects (they could point anywhere), never uses a proxy (it would hide the real destination), re-checks the address it actually dials (DNS rebinding), and does not pass the reply body back, since POST /api/alerts/test would echo it to the caller. Delivery errors are rebuilt without URL userinfo, path, query or fragment because webhook secrets live there, and any URL quoted inside the inner error is cut to scheme://host.

**Consequences.** Do not 'simplify' to http.DefaultClient, add redirect following, or log raw url errors. Injecting a custom Doer skips the dial-time check (see excuses.md).

**Where:** 
- internal/alerts/deliver.go
- internal/alerts/deliver.go
- internal/alerts/deliver.go
- internal/alerts/deliver.go
- internal/alerts/deliver.go
- internal/alerts/deliver.go
- internal/alerts/deliver.go
- internal/alerts/engine.go
- internal/alerts/settings.go

### Alert engine locking and restarts

**Context.** The engine is called by the hub under its own locks, evaluates a state machine per rule and device, and must behave sensibly across hub restarts.

**Decision.** Options.Devices may run with the engine's lock held, so it must not call back into the Engine; Broadcast and the power/cost callbacks (Energy) run outside the lock because they are the hub's code and may call back. A restart looks like every device dropping, so devices already offline at start are marked 'carried' (or firing in saved.Firing, already announced) and do not announce coming back. CheckCost re-validates the rule with hasRule after waiting on Energy because settings may change meanwhile. The power estimate is computed only when an enabled power rule covers the device. Event-log writes are debounced (logDelay, upstream's 1 s) so a burst costs one write; Wait and ClearLog flush at once.

**Consequences.** Removing the lock-order rules or the carried flag brings back the restart alert flood found in skeptic pass 2 and the missing 'back online' found in pass 3.

**Where:** 
- internal/alerts/engine.go
- internal/alerts/engine.go
- internal/alerts/engine.go
- internal/alerts/engine.go
- internal/alerts/engine.go
- internal/alerts/engine.go
- internal/alerts/engine.go
- internal/alerts/engine.go
- internal/alerts/engine.go
- internal/alerts/engine.go
- internal/alerts/engine.go

### Alert parity with upstream

**Context.** Alerts is a port of upstream alerts.js, and dashboards and hand-edited alerts.json files must keep working both ways.

**Decision.** Settings input is as lenient as upstream's validSettings, with JS Number() and truthiness semantics and Math.round halves-to-+Inf. Message wording and delivery payloads follow upstream: the notification link '/#/alerts' is always sent (bare path when no hub URL), the webhook body carries both content (Discord) and text (Slack), the disk rule uses the fullest disk over 1 GB (smaller ones are boot and recovery partitions), and currency formatting follows Intl.NumberFormat('en-US'). The id generator ignores the crypto/rand error because that call never fails (it panics instead).

**Consequences.** Changing wording, payload keys or coercion breaks round-tripping with upstream settings files and clients.

**Where:** 
- internal/alerts/engine.go
- internal/alerts/engine.go
- internal/alerts/engine.go
- internal/alerts/settings.go
- internal/alerts/settings.go
- internal/alerts/settings.go
- internal/alerts/settings.go
- internal/alerts/settings.go
- internal/alerts/settings.go
- internal/alerts/text.go
- internal/alerts/text.go
- internal/alerts/text.go

### History files and upstream compatibility

**Context.** History files move freely between a machine-monitor hub and a device-monitor hub, so on-disk format and aggregation are fixed by upstream history.js (c3a4c41).

**Decision.** NDJSON rows are exactly what JSON.stringify({id, ...p}) writes; times are Unix ms float64; minute averages with peaks (*Max fields), weighted merge of two points for the same minute using sample counts (rows from before counts count as 1) so a restart within a minute merges (upstream v1.2.29); 30-day prune; downsampling behind GET /api/history/:id (empty result is [], not null; defaults for range, points and finite-number parsing as upstream's server.js). Points returned by a Store share their *float64 values with it: callers read, never write. File names escape bytes outside [a-z0-9_-] including the escape marker, so lower-case machine IDs keep their names and upper-case IDs cannot collide on case-insensitive filesystems. Legacy files shared by colliding IDs are split by device before merging, never merge minutes across IDs, finish every canonical file before removing a legacy one (a completed canonical file wins on retry, preventing double counts), and complete pending migrations before Remove so a retained legacy file cannot resurrect a removed device. ReadDir reads with the same parsing but never writes, so it is safe beside a live hub.

**Consequences.** Fixtures under testdata/upstream come from upstream's own history.js and the tests check byte-identical files.

**Where:** 
- internal/alerts/settings.go
- internal/history/aggregate.go
- internal/history/aggregate.go
- internal/history/collision_test.go
- internal/history/history.go
- internal/history/history.go
- internal/history/history.go
- internal/history/history.go
- internal/history/history.go
- internal/history/history.go
- internal/history/history.go
- internal/history/history.go
- internal/history/history.go
- internal/history/history.go
- internal/history/history.go
- internal/history/history.go
- internal/history/history.go
- internal/history/history_test.go
- internal/history/history_test.go
- internal/history/params.go
- internal/history/readdir_test.go
- internal/history/upstream_test.go
- internal/history/upstream_test.go

### History durable writes

**Context.** Rows are appended once a minute per device for 30 days; crashes and full disks must not lose or garble history.

**Decision.** Appends cannot use WriteFile0600 (it rewrites the whole file, ~40k rows at 30 days), so files open 0600 with O_APPEND so writes land at the end even if another process appended. A failed or short write is truncated back through a second handle (an O_APPEND handle cannot truncate on Windows); if that fails too, the next append starts on a fresh line and parseLine re-syncs at the next row start, so a partial row with the next glued on loses only the partial one. Failed minutes are retried oldest first on the next commit (order preserved), given up after maxTries and capped at maxUnwritten so a dead disk cannot grow memory. Write errors are reported once per kind and re-armed by a success. Add keeps the sample in memory even if the write fails and requires finite values (Normalize guarantees it). encodeLine ignoring its error is safe: no maps, no NaN.

**Consequences.** Tests in partial_test.go pin each of these behaviours.

**Where:** 
- internal/history/history.go
- internal/history/history.go
- internal/history/history.go
- internal/history/history.go
- internal/history/history.go
- internal/history/history.go
- internal/history/history.go
- internal/history/history.go
- internal/history/history.go
- internal/history/partial_test.go
- internal/history/partial_test.go
- internal/history/partial_test.go
- internal/history/partial_test.go

### JS number and rounding parity

**Context.** Upstream is JavaScript; hub and clients compare numbers and strings it produced, so Go must reproduce JS number semantics.

**Decision.** toFixed rounds by the exact binary value with exact ties away from zero (0.15 is really 0.1499999..., so toFixed(1) gives 0.1 where Math.round(v*10)/10 and math.Round give 0.2); strconv breaks true ties to even, so exact ties (odd multiples of 1/8 at 2 decimals) are handled first. Math.round halves go toward +Inf. -0 is normalised away (toFixed never yields -0; JSON.stringify writes 0). Number() parsing accepts numeric strings (hex, exponents, surrounding JS whitespace, '' and spaces are 0), rejects Go-only forms (sign/underscore forms big.Int accepts), and out-of-range gives ±Inf or ±0 as JS does. Number.prototype.toString writes 1e-7 and 1e+21 where Go writes 1e-07. isJSSpace follows String.trim (includes U+FEFF, excludes U+0085). Query numbers must be finite or 'Infinity'/'1e999' become NaN bucket times.

**Consequences.** Expected values in tests come from upstream's node runtime; do not replace with math.Round.

**Where:** 
- internal/history/aggregate.go
- internal/history/aggregate.go
- internal/history/params.go
- internal/history/params.go
- internal/history/upstream_test.go
- internal/power/energy.go
- internal/power/energy.go
- internal/power/energy_test.go
- internal/power/energy_test.go
- internal/power/power.go
- internal/power/power.go
- internal/report/normalize.go
- internal/report/normalize.go
- internal/report/normalize.go
- internal/report/normalize.go
- internal/report/normalize.go
- internal/report/normalize.go
- internal/report/normalize.go
- internal/report/normalize.go
- internal/report/normalize_test.go

### Power model

**Context.** Power figures are estimates ported from upstream power.js and the energy part of server.js, within ~20% of a plug-in meter.

**Decision.** Measured values are used where hardware reports them; everything else is estimated (CPU from load, discrete GPU from load, fixed allowance for the rest, PSU loss). Adapters under 2 GiB and GPUs the agent flags integrated are assumed to share CPU package power; a GPU's name says nothing, only agent sensors can tell. HW is remembered so history points recorded without measured power can still be estimated while a device is offline. Unknown or zero threads count as 8. Accumulate counts each point as exactly one minute, so a gap adds nothing instead of being interpolated, and cost uses the rate in force at each point's local time. Overlapping time-of-use periods are refused with ErrOverlappingPeriods because upstream prices the overlap by the first period only, double counting daily totals; LoadSettings keeps flat rate and currency when an old file has overlaps. A rate that rounds to 0 is refused as upstream does. RateAt reads t in its location; first matching period wins.

**Consequences.** Test expected numbers come from running upstream's power.js (testdata/upstream.js).

**Where:** 
- internal/power/energy.go
- internal/power/power.go
- internal/power/power.go
- internal/power/power.go
- internal/power/power.go
- internal/power/power.go
- internal/power/power_test.go
- internal/power/power_test.go
- internal/power/power_test.go
- internal/power/price.go
- internal/power/price.go
- internal/power/price.go
- internal/power/price.go
- internal/power/price_test.go

### Report normalization

**Context.** Every value from another machine is untrusted; Normalize is a port of upstream security.js and must stay compatible with its agents while being stricter where the hub is exposed.

**Decision.** Normalize rebuilds the report from known fields only, coercing and clamping as JS Number()/String() do, with string limits counted in UTF-16 code units (a cut that would split a surrogate pair drops the half pair, so output is valid UTF-8 and never longer than upstream's). Decoding uses UseNumber-style handling so 1e999 stays decodable and becomes nil, as JS does with Infinity; trailing data after the JSON value is rejected as JSON.parse does. Beyond upstream, CleanText strips escape sequences, bidi controls, Arabic letter mark, zero-width and C1 controls through the shared textclean policy. Lists are cut to their limit before filtering non-objects. AI is nil when the agent does not check and empty when nothing is loaded; Processes is normalised separately and kept out of the stored report; the integrated GPU flag survives only as a real boolean true and upstream agents never send it. Additive fields (not in upstream) follow the same rules as their siblings. Conventions: unknown is nil/null never 0, null equals a missing key, all numbers are *float64.

**Consequences.** FuzzNormalize pins the invariants the hub relies on; parity bodies come from running upstream security.js.

**Where:** 
- internal/report/normalize.go
- internal/report/normalize.go
- internal/report/normalize.go
- internal/report/normalize.go
- internal/report/normalize.go
- internal/report/normalize.go
- internal/report/normalize.go
- internal/report/normalize.go
- internal/report/normalize.go
- internal/report/normalize_test.go
- internal/report/normalize_test.go
- internal/report/normalize_test.go
- internal/report/normalize_test.go
- internal/report/normalize_test.go
- internal/report/normalize_test.go
- internal/report/report.go
- internal/report/report.go
- internal/report/report.go
- internal/report/report.go

### Alert delivery guard (SSRF) and restart behaviour

**Context:** Anyone holding the key can set webhook/ntfy URLs, so the hub could be bounced at internal services; webhook secrets live in path/query.

**Decision:** Redirects are failures and never followed; no proxy is used (it would hide the real destination); the hub's own LAN/Tailscale addresses are refused as literals, behind a name and at dial time; failures log and report host only. After a hub restart every device looks offline, so already-offline devices are marked firing without notifying, and an offline alert announced before restart still produces "back online".

**Consequences:** Removing the dial-time check or enabling the default proxy reopens SSRF.

**Where:** `internal/alerts/alerts_test.go`

### History file safety and upstream byte compatibility

**Context:** History is NDJSON shared with upstream's format; hubs crash or restart mid-write.

**Decision:** A partial last line is cut on load so the next append starts clean; failed migrations persist the full loaded history (`needsRewrite`); files must be byte-identical to upstream's after replaying its operations; odd device ids round-trip through safe file names.

**Consequences:** Simplifying load/rewrite can lose a minute or whole histories.

**Where:** `internal/history/partial_test.go`, `internal/history/history.go`, `internal/history/upstream_test.go`

### Report schema parity with upstream

**Context:** Reports come from untrusted machines and upstream agents.

**Decision:** Normalize drops the token and normalizes `processes` separately (nil if not an array; the hub must move it out before storing). String limits count UTF-16 code units like JS slice and never split a surrogate pair. Non-arrays become `[]`, not nil. Nulls decode to nil, never 0, and null equals a missing key.

**Consequences:** Differences show up as upstream-visible JSON changes.

**Where:** `internal/report/normalize.go`, `internal/report/normalize_test.go`, `internal/report/report_test.go`
