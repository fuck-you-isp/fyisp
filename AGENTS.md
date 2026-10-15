# AGENTS.md

How to work on fyisp, for people and coding agents. User docs are in [README.md](README.md). Rules for adding endpoints are in [CONTRIBUTING.md](CONTRIBUTING.md). Security reports go to [SECURITY.md](SECURITY.md).

fyisp is one static Go binary (`./cmd/fyisp`). It probes services with TCP connects, STUN and ICMP, keeps up to 90 days in one SQLite file, and serves a dashboard and a JSON API on 127.0.0.1:3000.

## Repo map

| Path | What it is |
|---|---|
| `cmd/fyisp` | The binary: flags, wiring, subcommands (`profiles`, `export`, `paths`, `version`) |
| `cmd/fyisp-webdev` | Dev server: the real web UI and API over fake probes and an in-memory store. Never released |
| `internal/model` | Types shared by every package |
| `internal/profile` | Target catalog (`catalog/*.yml`), named profiles (`profiles.yml`), the default profile (`default.yml`), Home tiles (`tiles.go`), `--config` loading |
| `internal/probe` | TCP, HTTPS, ICMP and STUN probes, failure-reason codes, the scheduler, `Fake` |
| `internal/netinfo` | Gateway and ISP-edge discovery, Wi-Fi or wired, in-container detection (per-OS files) |
| `internal/netinfo/hops` | Unprivileged TTL-limited ICMP echo, used by netinfo and trace |
| `internal/trace` | Always-on and on-demand (Investigate) traceroutes, routes, route changes |
| `internal/asn` | Built-in IPv4 → AS table (`data/ip2asn-v4.bin`), built by `internal/asn/cmd/asngen` |
| `internal/store` | SQLite store: hourly zstd blobs, summaries, incidents, routes, speed tests, `Fake` |
| `internal/store/blob` | Encodes one series' hour as a compact blob |
| `internal/store/synth` | Deterministic synthetic series for store tests and benchmarks |
| `internal/baseline` | Each series' "normal", and "worse than usual?" |
| `internal/verdict` | Whose fault it is (LAN, ISP, upstream, DNS, service, UDP), plus the incident log |
| `internal/tiles` | Judges the Home tiles minute by minute (Good, Slow/Choppy, Down) and finds problems |
| `internal/callq` | Call-quality score (E-model R → MOS → band) from STUN bursts |
| `internal/speedtest` | Pure-Go iperf3 client, scheduler, queue, findings. Servers are in `internal/profile/catalog/iperf3/` |
| `internal/metrics` | Prometheus `/metrics` |
| `internal/export` | `fyisp export` (CSV of raw samples, `--speedtests`) |
| `internal/web` | HTTP handler, JSON API, embedded UI (`static/`), goldens (`testdata/`) |
| `internal/web/homescen` | Hand-written Home scenarios used by the goldens and `fyisp-webdev -home` |
| `test/netns` | Network-fault harness (Linux, sudo, build tag `netns`) |
| `test/ui` | Playwright specs and fixtures for the UI |
| `scripts/` | `run.sh` and `run.ps1` launchers, and their test |
| `packaging/` | systemd unit |
| `docs/` | API docs (`HOME-API`, `SPEEDTEST-API`, `TRACE-API`) and operator docs (`OPERATORS`, `IPERF3-TERMS`, `STUN-TERMS`) |

## Build and test: Docker only

Don't install Go or Node on the host. Everything runs through the [Dockerfile](Dockerfile), and each stage's comment documents its command.

