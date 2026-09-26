# syntax=docker/dockerfile:1.7
# All building and testing goes through this file: no local Go toolchain needed.
#   docker build --target test .                                   # vet + tests
#   docker buildx build --target dist --output type=local,dest=dist .   # all release binaries
#   docker buildx build --target modfiles --output type=local,dest=. .  # go mod tidy -> go.mod/go.sum
#   docker buildx build --target fmt --output type=local,dest=. .       # gofmt -w all Go files
#   docker build -t fyisp .                                         # runtime image (default target)

ARG GO_IMAGE=golang:1.27.1-bookworm@sha256:69a7b9788769bec032d238959b61854e9ae87f57be9029ec04e9885fabf99195

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
RUN gofmt -w $(find . -name '*.go' -not -path './dist/*') && mkdir /out && find . -name '*.go' | tar cf - -T - | tar xf - -C /out
FROM scratch AS fmt
COPY --from=gofmt /out/ /

FROM base AS deps
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download && go mod verify

FROM deps AS source
COPY . .

FROM source AS test
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    test -z "$(gofmt -l . | tee /dev/stderr)" && go vet ./... && CGO_ENABLED=1 go test -race ./...

# Live tunnel tests (need network; they hit trycloudflare.com). See internal/tunnel/live_test.go.
#   docker build --target livetest -t fyisp-livetest .
#   docker run --rm -e FYISP_LIVE_TUNNEL=1 fyisp-livetest -test.run Live -test.v
FROM source AS livetest-build
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=1 go test -c -race -o /tunnel.test ./internal/tunnel
FROM debian:bookworm-slim AS livetest
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
printf 'fyisp includes the following third-party Go modules. Their license and notice\nfiles follow. See NOTICE for a summary.\n\n' > $out
while read -r path ver dir; do
  printf '================================================================================\n%s %s\n================================================================================\n' "$path" "$ver" >> $out
  found=0
  for f in $(find "$dir" -maxdepth 1 -type f \( -iname 'LICEN[CS]E*' -o -iname 'COPYING*' -o -iname 'NOTICE*' \) | sort); do
    printf -- '--- %s\n' "$(basename "$f")" >> $out; cat "$f" >> $out; printf '\n' >> $out; found=1
  done
  [ $found = 1 ] || { echo "no license file for $path in $dir" >&2; exit 1; }
done < /tmp/mods
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
ARG TARGETOS TARGETARCH TARGETVARIANT
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
    ! echo "$out" | grep -v -e 'Failed to .* bus' -e 'System has not been booted' -e '^$' | grep .

# Runtime image: one static binary, non-root.
FROM source AS build-image
ARG TARGETOS TARGETARCH TARGETVARIANT VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOARM=$(case "$TARGETVARIANT" in v6) echo 6;; v7|v8) echo 7;; *) echo "";; esac) \
    GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags "-s -w -X main.version=$VERSION" -o /fyisp ./cmd/fyisp \
 && mkdir -m 0700 /data

FROM scratch AS image
COPY --from=build-image /fyisp /fyisp
# /data must exist in the image, owned by the runtime user: Docker copies its
# ownership into new named volumes, otherwise they are root-owned and unwritable.
COPY --from=build-image --chown=65532:65532 /data /data
COPY LICENSE NOTICE /usr/share/doc/fyisp/
COPY --from=licenses /THIRD_PARTY_LICENSES.txt /usr/share/doc/fyisp/
USER 65532:65532
VOLUME /data
EXPOSE 3000
STOPSIGNAL SIGTERM
ENTRYPOINT ["/fyisp", "--data-dir=/data", "--listen=0.0.0.0:3000"]
