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

The binaries are not code-signed yet:

- **macOS:** Gatekeeper blocks a binary downloaded with a browser. After verifying it, run `xattr -d com.apple.quarantine fyisp-darwin-arm64`. (The launcher doesn't need this: `curl` doesn't set the quarantine flag.)
- **Windows:** SmartScreen may say "Windows protected your PC" for a browser download: choose *More info → Run anyway*.

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

### systemd (Linux service)

```sh
sudo install -m 0755 fyisp-linux-amd64 /usr/local/bin/fyisp
sudo curl --proto '=https' --tlsv1.2 -fsSL -o /etc/systemd/system/fyisp.service \
  https://raw.githubusercontent.com/fuck-you-isp/fyisp/main/packaging/fyisp.service
sudo systemctl daemon-reload && sudo systemctl enable --now fyisp
```

[`packaging/fyisp.service`](packaging/fyisp.service) runs fyisp as a transient unprivileged user (`DynamicUser`) with a read-only system, no access to home directories and a system-call filter; data lives in `/var/lib/fyisp`. The comments in the unit show how to enable `--share` or `CAP_NET_RAW` with a drop-in (`sudo systemctl edit fyisp`).

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
```

Add `--build-arg VERSION=v0.1.0` to stamp a version. Releases are built by [`.github/workflows/release.yml`](.github/workflows/release.yml) from a `v*` tag using the same targets, and carry [build provenance attestations](https://docs.github.com/actions/security-for-github-actions/using-artifact-attestations).

## License

MIT, see [LICENSE](LICENSE). fyisp includes third-party code under its own licenses, see [NOTICE](NOTICE).
