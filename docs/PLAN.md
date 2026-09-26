# Plan: FYISP v0.1, a single Go binary (`fyisp`)

## Context
FYISP ("F*** You ISP") measures how well a home connection reaches hyperscalers and popular services, so people can prove their ISP is at fault. Today it runs as 4 containers (`docker-compose.yml`):
- `syepes/network_exporter` pinging 87 targets with ICMP every 3s
- Prometheus, keeping 2 days of data
- Grafana, showing 7 panels
- a cloudflared quick tunnel

That's 1.69 GB of images using 7–9% of a CPU core and 187–451 MiB of RAM (measured). It also has bugs:
- A failed ping is plotted as 0 ms, and loss is never shown.
- Only 1 in 5 pings is stored.
- Panel 1 excludes `AMZN.*` when the targets are named `AWS-*`.
- Hostnames are resolved only once, at startup.
- The Grafana admin login is exposed through the tunnel.

**Goal of v0.1:** replace the stack properly with one static binary. It needs no install, no admin rights and no Docker. It probes over HTTPS, TCP and ICMP, keeps every sample for 90 days in a local SQLite file, shows correct charts (gaps and loss coloured by failure reason), and can share a redacted public link on request. v0.2 and v0.3 build on it (see Roadmap).

**Decisions:**
- **Repo:** `fuck-you-isp/fyisp`, private while in development and **made public at the v0.1 release**. Local clone at `/workspaces/glueops/fyisp`.
- **Tunnel:** embed the cloudflared library. **Sharing is opt-in** (`--share` or a button in the local UI), and the public view is **redacted**.
- **Data:** a **persistent per-OS directory** by default; 90-day cap.
- **Testing:** passwordless sudo may be used **only** for throwaway network-namespace fault tests and to install `mtr-tiny`.
- **Privacy:** **no telemetry**. Traffic goes only to probe targets, the system DNS resolver, and Cloudflare when sharing is on.

## Roadmap
- **v0.1 (this plan):**
  - Probes: HTTPS, TCP and ICMP to the 87 targets.
  - Targets: the embedded `default` profile, plus a local file via `--config`.
  - Storage: SQLite, `fyisp export`, and `/metrics` compatible with network_exporter.
  - UI: 7 panels.
  - Sharing: an opt-in, redacted share link.
  - Distribution: the one-line launcher, a Docker image, and a systemd unit.
- **v0.2: blame.**
  - Layers: gateway, ISP edge, and anycast addresses (1.1.1.1 / 8.8.8.8 / 9.9.9.9).
  - A verdict banner and an outage log.
  - 7 always-on traceroutes and an Investigate view: per-hop table with ASN (Team Cymru; decide on-by-default in v0.2), per-hop timelines, and route-change detection.
  - "Now vs your normal" baseline comparison.
  - An evidence report and annotations.
  - A `https_conn` series recording connect/TLS time on new connections.
- **v0.3:**
  - A public, signed `fuck-you-isp/profiles` repo: fetched by the binary, live reload, `--profile`, `--profile-url`, `--profile-pin`.
  - An on-demand bufferbloat test.
  - Alerts.
- **Later:**
  - live CD (rebuilt around the binary)
  - browser quick-check page
  - macOS and Windows code signing
  - Azure targets
  - Android/Termux
  - fleet mode
  - ISP comparison

## Step 0: benchmarks (done 2026-09-25/26)
The code and results are in the session scratchpad (`bench-storage/`, `bench-storage-extra/`, `bench-probes/`).

**Probe prototype vs the current stack:**

| | Current stack | Prototype, ICMP-only | Prototype, full probe mix (87 targets) |
|---|---|---|---|
| CPU | 7–9% of a core | 1.2% | 1.7% |
| RAM | 187–451 MiB | 20 MiB | 46 MiB |

The prototype binary is 6.4 MB before the tunnel is added.

