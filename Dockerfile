# syntax=docker/dockerfile:1.7
# All building and testing goes through this file: no local Go toolchain needed.
#   docker build --target test .                                   # vet + tests
#   docker buildx build --target dist --output type=local,dest=dist .   # all release binaries
#   docker buildx build --target modfiles --output type=local,dest=. .  # go mod tidy -> go.mod/go.sum
#   docker buildx build --target fmt --output type=local,dest=. .       # gofmt -w all Go files
#   docker buildx build --target asndb --output type=local,dest=. .     # refresh internal/asn/data/ip2asn-v4.bin
#   docker buildx build --target dbfixture --output type=local,dest=. . # regenerate internal/store/testdata/<release>.db (after tagging a release)
#   docker buildx build --target uitest-out --output type=local,dest=out --no-cache-filter uitest-run .  # UI tests (Playwright) -> out/shots, out/report
#   docker build -t fyisp .                                         # runtime image (default target)
#   docker build --target imagetest .                               # runtime image smoke test (--ephemeral)
#   docker build --target livetest -t fyisp-livetest .              # live --tunnel tests: see internal/tunnel/live_test.go

ARG GO_IMAGE=golang:1.27.3-bookworm@sha256:972b4110ba0f47539a975c31e52c64765e1ff19f2d74469ae4a595c6676e80a5
# The browser image for the UI tests; its Playwright must match test/ui/package.json.
ARG PLAYWRIGHT_IMAGE=mcr.microsoft.com/playwright:v1.64.0-noble@sha256:06a9939e57531807f8d5fd76ce44b53165ffb7d7501d87ab10e285c20b1e971f

FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS base
ENV CGO_ENABLED=0 GOTOOLCHAIN=local GOFLAGS=-trimpath
WORKDIR /src

FROM base AS tidy
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod tidy
FROM scratch AS modfiles
COPY --from=tidy /src/go.mod /src/go.sum /

FROM base AS gofmt
COPY . .
RUN gofmt -w $(find . -name '*.go') && mkdir /out && find . -name '*.go' | tar cf - -T - | tar xf - -C /out
FROM scratch AS fmt
COPY --from=gofmt /out/ /

FROM base AS deps
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download && go mod verify

FROM deps AS source
COPY . .

# The OS-specific netinfo code (and its tests) is also vetted for macOS and
# Windows, so a change there can't break those builds unnoticed. The netns
# harness (build tag netns) is vetted too, so an API change can't break it
# until the nightly run.
FROM source AS test
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    test -z "$(gofmt -l . | tee /dev/stderr)" && go vet ./... \
 && GOOS=darwin go vet ./internal/netinfo/... && GOOS=windows go vet ./internal/netinfo/... \
 && GOOS=linux go vet -tags netns ./test/netns \
 && CGO_ENABLED=1 go test -race ./...

# Built-in IP-to-ASN table: download the latest iptoasn.com IPv4 data (public
# domain, PDDL) and convert it with internal/asn/cmd/asngen. ADD re-checks the
# URL on every build, so a new upstream file is always picked up. The version
# is the file's Last-Modified date (today if the server sent none).
#   docker buildx build --target asndb --output type=local,dest=. .
FROM source AS asndb-gen
ARG ASNDB_URL=https://iptoasn.com/data/ip2asn-v4.tsv.gz
ADD ${ASNDB_URL} /asndb/ip2asn-v4.tsv.gz
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build <<'SH'
set -eu
v=$(date -u -r /asndb/ip2asn-v4.tsv.gz +%F)
case "$v" in 19*|20[01]*) v=$(date -u +%F);; esac
ls -l /asndb/ip2asn-v4.tsv.gz
mkdir -p /out internal/asn/data
# asngen links internal/asn, which embeds the table: any placeholder will do.
[ -e internal/asn/data/ip2asn-v4.bin ] || : > internal/asn/data/ip2asn-v4.bin
go run ./internal/asn/cmd/asngen -in /asndb/ip2asn-v4.tsv.gz -out /out/ip2asn-v4.bin -version "$v" -source "$ASNDB_URL"
SH
FROM scratch AS asndb
COPY --from=asndb-gen /out/ip2asn-v4.bin /internal/asn/data/

