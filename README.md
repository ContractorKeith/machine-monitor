# machine-monitor

A terminal app that watches CPU, GPU, memory, temperatures, disks, network, processes and
power on one machine, or on every machine on your network, from a single binary on Windows,
macOS and Linux. No runtime dependencies and no admin rights.

**Status:** in development toward v1. The build plan is in [docs/PLAN.md](docs/PLAN.md).

<!-- Screenshots: dashboard, device view and history go here once the views settle. -->

## Install

### On a machine that should report to your hub (one line)

The hub serves an installer with its address and key filled in. It downloads the release the
hub runs from GitHub, checks it against the SHA-256 in the release's signed `checksums.txt`,
installs it in your home directory, pairs with the hub and starts the agent at login.

Linux and macOS (installs to `~/.local/bin`):

```sh
curl -sSL --fail-with-body -H "Authorization: Bearer HUB_KEY" http://HUB:7777/install.sh | sh
```

Windows PowerShell 5.1 or 7 (installs to `%LOCALAPPDATA%\Programs\machine-monitor`):

```powershell
irm -Headers @{Authorization='Bearer HUB_KEY'} http://HUB:7777/install.ps1 | iex
```

`HUB_KEY` is the hub's key: `machine-monitor hub --show-key` on the hub prints it. The hub
shows it in full when it starts in a terminal, and masked when its output goes to a log. The hub must be a release build, not one
you built yourself.

Keep `--fail-with-body` and don't add `-f`: macOS curl refuses the two together. With it,
the hub's refusals (wrong key, unverifiable release) reach `sh` as a script that prints why
and exits 1, and every step inside the script exits non-zero on failure. If curl itself fails
before any script arrives (no network, or curl older than 7.76, such as Ubuntu 20.04, which
doesn't know `--fail-with-body`), `sh` receives nothing and the pipeline exits 0 after
curl's error. To get a reliable exit status there, download first:

```sh
curl -fsSL -H "Authorization: Bearer HUB_KEY" -o install.sh http://HUB:7777/install.sh && sh install.sh
```

Behind an HTTPS proxy such as `tailscale serve`, list the proxy's address in the hub's
`config.json` as `"trustedProxies"`. For `tailscale serve` on the hub's own machine that is
`["127.0.0.1", "::1"]`: the installer then links to the `https://` address
(`X-Forwarded-Proto` from anywhere else is ignored). A loopback entry is trusted for that
only, never for `X-Forwarded-For`: every program on the machine connects from loopback, so
the hub can't tell the proxy's forwarded client address from one another local user made
up. Clients that come in through `tailscale serve` therefore share one set of rate limits
and pairing tries. Only a proxy on another machine, listed by its own address, has its
`X-Forwarded-For` believed, and only the entry it appended.

### Homebrew (macOS)

```sh
brew install --cask contractorkeith/tap/machine-monitor
```

On Linux, use the one-line installer or a manual install.

### Scoop (Windows)

The manifest is published from the first release on:

```powershell
scoop install https://raw.githubusercontent.com/ContractorKeith/machine-monitor/main/packaging/scoop/machine-monitor.json
```

### Manual

Download the archive for your OS and CPU from
[Releases](https://github.com/ContractorKeith/machine-monitor/releases), check it against
`checksums.txt`, and put `machine-monitor` (or `machine-monitor.exe`) on your PATH.
To check the signature on `checksums.txt` with [cosign](https://github.com/sigstore/cosign):

```sh
cosign verify-blob checksums.txt --bundle checksums.txt.sigstore.json \
  --certificate-identity "https://github.com/ContractorKeith/machine-monitor/.github/workflows/release.yml@refs/tags/vX.Y.Z" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

### Updating

```sh
machine-monitor update --check   # say whether a newer release exists and what changed
machine-monitor update           # download, verify checksum and signature, replace this binary
```

Homebrew and Scoop installs update through `brew upgrade` and `scoop update` instead. A
running hub or agent keeps the old version until it restarts.

## Quick start

**Just this machine:**

```sh
machine-monitor
```

**A hub** that collects every machine, keeps 30 days of history and evaluates alerts. It
monitors its own machine too and prints its addresses and key (masked unless it is running in
a terminal):

```sh
machine-monitor hub                         # in the foreground
machine-monitor service install --role hub  # or start it at login
machine-monitor hub --show-key              # print the key
machine-monitor pair                        # a one-time pairing code for connect, valid 10 minutes
```

A SIGHUP leaves the hub running with its current settings and pairings. Restart the hub
after editing its configuration; SIGINT and SIGTERM stop it and flush pending history.

**An agent** on each other machine. The one-line installer above does all of this; by hand:

```sh
machine-monitor connect --no-tui http://HUB:7777                   # asks for a code from `pair`
echo HUB_KEY | machine-monitor connect --no-tui --key-stdin http://HUB:7777  # or the key
machine-monitor service install --role agent      # reports to the hub you paired with
```

`connect` with no URL looks for hubs on the LAN and lets you pick one. After pairing it opens
the TUI on the hub when stdout is a terminal (`--no-tui` skips that); to report this machine,
run `machine-monitor agent` or install the service as above. It checks that the hub really
holds the key (an HMAC challenge) before it saves or sends it. Keys never go in a URL you
give to `agent` or `service install`; pair with `connect --key-stdin` instead.

**The TUI against a hub**, from any terminal, including over SSH:

```sh
machine-monitor --hub http://HUB:7777
```

Other commands: `machine-monitor status --json` prints one report, `machine-monitor export ID|NAME
--range 7d` exports a device's history, `machine-monitor help` lists everything.

## Network and security

The network total adds up every interface except loopback, as upstream device-monitor does.
Virtual interfaces such as `tailscale0` or `docker0` are included, so traffic that crosses a
VPN or bridge is counted on both the virtual and the physical link and the total can read
higher than the physical NIC alone. The per-interface rates are in `status --json`.

| Port | Protocol | Used for |
|---|---|---|
| 7777 | TCP | Hub HTTP API: agent reports, TUI connections, installers |
| 7778 | UDP | LAN discovery of hubs (same probe as upstream device-monitor) |

Open 7777/TCP (and 7778/UDP for discovery) on the hub's firewall for your local network only.

- The hub speaks plain HTTP. It is meant for your LAN or your tailnet, never the internet: it
  refuses connections from public addresses and requests addressed to names other than its
  own.
- **Use [Tailscale](https://tailscale.com)** for machines outside your LAN, or when you don't
  trust everything on it. Traffic between tailnet devices is encrypted and the hub accepts
  tailnet addresses.
- Everything except the hub's identity and pairing needs the hub key. Pairing codes are
  six digits and live ten minutes; one is active at a time. Each source (an IPv4 address or
  IPv6 /64) gets five wrong tries, and 100 wrong tries in all end the code.
- Every report from another machine is validated and clamped before it is stored or shown.
- Releases are signed: `update` and the hub's installers refuse any archive whose checksum,
  or whose `checksums.txt` signature from this repo's release workflow, doesn't verify.

## Credit

machine-monitor reimplements, as a TUI, the features of
[Device Monitor](https://github.com/Forward-Future/device-monitor) by Matt / Forward Future
(MIT). Logic ported from it (the power model, electricity price table, alert rules and report
validation) keeps its MIT notice in the source files it lands in.

## License

MIT. See [LICENSE](LICENSE).
