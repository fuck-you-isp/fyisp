#!/bin/sh
# Tests the launchers against a fake release served on 127.0.0.1. Run inside
# the Dockerfile's launchertest / launchertest-pwsh stages:
#
#   docker build --target launchertest .        # run.sh under dash and bash
#   docker build --target launchertest-pwsh .   # run.ps1 under PowerShell 7
#
# Usage: launcher_test.sh <dir with run.sh/run.ps1> <fyisp binary> <asset name> <shell>...
# where each <shell> is sh, bash or pwsh.
set -eu

src=$1 bin=$2 asset=$3
shift 3

work=$(mktemp -d)
srv="$work/srv"
port=8765
base="http://127.0.0.1:$port"
fail=0

pass() { printf 'PASS %s\n' "$*"; }
bad() {
	printf 'FAIL %s\n' "$*"
	fail=1
}

# ---- fake releases ----
mkdir -p "$srv/good" "$srv/tampered" "$srv/unlisted"
cp "$bin" "$srv/good/$asset"
(cd "$srv/good" && sha256sum "$asset" >SHA256SUMS && echo "0000  fyisp-other-os" >>SHA256SUMS)

# The tampered "binary" leaves a marker if anything ever executes it.
marker="$work/tampered-binary-ran"
printf '#!/bin/sh\ntouch %s\necho fyisp tampered\n' "$marker" >"$srv/tampered/$asset"
chmod +x "$srv/tampered/$asset"
cp "$srv/good/SHA256SUMS" "$srv/tampered/SHA256SUMS"

cp "$bin" "$srv/unlisted/$asset"
echo "0000  fyisp-other-os" >"$srv/unlisted/SHA256SUMS"

python3 -m http.server "$port" --bind 127.0.0.1 --directory "$srv" >"$work/http.log" 2>&1 &
httpd=$!
trap 'kill $httpd 2>/dev/null; rm -rf "$work"' EXIT
i=0
until curl -fsS -o /dev/null "$base/good/SHA256SUMS" 2>/dev/null; do
	i=$((i + 1))
	[ $i -lt 50 ] || {
		echo "fake release server did not start"
		cat "$work/http.log"
		exit 1
	}
	sleep 0.1
done

# run <shell> <base url or ""> <args...>: runs the launcher, output in $work/out, status in $rc.
run() {
	shell=$1 url=$2
	shift 2
	tmpd="$work/tmp"
	rm -rf "$tmpd" && mkdir -p "$tmpd"
	set +e
	case "$shell" in
	pwsh) env TMPDIR="$tmpd" FYISP_BASE_URL="$url" pwsh -NoProfile -NonInteractive -File "$src/run.ps1" "$@" >"$work/out" 2>&1 ;;
	pipe) env TMPDIR="$tmpd" FYISP_BASE_URL="$url" sh -s -- "$@" <"$src/run.sh" >"$work/out" 2>&1 ;;
	*) env TMPDIR="$tmpd" FYISP_BASE_URL="$url" "$shell" "$src/run.sh" "$@" >"$work/out" 2>&1 ;;
	esac
	rc=$?
	set -e
	leftovers=$(find "$tmpd" -mindepth 1 -maxdepth 1 | wc -l)
}

for shell in "$@"; do
	variants=$shell
	[ "$shell" = sh ] && variants="sh pipe"
	for sh in $variants; do
		run "$sh" "$base/good" --version
		if [ $rc -eq 0 ] && grep -q '^fyisp ' "$work/out" && grep -q 'sha256 ok' "$work/out"; then
			pass "$sh: good checksum runs fyisp --version: $(grep '^fyisp ' "$work/out")"
		else
			bad "$sh: good release (rc=$rc): $(cat "$work/out")"
		fi
		[ "$leftovers" -eq 0 ] || bad "$sh: temp dir not cleaned up"

		rm -f "$marker"
		run "$sh" "$base/tampered" --version
		if [ $rc -ne 0 ] && [ ! -e "$marker" ] && grep -qi 'checksum mismatch' "$work/out"; then
			pass "$sh: tampered binary rejected (rc=$rc) without running"
		else
			bad "$sh: tampered release (rc=$rc, ran=$([ -e "$marker" ] && echo yes || echo no)): $(cat "$work/out")"
		fi
		[ "$leftovers" -eq 0 ] || bad "$sh: temp dir not cleaned up after failure"

		run "$sh" "$base/unlisted" --version
		if [ $rc -ne 0 ] && grep -q 'not listed in SHA256SUMS' "$work/out"; then
			pass "$sh: binary missing from SHA256SUMS rejected"
		else
			bad "$sh: unlisted (rc=$rc): $(cat "$work/out")"
		fi

		run "$sh" "$base/nonexistent" --version
		if [ $rc -ne 0 ] && grep -q 'download failed' "$work/out"; then
			pass "$sh: missing release fails"
		else
			bad "$sh: missing release (rc=$rc): $(cat "$work/out")"
		fi

		run "$sh" "http://example.com/release" --version
		if [ $rc -ne 0 ] && grep -q 'must start with https://' "$work/out"; then
			pass "$sh: plain http to a remote host refused"
		else
			bad "$sh: http remote (rc=$rc): $(cat "$work/out")"
		fi

		run "$sh" "" --version
		if [ $rc -ne 0 ] && grep -q 'not from a release' "$work/out"; then
			pass "$sh: unreleased copy (no pinned version) refuses to guess"
		else
			bad "$sh: unpinned (rc=$rc): $(cat "$work/out")"
		fi
	done
done

# A pinned copy builds the GitHub URL (checked without downloading).
if [ -f "$src/run.sh" ]; then
	sed 's/@FYISP_VERSION@/v9.9.9/' "$src/run.sh" >"$work/pinned.sh"
	if grep -q 'FYISP_VERSION="v9.9.9"' "$work/pinned.sh" && ! grep -q '@FYISP_VERSION@"' "$work/pinned.sh"; then
		pass "sh: version placeholder is filled by sed"
	else
		bad "sh: version placeholder not replaceable"
	fi
fi

exit $fail