# The database fixture of the last release, for TestUpgradeFromLastRelease:
# the generator (internal/store/testdata/genfixture) runs in a checkout of
# the release tag, so the file is written by that release's own store.
# Regenerate it after tagging a release (set RELEASE and lastRelease in
# internal/store/upgrade_test.go to the new tag, and delete the old file).
#   docker buildx build --target dbfixture --output type=local,dest=. .
FROM base AS dbfixture-gen
ARG RELEASE=v0.12.1
RUN git clone --depth 1 --branch ${RELEASE} https://github.com/fuck-you-isp/fyisp /rel
COPY internal/store/testdata/genfixture/main.go /rel/cmd/genfixture/
WORKDIR /rel
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go run ./cmd/genfixture -out /gen && mkdir /out && cp /gen/fyisp.db /out/${RELEASE}.db && ls -l /out
FROM scratch AS dbfixture
COPY --from=dbfixture-gen /out/ /internal/store/testdata/

# Live tunnel tests (need network; they open quick tunnels on
# trycloudflare.com, which rate-limits them: run by hand or through the
# manual "live tunnel" workflow). See internal/tunnel/live_test.go.
#   docker build --target livetest -t fyisp-livetest .
#   docker run --rm -e FYISP_LIVE_TUNNEL=1 fyisp-livetest -test.run Live -test.v
FROM source AS livetest-build
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=1 go test -c -race -o /tunnel.test ./internal/tunnel
FROM debian:stable-slim AS livetest
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates iptables && rm -rf /var/lib/apt/lists/*
COPY --from=livetest-build /tunnel.test /tunnel.test
ENTRYPOINT ["/tunnel.test"]

# License texts (LICENSE*, COPYING*, NOTICE*) of every module linked into
# fyisp on any release platform, for the release assets and the image.
FROM source AS licenses
RUN --mount=type=cache,target=/go/pkg/mod <<'SH'
set -eu
for os in linux darwin windows; do
  GOOS=$os go list -deps -f '{{with .Module}}{{if not .Main}}{{.Path}} {{.Version}}{{with .Replace}}(replaced-by:{{.Path}}@{{.Version}}){{end}} {{.Dir}}{{end}}{{end}}' ./cmd/fyisp
done | sort -u > /tmp/mods
out=/THIRD_PARTY_LICENSES.txt
printf 'fyisp includes the following third-party software: the Go modules linked into\nit, the Go standard library and runtime, and uPlot (vendored in the web UI).\nTheir license and notice files follow. See NOTICE for a summary.\n\n' > $out
while read -r path ver dir; do
  printf '================================================================================\n%s %s\n================================================================================\n' "$path" "$ver" >> $out
  found=0
  for f in $(find "$dir" -maxdepth 1 -type f \( -iname 'LICEN[CS]E*' -o -iname 'COPYING*' -o -iname 'NOTICE*' \) | sort); do
    printf -- '--- %s\n' "$(basename "$f")" >> $out; cat "$f" >> $out; printf '\n' >> $out; found=1
  done
  [ $found = 1 ] || { echo "no license file for $path in $dir" >&2; exit 1; }
done < /tmp/mods
printf '================================================================================\nGo standard library and runtime %s\n================================================================================\n--- LICENSE\n' "$(go env GOVERSION)" >> $out
cat "$(go env GOROOT)/LICENSE" >> $out; printf '\n' >> $out
printf '================================================================================\nuPlot (internal/web/static/vendor)\n================================================================================\n--- uPlot.LICENSE\n' >> $out
cat internal/web/static/vendor/uPlot.LICENSE >> $out; printf '\n' >> $out
wc -l /tmp/mods $out
SH

# Cross-compile the release matrix. VERSION is stamped into the binary.
FROM source AS build-all
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build <<'SH'
set -eu
mkdir -p /out
for t in linux/amd64 linux/arm64 linux/arm/7 windows/amd64 windows/arm64 darwin/amd64 darwin/arm64; do
  os=${t%%/*}; rest=${t#*/}; arch=${rest%%/*}; arm=""; [ "$arch" = arm ] && arm=${rest#*/}
  ext=""; [ "$os" = windows ] && ext=.exe
  name=fyisp-$os-$arch${arm:+v$arm}$ext
  GOOS=$os GOARCH=$arch GOARM=$arm go build -ldflags "-s -w -X main.version=$VERSION" -o /out/$name ./cmd/fyisp
done
cd /out && sha256sum fyisp-* > SHA256SUMS && ls -l
SH
FROM scratch AS dist
COPY --from=build-all /out/ /
COPY --from=licenses /THIRD_PARTY_LICENSES.txt /

