#!/bin/sh
# fyisp launcher for Linux and macOS: downloads the pinned fyisp release for
# this machine into a temporary directory, verifies its SHA-256 against the
# release's SHA256SUMS (mandatory) and, if the GitHub CLI is installed and
# logged in, its build provenance attestation; then runs it with your
# arguments. Nothing is installed; the download is deleted when fyisp exits.
#
#   curl --proto '=https' --tlsv1.2 -fsSL https://github.com/fuck-you-isp/fyisp/releases/download/<version>/run.sh | sh
#   curl ... | sh -s -- --share          # pass arguments to fyisp
#
# Environment:
#   FYISP_BASE_URL  download from here instead of the GitHub release (must be
#                   https://, or http:// on localhost / 127.0.0.1 for tests)
#   TMPDIR          where the temporary directory is created (must allow exec)
#
# The whole script is inside main(), called on the last line, so a truncated
# download never runs a partial script.

main() {
	set -eu

	# Filled in by the release workflow; do not edit by hand.
	FYISP_VERSION="@FYISP_VERSION@"
	repo="fuck-you-isp/fyisp"

	say() { printf 'fyisp-run: %s\n' "$*" >&2; }
	die() {
		say "error: $*"
		exit 1
	}

	# ---- where to download from ----
	base="${FYISP_BASE_URL:-}"
	if [ -z "$base" ]; then
		case "$FYISP_VERSION" in
		@*) die "this run.sh is not from a release (no version pinned); download it from https://github.com/$repo/releases or set FYISP_BASE_URL" ;;
		esac
		base="https://github.com/$repo/releases/download/$FYISP_VERSION"
	fi
	base="${base%/}"
	case "$base" in
	https://*) proto="=https" ;;
	http://localhost/* | http://localhost:* | http://localhost | http://127.0.0.1/* | http://127.0.0.1:* | http://127.0.0.1)
		proto="=http"
		say "warning: downloading over plain HTTP from $base (test mode)"
		;;
	*) die "FYISP_BASE_URL must start with https:// (http:// only for localhost or 127.0.0.1): $base" ;;
	esac

	# ---- which binary ----
	os=$(uname -s)
	arch=$(uname -m)
	case "$os" in
	Linux) os=linux ;;
	Darwin) os=darwin ;;
	MINGW* | MSYS* | CYGWIN*) die "on Windows use run.ps1 (PowerShell) instead" ;;
	*) die "unsupported operating system: $os" ;;
	esac
	case "$arch" in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	armv7* | armv8l) arch=armv7 ;;
	*) die "unsupported CPU architecture: $arch (fyisp ships amd64, arm64 and armv7)" ;;
	esac
	# An x86_64 shell under Rosetta on Apple silicon: use the native binary.
	if [ "$os" = darwin ] && [ "$arch" = amd64 ] &&
		[ "$(sysctl -n sysctl.proc_translated 2>/dev/null || echo 0)" = 1 ]; then
		arch=arm64
	fi
	if [ "$os" = darwin ] && [ "$arch" = armv7 ]; then
		die "unsupported CPU architecture for macOS: $arch"
	fi
	asset="fyisp-$os-$arch"

	command -v curl >/dev/null 2>&1 || die "curl is required"
	if command -v sha256sum >/dev/null 2>&1; then
		sha256() { sha256sum "$1" | awk '{print $1}'; }
	elif command -v shasum >/dev/null 2>&1; then
		sha256() { shasum -a 256 "$1" | awk '{print $1}'; }
	elif command -v openssl >/dev/null 2>&1; then
		sha256() { openssl dgst -sha256 -r "$1" | awk '{print $1}'; }
	else
		die "need sha256sum, shasum or openssl to verify the download"
	fi

	# ---- download into a private temporary directory ----
	tmp=$(mktemp -d "${TMPDIR:-/tmp}/fyisp.XXXXXXXX") || die "cannot create a temporary directory"
	trap 'rm -rf "$tmp"' EXIT
	trap 'exit 130' INT
	trap 'exit 143' TERM HUP

	fetch() {
		curl --proto "$proto" --proto-redir "$proto" --tlsv1.2 -fsSL --retry 3 -o "$2" "$1" ||
			die "download failed: $1"
	}
	say "downloading $asset from $base"
	fetch "$base/SHA256SUMS" "$tmp/SHA256SUMS"
	fetch "$base/$asset" "$tmp/$asset"

	# ---- verify (mandatory) ----
	want=$(awk -v f="$asset" '$2 == f || $2 == "*" f { print $1; exit }' "$tmp/SHA256SUMS")
	[ -n "$want" ] || die "$asset is not listed in SHA256SUMS"
	got=$(sha256 "$tmp/$asset")
	if [ "$got" != "$want" ]; then
		die "checksum mismatch for $asset: got $got, want $want; not running it"
	fi
	say "sha256 ok: $got"

	if [ -z "${FYISP_BASE_URL:-}" ] && command -v gh >/dev/null 2>&1 && gh auth status >/dev/null 2>&1; then
		gh attestation verify "$tmp/$asset" --repo "$repo" >/dev/null ||
			die "build provenance attestation check failed for $asset; not running it"
		say "attestation ok (built by github.com/$repo)"
	fi

	chmod 0755 "$tmp/$asset"

	# ---- run ----
	# Not exec: the shell stays to delete the download when fyisp exits.
	# Ctrl-C reaches fyisp directly (same process group); the INT trap above
	# only fires once fyisp has exited.
	status=0
	"$tmp/$asset" "$@" || status=$?
	if [ "$status" -eq 126 ]; then
		say "could not execute $tmp/$asset (is ${TMPDIR:-/tmp} mounted noexec? set TMPDIR)"
	fi
	exit "$status"
}

main "$@"
