#!/bin/sh
# machine-monitor agent installer for Linux and macOS, served by a machine-monitor hub with
# the values below filled in. No admin rights needed.
#   curl -sSL --fail-with-body -H "Authorization: Bearer KEY" http://HUB:7777/install.sh | sh
#
# It downloads the release the hub runs from GitHub, checks the archive against the SHA-256
# the hub pinned (from the release's signed checksums.txt), installs the binary in
# ~/.local/bin, pairs with the hub and starts the agent at login.
#
# If a step after the binary is installed fails, the install is rolled back: what this run
# added is removed, and a binary or client.json it replaced is put back.
#
# Everything is inside main, called on the last line: an empty or cut-off download runs
# nothing instead of half an install.
set -eu

fail() {
	echo "machine-monitor install: $*" >&2
	exit 1
}

# sha256 FILE OUT writes FILE's SHA-256 to OUT. No pipelines: each tool's own exit status is
# checked, so a failed hash can't pass as an empty one.
sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" >"$2.raw" || return 1
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" >"$2.raw" || return 1
	elif command -v openssl >/dev/null 2>&1; then
		openssl dgst -sha256 -r "$1" >"$2.raw" || return 1
	else
		echo "no sha256sum, shasum or openssl to check the download with" >"$2"
		return 2
	fi
	read -r sum _ <"$2.raw" || return 1
	printf '%s\n' "$sum" >"$2"
}

# present is true for a file or a symlink, including a dangling one.
present() { [ -f "$1" ] || [ -L "$1" ]; }

# restore puts the backup $1 back at $2. A symlink is recreated as a link (cp -P), never
# followed, so a linked binary or client.json comes back as the link.
restore() {
	if [ -L "$1" ]; then
		rm -f "$2" && cp -P "$1" "$2"
	else
		cp -p "$1" "$2.restore" && mv -f "$2.restore" "$2"
	fi
}

# cleanup runs on every exit. After a failed install it undoes what this run did: restores the
# binary and client.json it replaced, and removes the ones it added.
cleanup() {
	status=$?
	if [ "$rollback" = yes ]; then
		echo "Rolling back the install." >&2
		if present "$tmp/previous"; then
			restore "$tmp/previous" "$bin"
		else
			rm -f "$bin"
		fi
		if present "$tmp/previous-client"; then
			restore "$tmp/previous-client" "$client"
		else
			rm -f "$client"
		fi
	fi
	if [ -n "${dir:-}" ]; then rm -f "$dir/.machine-monitor.new" "${bin:-$dir/machine-monitor}.restore"; fi
	rm -rf "$tmp"
	exit "$status"
}

main() {
	rollback=no
	tmp=""
	HUB='__HUB__'
	KEY='__KEY__'
	VERSION='__VERSION__'
	BASE='__BASE__'
	SUMS='__SUMS__'

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
	want=""
	while read -r sum file; do
		if [ "$file" = "$name" ]; then want=$sum; fi
	done <<EOF_SUMS
$SUMS
EOF_SUMS
	[ -n "$want" ] || fail "release $VERSION has no build for $os/$arch"

	tmp=$(mktemp -d "${TMPDIR:-/tmp}/machine-monitor.XXXXXX") || fail "can't create a temp directory"
	trap cleanup EXIT
	trap 'exit 1' HUP INT TERM

	echo "Downloading machine-monitor $VERSION for $os/$arch..."
	curl -sSL --fail --max-time 300 --retry 3 -o "$tmp/$name" "$BASE/$name" || fail "download failed: $BASE/$name"
	sha256 "$tmp/$name" "$tmp/sum" || fail "can't check the download: $(cat "$tmp/sum" 2>/dev/null || echo "hashing failed")"
	read -r got <"$tmp/sum" || fail "can't check the download"
	[ "$got" = "$want" ] || fail "the download failed its SHA-256 check (got $got, want $want); not installing"
	tar -xzf "$tmp/$name" -C "$tmp" machine-monitor || fail "can't unpack $name"

	dir="$HOME/.local/bin"
	bin="$dir/machine-monitor"
	mkdir -p "$dir" || fail "can't create $dir"
	# Where connect saves the hub and key (internal/config): noted now to undo it on failure.
	if [ -n "${MACHINE_MONITOR_DIR:-}" ]; then
		client="$MACHINE_MONITOR_DIR/client.json"
	elif [ "$os" = darwin ]; then
		client="$HOME/Library/Application Support/machine-monitor/client.json"
	else
		client="${XDG_CONFIG_HOME:-$HOME/.config}/machine-monitor/client.json"
	fi
	if present "$client"; then cp -Pp "$client" "$tmp/previous-client" || fail "can't back up $client"; fi
	if present "$bin"; then cp -Pp "$bin" "$tmp/previous" || fail "can't back up $bin"; fi
	# Copy next to the target, then rename: a running agent's binary is swapped atomically.
	cp "$tmp/machine-monitor" "$dir/.machine-monitor.new" || fail "can't write $dir"
	chmod 755 "$dir/.machine-monitor.new" || fail "can't make $dir/.machine-monitor.new executable"
	rollback=yes
	mv -f "$dir/.machine-monitor.new" "$bin" || fail "can't replace $bin"
	echo "Installed $bin"

	# Saves the hub and key in client.json, after the hub proves it holds the key. The key
	# goes on stdin so it never shows in the process list. (A pipeline's status is its last
	# command's: connect's.)
	printf '%s\n' "$KEY" | "$bin" connect --no-agent --key-stdin "$HUB" || fail "pairing with $HUB failed"
	"$bin" service install --role agent --hub "$HUB" </dev/null || fail "service install failed"

	rollback=no
	case ":$PATH:" in
	*":$dir:"*) ;;
	*) echo "Add $dir to your PATH to run machine-monitor from a terminal." ;;
	esac
	echo "This machine now reports to $HUB. Remove with: machine-monitor service uninstall --role agent"
}

main "$@"