**Storage options compared:**
- Candidates were custom files, SQLite (rows and blobs), Pebble, Prometheus TSDB and DuckDB.
- **Chosen:** SQLite with hourly zstd blobs, measured at **1.19 bytes/sample live** and **1.10 after VACUUM**.
- **Rejected:**
  - One row per sample: 14–18 bytes/sample, and 191 GB/day of disk writes.
  - Prometheus TSDB: RAM peaks around 765 MB, and it adds 17.8 MB to the binary.
  - DuckDB: needs cgo, adds 58 MB, and uses 290–510 MB of RAM.
  - Pebble: 2–3× bigger, and adds 16 MB.

**Storage write pattern:**
- Rewriting the current hour every minute with a time-first primary key costs **0.33 GB/day of disk writes**. Keying by series first costs 13 GB/day.
- One query per panel takes **34 ms** for 90 series over 2 days.

**Probe findings:**
- **Unprivileged traceroute works** using a ping socket with `IP_TTL` + `IP_RECVERR` (v0.2).
- **Servers close idle connections:** 38% of HTTPS requests went out on a new connection (S3 closes idle connections after about 6s).
- **`HEAD` fails** on S3 and Hetzner.
- **github.com `/` returns 576 KB.**
- **`lens.l.google.com` doesn't answer on TCP port 443.**
- **Log noise:** http2 "UnknownFrame" messages.
- **Idle TLS connections** cost about 40–60 KB each.

## v0.1 specification

### Probes (`internal/probe`)
- **Per target:** HTTPS and TCP every 15s. ICMP runs every 5s, spread across the interval rather than sent in bursts. The schedule is phased so probes don't all fire at once.
- **HTTPS:**
  - A `GET` (never `HEAD`) to the target's path. The default is `/`; github uses `/robots.txt`.
  - One `http.Client` per target, with keep-alive.
  - **RTT = `httptrace` WroteRequest → GotFirstResponseByte on every request**, reused connection or not. `Reused` is recorded.
  - Nothing is discarded, and any HTTP status counts as a success.
  - http2 UnknownFrame logs are silenced.
- **TCP:** connect to port 443 with a 1s timeout, then `SetLinger(0)` so no socket lingers after closing.
- **ICMP:**
  - Linux/macOS: an unprivileged `udp4` socket, falling back to a raw socket. Replies are matched by payload (per-process marker, target index, counter).
  - Windows: `IcmpSendEcho2Ex` through `windows.NewLazySystemDLL("iphlpapi.dll")`. No cgo and no admin rights.
  - macOS: verify the IP header is stripped.
- **Failure reasons:** timeout, refused, reset, unreachable, DNS, TLS, HTTP, no-network, other.
- **Capabilities:** detected at startup and shown in the UI and log, e.g. "ICMP unavailable → TCP/HTTPS only (see `net.ipv4.ping_group_range`)". fyisp never elevates.
- **DNS:** each host is re-resolved every 15 minutes; unresolved hosts are retried every 10s. Series are identified by target name, not IP.
- **`Google-Meet`:** HTTPS/TCP go to `meet.google.com`, ICMP stays on `lens.l.google.com`. Verify during spike S4.

### Targets (`internal/profile`)
- The embedded `default.yml` holds the 87 targets and 7 groups. A test checks the names match `network_exporter.yml.template` exactly. The panel 1 filter bug is fixed.
- `--config file.yml` uses the same format:
  - `extends: [default]`, then add, remove or override targets by name.
  - Or a standalone profile.
  - Local files may target private IPs.
- Validation: names are unique, at most 300 targets, a minimum interval is enforced, and every group exists.

### Storage (`internal/store`, SQLite via `modernc.org/sqlite`)
- **Where data lives:** `--data-dir` defaults to:
  - `$XDG_STATE_HOME/fyisp` (or `~/.local/state/fyisp`) on Linux
  - `~/Library/Application Support/fyisp` on macOS
  - `%LOCALAPPDATA%\fyisp` on Windows
  - `$STATE_DIRECTORY` under systemd
  - `/data` in Docker (a `VOLUME`; warns if not mounted)

  `--ephemeral` uses a temporary directory that is deleted on exit. `fyisp paths` prints every location.
