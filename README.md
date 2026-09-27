# fyisp

Single-binary internet quality monitor: probes hyperscalers &amp; popular services, stores 90 days locally, shows whose fault it is, shares a public link.

fyisp ("F\*\*\* You ISP") measures how well your connection reaches AWS, Azure, Google Cloud, Cloudflare, GitHub, video-call services and more, so you can show your ISP where the problem is. It replaces the old [docker-compose stack](https://github.com/fuck-you-isp/docker-compose) (network_exporter + Prometheus + Grafana + cloudflared) with one static binary:

- **Probes** every target over HTTPS and TCP every 15s and ICMP every 5s, without admin rights.
- **Stores every sample for 90 days** in a local SQLite file (about 250 MB), including *why* a probe failed: timeout, refused, reset, unreachable, DNS, TLS, no network.
- **Shows charts** at <http://127.0.0.1:3000>. Failures are gaps coloured by reason, never a fake 0 ms.
- **Shares a public, read-only, redacted link** if you ask it to (`--share`).
- **No telemetry.** fyisp only talks to the probe targets, your DNS resolver and, only while sharing, Cloudflare.

> **Status:** v0.1 is in development. The downloads, one-line launchers and container image below work once the repository is made public at the v0.1 release.

## Quick start

### Linux and macOS

```sh
curl --proto '=https' --tlsv1.2 -fsSL https://github.com/fuck-you-isp/fyisp/releases/latest/download/run.sh | sh
```

Then open <http://127.0.0.1:3000>. Pass options after `sh -s --`, e.g. `... | sh -s -- --share`.

The launcher (`run.sh`) is pinned to the release it came from. It downloads the matching binary into a temporary directory, **refuses to run it unless its SHA-256 matches the release's `SHA256SUMS`**, also checks the GitHub build provenance attestation if the [GitHub CLI](https://cli.github.com/) is installed and logged in, runs it, and deletes it when fyisp exits. Nothing is installed.

### Windows (PowerShell)

```powershell
& ([scriptblock]::Create((irm https://github.com/fuck-you-isp/fyisp/releases/latest/download/run.ps1)))
```

Add fyisp options at the end, e.g. `... ))) --share`. `run.ps1` does the same checks as `run.sh`.

### Download the binary yourself

Get `fyisp-<os>-<arch>` from the [latest release](https://github.com/fuck-you-isp/fyisp/releases/latest) (`linux-amd64`, `linux-arm64`, `linux-armv7`, `darwin-amd64`, `darwin-arm64`, `windows-amd64.exe`, `windows-arm64.exe`) together with `SHA256SUMS`, then verify and run it:

```sh
sha256sum -c SHA256SUMS --ignore-missing     # macOS: shasum -a 256 -c SHA256SUMS --ignore-missing
gh attestation verify fyisp-linux-amd64 --repo fuck-you-isp/fyisp   # optional
chmod +x fyisp-linux-amd64 && ./fyisp-linux-amd64
```

### Avoiding "unknown publisher" warnings

The binaries are not code-signed yet. Windows SmartScreen and macOS Gatekeeper only check files that carry the browser's "downloaded from the internet" mark, so:

- **Recommended: use the launchers above.** `run.ps1` (`irm`) and `run.sh` (`curl`) download without that mark, so there is no warning, and they refuse to run anything whose SHA-256 doesn't match `SHA256SUMS`.
- **Windows, browser download:** SmartScreen says "Windows protected your PC". Either choose *More info → Run anyway*, or, after verifying the checksum, remove the mark once: `Unblock-File .\fyisp-windows-amd64.exe`. Double-clicking the `.exe` opens a console window with the log and the dashboard address; closing the window stops fyisp. To pass options, run it from a terminal or put them in a shortcut's *Target*.
- **macOS, browser download:** Gatekeeper blocks it. After verifying it, run `xattr -d com.apple.quarantine fyisp-darwin-arm64`.

Code signing is planned: free Windows signing through the [SignPath Foundation](https://signpath.org/) once the repository is public (it requires a public open-source project), and Apple notarization if there is demand.

### Docker

```sh
docker run -d --name fyisp --restart unless-stopped -p 3000:3000 -v fyisp:/data ghcr.io/fuck-you-isp/fyisp
```

Images are published for `linux/amd64`, `linux/arm64` and `linux/arm/v7`. The container runs as an unprivileged user (65532) from a `scratch` image and keeps its data in the `/data` volume. `-p 3000:3000` makes the UI reachable from your network; use `-p 127.0.0.1:3000:3000` to keep it on this machine. ICMP works without extra capabilities on Docker 20.10+, which allows unprivileged ping sockets inside containers.

fyisp options go **after the image name** (the image already sets `--data-dir=/data --listen=0.0.0.0:3000`):

```sh
# Share a public read-only link from the start; the URL is in the logs
docker run -d --name fyisp --restart unless-stopped -p 3000:3000 -v fyisp:/data ghcr.io/fuck-you-isp/fyisp --share
docker logs fyisp 2>&1 | grep 'public link ready'

# Or turn sharing on and off from the UI: set an admin token
docker run -d --name fyisp --restart unless-stopped -p 3000:3000 -v fyisp:/data ghcr.io/fuck-you-isp/fyisp --admin-token "$(openssl rand -hex 16)"
docker inspect fyisp --format '{{join .Args " "}}'   # shows the token again
```

Inside a container fyisp listens on all interfaces, so anyone on your network could reach the UI. That is why its *Create public link* / *Stop sharing* buttons are off by default and the UI says *Share controls disabled*. `--share` still starts the link at startup without them. With `--admin-token`, the buttons come back and the browser asks for the token once per tab. If you open the UI by a name instead of an IP address (`http://nas.local:3000`, a Tailscale MagicDNS name), add `--allow-host nas.local` (repeatable or comma-separated). Otherwise fyisp refuses the request with *421 unknown host*, which protects against DNS rebinding.

**Use host networking on Linux for accurate verdicts.** With Docker's default bridge network the container's default gateway is Docker's bridge (`172.17.0.1`), not your home router, so the *Gateway* layer measures the bridge (always fine) and the *ISP edge* found by the path discovery is your router's WAN side or later. A Wi-Fi or router problem then shows up one layer too far out (as *ISP* or *Upstream*). Run the container in the host's network namespace instead:

```sh
docker run -d --name fyisp --restart unless-stopped --network host -v fyisp:/data ghcr.io/fuck-you-isp/fyisp --listen=127.0.0.1:3000
```

With `--network host`, `-p` is ignored: fyisp listens on the host directly (the image's default `--listen=0.0.0.0:3000` makes it reachable from your network; the `--listen=127.0.0.1:3000` above keeps it on this machine).

**Docker Desktop (macOS, Windows)** runs containers inside a Linux VM: the container's gateway is the VM's virtual network, every probe is NATed by the VM, and `--network host` means the VM's network, not your Mac's or PC's. The charts are still useful, but the Gateway and ISP edge layers (and so the *Wi-Fi/router* vs *ISP* verdicts) are not about your real network there. For layer-accurate verdicts, run the native binary on macOS and Windows (see [Quick start](#quick-start)).

### systemd (Linux service)

```sh
sudo install -m 0755 fyisp-linux-amd64 /usr/local/bin/fyisp
sudo curl --proto '=https' --tlsv1.2 -fsSL -o /etc/systemd/system/fyisp.service \
  https://raw.githubusercontent.com/fuck-you-isp/fyisp/main/packaging/fyisp.service
sudo systemctl daemon-reload && sudo systemctl enable --now fyisp
```

[`packaging/fyisp.service`](packaging/fyisp.service) runs fyisp as a transient unprivileged user (`DynamicUser`) with a read-only system, no access to home directories and a system-call filter; data lives in `/var/lib/fyisp`. The comments in the unit show how to enable `--share` or `CAP_NET_RAW` with a drop-in (`sudo systemctl edit fyisp`).

## Whose fault is it? (verdicts)

Above the charts, fyisp shows one plain-English verdict, re-evaluated every 5 seconds over the last minute of probes, and keeps an outage log of every period that was not *ok*. A target is unhealthy when it loses at least 20% of its probes or its median latency is far above its own 30-minute baseline. The first matching kind wins:

| Kind | Meaning |
|---|---|
| `no_network` | This device has no working connection (no route, every layer unreachable). |
| `lan` | The *Gateway* (your router) is unhealthy: Wi-Fi, cable or the router itself. |
| `isp` | The router is fine, the *ISP edge* (first public hop past it) is not: your ISP's access network. |
| `upstream` | Router and ISP edge are fine, but the anycast resolvers or most monitored services (in more than one group) fail: beyond your ISP. |
| `dns` | The path works, but at least 30% of the services can't be resolved. |
| `service` | Only some services are unhealthy: their problem, not yours. |
| `ok` / `warming_up` | Everything looks fine / the first minute after start. |

Loss on an inner link shows on every layer beyond it, so the verdict blames the innermost lossy layer: the gateway or ISP edge is blamed once it has lost at least 5 probes and 10% of them and the layers beyond it lose at least half as much. A layer further out is blamed only when its loss is significantly higher than that of the layers inside it (a two-proportion test on the minute's counts), so a few unlucky probes can't point at the wrong layer.

A new problem must hold for about 10 seconds before it is shown, and the verdict returns to *ok* only after a full minute without problems. Verdict summaries never contain addresses or host names, so they are safe on the public link. Every host name is looked up again every 60 seconds. After one failed lookup fyisp keeps probing the last good address; after two in a row (about 70 seconds into a resolver outage) the host's probes count as lost with reason *DNS*, as a real application would fail too, until a lookup succeeds again.

The layers come from the built-in **Network path** group, shown first: the *Gateway* and the *ISP edge* (found automatically from the routing table and a short traceroute; pinged every second) and three public anycast resolvers (1.1.1.1, 8.8.8.8, 9.9.9.9; ICMP and TCP). A profile passed with `--config` keeps this group unless it sets `path: false` at the top level (`--no-path` does the same for `--profile`); without it fyisp can still tell `dns`, `service` and `upstream` apart, but not `lan` from `isp`.

## What to measure (profiles)

By default fyisp measures 87 targets: common call services, DNS resolvers, dev tunnels, a few dev services, AWS, Hetzner and Google Cloud. `--profile` picks others from the built-in **target catalog** (one file per provider in [`internal/profile/catalog/`](internal/profile/catalog/), every endpoint checked by hand):

```sh
fyisp profiles                       # list the profiles, with target and panel counts
fyisp profiles aws                   # the targets of a profile (region, city, country)
fyisp --profile aws,europe           # combine profiles (a union)
fyisp --profile clouds --geo eu      # only the targets of a profile in one region (na sa eu me af as oc)
fyisp --profile default,dns          # the default targets plus every public resolver
```

| Profiles | What |
|---|---|
| `default` | the 87 original targets (used without `--profile`) |
| `all` | every target in the catalog |
| `hyperscalers`, `devclouds`, `clouds` | AWS, Google Cloud, Azure, Oracle, IBM, Alibaba, Tencent, Huawei / VPS, bare-metal, regional and object-storage clouds (DigitalOcean, Linode, Vultr, Hetzner, OVH, ...) / both; one panel per provider and region |
| `storage` | object storage clouds (Backblaze B2, Wasabi, IDrive e2, R2, ...) plus Hetzner, Linode and OVH object storage |
| `cdn`, `dns` | CDN edges, public DNS resolvers (DNS-over-HTTPS) |
| `common`, `dev`, `streaming`, `gaming` | everyday services, developer services, streaming, game platforms |
| `north-america`, `south-america`, `europe`, `middle-east`, `africa`, `asia`, `oceania` | every catalog target located there (anycast endpoints have no region and are left out) |
| `aws`, `gcp`, `azure`, `hetzner`, ... | every provider in the catalog is a profile of its own |

Target names are identities (a name keeps its history): when a profile is combined with `default` and both have a target of the same name (e.g. `AWS-us-east-1`), the catalog's definition is used. `default` on its own never changes.

There is no limit on the number of targets; you can run every profile at once (`--profile all`); profiles of more than 150 targets open on the [Overview](#the-overview-large-profiles) instead of hundreds of charts. The cost grows roughly linearly with the target count. Per 100 targets: about 1.4% of one CPU core, 11 KB/s of download, 5 KB/s of upload and 0.4 GB of disk for 90 days.

Measured on 2026-09-27 (Linux amd64, Docker; 25 minutes per profile after a 5-minute warm-up; the Network path group included). The cloud rows were measured with the larger v0.4.0 catalog; since v0.4.1 the catalog is slightly larger (`clouds` 1330, `all` 1599 targets; GPU clouds, PaaS and sanctioned locations removed), so scale by target count:

| Profile | Targets | CPU (one core) | Memory (RSS) | Download / upload | Traffic per month | Disk for 90 days |
|---|---:|---:|---:|---:|---:|---:|
| `dns` | 33 | 0.8% | 65 MB | 3.8 / 1.9 KB/s | 15 GB | 0.13 GB |
| `common` | 72 | 1.1% | 72 MB | 8.0 / 2.6 KB/s | 28 GB | 0.24 GB |
| `default` | 92 | 3.3% | 84 MB | 21 / 9 KB/s | 81 GB | 0.57 GB |
| `hyperscalers` | 299 | 4.9% | 115 MB | 34 / 17 KB/s | 135 GB | 1.4 GB |
| `europe` | 381 | 6.0% | 111 MB | 41 / 21 KB/s | 165 GB | 1.4 GB |
| `devclouds` | 743 | 11.6% | 155 MB | 80 / 41 KB/s | 320 GB | 3.1 GB |
| `clouds` | 1037 | 15.8% | 205 MB | 114 / 58 KB/s | 456 GB | 4.5 GB |
| `all` | 1306 | 18.4% | 231 MB | 142 / 70 KB/s | 564 GB | 5.4 GB |

- `default` costs more CPU than its size suggests because it runs 7 continuous hop-by-hop traces.
- TLS session resumption, added after this run, cut download by a further ~23% and CPU by ~10% on `all` (110 KB/s, 18.4% vs 20.3% in a side-by-side run).
- Traffic per month matters on metered connections: every target is probed every 15 seconds (ping every 5 seconds), around the clock.
- Disk is about 1.4 bytes per measurement (compressed hourly blocks), plus about 40% for summaries and indexes.

A `--config` file can start from any profiles and change them:

```yaml
name: mine
extends: [aws, europe]            # or [default]
remove: [AWS-eu-south-2]
groups: [{id: home, title: Home}]
add:
  - {name: nas, host: 192.168.1.10, group: home, kinds: [icmp]}
```

Profiles are defined in [`internal/profile/profiles.yml`](internal/profile/profiles.yml) as selections over the catalog (by provider, provider kind, tag and region); the file documents the format.

Missing a provider or region, or found a dead endpoint? Pull requests are welcome; see [CONTRIBUTING.md](CONTRIBUTING.md#adding-or-fixing-endpoints).

### The Overview (large profiles)

With more than 150 targets (e.g. `--profile clouds` or `all`) the dashboard opens on the **Overview** rather than one chart per panel; the **Overview | Charts** switch under the Network path panel changes view on any profile. The Overview shows one probe kind at a time (the first selected of HTTPS, TCP, ICMP) over the selected time range:

- **Breadth line**: how many targets are worse than normal at the same time, across how many providers and regions. When a quarter or more of them, across several providers and regions, degrade together, the common factor is your side. It backs up the verdict and never overrides it; in the first day, before targets have a normal, it counts only packet loss.
- **Heatmap**: one row per provider (grouped by kind), one column per region. A cell is coloured by how much slower than normal its targets are (median), or by latency until half the targets have a normal (about a day); a red dot marks loss, a red cell with × heavy loss (20% or more), hatching means not measured. Click a cell or a provider to filter the table.
- **Table**: every target with its mean round-trip time over the range, its normal, the ratio, loss and a status (failing: 20% loss or more; lossy: 1% or more; very slow: 3× normal or more; slow: 1.5× or more). Sort by any column, filter by text, region or problems only. Click a row to see that target alone on its chart; Back returns to the Overview.

The Overview's window is at most 30 days (it reads every target at once). Filters, sort, view and focus are kept in the URL, so a link opens the same view; the public link shows the same Overview.

## ICMP permissions

fyisp never asks for admin rights. On Linux it uses unprivileged ping sockets, which the kernel allows for the groups in `net.ipv4.ping_group_range` (most distributions allow everyone). If fyisp logs `ICMP unavailable → TCP/HTTPS only`, allow your group:

```sh
sudo sysctl -w net.ipv4.ping_group_range="0 2147483647"
```

or carry on without ICMP: the HTTPS and TCP probes still work. macOS and Windows need nothing.

## Sharing a public link (`--share`)

Sharing is **off by default**. Start it with `fyisp --share` or the *Share* button in the local UI; fyisp then prints exactly what becomes public.

- fyisp opens a [Cloudflare quick tunnel](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/do-more-with-tunnels/trycloudflare/) from inside the process (no `cloudflared` install, no Cloudflare account) and prints a link like `https://<random-words>.trycloudflare.com/s/<secret>/`.
- **What is public:** the charts and their read-only data API, under a random 128-bit secret path. Nothing else exists on the public side: no `/metrics`, no settings, no sharing controls, no write routes, and requests are rate-limited.
- **What is redacted:** LAN and gateway addresses, hostname, username, file paths and build details never appear on the public side.
- The tunnel tries QUIC (UDP port 7844) first and switches to HTTP/2 over TCP port 443 after about 30 seconds if UDP is blocked. Force one with `--share-protocol quic` or `--share-protocol http2`.
- The link is random. It changes when fyisp restarts, or when the tunnel has to be re-created after 5 minutes without a connection. The browser on your machine always opens `127.0.0.1`, never the public link.
- Quick tunnels are a free Cloudflare service meant for testing and demos. They come with no uptime guarantee, are rate-limited, and are subject to [Cloudflare's terms](https://www.cloudflare.com/website-terms/). fyisp is not affiliated with Cloudflare.

## Prometheus metrics

The local listener serves `/metrics` (never on the public link). ICMP results use the same `ping_*` metric names, help, types and labels as [network_exporter](https://github.com/syepes/network_exporter), so dashboards and alerts from the old stack keep working. Differences: the `target` label is the hostname (the old stack used the IP it resolved once at startup), and `target_ip` is empty. fyisp also exports `fyisp_https_rtt_seconds`, `fyisp_tcp_rtt_seconds`, `fyisp_probe_samples_total` and `fyisp_probe_lost_total{reason="..."}`.

## Where data lives, and uninstalling

`fyisp paths` prints every location fyisp uses. By default:

| Platform | Data directory |
|---|---|
| Linux | `$XDG_STATE_HOME/fyisp` (usually `~/.local/state/fyisp`) |
| macOS | `~/Library/Application Support/fyisp` |
| Windows | `%LOCALAPPDATA%\fyisp` |
| systemd unit | `/var/lib/fyisp` (`$STATE_DIRECTORY`) |
| Docker | the `/data` volume |

Change it with `--data-dir`, or use `--ephemeral` for a temporary directory that is deleted on exit. Samples older than 90 days are deleted automatically (`--retention`).

To uninstall, stop fyisp, then delete its data directory and the binary. The launchers leave nothing behind.

- **Linux:** `rm -rf ~/.local/state/fyisp` (or the directory `fyisp paths` printed).
- **macOS:** `rm -rf ~/Library/Application\ Support/fyisp`.
- **Windows:** `Remove-Item -Recurse "$env:LOCALAPPDATA\fyisp"`.
- **Docker:** `docker rm -f fyisp && docker volume rm fyisp`.
- **systemd:** `sudo systemctl disable --now fyisp && sudo rm /etc/systemd/system/fyisp.service /usr/local/bin/fyisp && sudo rm -rf /var/lib/private/fyisp`.

fyisp does not update itself: run the launcher from a newer release, or replace the binary.

## Building from source

Everything builds in Docker; no Go toolchain is needed on your machine.

```sh
docker build --target test .                                         # gofmt check, go vet, go test -race
docker buildx build --target dist --output type=local,dest=dist .    # the 7 release binaries + SHA256SUMS
docker build -t fyisp .                                              # the runtime image
docker build --target launchertest .                                 # run.sh against a fake release
docker build --target launchertest-pwsh .                            # run.ps1 under PowerShell 7
docker build --target servicetest .                                  # systemd-analyze verify on the unit
docker buildx build --target fmt --output type=local,dest=. .        # gofmt -w
docker buildx build --target modfiles --output type=local,dest=. .   # go mod tidy
docker buildx build --target asndb --output type=local,dest=. .      # refresh the built-in IP-to-ASN table
```

Hop owners (AS number and name) come from a table built into the binary, [`internal/asn/data/ip2asn-v4.bin`](internal/asn/data), generated from the [iptoasn.com](https://iptoasn.com/) IPv4 data; fyisp never looks them up over the network. The `asndb` target downloads the latest file and regenerates the table; releases do this automatically and fall back to the committed table if iptoasn.com is unreachable.

Add `--build-arg VERSION=v0.1.0` to stamp a version. Releases are built by [`.github/workflows/release.yml`](.github/workflows/release.yml) from a `v*` tag using the same targets, and carry [build provenance attestations](https://docs.github.com/actions/security-for-github-actions/using-artifact-attestations).

### Network-fault tests (Linux, sudo)

`test/netns/run.sh` builds fyisp and the harness in Docker, then (with sudo) builds throwaway network namespaces (client ↔ gateway ↔ ISP router ↔ internet), runs fyisp in the client namespace as uid 65532 without capabilities, and injects faults on the ISP router: `nft drop` (timeout, holes in the panel, `ping_loss_percent` 1), `nft reject with tcp reset` (refused), a withdrawn route (unreachable), netem 20% loss (matching loss ratio) and 80 ms delay (RTT +80 ms), an unresolvable name (dns), an untrusted certificate (tls), and a restart on a persistent `--data-dir` ("not measured", never loss). It also checks the path discovery (gateway and ISP edge) and, with the Network path group enabled, the verdict and outage log for each layer: loss between client and gateway (`lan`), between gateway and ISP (`isp`), anycast and most targets dropped (`upstream`), an unreachable resolver, both while running and at startup (`dns`), and one dropped target (`service`); the faulty layer must be the first problem shown.

Every scenario gets its own topology (namespaces `fyt-<run>-<n>-{cli,gw,isp,net}`, responders, resolver, certificates, fyisp process and data directory), so the scenarios run in parallel and a full run takes about as long as the slowest one, about 5 minutes (almost all of it fyisp's own timings: the 60 s verdict warm-up, fault windows, the 60 s return to ok). The namespaces are deleted when each scenario ends, and `run.sh` checks that none of its run are left and prints the wall time.

```sh
test/netns/run.sh                              # everything, in parallel (~5 min)
test/netns/run.sh -test.run TestVerdict        # only the verdict scenarios
test/netns/run.sh -test.run 'Faults/b-nft-drop'
FYISP_NETNS_PARALLEL=0 test/netns/run.sh       # one scenario at a time, for debugging (~35 min)
FYISP_NETNS_OUT=out test/netns/run.sh          # keep API responses and logs, one directory per scenario
```

With a Go toolchain: `sudo -E go test -tags netns -v -timeout 20m ./test/netns` (`-parallel 1` and a longer timeout, e.g. `-timeout 90m`, run it serially).

## License

MIT, see [LICENSE](LICENSE). fyisp includes third-party code under its own licenses, see [NOTICE](NOTICE). IP-to-ASN data from [iptoasn.com](https://iptoasn.com/) (PDDL).
