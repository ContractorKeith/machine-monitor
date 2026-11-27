#!/bin/sh
set -eu

fail() {
	echo "machine-monitor install: $*" >&2
	exit 1
}

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

present() { [ -f "$1" ] || [ -L "$1" ]; }

restore() {
	if [ -L "$1" ]; then
		rm -f "$2" && cp -P "$1" "$2"
	else
		cp -p "$1" "$2.restore" && mv -f "$2.restore" "$2"
	fi
}

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
	if [ -n "${MACHINE_MONITOR_DIR:-}" ]; then
		client="$MACHINE_MONITOR_DIR/client.json"
	elif [ "$os" = darwin ]; then
		client="$HOME/Library/Application Support/machine-monitor/client.json"
	else
		client="${XDG_CONFIG_HOME:-$HOME/.config}/machine-monitor/client.json"
	fi
	if present "$client"; then cp -Pp "$client" "$tmp/previous-client" || fail "can't back up $client"; fi
	if present "$bin"; then cp -Pp "$bin" "$tmp/previous" || fail "can't back up $bin"; fi
	cp "$tmp/machine-monitor" "$dir/.machine-monitor.new" || fail "can't write $dir"
	chmod 755 "$dir/.machine-monitor.new" || fail "can't make $dir/.machine-monitor.new executable"
	rollback=yes
	mv -f "$dir/.machine-monitor.new" "$bin" || fail "can't replace $bin"
	echo "Installed $bin"

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
