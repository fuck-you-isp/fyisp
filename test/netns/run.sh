#!/usr/bin/env bash
# Network-fault harness for fyisp (Linux, needs sudo). See netns_test.go.
#
#   test/netns/run.sh                      # build in Docker, run all scenarios (~9 min)
#   test/netns/run.sh -test.run 'Faults/b' # extra args go to the test binary
#
# Builds fyisp and the test with the Go image pinned in the repo Dockerfile
# (no local Go toolchain), then runs the test as root. sudo is used for:
# network namespaces fyt-{client,gw,isp,inet}, veth pairs created inside
# them, nft/tc/routes inside fyt-isp, and running processes in them. fyisp
# itself runs as uid 65532 without capabilities. The host's interfaces,
# routes and firewall are never touched.
#
# FYISP_NETNS_OUT=dir keeps the raw API responses and fyisp's logs.
set -euo pipefail
root=$(cd "$(dirname "$0")/../.." && pwd)
bin=$(mktemp -d)
namespaces="fyt-client fyt-gw fyt-isp fyt-inet"

cleanup() {
  rc=$?
  for ns in $namespaces; do
    if [ -e "/run/netns/$ns" ]; then
      for pid in $(sudo ip netns pids "$ns" 2>/dev/null); do sudo kill -9 "$pid" 2>/dev/null || true; done
      sudo ip netns del "$ns" 2>/dev/null || true
    fi
  done
  rm -rf "$bin"
  left=$(ip netns list | grep -E '^fyt-' || true)
  if [ -n "$left" ]; then echo "run.sh: namespaces left behind: $left" >&2; rc=1; fi
  exit "$rc"
}
trap cleanup EXIT INT TERM

go_image=$(sed -n 's/^ARG GO_IMAGE=//p' "$root/Dockerfile")
docker buildx build -q -f "$root/test/netns/Dockerfile" --build-arg GO_IMAGE="$go_image" \
  --platform linux/amd64 --output type=local,dest="$bin" "$root" >/dev/null
out_env=()
[ -n "${FYISP_NETNS_OUT:-}" ] && out_env=(FYISP_NETNS_OUT="$(realpath -m "$FYISP_NETNS_OUT")")
sudo env FYISP_BIN="$bin/fyisp" "${out_env[@]}" "$bin/netns.test" -test.v -test.timeout 25m "$@"
