# AGENTS.md

How to work on fyisp, for people and coding agents. User docs are in [README.md](README.md) and [docs/](docs/) (the API index is [docs/API.md](docs/API.md)), the rules for adding endpoints in [CONTRIBUTING.md](CONTRIBUTING.md), and security reports go to [SECURITY.md](SECURITY.md).

fyisp is one static Go binary (`./cmd/fyisp`). It probes services with TCP connects, STUN and ICMP, keeps up to 90 days in one SQLite file, and serves a dashboard and a JSON API on 127.0.0.1:3000.

## Repo map

| Path | What it is |
|---|---|
| `cmd/fyisp` | The binary: flags, wiring, subcommands (`profiles`, `export`, `paths`, `version`) |
| `cmd/fyisp-webdev` | Dev server: the real UI and API over fake probes. Never released |
| `internal/profile` | Target catalog (`catalog/*.yml`), named profiles, `default.yml`, Home tiles, `--config` |
| `internal/probe`, `netinfo`, `trace` | Probes, gateway and ISP-edge discovery, traceroutes |
| `internal/store` | SQLite store (hourly zstd blobs, summaries, incidents, routes, speed tests) |
| `internal/verdict`, `tiles`, `callq`, `baseline` | Whose fault it is, Home tiles, call quality, "normal" for each series |
| `internal/speedtest` | Pure-Go iperf3 client and scheduler. Servers are in `internal/profile/catalog/iperf3/` |
| `internal/web` | HTTP handler, JSON API, embedded UI (`static/`), goldens (`testdata/`) |
| `internal/metrics`, `export`, `asn` | `/metrics`, `fyisp export`, the built-in IP → AS table |
| `test/netns`, `test/ui` | Network-fault harness (Linux, sudo), Playwright specs and fixtures |
| `scripts/`, `packaging/`, `docs/` | Launchers, the systemd unit, user, API and operator docs |

## Build and test: Docker only

Don't install Go or Node on the host. Everything runs through the [Dockerfile](Dockerfile).

| Task | Command |
|---|---|
| gofmt check, vet, `go test -race ./...` | `docker build --target test .` |
| UI tests (Playwright), with screenshots and report | `docker buildx build --target uitest-out --output type=local,dest=out --no-cache-filter uitest-run .` |
| Launcher tests | `docker build --target launchertest .` and `docker build --target launchertest-pwsh .` |
| systemd unit: `systemd-analyze verify`, and fails if `ProcSubset=` is set | `docker build --target servicetest .` |
| Runtime image smoke test | `docker build --target imagetest .` |
| Release binaries and SHA256SUMS | `docker buildx build --target dist --output type=local,dest=dist .` |
| Runtime image | `docker build -t fyisp .` |
| `go mod tidy` | `docker buildx build --target modfiles --output type=local,dest=. .` |
| `gofmt -w` | `docker buildx build --target fmt --output type=local,dest=. .` |
| Regenerate the UI fixtures from the Home goldens | `docker buildx build --target uifixtures --output type=local,dest=. .` |
| Regenerate the last release's database fixture (after tagging a release) | `docker buildx build --target dbfixture --output type=local,dest=. .` |
| Network-fault tests (**Linux, needs sudo**, ~20 min) | `test/netns/run.sh` (one scenario: `test/netns/run.sh -test.run 'NetworkFaults/a-baseline'`) |

The Dockerfile's stage comments list the rest (`uitest-report`, `uilock`, `asndb`). `out/` is not git-ignored, so delete it after a UI run.

**One Go test, or any go command:** use the `source` stage. It needs network access, because modules are not in the image.

```sh
docker build --target source -t fyisp-src .
docker run --rm fyisp-src go test ./internal/web -run TestHomeGolden -count=1
docker run --rm fyisp-src sh -c 'GOOS=windows go vet ./...'   # after touching *_windows.go
```

**Run a CLI command against your tree:** `docker build -t fyisp-dev . && docker run --rm fyisp-dev profiles aws`.

**Update goldens.** `docker run -v` doesn't work everywhere (see Pitfalls), so copy the files out:

```sh
docker run --name fyisp-gold fyisp-src go test ./internal/web -run 'TestHomeGolden|TestSpeedGolden' -update
docker cp fyisp-gold:/src/internal/web/testdata ./internal/web/ && docker rm fyisp-gold
docker buildx build --target uifixtures --output type=local,dest=. .   # the UI fixtures come from the goldens
git diff internal/web/testdata test/ui/fixtures                        # review the copy changes
```