| Task | Command |
|---|---|
| gofmt check, vet (incl. darwin and windows netinfo, the netns harness), `go test -race ./...` | `docker build --target test .` |
| UI tests (Playwright against fyisp-webdev), with screenshots and report | `docker buildx build --target uitest-out --output type=local,dest=out --no-cache-filter uitest-run .` |
| UI tests, report even on failure (what CI runs) | `docker buildx build --target uitest-report --output type=local,dest=out .`, then `cat out/status` (0 = pass) |
| Launcher tests | `docker build --target launchertest .` and `docker build --target launchertest-pwsh .` |
| systemd unit lint | `docker build --target servicetest .` |
| Runtime image smoke test | `docker build --target imagetest .` |
| Release binaries, SHA256SUMS, third-party licenses | `docker buildx build --target dist --output type=local,dest=dist .` |
| Runtime image | `docker build -t fyisp .` |
| `go mod tidy` → go.mod and go.sum | `docker buildx build --target modfiles --output type=local,dest=. .` |
| `gofmt -w` | `docker buildx build --target fmt --output type=local,dest=. .` |
| Regenerate the UI fixtures from the Home goldens | `docker buildx build --target uifixtures --output type=local,dest=. .` |
| Regenerate `test/ui/package-lock.json` | `docker buildx build --target uilock --output type=local,dest=. .` |
| Refresh the IP → AS table (downloads from iptoasn.com) | `docker buildx build --target asndb --output type=local,dest=. .` |
| Network-fault tests (**Linux, needs sudo**, ~20 min) | `test/netns/run.sh` (see below) |

`out/` is not in `.gitignore` or `.dockerignore`: delete it after a UI run.

**One Go test, or any go command.** Use the `source` stage. It needs network access, because modules sit in a build cache, not in the image.

```sh
docker build --target source -t fyisp-src .
docker run --rm fyisp-src go test ./internal/web -run TestHomeGolden -count=1 -v
docker run --rm fyisp-src sh -c 'CGO_ENABLED=1 go test -race ./internal/speedtest'
docker run --rm fyisp-src sh -c 'GOOS=windows go vet ./...'   # after touching *_windows.go
```

**Run a CLI command against your tree:** `docker build -t fyisp-dev . && docker run --rm fyisp-dev profiles aws`. The image's entrypoint is `/fyisp`.

**Update goldens.** `docker run -v` doesn't work everywhere (see Pitfalls), so copy the files out:

```sh
docker build --target source -t fyisp-src .
docker run --name fyisp-gold fyisp-src go test ./internal/web -run 'TestHomeGolden|TestSpeedGolden' -update
docker cp fyisp-gold:/src/internal/web/testdata ./internal/web/ && docker rm fyisp-gold
docker buildx build --target uifixtures --output type=local,dest=. .   # the UI fixtures come from the goldens
git diff internal/web/testdata test/ui/fixtures                        # review the copy changes
```

The goldens are where user-facing copy is reviewed, so read the diff. `internal/metrics/testdata/speed.golden` and `udp.golden` have no `-update` flag: edit them from the "got" output the test prints. `network_exporter.golden` was captured from the real network_exporter image and should not change.

**fyisp-webdev** serves the real UI and API over fake data, with no network access:

```sh
docker build --target webdev -t fyisp-webdev .
docker run --rm -p 3000:3000 fyisp-webdev /fyisp-webdev -listen 0.0.0.0:3000 -home blip
```

Useful flags: `-home <scenario>` (names in `homescen.Names`), `-verdict isp|lan|...`, `-calls poor|blocked`, `-profile all -backfill 90m`, `-admin-token x`, `-no-speed`, `-no-trace`. Open it as `localhost` or an IP address: any other Host name gets 421 (see Pitfalls).

**Network-fault tests** build in Docker, then use sudo for throwaway network namespaces. They run nightly in CI (`netns.yml`), not per PR.

```sh
test/netns/run.sh                                     # everything, in parallel
test/netns/run.sh -test.run 'NetworkFaults/a-baseline'  # one scenario (args go to the test binary)
FYISP_NETNS_OUT=out test/netns/run.sh                 # keep API responses and logs per scenario
FYISP_NETNS_PARALLEL=0 test/netns/run.sh              # one scenario at a time (~3 h)
```

