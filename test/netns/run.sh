#!/usr/bin/env bash
# Network-fault harness for fyisp (Linux, needs sudo). See netns_test.go.
#
#   test/netns/run.sh                      # build in Docker, run all scenarios in parallel (~5 min)
#   test/netns/run.sh -test.run 'Faults/b' # extra args go to the test binary
#   FYISP_NETNS_PARALLEL=0 test/netns/run.sh   # one scenario at a time (~35 min)
#
# Builds fyisp and the test with the Go image pinned in the repo Dockerfile
# (no local Go toolchain), then runs the test as root. Every scenario gets
# its own topology of network namespaces fyt-<run>-<n>-{cli,gw,isp,net};
# sudo is used for those namespaces, veth pairs created inside them,
# nft/tc/routes inside them, and running processes in them. fyisp itself
# runs as uid 65532 without capabilities. The host's interfaces, routes and
# firewall are never touched.
#
# FYISP_NETNS_OUT=dir keeps the raw API responses and fyisp's logs, one
# subdirectory per scenario.
set -euo pipefail
root=$(cd "$(dirname "$0")/../.." && pwd)
bin=$(mktemp -d)
# This run's namespaces are fyt-$run_id-*: cleanup never touches another
# run's namespaces.
run_id=$(od -An -N2 -tx1 /dev/urandom | tr -d ' \n')
start=$SECONDS

cleanup() {
  rc=$?
  for f in /run/netns/fyt-"$run_id"-*; do
    [ -e "$f" ] || continue
    ns=${f##*/}
    for pid in $(sudo ip netns pids "$ns" 2>/dev/null); do sudo kill -9 "$pid" 2>/dev/null || true; done
    sudo ip netns del "$ns" 2>/dev/null || true
  done
  rm -rf "$bin"
  left=$(ip netns list | grep -E "^fyt-$run_id-" || true)
  if [ -n "$left" ]; then echo "run.sh: namespaces left behind: $left" >&2; rc=1; fi
  t=$((SECONDS - start))
  echo "run.sh: wall time $((t / 60))m$((t % 60))s (build + test), exit $rc" >&2
  exit "$rc"
}
trap cleanup EXIT INT TERM

go_image=$(sed -n 's/^ARG GO_IMAGE=//p' "$root/Dockerfile")
docker buildx build -q -f "$root/test/netns/Dockerfile" --build-arg GO_IMAGE="$go_image" \
  --platform linux/amd64 --output type=local,dest="$bin" "$root" >/dev/null
echo "run.sh: built in $((SECONDS - start))s, run id $run_id" >&2

env=(FYISP_BIN="$bin/fyisp" FYISP_NETNS_RUNID="$run_id")
[ -n "${FYISP_NETNS_OUT:-}" ] && env+=(FYISP_NETNS_OUT="$(realpath -m "$FYISP_NETNS_OUT")")
# The longest scenario takes about 5 minutes; one at a time, all of them
# take about 35. A -test.timeout among the extra args wins.
timeout=20m
if [ "${FYISP_NETNS_PARALLEL:-1}" = 0 ]; then
  env+=(FYISP_NETNS_PARALLEL=0)
  timeout=90m
fi
test_start=$SECONDS
rc=0
sudo env "${env[@]}" "$bin/netns.test" -test.v -test.timeout "$timeout" "$@" || rc=$?
echo "run.sh: tests took $((SECONDS - test_start))s" >&2
exit "$rc"