**fyisp-webdev** serves the real UI over fake data. Open it as `localhost`, because any other Host name gets 421.

```sh
docker build --target webdev -t fyisp-webdev .
docker run --rm -p 3000:3000 fyisp-webdev /fyisp-webdev -listen 0.0.0.0:3000 -home blip
```

## Rules that tests enforce

- **Store schema:** `internal/store/sqlite.go` holds the full DDL, `SchemaVersion()` and `migrations`. Changes are additive only: new tables, columns and indexes; never drop, rename or retype anything, and keep reading the stored blob formats. A DDL change goes into `schema`, bumps `schemaVersion`, adds the step from the previous version to `migrations` (one transaction upgrades the file in place), and updates `tables` and `wantSchema` (`schema_test.go`). `internal/store/testdata/v0.11.0.db` is a database written by the last release (the `dbfixture` stage); `upgrade_test.go` checks that it opens with all of its rows, that every table, column and index in it survives with the same type, and that `migrations` cover every step from its schema. If a step is missing, fyisp refuses to start and leaves the file alone. A database from before v0.11.0 (schema older than 8) is replaced with an empty one, and a newer one is refused.
- **Names are identities.** Catalog target names, iperf3 server ids and region ids key the stored history. Never rename them.
- **Catalog:** the format is in [SCHEMA.md](internal/profile/catalog/SCHEMA.md). `docker build --target test .` runs all the catalog checks. Changing `default.yml` or a `tile:` tag changes the Home goldens.
- **iperf3 servers:** add the server to its provider file and to a region in `regions.yml`. Add a new metro to `metroZones` (`internal/speedtest/catalog.go`), and update the count in `TestBuiltinCatalog` and the table in `docs/IPERF3-TERMS.md`.
- **Web UI:** plain ES modules, no build step, and a strict CSP. That means no inline scripts, styles or handlers, and no `innerHTML`. Build the DOM with `h()` from `core.js` and reuse its helpers (`fmtMs`, `fetchJSON`, `apiWrite`, ...). An API change also updates `docs/API.md` and `docs/*-API.md`.

## Conventions

- **Commits:** Conventional Commits, lowercase, e.g. `feat(web)!: ...`, `fix(ui): ...`, `docs(readme): ...`. `!` marks a breaking change. The body says why.
- **PRs:** keep them focused. They are merged with merge commits. Open an issue before large changes.
- **CI** runs the Docker targets above, the UI tests when the UI changes, and native `go test` on Windows and macOS. The netns suite runs nightly. Known flakes: #37 (macOS Intel timing) and #43 (netns `S2b-provider-lossy`); rerun them once. Write timing tests with `testing/synctest`, not sleeps.
- **Releases:** pushing a `v*` tag runs `release.yml`. After tagging, set the new tag in the `dbfixture` stage (`RELEASE`) and in `lastRelease` (`internal/store/upgrade_test.go`), regenerate the fixture with `dbfixture`, delete the old one and commit.

## Project policies

- **Roll forward,** with additive migrations from the last release's schema. No compat shims or deprecation periods. Remove dead code. fyisp makes no backups: users handle their own.
- **Docs describe current behaviour**, with no version history. Check every statement against the code.
- **No telemetry.** A new outbound connection must be documented in README "Privacy" and SECURITY.md.
- **Never store, log or serve the user's public IP address** (`cmd/fyisp/privacy_test.go`).
- **Be kind to operators.** Raising any probe frequency or volume needs an update to `docs/OPERATORS.md`, `IPERF3-TERMS.md` or `STUN-TERMS.md`.

## Pitfalls

- **Docker-outside-of-Docker** (devcontainers, some CI runners): `docker run -v $PWD:/src` mounts a path on the Docker host, not your checkout. Use `--output type=local` stages or `docker cp`.
- **Per-OS files** (`_windows.go`, `_darwin.go`, ...): `test` vets only `netinfo` for other OSes. Run `GOOS=windows go vet ./...` or rely on the native CI.
- **Host allowlist:** the handler answers 421 to unknown Host headers, so tests must set `r.Host`.
- **SQLite pragmas:** modernc runs `_pragma` DSN values in lexicographic order, so persistent pragmas are set in code (`sqlite.go`), not in the DSN.
- **Stale fixtures:** nothing checks that `test/ui/fixtures` match the goldens. Rerun `uifixtures` after updating goldens.
