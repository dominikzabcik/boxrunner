#!/bin/sh
# Installs the boxrunner agent on this box, then runs `boxrunner install`.
#
#   curl -fsSL <url of this script> | sudo sh -s -- --worker URL --token TOKEN
#
# Everything runs inside main, called on the last line, so a download cut off
# halfway runs nothing. The binary must match a checksum written into this
# script when the release was made; with gh signed in, its build attestation
# is checked too. Read it first with: curl -fsSL <url> | less
set -eu

VERSION="__VERSION__"
CHECKSUMS="__CHECKSUMS__"
REPO="dominikzabcik/boxrunner"

die() {
	echo "boxrunner: $*" >&2
	exit 1
}

download() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL --retry 3 -o "$2" "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -q -O "$2" "$1"
	else
		die "needs curl or wget"
	fi
}

main() {
	[ "$(uname -s)" = Linux ] || die "boxrunner runs on Linux; this is $(uname -s)"
	case "$(uname -m)" in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*) die "no build for $(uname -m); boxrunner supports amd64 and arm64" ;;
	esac
	[ "$(id -u)" = 0 ] || die "run as root, for example: curl -fsSL <url> | sudo sh -s -- --worker URL --token TOKEN"
	command -v sha256sum >/dev/null 2>&1 || die "needs sha256sum (coreutils)"

	base="${BOXRUNNER_DOWNLOAD:-https://github.com/$REPO/releases/download/v$VERSION}"
	file="boxrunner-linux-$arch"
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT INT TERM

	echo "==> download boxrunner $VERSION for linux/$arch"
	download "$base/$file" "$tmp/boxrunner"

	want=$(printf '%s\n' "$CHECKSUMS" | awk -v f="$file" '$2 == f { print $1 }')
	got=$(sha256sum "$tmp/boxrunner" | cut -d' ' -f1)
	[ -n "$want" ] || die "this installer has no checksum for $file"
	[ "$want" = "$got" ] || die "checksum mismatch for $file: expected $want, got $got. Nothing was installed."
	echo "==> checksum ok"

	if command -v gh >/dev/null 2>&1 && gh auth status >/dev/null 2>&1; then
		gh attestation verify "$tmp/boxrunner" --repo "$REPO" >/dev/null ||
			die "the build attestation does not match $file. Nothing was installed."
		echo "==> build attestation ok"
	else
		echo "==> skipped the build attestation check (needs gh, signed in)"
	fi

	install -m 755 "$tmp/boxrunner" /usr/local/bin/boxrunner
	# Under `curl | sh` standard input is this script, so questions go to the terminal.
	if [ -t 1 ] && (exec </dev/tty) 2>/dev/null; then
		exec /usr/local/bin/boxrunner install "$@" </dev/tty
	fi
	exec /usr/local/bin/boxrunner install "$@"
}

main "$@"