- **Permissions:** the directory is 0700 and the database files are 0600.
- **Pragma order** (before the first CREATE TABLE): `page_size=16384`, then `auto_vacuum=INCREMENTAL`, then `journal_mode=WAL`, then `synchronous=NORMAL`.
- **Connections:** one writer (`MaxOpenConns=1`) and a separate read-only pool, so the per-minute write never blocks UI reads.
- **Schema:**
  - `samples(hour, series, data BLOB, PK(hour, series)) WITHOUT ROWID`: time-first key.
  - `summary_1h(hour, series, n, lost, lost_by BLOB, min, mean, max, p95, PK(hour, series)) WITHOUT ROWID`
  - `series(id, target, kind, interval_ms, UNIQUE(target, kind))`
  - `meta(...)`
  - `PRAGMA user_version` holds the schema version. Migrations are embedded and backed up first (`VACUUM INTO`), and a database from a newer version is refused.
- **Blob format v1:**
  - A version byte, then `uvarint(slot0_unix_ms)`, `uvarint(interval_ms)`, then one uvarint per slot, then zstd at max level (SpeedBestCompression, no dictionary).
  - For each slot: if `u&1==0`, `u>>1` is the zigzag-encoded change from the previous value, in 10 µs units.
  - If `u&1==1`, the slot has no value and `code=u>>1`: **0 = not measured** (restart or sleep), **1–15 = loss reason**.
- **Slots come from wall-clock UTC:** `slot = floor((now − hourStart − phase)/interval)`. Skipped slots are filled with code 0. If the clock steps backwards, samples are dropped until it catches up.
- **Writes:**
  - The current hour stays in memory and is rewritten every 60s and on shutdown, so a crash loses at most 1 minute.
  - When an hour closes, its final blob and `summary_1h` row are written.
- **Retention:** `--retention 90d` is both the default and the maximum. Pruning is a `DELETE ... WHERE hour < ?` followed by `PRAGMA incremental_vacuum`.
- **Queries:**
  - `Panel` is **one SQL query per panel** (`hour BETWEEN .. AND series IN (..)`).
  - Ranges up to 48h read raw blobs, bucketed to 1000 points or fewer. Longer ranges read `summary_1h`.
- **Disk full:** keep probing, buffer up to about 10 MB in memory, show a red banner, and retry every 60s. Refuse to start if less than 200 MB is free, unless `--force`.
- **Expected size** at v0.1's ~23 samples/s: about 220 MB for 90 days of raw data, plus about 35 MB of summaries.

### Export and metrics
- **`fyisp export`** (`--format csv|sqlite --from --to --target --kind --tier raw|1h`):
  - Columns: `target, kind, ts, rtt_ms, lost, reason`.
  - Output files are 0600.
  - The output matches `Reader.Raw()` exactly.
- **`/metrics`:**
  - Served on the local listener only, from a private registry.
  - `ping_*` metrics compatible with network_exporter, checked by a golden test captured with `docker run ghcr.io/fuck-you-isp/network-exporter:main`. No bind mounts needed.
  - Also `fyisp_{https,tcp}_rtt_seconds`.

### Web UI (`internal/web`)
- **Stack:** plain ES-module JavaScript plus vendored `uPlot.iife.min.js` (MIT), embedded with `//go:embed`. There's **no build step**, no CDN and no inline scripts.
- **Charts:**
  - 7 panels from the profile's groups.
  - An HTTPS line with a min–max band; ICMP and TCP can be toggled on.
  - **Failures break the line into gaps**, and a **loss strip is coloured by reason**. Grey hatching means "not measured".
