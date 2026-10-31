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
curl -fsSL -H "Authorization: Bearer HUB_KEY" http://HUB:7777/install.sh | sh
```

Windows PowerShell 5.1 or 7 (installs to `%LOCALAPPDATA%\Programs\machine-monitor`):

```powershell
irm -Headers @{Authorization='Bearer HUB_KEY'} http://HUB:7777/install.ps1 | iex
```

`HUB_KEY` is the key the hub prints when it starts. The hub must be a release build, not one
you built yourself.

### Homebrew (macOS, Linux)

```sh
brew install --cask contractorkeith/tap/machine-monitor
```

### Scoop (Windows)

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
monitors its own machine too and prints its key and addresses:

```sh
machine-monitor hub                         # in the foreground
machine-monitor service install --role hub  # or start it at login
```

**An agent** on each other machine. The one-line installer above does all of this; by hand:

```sh
machine-monitor connect --no-agent http://HUB:7777                 # asks for a pairing code
machine-monitor connect --no-agent "http://HUB:7777/?key=HUB_KEY"  # or pairs with the key
machine-monitor service install --role agent --hub http://HUB:7777
```

Without `--no-agent`, `connect` starts reporting in the foreground right away. It checks
that the hub really holds the key (an HMAC challenge) before it saves or
sends it.

**The TUI against a hub**, from any terminal, including over SSH:

```sh
machine-monitor --hub http://HUB:7777
```

Other commands: `machine-monitor status --json` prints one report, `machine-monitor export ID
--range 7d` exports a device's history, `machine-monitor help` lists everything.

## Network and security

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
  six digits, live ten minutes and allow five tries.
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