## Data model and store

- **One schema, no migrations.** `internal/store/sqlite.go` has the full DDL in `schema` and the version in `SchemaVersion()` (currently 8).
- **Bump the version on any DDL change.** At the same time, update `tables` (sqlite.go) and `wantSchema` in `schema_test.go`.
- **An older database is replaced** with an empty one on first start, and this is logged. A newer one is refused (`ErrNewerSchema`) and left untouched. There are no upgrade paths, compat shims or backups. Mention the reset in the release notes.
- Samples are hourly zstd blobs per (hour, series), with WAL. The current hour is rewritten every minute (`Flush`) and data is pruned at 90 days.
- A target's **name is its identity** in the store (catalog targets, iperf3 server ids, region ids). Renaming one starts a new history, so never rename.

## Catalog, profiles and speed-test servers

- **TCP and STUN targets:** `internal/profile/catalog/<provider>.yml`. The format is in [SCHEMA.md](internal/profile/catalog/SCHEMA.md), and the rules for choosing endpoints are in [CONTRIBUTING.md](CONTRIBUTING.md#adding-or-fixing-endpoints).
  - Guarded by `TestEmbeddedCatalog`, `TestCatalogErrors`, `TestTilesCatalog` (every `tile:<id>` target must be listed in `profile.Tiles`), `TestShippedUDP` and `TestShippedProfilesTCPOnly`.
  - A new STUN host also needs a reviewed entry in `docs/STUN-TERMS.md`.
- **Named profiles:** `internal/profile/profiles.yml`. Every provider file is also a profile automatically. Guarded by `TestBuiltinProfiles`.
- **Default profile:** `internal/profile/default.yml`, guarded by `TestDefaultMatchesCatalog`. Changing it changes the Home goldens, so regenerate them.
- **iperf3 servers:** `internal/profile/catalog/iperf3/<provider>.yml` (format in the comment at the top of `internal/speedtest/catalog.go`). Regions and fallback chains are in `regions.yml`. To add a server:
  1. Add the server with `sources`, `metro`, `geo`, a public address, a unique `id` and `verified.date` (`TestCatalogErrors` in `internal/speedtest`).
  2. Put it in a region in `regions.yml`: same-metro servers under `servers`, others under `nearby` or `others`.
  3. Add its metro to `metroZones` (`catalog.go`) if the metro is new.
  4. Update the server count and expectations in `TestBuiltinCatalog`.
  5. Update the operator table in `docs/IPERF3-TERMS.md`.

## Web UI

- **No build step.** `internal/web/static/*.js` are plain ES modules, type-checked with `// @ts-check` JSDoc and embedded with `go:embed`. The only vendored library is uPlot (`static/vendor`).
- **CSP** (`api.go`) has `script-src 'self'; style-src 'self'`: no inline scripts, styles or handlers, and no `innerHTML` or other HTML string sinks.
- **Build the DOM with `h(tag, props, ...kids)` from `core.js`.** Set styles with `el.style.setProperty` and handlers with `on*` props. `TestStaticJSNoHTMLSinks`, `TestStaticIndexNoInline` and the Playwright `home-csp` spec enforce this.
- **Use the `core.js` helpers** instead of writing new ones: `fmtMs`, `fmtMbps`, `fmtDur`, `fmtWhen`; `fetchJSON` (retries) and `fetchShared`; `apiWrite` (CSRF and admin token); `showTip`, `whyButton`, `dialog`; `css()` for theme values.
- An API change also updates `docs/*-API.md`.

## Conventions

- **Commits:** Conventional Commits, lowercase, with a scope where it helps: `feat(web)!: ...`, `fix(ui): ...`, `refactor(store)!: ...`, `docs(readme): ...`, `test(netns): ...`, `chore: ...`. `!` marks a breaking change. The body says why.
- **Branches:** `feat/...`, `fix/...`, `docs/...`, `cleanup/...`.
- **PRs:** keep them focused; they are merged with merge commits (not squash). In the PR body, use bullets with a bold lead per area. Open an issue before large changes.
- **CI** (`.github/workflows/ci.yml`) has three jobs:
  - **docker:** `test`, a clean `modfiles` diff, `dist`, the launcher, servicetest and imagetest targets, arm64 and arm/v7 runtime images, and the 50 MB binary budget.
  - **ui:** Playwright, only when `internal/`, `test/ui/`, webdev, go.mod or the Dockerfile change.
  - **native:** `go vet` and `go test` on Windows, macOS and macOS Intel.
- **Known flakes:** #37 (timing tests on `macos-15-intel`) and #43 (netns `TestHome10/S2b-provider-lossy`). Rerun the job once. Write new timing tests with `testing/synctest`, not sleeps.
- **Releases:** push a `v*` tag, and `release.yml` tests, cross-builds, attests and publishes the GitHub release and `ghcr.io/fuck-you-isp/fyisp:<tag>` (and `:latest` unless the tag has a `-`). Then put hand-written notes above the generated install block: `# fyisp vX.Y.Z: <theme>`, a 1-2 line summary, a database note if the schema changed, and `## Added`, `## Fixed`, `## Removed`, `## Docs` sections with bold-lead bullets written for users.

## Project policies

- **Roll forward.** No migrations, upgrade paths, compat shims or deprecation periods. Remove dead code and API fields when nothing reads them.
- **Docs describe current behaviour** only, with no version history ("since vX"). Check every statement against the code.
- **No telemetry, ever.** fyisp talks only to the targets it checks, the network path it traces, the user's DNS resolver, and the iperf3 servers when speed tests are on. A new outbound connection must be documented in README "Privacy" and SECURITY.md.
- **Privacy:** never store, log or serve the user's public IP address (`cmd/fyisp/privacy_test.go`). No third-party lookups: ASNs come from the built-in table.
- **Be kind to operators.** TCP targets get a connect and close, with no data sent. STUN targets get Binding requests only, from documented public servers. iperf3 runs one test at a time and backs off from busy servers. Raising any frequency or volume needs an update to `docs/OPERATORS.md`, `IPERF3-TERMS.md` or `STUN-TERMS.md`, and the owner's sign-off.

## Pitfalls

- **Docker-outside-of-Docker** (devcontainers, some CI runners): `docker run -v $PWD:/src` mounts a path on the *Docker host*, not your checkout. Use `--output type=local` stages or `docker cp` instead. Give the containers and images you create unique names, and remove them when you're done.
- **The order of the modernc SQLite pragmas.** `_pragma` DSN values run in *lexicographic* order, so a `page_size` in the DSN is silently ignored after `journal_mode=WAL`. Persistent pragmas are set explicitly in `build` and `openConns` (`sqlite.go`), so keep them there.
- **Build tags.** `test/netns` is `//go:build netns && linux`, so plain `go test ./...` skips it. There are `_windows`, `_darwin`, `_linux`, `_unix` and `_other` files in `netinfo`, `probe`, `store`, `speedtest` and `cmd/fyisp`. The `test` target vets only `netinfo` for darwin and windows: run `GOOS=windows go vet ./...` (above) or `dist`, or wait for the native CI.
- **Host allowlist.** The handler answers 421 to any Host other than `localhost`, `127.0.0.1`, `::1` or the bound IP (plus IP literals when bound beyond loopback). Real deployments add names with `--allow-host`, which `fyisp-webdev` does not have. Tests must set `r.Host`.
- **Goldens.** A change to Home copy, tiles, the default profile or `/api/speed` fails `TestHomeGolden` or `TestSpeedGolden`. Regenerate the goldens, then `uifixtures`. Nothing checks that the fixtures match the goldens, so stale fixtures pass silently.