- **Navigation:** shared crosshair and zoom. Ranges: 5m, 30m, 1h, 6h, 24h, 48h, 7d, 30d, 90d. View state lives in the URL hash.
- **Other:** dark mode, a phone layout, a "warming up 0/87" state, a probe-capability panel, and a per-panel CSV link.
- **API:** `GET /api/status`, `/api/profile` (names and groups only), `/api/panel?group=&from=&to=&points=`. Each panel is one API call, which is one SQL query.
- **Local listener** (`--listen 127.0.0.1:3000`, trying 3001–3010 if busy unless set explicitly):
  - Host-header allowlist: anything else gets 421.
  - POST routes (start/stop sharing) must be same-origin and carry a per-process CSRF token.
  - With `--listen 0.0.0.0`, those POST routes are disabled unless `--admin-token` is set.
  - `/metrics` is served here, and pprof only with `--pprof 127.0.0.1:…`.
  - The browser opens to `127.0.0.1`, never to the tunnel URL.
- **Public listener** (loopback on an ephemeral port; the tunnel's origin): routes come from an allowlist, and anything else doesn't exist.
  - UI and read-only API under a random 128-bit secret path (`/s/<token>/…`).
  - GET/HEAD only, with a parameter whitelist.
  - Range limited to 90d.
  - Rate limits: 20 rps with a burst of 40 overall, and 5 rps per `Cf-Connecting-Ip`. At most 8 concurrent panel queries, and a 10s cache.
  - Timeouts: ReadHeader 5s, Write 30s.
  - Headers: CSP, nosniff, no-referrer.
  - **Redaction:** no LAN IPs, gateway, hostname, username, paths or version build info. Tested with golden files.

### Tunnel (`internal/tunnel`, cloudflared pinned to `2026.9.3`)
- **Off by default.** Start it with `--share` or the local UI button. When it starts, fyisp prints what will be public.
- **Module setup:**
  - Copy cloudflared's `replace` lines for quic-go, urfave/cli and yaml.v3. Skip the `golang_client` typo.
  - `go.mod` declares `go 1.26.0`.
- **Metrics registry:**
  - Before the first `NewSupervisor`, set `prometheus.DefaultRegisterer = noopRegisterer{}` once and never swap it again. This works around cloudflared's `supervisor.go:75`.
  - Call `origins.NewMetrics(prometheus.NewRegistry())`.
- **Configuration:**
  - The origin comes from `ingress.ParseIngress` pointing at the public listener's loopback URL.
  - `OriginDNSService` must be non-nil, and `ICMPRouterServer` stays nil.
  - Defaults: Retries 5, GracePeriod 30s, MaxEdgeAddrRetries 8, RPCTimeout 5s, QUIC flow-control limits 30/6 MiB, EdgeIPVersion auto, HAConnections 1.
  - Report cloudflared's version string to Cloudflare.
- **Protocol:** `auto` (QUIC with HTTP/2 fallback). This is a deliberate difference from upstream, which uses QUIC only.
- **Reconnects:** a watchdog driven by connection events re-provisions after 5 minutes without a connection.
- **Interface:** `Run`, `State` and `Subscribe` (see the contracts).
- **Plan B:** after a 3h timebox, drive cloudflared's own CLI code in-process.

### Ops
- **Single instance:** a lock on `<data-dir>/fyisp.lock` (`flock` or `LockFileEx`). A second instance exits with code 3 and prints the running instance's PID and URL.
- **Shutdown** on SIGINT, SIGTERM or console close:
  1. stop the probes
  2. flush the current hour
  3. `wal_checkpoint(TRUNCATE)`
  4. close the tunnel
  5. shut down the HTTP servers (5s)
  6. release the lock

  A second signal exits immediately.
- **Logging:** `slog` to stderr; text on a terminal, JSON otherwise.
  - At startup: version, data directory, probe modes, target count, local URL and share URL.
  - After that, only state changes, rate-limited.
- **First run:** print a large "Open http://127.0.0.1:3000" line. The README covers Gatekeeper and SmartScreen warnings for unsigned downloads.
- **Uninstall:** documented steps, using `fyisp paths`. No auto-update.
- **systemd** (`packaging/fyisp.service`):
  - `DynamicUser`, `StateDirectory=fyisp`, `ProtectSystem=strict`, `ProtectHome`, `PrivateTmp`, `NoNewPrivileges`.
  - `RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK`, `SystemCallFilter=@system-service`.
  - `CAP_NET_RAW` only in a commented-out drop-in.
  - `Restart=on-failure`, `TimeoutStopSec=15`.
- **Docker:**
  - A scratch image cross-compiled with `--platform=$BUILDPLATFORM` (arm/v8 maps to GOARM=7), running as user 65532.
  - `VOLUME /data`, `STOPSIGNAL SIGTERM`.
  - Built with the org's `image-publish.yml`. The GHCR package goes public at release.
- **Launchers** (`scripts/run.sh`, `scripts/run.ps1`):
  - The version is **pinned**, and the script checks the download against `SHA256SUMS` (mandatory). If `gh` is present, it also runs `gh attestation verify`.
  - Downloads use `curl --proto =https --tlsv1.2`. The script body sits in a `main()` called only at the end, so a partial download can't run half a script.
  - It runs from a temp directory. `FYISP_BASE_URL` overrides the download location for tests.
  - They only work publicly once the repo is public at the v0.1 release.

### Repo layout
- `cmd/fyisp/main.go`: flags, wiring, subcommands `export` and `paths`, and `x509roots/fallback`.
- `internal/`: `model`, `profile`, `probe`, `store`, `export`, `metrics`, `web` (with `static/`), `tunnel`, `deps` (tools build tag).
- `test/netns/`
- `packaging/`, `scripts/`, `Dockerfile`, `.github/workflows/`
- `docs/PLAN.md`: this plan
- `README.md`, `NOTICE`: Apache-2.0 for cloudflared; MIT for quic-go and uPlot; the trycloudflare disclaimer.

**Flags:**
- `--listen`, `--share`, `--share-protocol auto`
- `--data-dir`, `--ephemeral`, `--retention 90d`
- `--config`, `--open-browser`, `--admin-token`, `--pprof`, `--log-format`, `--force`, `--version`

### Contracts (lead commits these with fakes before the agents start)
```go
// internal/model: no dependencies
type ProbeKind uint8 // KindHTTPS=1, KindTCP=2, KindICMP=3
type Reason uint8    // Gap=0 (not measured), Timeout, Refused, Reset, Unreachable, DNS, TLS, HTTP, NoNetwork, Other=15
type Group  struct{ ID, Title string; Order int }
type Target struct{ Name, Host, Group string; Port int; Path string; Kinds []ProbeKind; Interval time.Duration; HostOverrides map[ProbeKind]string }
type Profile struct{ Name, Version string; Groups []Group; Targets []Target }
type SeriesKey struct{ Target string; Kind ProbeKind }
type Sample struct{ Key SeriesKey; Slot time.Time; RTT time.Duration; Lost bool; Reason Reason; Reused bool; Err string }
type Sink interface{ Observe(Sample) } // non-blocking

// internal/profile
func Default() (*model.Profile, error)
func Load(path string) (*model.Profile, error)
func Validate(p *model.Profile, l Limits) error

// internal/probe
type Caps struct{ ICMP string /* udp|raw|iphlpapi|unavailable */; TCP, HTTPS bool }
type Runner interface{ Run(ctx context.Context, p *model.Profile, sink model.Sink) error; Caps() Caps }
func New(o Options) Runner

// internal/store
type Writer interface{ model.Sink; Flush(ctx context.Context) error; Prune(ctx context.Context, before time.Time) error }
type PanelQuery  struct{ Keys []model.SeriesKey; From, To time.Time; MaxPoints int }
type PanelResult struct{ Tier string; Start time.Time; Step time.Duration; Series []SeriesCols }
type SeriesCols  struct{ Key model.SeriesKey; Mean, Min, Max []float32; N, Lost []uint32; LostBy map[model.Reason][]uint32 }
type RawPoint    struct{ Key model.SeriesKey; TS time.Time; RTTms float64; Lost bool; Reason model.Reason }
type Reader interface {
    Panel(ctx context.Context, q PanelQuery) (*PanelResult, error)
    Raw(ctx context.Context, keys []model.SeriesKey, from, to time.Time, fn func(RawPoint) error) error
    Series(ctx context.Context) ([]SeriesInfo, error)
    Stats(ctx context.Context) (Stats, error)
}
func Open(dir string, o Options) (interface{ Writer; Reader; Close() error }, error)

// internal/tunnel
type State struct{ Phase Phase; URL, Protocol, Location string; Since time.Time; LastErr string }
type Tunnel interface{ Run(ctx context.Context) error; State() State; Subscribe() (<-chan Event, func()) }
func New(o Options) Tunnel

// internal/metrics
func New(p func() *model.Profile) *Collector // implements model.Sink; Handler() http.Handler

// internal/web
type Deps struct{ Profile func() *model.Profile; Store store.Reader; Status func() Status; Metrics http.Handler; Share ShareControl; Log *slog.Logger }
func Local(d Deps) http.Handler
func Public(d Deps, secret string) http.Handler
```

## Execution
**Toolchain: Docker only.** No Go or other toolchain is installed on the host (user requirement).
- Everything goes through the repo `Dockerfile`, based on `golang:1.27.1-bookworm` pinned by digest:
  - `--target test`: gofmt, vet and `go test -race`
  - `--target dist --output type=local,dest=dist`: the 7-target release matrix plus SHA256SUMS
  - `--target modfiles --output type=local,dest=.`: `go mod tidy`
  - the default `image` target: the runtime image
- BuildKit cache mounts hold the module and build caches.
- Docker here uses the host's daemon (Docker-outside-of-Docker), so bind mounts don't work. Get artifacts out with `--output`, and use `docker run` without `-v`.
- CI uses the same Dockerfile targets.

**1. Lead: scaffold and spikes** (about half a day). Each spike has a pass/fail gate:
- **S1, dependencies:** cloudflared, modernc sqlite, zstd and x/net/icmp in one module.
  - `go mod tidy` gives a stable result.
  - `CGO_ENABLED=0` builds for linux amd64/arm64/armv7, windows amd64/arm64 and darwin amd64/arm64.
  - The stripped linux/amd64 binary is 45 MB or less.
- **S2, in-process tunnel:** provision a tunnel and `curl` a loopback page through it. Cancel, provision again, and check goleak is clean over both quic and http2.
- **S3, SQLite:** the pragma order holds, the writer and reader connections work, and a `kill -9` during the per-minute rewrite is followed by a clean `integrity_check`.
- **S4, platforms:** ICMP works on GitHub windows-latest and macos-latest runners, and the Google-Meet endpoints respond.

Then the lead commits `go.mod` (only the lead edits it; `internal/deps` pre-adds every dependency), the contracts, and the fakes (`store/fake`, `probe/fake`, `tunnel/fake`).

**2. Four agents in parallel**, each in `/workspaces/glueops/fyisp-wt/<name>` on its own `feat/<name>` branch. Each must pass `go vet`, `test -race` and the cross-build matrix.
- **A, probe + profile:**
  - 127.0.0.1 answers on all 3 probe kinds.
  - 192.0.2.1 gives Timeout, a closed local port gives Refused, and a bad hostname gives DNS.
  - It runs unprivileged.
  - Tests pass on the Windows and macOS runners.
  - The 87 target names match exactly, and `extends` works.
- **B, store + export:**
  - A round-trip property test covers every reason code and gaps.
  - `kill -9` loses 60s or less and passes `integrity_check`.
  - Prune plus vacuum shrinks the file.
  - Panel queries take under 100 ms for 48h raw and for 90d of hourly summaries.
  - The re-run benchmark is within 20% of step 0.
  - Export matches `Raw()`.
- **C, web** (works on fakes from day 1):
  - Every UI feature above.
  - `httptest` checks: the public side returns 404 for `/metrics` and pprof, 405 for POST and 429 under a burst, and contains no private addresses. The local side returns 421 for a foreign Host and 403 for a cross-origin POST.
- **D, tunnel + metrics + release:**
  - The live tunnel test (`FYISP_LIVE_TUNNEL=1`) passes over quic and http2 and re-provisions twice.
  - The `/metrics` golden test passes.
  - Delivers the Dockerfile, `run.sh`/`run.ps1`, systemd unit and workflows.

**3. Merge order:** lead scaffold → B → A → D → C, rebased onto the real store. Then the lead wires `main.go`, writes the integration test (a 3-minute run, then export, then compare), and a reviewer agent reviews the whole repo.

**CI (v0.1, actions pinned by SHA):**
- **lint:** `gofmt`, `vet`, staticcheck, `go mod tidy` diff, `go mod verify`, a check that the replace directives match cloudflared 2026.9.3, and govulncheck (not blocking at first).
- **unit:** `go test -race` on ubuntu, and `go test` on windows-latest and macos (arm64 and amd64).
- **unprivileged probe:** 60s runs with `--ephemeral` on ubuntu, macos and windows, the Windows one as a standard user.
- **netns:** fault scenarios on ubuntu with sudo.
- **cross-build:** reproducible, with a 50 MB size limit.
- **image:** builds for amd64, arm64 and arm/v7.
- **launcher:** tested against a local artifact server; a tampered binary must fail without running.
- **nightly / manual:** the live tunnel test, including a crawl of what's publicly exposed.
- **release on tag:** binaries, `SHA256SUMS`, attest-build-provenance, and the public GHCR package.

## Verification (v0.1)
**Build**
- The whole CI matrix above is green.
- Windows and macOS runtime checks run on CI runners only.

**Local run as a normal user**
- The log shows the probe modes, 87 targets and the local URL.
- With `--share`, a trycloudflare URL with a secret path is printed.
- The browser opens `127.0.0.1`.

**Correctness**, using a netns harness with sudo (`sudo -E go test -tags netns ./test/netns`, topology client ↔ gw ↔ isp ↔ inet):
- Dropping a target with `nft drop` shows gaps and a timeout-coloured strip, and `/metrics` reports loss 1.
- Dropping it with `nft reject` shows refused.
- A netem loss of 20% shows a matching loss ratio.
- A DNS failure shows DNS.
- Changing a host's IP gets picked up on re-resolve.
- A clock-step or suspend fake produces "not measured", never loss.
- Panel 1 has no AWS series.

**Storage**
- A 1-hour run followed by export matches `Raw()` sample for sample.
- SIGKILL loses 1 minute or less.
- Retention plus `incremental_vacuum` shrinks the file.
- A database with a newer schema version is refused, and a v1 fixture opens.
- A second instance exits with code 3.
- Files are 0600 and the directory 0700.

**Public surface** (nightly, through the real URL)
- Only the allowlisted routes exist.
- No private addresses appear in any response.
- A burst gets 429.
- Without `--share`, there is no egress to Cloudflare.
- With sharing off, a 10-minute egress capture shows only the targets and the resolver.

**Parity**
- Run alongside the legacy stack for 10 minutes: ICMP RTT per target agrees within about 10%.
- Compare fyisp's RSS and CPU against `docker stats` for the legacy stack, at 10 minutes and at 2 hours.

**UI**
- claude-in-chrome screenshots, if the extension is connected: panels filled, dark mode and phone layout, 48h raw and 90d summary ranges.
