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

# Runtime image: one static binary, non-root.
FROM source AS build-image
ARG TARGETOS TARGETARCH TARGETVARIANT VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOARM=$(case "$TARGETVARIANT" in v6) echo 6;; v7|v8) echo 7;; *) echo "";; esac) \
    GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags "-s -w -X main.version=$VERSION" -o /fyisp ./cmd/fyisp

FROM scratch AS image
COPY --from=build-image /fyisp /fyisp
USER 65532:65532
VOLUME /data
EXPOSE 3000
STOPSIGNAL SIGTERM
ENTRYPOINT ["/fyisp", "--data-dir=/data", "--listen=0.0.0.0:3000"]