# Launcher tests: scripts/run.sh (dash, bash, piped) and scripts/run.ps1
# (PowerShell 7) download a fake release from 127.0.0.1; a good checksum must
# run `fyisp --version`, a tampered binary must fail without running.
#   docker build --target launchertest .
#   docker build --target launchertest-pwsh .
FROM source AS launcher-bin
ARG TARGETOS TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build <<'SH'
set -eu
arm=""; name=fyisp-$TARGETOS-$TARGETARCH
if [ "$TARGETARCH" = arm ]; then arm=7; name=fyisp-$TARGETOS-armv7; fi
GOOS=$TARGETOS GOARCH=$TARGETARCH GOARM=$arm go build -ldflags "-s -w -X main.version=launcher-test" -o /lt/fyisp ./cmd/fyisp
echo "$name" > /lt/asset
SH
FROM debian:stable-slim AS launchertest
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl python3 bash && rm -rf /var/lib/apt/lists/*
COPY --from=launcher-bin /lt/ /lt/
COPY scripts/ /lt/scripts/
RUN sh /lt/scripts/launcher_test.sh /lt/scripts /lt/fyisp "$(cat /lt/asset)" sh bash

FROM mcr.microsoft.com/powershell:latest@sha256:810c4f1e0c9d23022c3ec18c50a6205ee4b60766f1739d329b2948df1fd7d5b0 AS launchertest-pwsh
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl python3 && rm -rf /var/lib/apt/lists/*
COPY --from=launcher-bin /lt/ /lt/
COPY scripts/ /lt/scripts/
RUN sh /lt/scripts/launcher_test.sh /lt/scripts /lt/fyisp "$(cat /lt/asset)" pwsh

# systemd unit lint: docker build --target servicetest .
FROM debian:stable-slim AS servicetest
RUN apt-get update && apt-get install -y --no-install-recommends systemd && rm -rf /var/lib/apt/lists/*
COPY --from=launcher-bin /lt/fyisp /usr/local/bin/fyisp
COPY packaging/fyisp.service /etc/systemd/system/fyisp.service
RUN out=$(systemd-analyze verify /etc/systemd/system/fyisp.service 2>&1); echo "$out"; \
    ! echo "$out" | grep -v -e 'Failed to .* bus' -e 'System has not been booted' -e '^$' | grep . && \
    ! grep -n '^ProcSubset=' /etc/systemd/system/fyisp.service

# UI tests: Playwright against fyisp-webdev (fake probes, real API), all in
# one container, so it works with Docker-outside-of-Docker. Separate from
# `test` because the browser image is ~2.8 GB.
#   docker buildx build --target uitest .                                     # pass/fail
#   docker buildx build --target uitest-out --output type=local,dest=out .     # + out/shots, out/report
#   docker buildx build --target uitest-report --output type=local,dest=out .  # report even when tests fail
#   docker buildx build --target uilock --output type=local,dest=. .           # regenerate test/ui/package-lock.json
# Unchanged inputs reuse the cached run; add --no-cache-filter uitest-run to run the tests again.

FROM source AS webdev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -o /fyisp-webdev ./cmd/fyisp-webdev

FROM ${PLAYWRIGHT_IMAGE} AS uilock-gen
WORKDIR /ui
COPY test/ui/package.json ./
RUN npm install --package-lock-only --ignore-scripts --no-audit --no-fund
FROM scratch AS uilock
COPY --from=uilock-gen /ui/package-lock.json /test/ui/

FROM ${PLAYWRIGHT_IMAGE} AS uitest-run
WORKDIR /ui
COPY test/ui/package.json test/ui/package-lock.json ./
RUN npm ci --ignore-scripts --no-audit --no-fund
COPY test/ui/ ./
COPY internal/web/testdata/home/ /gold/
RUN node fixtures/gen.js /gold fixtures
COPY --from=webdev /fyisp-webdev /usr/local/bin/fyisp-webdev
# Records the result instead of failing, so uitest-report can still export it.
RUN <<'SH'
set -u
mkdir -p out
fyisp-webdev -listen 127.0.0.1:3000 -verdict ok > out/webdev.log 2>&1 &
ok=0
for i in $(seq 1 100); do
  node -e "fetch('http://127.0.0.1:3000/').then(r=>process.exit(r.ok?0:1),()=>process.exit(1))" && { ok=1; break; }
  sleep 0.2
done
if [ $ok = 1 ]; then npx playwright test; echo $? > out/status; else echo "webdev did not start"; echo 1 > out/status; fi
cat out/webdev.log | tail -20
kill $! 2>/dev/null
exit 0
SH
FROM uitest-run AS uitest
RUN s=$(cat out/status); [ "$s" = 0 ] || { echo "UI tests failed ($s): export --target uitest-report for the report"; exit 1; }
FROM scratch AS uitest-out
COPY --from=uitest /ui/out/ /
FROM scratch AS uitest-report
COPY --from=uitest-run /ui/out/ /

# Runtime image: one static binary, non-root.
FROM source AS build-image
ARG TARGETOS TARGETARCH TARGETVARIANT VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOARM=$(case "$TARGETVARIANT" in v6) echo 6;; v7|v8) echo 7;; *) echo "";; esac) \
    GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags "-s -w -X main.version=$VERSION" -o /fyisp ./cmd/fyisp \
 && mkdir -p /rootfs && mkdir -m 0700 /rootfs/data && mkdir -m 1777 /rootfs/tmp

FROM scratch AS runtime
COPY --from=build-image /fyisp /fyisp
# /data must exist in the image, private to the runtime user: Docker copies its
# owner and mode into new named volumes, otherwise they are root-owned and
# unwritable. scratch has no /tmp, and --ephemeral keeps its data in
# os.TempDir(). Both are copied via their parent: COPY sets a copied
# directory's own mode to 755, and --chmod drops the sticky bit.
COPY --from=build-image --chown=65532:65532 /rootfs/ /
COPY LICENSE NOTICE /usr/share/doc/fyisp/
COPY --from=licenses /THIRD_PARTY_LICENSES.txt /usr/share/doc/fyisp/
USER 65532:65532
VOLUME /data
EXPOSE 3000
STOPSIGNAL SIGTERM
# Defaults as environment variables (flags win), so subcommands work too:
# docker run IMG version | profiles | export | paths.
# container=oci tells fyisp it runs in a container (netinfo.InContainer),
# also where the runtime creates no /.dockerenv.
ENV FYISP_DATA_DIR=/data FYISP_LISTEN=0.0.0.0:3000 container=oci
ENTRYPOINT ["/fyisp"]

# Runtime image smoke test: /data is 0700 and /tmp 1777, both owned by the
# runtime user; `docker run IMG --ephemeral` must start (the API answers with
# $FYISP_ADMIN_TOKEN, and only with it), and exit 0 on SIGTERM with its temporary directory removed. The
# image's root filesystem is run with chroot as the runtime user, because a
# build step cannot start a container.
#   docker build --target imagetest .
FROM debian:stable-slim AS imagetest
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl && rm -rf /var/lib/apt/lists/*
COPY --from=runtime / /img/
RUN <<'SH'
set -eu
stat -c '%a %u:%g %n' /img/tmp /img/data
[ "$(stat -c '%a %u:%g' /img/tmp)" = "1777 65532:65532" ]
[ "$(stat -c '%a %u:%g' /img/data)" = "700 65532:65532" ]
tok=0123456789abcdef0123456789abcdef
env -i FYISP_DATA_DIR=/data FYISP_LISTEN=127.0.0.1:3000 FYISP_ADMIN_TOKEN=$tok container=oci \
  /usr/sbin/chroot --userspec=65532:65532 /img /fyisp --ephemeral > /fyisp.log 2>&1 &
pid=$!
ok=0
for i in $(seq 1 100); do
  curl -fsS -H "Authorization: Bearer $tok" -o /status.json http://127.0.0.1:3000/api/status && { ok=1; break; }
  kill -0 $pid 2>/dev/null || break
  sleep 0.2
done
if [ $ok = 1 ]; then
  code=$(curl -sS -o /dev/null -w '%{http_code}' http://127.0.0.1:3000/api/status || :)
  [ "$code" = 401 ] || { echo "/api/status without the admin token: $code, want 401"; ok=2; }
  kill -TERM $pid
else kill -KILL $pid 2>/dev/null || :; fi
rc=0; wait $pid || rc=$?
[ $ok != 0 ] || { cat /fyisp.log; echo "fyisp --ephemeral did not start"; exit 1; }
[ $ok = 1 ] || exit 1
[ $rc = 0 ] || { cat /fyisp.log; echo "fyisp exited with $rc"; exit 1; }
[ -z "$(ls -A /img/tmp)" ] || { ls -la /img/tmp; echo "temporary directory left behind"; exit 1; }
echo "imagetest ok: $(head -c 200 /status.json)"
SH

# The runtime image, last so it is the default target.
FROM runtime AS image
