#!/bin/sh
# machine-monitor agent installer for Linux and macOS, served by a machine-monitor hub with
# the values below filled in. No admin rights needed.
#   curl -fsSL -H "Authorization: Bearer KEY" http://HUB:7777/install.sh | sh
#
# It downloads the release the hub runs from GitHub, checks the archive against the SHA-256
# the hub pinned (from the release's signed checksums.txt), installs the binary in
# ~/.local/bin, pairs with the hub and starts the agent at login.
set -eu

HUB='__HUB__'
KEY='__KEY__'
VERSION='__VERSION__'
BASE='__BASE__'
SUMS='__SUMS__'

fail() {
	echo "machine-monitor install: $*" >&2
	exit 1
}

case "$(uname -s)" in
Linux) os=linux ;;
Darwin) os=darwin ;;
*) fail "unsupported OS $(uname -s); on Windows run install.ps1 from PowerShell" ;;
esac
case "$(uname -m)" in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) fail "unsupported CPU $(uname -m)" ;;
esac
# A shell under Rosetta reports x86_64 on Apple silicon; install the native build.
if [ "$os" = darwin ] && [ "$arch" = amd64 ] && [ "$(sysctl -n hw.optional.arm64 2>/dev/null || true)" = 1 ]; then
	arch=arm64
fi

name="machine-monitor_${VERSION}_${os}_${arch}.tar.gz"
want=$(printf '%s\n' "$SUMS" | awk -v n="$name" '$2 == n { print $1 }')
[ -n "$want" ] || fail "release $VERSION has no build for $os/$arch"

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | cut -d' ' -f1
	elif command -v openssl >/dev/null 2>&1; then
		openssl dgst -sha256 "$1" | awk '{ print $NF }'
	else
		fail "no sha256sum, shasum or openssl to check the download with"
	fi
}

tmp=$(mktemp -d "${TMPDIR:-/tmp}/machine-monitor.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

echo "Downloading machine-monitor $VERSION for $os/$arch..."
curl -fsSL "$BASE/$name" -o "$tmp/$name" || fail "download failed: $BASE/$name"
got=$(sha256 "$tmp/$name")
[ "$got" = "$want" ] || fail "the download failed its SHA-256 check (got $got, want $want); not installing"
tar -xzf "$tmp/$name" -C "$tmp" machine-monitor || fail "can't unpack $name"

dir="$HOME/.local/bin"
bin="$dir/machine-monitor"
mkdir -p "$dir"
# Copy next to the target, then rename: a running agent's binary is swapped atomically.
cp "$tmp/machine-monitor" "$dir/.machine-monitor.new"
chmod 755 "$dir/.machine-monitor.new"
mv -f "$dir/.machine-monitor.new" "$bin"
echo "Installed $bin"

# Saves the hub and key in client.json, after the hub proves it holds the key.
"$bin" connect --no-agent "$HUB/?key=$KEY"
"$bin" service install --role agent --hub "$HUB"

case ":$PATH:" in
*":$dir:"*) ;;
*) echo "Add $dir to your PATH to run machine-monitor from a terminal." ;;
esac
echo "This machine now reports to $HUB. Remove with: machine-monitor service uninstall --role agent"
