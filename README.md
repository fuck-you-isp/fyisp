# fyisp

Is your internet good enough for calls and work right now, and when it isn't, is it your Wi-Fi or your internet provider? fyisp answers that in plain words, keeps 90 days of evidence, and makes a report you can send to your provider.

fyisp ("F\*\*\* You ISP") is one small program for a home or a remote worker. It runs on your computer (or a NAS or Raspberry Pi), checks your connection every few seconds, and shows the answer at <http://127.0.0.1:3000>:

- **Calls, work apps and websites.** It checks the path that call audio and video take, the call and chat apps (Teams, Zoom, Google Meet, Slack, Webex, Discord, WhatsApp), Microsoft 365 and Google Workspace, and the networks most websites are served from.
- **Whose fault it is.** It also checks your own router and your provider's first hop, so it can say *your Wi-Fi or router*, *your internet provider*, *the wider internet* or *one app*.
- **When it was bad.** A timeline of today and this week, a *Mark a bad call* button, and *What happened at …?* for any time you pick.
- **Proof for your provider.** *Make a report* gives you a link to a read-only, redacted 7-day report (no addresses, no notes) that you can send to your provider or your IT team.
- **Private.** No account, no telemetry, nothing to install to try it. fyisp only talks to the services it checks, your DNS resolver and, only while you share a report, Cloudflare. Its data stays on your machine.

Technical users get every number under **Details**: charts per service, traceroutes, call-quality scores, the outage log, Prometheus metrics, and 1,400 more endpoints (clouds, CDNs, gaming, regions of the world) [one flag away](#what-to-measure-profiles).

> **New in v1.0: the default changed.** Plain `fyisp` now checks the 22 targets above (calls, work apps, websites) instead of the 91 targets of v0.x (mostly cloud regions and developer services), and opens on the plain-words **Home** view. The old cloud regions are one flag away: `--profile clouds` (or `aws`, `gcp`, `hetzner`, … ; see [What to measure](#what-to-measure-profiles)). Stored history is kept; series that are no longer measured age out after 90 days.

## Quick start

### Linux and macOS

```sh
curl --proto '=https' --tlsv1.2 -fsSL https://github.com/fuck-you-isp/fyisp/releases/latest/download/run.sh | sh
```

Then open <http://127.0.0.1:3000>. Pass options after `sh -s --`, e.g. `... | sh -s -- --profile streaming`.

The launcher (`run.sh`) is pinned to the release it came from. It downloads the matching binary into a temporary directory, **refuses to run it unless its SHA-256 matches the release's `SHA256SUMS`**, also checks the GitHub build provenance attestation if the [GitHub CLI](https://cli.github.com/) is installed and logged in, runs it, and deletes it when fyisp exits. Nothing is installed, so fyisp stops when you close the terminal: see [Keep it running](#keep-it-running).

### Windows (PowerShell)

```powershell
& ([scriptblock]::Create((irm https://github.com/fuck-you-isp/fyisp/releases/latest/download/run.ps1)))
```

Add fyisp options at the end, e.g. `... ))) --profile streaming`. `run.ps1` does the same checks as `run.sh`.

### What you see: Home and Details

![fyisp Home: one answer, the path from your router to the internet, tiles for calls, work apps and websites, and today's timeline](docs/img/home.png)

**Home** opens first. It answers one question, *is my internet OK for calls and work?*, in plain words, without numbers:

- **The answer card**: one sentence (*Your internet is fine*, *Your internet provider is having trouble*, *No internet connection*), the chain *Your router → Your provider → The internet → Apps & websites* with a mark on the link that is most likely at fault, *What does this mean?* and, when something is wrong, *What you can try*.
- **Mark a bad call**: one click saves "now" (with Undo) and says what fyisp saw at that minute, including when your connection looked fine and the problem was more likely the app or the other person's connection. *Was it earlier?* picks another time.
- **Tiles** for *Calls*, *Work apps* and *Websites* (and *Other services* when you measure more): *Working*, *Slow*, *Not working*, or for calls *Good*, *Some glitches*, *Choppy*, *Breaking up*, *Blocked*. A tile names the one app that fails, e.g. *Zoom isn't answering*.
- **When was it bad?**: today or this week, block by block, with a sentence such as *2 rough patches this week, both most likely your internet provider*. Time when fyisp wasn't running is hatched and never counted as bad.
- **What happened at …?** and **Make a report**.

**Details** (the switch at the top; Back returns to Home) is the full instrument panel: the verdict and outage log, notes, reports, the [Calls view](#call-quality) with scores per app, the [Overview](#the-overview-large-profiles), charts for every service, traceroutes (*Investigate*), CSV export, and the share controls. Clicking a tile on Home opens Details filtered to that tile.

The public report link shows the same Home in the third person (*Internet quality report*), with no addresses, notes or buttons, and Details one click away.

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
docker logs fyisp 2>&1 | grep -m1 -o 'http://[^ "]*#admin=[0-9a-f]*'
```

Open the link that the second command prints (`http://127.0.0.1:3000/#admin=…`) once in your browser. It turns on *Mark a bad call*, notes, reports and sharing for that browser; see [Controls under Docker](#controls-under-docker) below.

Images are published for `linux/amd64`, `linux/arm64` and `linux/arm/v7`. The container runs as an unprivileged user (65532) from a `scratch` image and keeps its data in the `/data` volume. `-p 3000:3000` makes the UI reachable from your network; use `-p 127.0.0.1:3000:3000` to keep it on this machine. ICMP works without extra capabilities on Docker 20.10+, which allows unprivileged ping sockets inside containers.

fyisp options go **after the image name** (the image already sets `--data-dir=/data --listen=0.0.0.0:3000`):

```sh
# Share a public read-only link from the start; the URL is in the logs
docker run -d --name fyisp --restart unless-stopped -p 3000:3000 -v fyisp:/data ghcr.io/fuck-you-isp/fyisp --share
docker logs fyisp 2>&1 | grep 'public link ready'

# Keep the dashboard read-only for everyone (no notes, reports or share buttons)
docker run -d --name fyisp --restart unless-stopped -p 3000:3000 -v fyisp:/data ghcr.io/fuck-you-isp/fyisp --admin-token=off
```

#### Controls under Docker

Inside a container fyisp listens on all interfaces, so anyone on your network could reach the UI. Changes (*Mark a bad call*, notes, reports, *Create public link* / *Stop sharing*) therefore need an admin token. On first start fyisp generates one, keeps it in the data volume (`/data/admin-token`, readable only by fyisp) and logs an "open this link" URL at every start, like Jupyter:

- Open the link once per browser. The page swaps the token for a cookie that only this dashboard sees and removes it from the address bar. The token is in the part after `#`, which browsers never send to a server, so it doesn't end up in proxy logs or `Referer` headers. Looking at the dashboard needs no token.
- Opening the dashboard from another machine: replace `127.0.0.1` with the server's address, e.g. `http://192.168.1.10:3000/#admin=…`.
- To sign every browser out, delete the token and restart; a new link is logged: `docker run --rm -v fyisp:/data busybox rm /data/admin-token && docker restart fyisp`.
- `--admin-token=<your own>` uses that token instead (the browser asks for it, or open `http://…:3000/#admin=<token>`). `--admin-token=off` disables changes entirely; `--share` still starts the public link at startup.
- The dashboard is plain HTTP: someone who can watch your network traffic could copy the cookie, as with any HTTP login. Keep it on a network you trust, or use `-p 127.0.0.1:3000:3000`.

Starting a trace (*Investigate*) needs no admin token: anyone who can open the dashboard may trace a profile target, for at most 250 rounds.

If you open the UI by a name instead of an IP address (`http://nas.local:3000`, a Tailscale MagicDNS name), add `--allow-host nas.local` (repeatable or comma-separated). Otherwise fyisp refuses the request with *421 unknown host*, which protects against DNS rebinding.

**Use host networking on Linux for accurate verdicts.** With Docker's default bridge network the container's default gateway is Docker's bridge (`172.17.0.1`), not your home router, so the *Gateway* layer measures the bridge (always fine) and the *ISP edge* found by the path discovery is your router's WAN side or later. A Wi-Fi or router problem then shows up one layer too far out (as *ISP* or *Upstream*). Run the container in the host's network namespace instead:

```sh
docker run -d --name fyisp --restart unless-stopped --network host -v fyisp:/data ghcr.io/fuck-you-isp/fyisp --listen=127.0.0.1:3000
```

With `--network host`, `-p` is ignored: fyisp listens on the host directly (the image's default `--listen=0.0.0.0:3000` makes it reachable from your network; the `--listen=127.0.0.1:3000` above keeps it on this machine).

**Docker Desktop (macOS, Windows)** runs containers inside a Linux VM: the container's gateway is the VM's virtual network, every probe is NATed by the VM, and `--network host` means the VM's network, not your Mac's or PC's. The charts are still useful, but the Gateway and ISP edge layers (and so the *Wi-Fi/router* vs *ISP* verdicts) are not about your real network there. For layer-accurate verdicts, run the native binary on macOS and Windows (see [Quick start](#quick-start)).

## Keep it running

fyisp can only tell you about time when it was running. Started from a terminal or the launcher, it stops when you close that window or log out, and Home shows those hours hatched: *fyisp wasn't running*. To have it start by itself, install the binary once and register it with your system, as below. It then runs in the background whenever you're logged in (or, on Linux and Docker, whenever the machine is on) and you just open <http://127.0.0.1:3000>.

A laptop that sleeps still leaves gaps; that's expected and never counted as a problem. For a complete record, run fyisp on something that is always on and on the same network: a NAS, a Raspberry Pi or a home server, with [Docker](#docker) or [systemd](#linux-systemd).

### macOS (LaunchAgent)

In Terminal (Apple silicon; on an Intel Mac use `fyisp-darwin-amd64`):

```sh
mkdir -p ~/.local/bin && cd ~/.local/bin
curl -fsSLO https://github.com/fuck-you-isp/fyisp/releases/latest/download/fyisp-darwin-arm64
curl -fsSLO https://github.com/fuck-you-isp/fyisp/releases/latest/download/SHA256SUMS
shasum -a 256 -c SHA256SUMS --ignore-missing && mv fyisp-darwin-arm64 fyisp && chmod +x fyisp

cat > ~/Library/LaunchAgents/io.github.fuck-you-isp.fyisp.plist <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>io.github.fuck-you-isp.fyisp</string>
  <key>ProgramArguments</key><array><string>$HOME/.local/bin/fyisp</string><string>--open-browser=false</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>$HOME/Library/Logs/fyisp.log</string>
  <key>StandardErrorPath</key><string>$HOME/Library/Logs/fyisp.log</string>
</dict></plist>
PLIST
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/io.github.fuck-you-isp.fyisp.plist
```

fyisp now starts whenever you log in. Its log is `~/Library/Logs/fyisp.log`. To stop it for good: `launchctl bootout gui/$(id -u)/io.github.fuck-you-isp.fyisp && rm ~/Library/LaunchAgents/io.github.fuck-you-isp.fyisp.plist`. Downloaded with `curl`, the binary carries no quarantine mark, so Gatekeeper doesn't block it.

### Windows (Task Scheduler)

In PowerShell (on an ARM PC use `fyisp-windows-arm64.exe`):

```powershell
$dir = "$env:LOCALAPPDATA\Programs\fyisp"; New-Item -ItemType Directory -Force $dir | Out-Null
$url = "https://github.com/fuck-you-isp/fyisp/releases/latest/download"
Invoke-WebRequest "$url/fyisp-windows-amd64.exe" -OutFile "$dir\fyisp.exe"
Invoke-WebRequest "$url/SHA256SUMS" -OutFile "$dir\SHA256SUMS"
$want = (Select-String -Path "$dir\SHA256SUMS" -Pattern 'fyisp-windows-amd64\.exe$').Line.Split(' ')[0]
if ((Get-FileHash "$dir\fyisp.exe").Hash -ne $want) { Remove-Item "$dir\fyisp.exe"; throw "checksum mismatch" }

$action  = New-ScheduledTaskAction -Execute "powershell.exe" -Argument "-NoProfile -WindowStyle Hidden -Command & '$dir\fyisp.exe' --open-browser=false"
$trigger = New-ScheduledTaskTrigger -AtLogOn -User $env:USERNAME
$settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([TimeSpan]::Zero) -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1)
Register-ScheduledTask -TaskName fyisp -Action $action -Trigger $trigger -Settings $settings
Start-ScheduledTask -TaskName fyisp
```

fyisp now starts, hidden, whenever you log in. To stop it for good: `Unregister-ScheduledTask -TaskName fyisp -Confirm:$false`, then end `fyisp.exe` in Task Manager.

### Linux (systemd)

```sh
curl -fsSLO https://github.com/fuck-you-isp/fyisp/releases/latest/download/fyisp-linux-amd64   # or -arm64, -armv7
curl -fsSLO https://github.com/fuck-you-isp/fyisp/releases/latest/download/SHA256SUMS
sha256sum -c SHA256SUMS --ignore-missing
sudo install -m 0755 fyisp-linux-amd64 /usr/local/bin/fyisp
sudo curl --proto '=https' --tlsv1.2 -fsSL -o /etc/systemd/system/fyisp.service \
  https://raw.githubusercontent.com/fuck-you-isp/fyisp/main/packaging/fyisp.service
sudo systemctl daemon-reload && sudo systemctl enable --now fyisp
```

fyisp now runs whenever the machine is on, from boot. [`packaging/fyisp.service`](packaging/fyisp.service) runs it as a transient unprivileged user (`DynamicUser`) with a read-only system, no access to home directories and a system-call filter; data lives in `/var/lib/fyisp`. Options go in a drop-in (`sudo systemctl edit fyisp`), e.g. `Environment=FYISP_PROFILE=default,streaming`; the comments in the unit show how to enable `--share` or `CAP_NET_RAW`.

### Docker, NAS and Raspberry Pi

The [Docker command](#docker) above already uses `--restart unless-stopped`: the container comes back after a reboot as long as Docker itself starts at boot (the default on Linux; on Docker Desktop, turn on *Start Docker Desktop when you sign in*).

## Whose fault is it? (verdicts)

Above the charts, fyisp shows one plain-English verdict, re-evaluated every 5 seconds over the last minute of probes, and keeps an outage log of every period that was not *ok*. A target is unhealthy when it loses at least 20% of its probes (and at least 2 in the minute: a single lost connect out of four is noise) or its median latency is far above its own 30-minute baseline. The first matching kind wins:

| Kind | Meaning |
|---|---|
| `no_network` | This device has no working connection (no route, every layer unreachable). |
| `lan` | The *Gateway* (your router) is unhealthy: Wi-Fi, cable or the router itself. |
| `isp` | The router is fine, the *ISP edge* (first public hop past it) is not: your ISP's access network. |
| `upstream` | Router and ISP edge are fine, but the anycast resolvers or most monitored services (in more than one group) fail: beyond your ISP. |
| `dns` | The path works, but at least 30% of the services can't be resolved. |
| `service` | Only some services are unhealthy: their problem, not yours. |
| `udp_blocked` | Everything above is fine, but UDP (STUN) to at least two independent providers has timed out 10 bursts in a row, port 53 included: calls fall back to slower TCP relays. Also a firewall on your own computer. |
| `stun_blocked` | As `udp_blocked`, but STUN on UDP port 53 still answers: the call ports are blocked, UDP itself is not. |
| `udp_degraded` | Everything above is fine, but the UDP path to at least two STUN providers has lost at least 5% of packets or jittered by 30 ms or more for 3 minutes. |
| `ok` / `warming_up` | Everything looks fine / the first minute after start. |

The UDP kinds wait 2 minutes after start (or for the first STUN answer), so a network that blocks UDP from the start is still caught, and a STUN server that is down alone never raises them. Hotel and office networks often block UDP for good: after 6 hours `udp_blocked`/`stun_blocked` is marked quiet and shown as information.

Loss on an inner link shows on every layer beyond it, so the verdict blames the innermost lossy layer: the gateway or ISP edge is blamed once it has lost at least 5 probes and 10% of them and the layers beyond it lose at least half as much. A layer further out is blamed only when its loss is significantly higher than that of the layers inside it (a two-proportion test on the minute's counts), so a few unlucky probes can't point at the wrong layer.

A new problem must hold for about 10 seconds before it is shown, and the verdict returns to *ok* only after a full minute without problems. Verdict summaries never contain addresses or host names, so they are safe on the public link. Every host name is looked up again every 60 seconds. After one failed lookup fyisp keeps probing the last good address; after two in a row (about 70 seconds into a resolver outage) the host's probes count as lost with reason *DNS*, as a real application would fail too, until a lookup succeeds again.

The layers come from the built-in **Network path** group, shown first: the *Gateway* and the *ISP edge* (found automatically from the routing table and a short traceroute; pinged every second) and three public anycast resolvers (1.1.1.1, 8.8.8.8, 9.9.9.9; ICMP and TCP). A profile passed with `--config` keeps this group unless it sets `path: false` at the top level (`--no-path` does the same for `--profile`); without it fyisp can still tell `dns`, `service` and `upstream` apart, but not `lan` from `isp`.

## Call quality

Call audio and video travel over UDP, and a TCP connect says little about them. fyisp therefore also measures the UDP path with tiny STUN requests, the same kind of packet a call app sends before every call, and answers two questions: *is my connection good for calls right now?* and *why did my call at 14:14 go bad?*

### The Calls view

Home's *Calls* tile and *Mark a bad call* are built on these measurements. **Details** has the full view: **Calls | Overview | Charts** switches between them, and Details opens on Calls whenever the profile has STUN targets (as `default` and `calls` do); otherwise on the charts, or on the Overview when the profile is large.

- **The top card** gives one answer, e.g. *● Good for calls · 32 ms · 0% loss · steady*, refreshed every few seconds with *updated 4 s ago*: the pre-call check. It is the worst of the path verdict (it never says Good while the banner shows an ISP outage, and it names that cause inline), the UDP verdicts below, your call path's score and the measured apps' scores. When it is not Good it says why, e.g. *Poor: 8% loss on your call path (worst minute 14:12)*.
- **Working hours**: a strip for Mon–Fri 09–17 local time, coloured per minute (good, fair, poor, unusable, no data), with a summary such as *Today 09–17: Good 92% · Poor 14:10–14:16*. Click a minute to ask about it.
- **What happened at …?**: pick a time (the default is the last 60 minutes) and get one sentence about the 15 minutes either side of it that names the layer at fault, e.g. *14:10–14:16: 8% loss at your ISP edge; Wi-Fi/router fine*. It uses the outage log, so it blames layers the same way the verdicts do. It links to the charts and to *Create evidence report*.
- **By app**, over the last 5 minutes: Meet (and Teams with the opt-in relay) gets a state, round trip and loss; expanding a row shows the MOS, jitter, p95 round trip and the last hour. Without the relay, Teams gets its own row: its calls take your call path, plus whether its website answers. Zoom, Slack, Discord, WhatsApp and Webex share one row (see below) that says whether their websites are reachable over TCP.
- **Mark a bad call** (the same button as on Home): one click saves a **private** note at 2 minutes ago with the call stats of that moment (state, MOS, round trip, jitter, loss; no addresses). *When?* moves it to 15 or 30 minutes ago. The note can be edited, and *Create evidence report* builds a report around it. On a phone the button sticks to the bottom of the screen.

A STUN server that stops answering while the others still answer is shown as *This test server isn't answering; your other call checks look normal*. After a start the view says *Not enough data yet* until 10 bursts have been measured.

On the public link, Details' Calls view shows only the top card, the working-hours strip and public notes: no app rows, no Teams relay region, no target names, and no Mark button.

### How it is measured

- Every STUN target gets **one long-lived UDP socket**, as a real call does (one address and port pair, so there is no new NAT mapping per sample). It is reopened only when the server's address changes or after an error.
- Each sample is a **burst of 5 STUN Binding Requests, 20 ms apart** (RFC 5389, 20 bytes each, no attributes; never TURN, never credentials). Bursts run every 5 seconds per target unless the [list below](#stun-servers) says otherwise.
- A burst records the **median round trip**, the **in-burst jitter** (the mean difference between consecutive round trips, close to what a call's jitter buffer sees) and **loss**: requests not answered within 1 s. Only timeouts count as loss. A refused port or a failed DNS lookup is *unknown*, never loss. `--timeout` does not apply to UDP.
- A burst never spans an address change, so a GeoDNS server switching region (the Teams relay) is never counted as jitter.
- Each one-minute bucket is scored on its own, and a window is judged by its **worst minute**: a 5-minute mean would hide a 30-second stall.

#### STUN servers

This is the one list of the STUN servers fyisp ships; the catalog files ([`calls-stun.yml`](internal/profile/catalog/calls-stun.yml), [`twilio.yml`](internal/profile/catalog/twilio.yml)) are the source of truth. Each server's terms are reviewed in [docs/STUN-TERMS.md](docs/STUN-TERMS.md).

| Target | Server | Every | Profile | Role |
|---|---|---|---|---|
| Meet-STUN | `stun.l.google.com:19302` | 5 s | `default`, `calls` | call path (anycast); Google Meet |
| Meet-STUN-3478 | `stun.l.google.com:3478` | 5 s | `calls` | call path; Google Meet, for networks that block 19302 |
| Cloudflare-STUN | `stun.cloudflare.com:3478` | 5 s | `default`, `calls` | call path (anycast) |
| Cloudflare-UDP53 | `stun.cloudflare.com:53` | 15 s | `default`, `calls` | call path; also tells *STUN ports blocked* from *UDP blocked* |
| Twilio-STUN-global | `global.stun.twilio.com:3478` | 5 s | `default`, `calls` | call path (GeoDNS to the nearest Twilio region) |
| Twilio-us1 … Twilio-in1 | `{us1,us2,ie1,de1,sg1,jp1,au1,br1,in1}.stun.twilio.com:3478` | 30 s | `calls` | fixed regions, a world map of the UDP path (not in the call path) |
| Teams-Relay | `worldaz.tr.teams.microsoft.com:3478` | 5 s | `calls` (opt-in) | call path (GeoDNS to an Azure region); Microsoft Teams |

### The score

A simplified ITU-T G.107 E-model turns round trip, jitter and loss into an R factor, and R into a mean opinion score (MOS, 1 to 4.5):

```
delay = rtt/2 + 2·jitter + 10                              (one way, ms)
R     = 93.2 − delay/40            (delay < 160 ms)
R     = 93.2 − (delay − 120)/10    (delay ≥ 160 ms)
R    −= 2.0 per % loss up to 5 %, then 2.5 per % above     (R clamped to 0..100)
MOS   = 1 + 0.035·R + 7·10⁻⁶·R·(R − 60)·(100 − R)
```

The loss penalty is softer than G.711's tables because Opus forward error correction and packet-loss concealment (Meet, Teams) hide some loss. The bands are G.107's user-satisfaction bands:

| Band | MOS | R |
|---|---|---|
| Good | 4.0 and up | about 80 and up |
| Fair | 3.6 to 4.0 | 70–80 |
| Poor | 3.1 to 3.6 | 60–70 |
| Unusable | below 3.1 | below 60 |

*Your call path* combines the anycast and GeoDNS STUN servers marked *call path* above: each minute takes the median of the servers' round trip, jitter and loss, so one bad server does not make the path bad.

### Which apps are measured

| App | Measured over | Profile |
|---|---|---|
| Google Meet | `stun.l.google.com`, the STUN server Chrome and Meet use | `default`, `calls` |
| Microsoft Teams | `default`: your call path (Teams calls take it) plus its website over TCP. With the relay: the Teams Transport Relay that Teams clients use | relay **opt-in**: `--profile default,calls` (or `--profile calls`) |
| Zoom, Slack, Discord, WhatsApp, Webex | not measurable: your call path plus their websites over TCP | `default`, `calls` |

Zoom, Slack (Amazon Chime), Discord, WhatsApp and Webex hand out a media server per meeting, only after you sign in, and those servers ignore STUN without that session. They can't be tested ahead of a call; if your call path is Good, they usually are too. fyisp still checks their websites over TCP.

The Teams relay is GeoDNS (Azure Traffic Manager), not anycast, and can switch region between lookups; the local view shows where it landed (*via Sweden Central*). It is opt-in because Microsoft does not document it as a public STUN service (see [docs/STUN-TERMS.md](docs/STUN-TERMS.md)).

### Verdicts, metrics and the off switch

- **Verdicts.** `udp_blocked`, `stun_blocked` and `udp_degraded` (see [Whose fault is it?](#whose-fault-is-it-verdicts)) fire only when the path and TCP are healthy and at least two independent STUN providers agree, so a single dead STUN server never raises one. After 6 hours a blocked verdict turns quiet: it is shown as information (*It has been this way for hours (common on hotel and office networks)*), and the Calls rows say *Blocked*, not *Unusable*. UDP loss never feeds the main verdict or a service's loss.
- **Metrics.** `fyisp_udp_rtt_seconds`, `fyisp_udp_jitter_seconds` and `fyisp_udp_loss_ratio` per STUN target, and `fyisp_call_mos{app}`; see [Prometheus metrics](#prometheus-metrics).
- **`--no-udp`** leaves out every STUN target and hides the Calls view. Nothing else changes.

### Limits

- **Anycast targets measure the path to the nearest edge.** Anycast and GeoDNS servers answer from a data centre near you, not from the meeting's media server or the other people on the call. Most call problems are on your own connection, which is exactly that path; a problem beyond the provider's edge is not seen.
- **Idle probes miss bufferbloat.** Calls often break when the link is busy (an upload, a backup, someone streaming), because queues only fill under load. fyisp's probes are tiny and see a quiet link, so a call can be bad while the strip is green. Mark the call, and compare with the TCP charts at that time.
- **IPv4 only.** STUN is measured over IPv4; a call that goes over IPv6 takes a path fyisp doesn't measure.
- The score is a model, not the app's own rating: expect it to agree on the band, not the decimal.

## Speed tests

fyisp can measure your download and upload speed around the clock, so you can see whether your connection gets slower at certain times of day (the classic evening dip when everyone streams), and whose network the slowdown is in. **It is off by default.** Switch it on with `--speedtest`, or pick a mode in Details → Speed (the choice is saved). "Test now" queues one test, and "Test every server" a sweep of every server, in any mode. Tests run one at a time from a single queue (scheduled ones included), and results you asked for are kept out of the time-of-day patterns.

```sh
fyisp --speedtest                                  # mode regions: 9 regions, each once an hour
fyisp --speedtest=regions,add=canada,us-northwest  # plus opt-in regions
fyisp --speedtest=nearby                           # 3 nearby providers and 1 far region, every 15 minutes
fyisp --speedtest=nearby,far=us-west               # plus another far region
fyisp --speedtest-server 192.168.1.20:5201 --speedtest=every=5m   # your own iperf3 server: no minimum
fyisp --speedtest-server Leaseweb-Tokyo,OVH-Sydney                 # chosen catalog servers (mode custom)
```

**Modes:**
- **`regions`**, the default once switched on: 9 regions (UK, Western Europe, Central Europe, US East, US Central, US West, East Asia, Southeast Asia and Oceania), each tested once an hour.
  - Only one test runs at a time; the regions are spread over the hour, about 400 seconds apart, in an order and at minutes that differ per install, with a minute of jitter on each hour's start.
  - Each region has servers from several providers in the same city. The provider rotates by hour and day, so every hour of the day is measured with every provider within a few days.
  - Opt-in regions (`add=`): US Northwest, US Mid-Atlantic, US Midwest, US Southeast, Canada, US Bay Area, South America, Central Asia, Netherlands and UK North.
  - Results show whether a dip is your line (every region and provider at once), one provider, one route or one server.
- **`nearby`**: the nearest server of 3 different providers (same city preferred), plus one far region (US East from Europe, London from the US; `far=` adds more), each every 15 minutes. The nearby servers are picked again daily. It is lighter on distance, heavier on frequency.
- **`custom`**: the servers you list with `--speedtest-server`, catalog ids or `host:port` of your own iperf3 server, each every 15 minutes.
- `every=` sets the interval per region or server (at least 15 minutes against public servers).

**What is sent, to whom.** Each test is an iperf3 test over IPv4, built into fyisp, so nothing has to be installed:
- download, then upload, each with 8 parallel TCP streams;
- the ramp-up (until the speed levels off, decided afterwards) is left out; a direction runs at most about 11 seconds, ramp-up included;
- then 3 to 10 seconds are measured, stopping once the speed has levelled off and held steady for 3 seconds (a second within 5% of the one before): up to 10 seconds each way, whatever the data; tests started from your share link are the same.
- speeds are what TCP delivers (goodput): about 4–6% below the line rate your ISP sells, because of packet headers. A 1 Gbit/s plan reads about 940–960 Mbit/s at best.

The servers are public iperf3 servers run by hosting companies and volunteers: Leaseweb (16), OVH (12), Clouvider (9), InterServer (3), Hurricane Electric, Scaleway, Eranium, Uztelecom and Edgoo. All 45 are listed with their sources in [`internal/profile/catalog/iperf3/`](internal/profile/catalog/iperf3/), with the regions in `regions.yml`. The list is built into fyisp and changes only with a release. Like every probe target, they see your public IP address.

**How much it costs your line.** A speed test fills the line while it runs, by design:
- **Time under test:** mode `regions` keeps the line under test about 5% of the time (9 tests of 10 to 25 seconds an hour); `nearby` about 7%. Details shows the planned share for each mode. fyisp warns when your settings would exceed 25%.
- **Data:** a test moves your line's speed for 4 to 11 seconds each way; there is no byte cap. Mode `regions`, the default, runs 216 tests a day:

  | Your line | Data a month (mode `regions`) |
  |---|---|
  | 25 Mbit/s | about 150 to 450 GB |
  | 100 Mbit/s | about 650 GB to 1.8 TB |
  | 1 Gbit/s | about 6.5 to 18 TB |
  | one "Test every server" sweep | Details estimates it from your recent tests |

  Mode `nearby` uses about 1.8 times as much. Details shows the data used in the last 24 hours and this month, and an estimate for each mode.
- **Fast lines:** a test still speeding up when its 10 seconds end (a far server on a fast line) shows "≥": the speed was at least this. Speed tests show when your speed drops, not whether you get your plan's advertised speed.
- **Metered lines:** leave speed tests off on metered or capped connections.

**Be kind to the servers.** The operators and what fyisp sends them are listed in [`docs/IPERF3-TERMS.md`](docs/IPERF3-TERMS.md); [`docs/OPERATORS.md`](docs/OPERATORS.md) is for the operators themselves. Each server port runs one test at a time.
- If a port is busy, or refuses for a moment (a stock iperf3 server re-listens after every test), fyisp tries up to 2 more ports.
- Then it moves to another provider in the same city, then to a nearby city. Single-port servers (Hurricane Electric) are never retried. If nothing answers, it records **"server busy"**, never a slow result.
- A slot never uses more than about 30 seconds and never delays the next one.
- A server that was busy 3 scheduled tests in a row rests for 6 hours.
- `--speedtest-skip=<id,…>` (or the toggle per server in Details → Speed) never tests a server again: honour an operator's request without waiting for a release.

**One test at a time.** Scheduled tests, Test now and sweeps share one queue: your own requests first, then scheduled ones, then the share link's. A running test is never interrupted, except when you cancel it. A scheduled test that waited more than 10 minutes is dropped. Details shows the queue with estimated start times.

**Share link.** Only if you turn it on (`--speedtest-public` or the toggle in Details → Speed; off by default), visitors to the share link can queue Test now or Test every server too, within time caps: see [Sharing a public link](#sharing-a-public-link---share) below.

Please don't run modes more often than every 15 minutes against these servers. For more frequent tests, run your own: `iperf3 -s` on a VPS or another machine, then `--speedtest-server host:5201`.

**What you get.** For each test:
- download and upload speed, and how long each was measured;
- a **score** against that server's own normal (the 90th percentile of its last 14 days), so slow and fast servers compare;
- TCP retransmits;
- the region, the server, and the fallback level (`none`, `same_metro` or `nearby_city`);
- whether the server's own link was the limit (a result within 10% of its published capacity);
- **latency under load**: the round-trip time to your ISP's first router (behind your line's queue) and to your own router, in the 2 minutes before the test and during its download and upload, pinged every 150 ms; lost pings count. A big jump at the ISP's router is bufferbloat, the reason calls break while someone uploads; a jump at your own router is your home network (often Wi-Fi).
- **connection type** (wired or Wi-Fi, Linux) and, locally, **the first network past your ISP** on the way to each server (a traceroute at most once a day per server).

Details → Speed shows:
- what the slowdown points to: your provider's network (every region and provider dips together, checked against your provider's first router), the link to one provider, one long-distance route, or one server's own busy hours, with the evidence and how sure it is;
- a heatmap of regions × hour of day by score, with how many tests and providers back each hour;
- a time series per region and server, a table, and compare;
- a note when a region mostly used its fallback in the last day.

When every region and provider dips at the same time of day, Home shows one line, for example *Evenings are slower: about 40% below your daytime speed (around 7 to 11 pm). That's still plenty for video calls.* Only scheduled tests count toward patterns: Test now and sweeps don't. Reports get the finding and a speed section by region. `fyisp export --speedtests` writes every test as CSV.

**Tests don't count against your connection.** Every probe that runs during a test, or in the 2 seconds after it, measures the test. Those probes are stored as "not measured". The verdict, call scores, Home and history ignore them (and don't call them time when fyisp wasn't running), charts shade the test windows, and a speed test never starts an incident or a bad call minute. "What happened at" says when a test overlapped. `/api/status` reports the share of the last 24 hours spent under test.

Limits: one test at a time, at most 15 seconds per direction and 45 seconds per slot, retries included; IPv4 only.

## What to measure (profiles)

By default fyisp measures **22 targets** a home and a remote worker rely on, plus your own [network path](#whose-fault-is-it-verdicts) (router, your provider's first hop, three public DNS resolvers):

| Home tile | Group in Details | Targets |
|---|---|---|
| **Calls** | Call path | 4 public STUN servers ([list](#stun-servers)): Google Meet's, Cloudflare's (on its STUN port and on port 53) and Twilio's |
| **Calls** | Call & chat apps | Microsoft Teams, Zoom, Google Meet, Slack, Webex, Discord, WhatsApp |
| **Work apps** | Work apps | Outlook, the Microsoft 365 portal, Microsoft sign-in, Gmail, Google Docs, Google Drive |
| **Websites** | Websites | Cloudflare, Akamai, Fastly, Amazon CloudFront and Google's edge, the networks most websites are served from |

Each service gets a TCP connect (handshake, then close; no data, no login) every 15 seconds. The STUN servers get a burst of 5 tiny Binding Requests every 5 seconds ([Call quality](#call-quality)); `--no-udp` leaves them out. A STUN server sees your public IP address, as every other target does. Its answer contains that address too (that is what STUN is for); fyisp skips it without decoding it and never stores, logs or exports it. `fyisp profiles default` lists every target with its host.

**Everything else is opt-in.** `--profile` picks from the built-in **target catalog** (about 1,400 endpoints, one file per provider in [`internal/profile/catalog/`](internal/profile/catalog/), every endpoint checked by hand). For containers, compose files and NAS templates, the environment variable `FYISP_PROFILE` does the same; `--profile` and `--config` win over it.

```sh
fyisp profiles                       # list the profiles, with target and panel counts
fyisp profiles aws                   # the targets of a profile (region, city, country)
fyisp --profile default,streaming    # the default plus streaming (combine profiles: a union)
fyisp --profile clouds --geo eu      # only the targets of a profile in one region (na sa eu me af as oc)
FYISP_PROFILE=default,calls fyisp    # the same as --profile default,calls
docker run ... -e FYISP_PROFILE=default,streaming ghcr.io/fuck-you-isp/fyisp
```

| Profiles | What |
|---|---|
| `default` | the 22 services above: calls, work apps, websites (used without `--profile`) |
| `calls` | call quality in depth: every [STUN server](#stun-servers), including the Teams relay and 9 Twilio regions, plus the call apps over TCP; `--profile default,calls` adds it to the default |
| `streaming` | YouTube, Netflix, Spotify, Disney+, Prime Video, Twitch, Apple TV and Music (left out of the default, see [Privacy](#privacy)) |
| `common` | everyday services: calls and chat, productivity, streaming, social sites |
| `gaming` | game platforms and cloud gaming (Xbox, PlayStation, Battle.net, GeForce NOW, ...) |
| `hyperscalers`, `devclouds`, `clouds` | AWS, Google Cloud, Azure, Oracle, IBM, Alibaba, Tencent, Huawei / VPS, bare-metal, regional and object-storage clouds (DigitalOcean, Linode, Vultr, Hetzner, OVH, ...) / both; one panel per provider and region |
| `storage` | object storage clouds (Backblaze B2, Wasabi, IDrive e2, R2, ...) plus Hetzner, Linode and OVH object storage |
| `cdn`, `dns`, `dev` | CDN edges, public DNS resolvers (their DNS-over-HTTPS/TLS endpoints), developer services |
| `north-america`, `south-america`, `europe`, `middle-east`, `africa`, `asia`, `oceania` | every catalog target located there (anycast endpoints have no region and are left out) |
| `all` | every target in the catalog |
| `aws`, `gcp`, `azure`, `hetzner`, ... | every provider in the catalog is a profile of its own |

Targets outside the default show on Home in an **Other services** tile: its state counts, but it never names one of hundreds of endpoints. Target names are identities (a name keeps its history), so a target in several profiles is one series.

There is no limit on the number of targets; you can run every profile at once (`--profile all`); profiles of more than 150 targets open Details on the [Overview](#the-overview-large-profiles) instead of hundreds of charts. The cost grows roughly linearly with the target count; see [What it costs](#what-it-costs).

### What it costs

Measured on 2026-10-10 with the v1.0 `default` (Linux amd64, Docker, Frankfurt area; CPU, memory and traffic from the `/metrics` `process_*` counters over 8.6 minutes, after an 8-minute warm-up; the Network path group included), next to the v0.5 runs of the same method:

| Profile | Targets | CPU (one core) | Memory (RSS) | Download / upload | Traffic per month | Disk for 90 days |
|---|---:|---:|---:|---:|---:|---:|
| **`default` (v1.0)** | 22 + 5 (4 STUN) | **0.8%** | **74 MB** | **0.75 / 0.76 KB/s** | **4.0 GB** | **0.08 GB** |
| `default` (v0.5, 91 targets) | 91 + 5 (4 STUN) | 2.4% | 77 MB | 1.7 / 2.0 KB/s | 10 GB | 0.29 GB |
| `calls` | 31 + 5 (15 STUN) | 0.6% | 60 MB | 0.9 / 0.8 KB/s | 4.5 GB | 0.07 GB |
| `all` (TCP only, v0.4) | 1411 | 5.9% | 96 MB | 10.3 / 18.8 KB/s | 76 GB | |

- Traffic per month matters on metered connections (mobile hotspots, LTE/5G home internet, satellite): every target is checked around the clock.
- Disk is extrapolated from the stored data, not from the file size, because a young database is mostly empty 16 KB pages. The v1.0 run covered one full clock hour: its compressed sample blobs took 32.9 KB and the hourly summaries about 0.9 KB per hour, × 2,160 hours = 73 MB, plus a few percent for SQLite's pages and indexes. (The v0.5 rows were fitted from two partial hours.)
- Per 100 extra TCP targets: about 0.4% of one CPU core, 0.7 KB/s of download and 1.3 KB/s of upload (a TCP connect sends more small packets than it receives). The two always-on traceroutes of `default` (Microsoft Teams and Cloudflare) cost a little CPU on top.

A `--config` file can start from any profiles and change them:

```yaml
name: mine
extends: [aws, europe]            # or [default]
remove: [AWS-eu-south-2]
groups: [{id: home, title: Home}]
add:
  - {name: nas, host: 192.168.1.10, group: home, kinds: [icmp]}
```

Targets without `kinds` get a TCP connect to `port` (default 443), like every shipped target. Your own targets may also ask for `icmp` (ping, as for the NAS above) or `https` (a GET of `path`, timed to the first response byte); the dashboard then offers a switch between the kinds.

Profiles are defined in [`internal/profile/profiles.yml`](internal/profile/profiles.yml) as selections over the catalog (by provider, provider kind, tag and region); the file documents the format.

Missing a provider or region, or found a dead endpoint? Pull requests are welcome; see [CONTRIBUTING.md](CONTRIBUTING.md#adding-or-fixing-endpoints).

### The Overview (large profiles)

With more than 150 targets (e.g. `--profile clouds` or `all`) the dashboard opens on the **Overview** rather than one chart per panel; the **Overview | Charts** switch under the Network path panel changes view on any profile. The Overview shows one probe kind at a time (TCP, unless a `--config` file adds HTTPS or ICMP targets and you switch) over the selected time range:

- **Breadth line**: how many targets are worse than normal at the same time, across how many providers and regions. When a quarter or more of them, across several providers and regions, degrade together, the common factor is your side. It backs up the verdict and never overrides it; in the first day, before targets have a normal, it counts only packet loss.
- **Heatmap**: one row per provider (grouped by kind), one column per region. A cell is coloured by how much slower than normal its targets are (median), or by latency until half the targets have a normal (about a day); a red dot marks loss, a red cell with × heavy loss (20% or more), hatching means not measured. Click a cell or a provider to filter the table.
- **Table**: every target with its mean round-trip time over the range, its normal, the ratio, loss and a status (failing: 20% loss or more; lossy: 1% or more; very slow: 3× normal or more; slow: 1.5× or more). Sort by any column, filter by text, region or problems only. Click a row to see that target alone on its chart; Back returns to the Overview.

The Overview's window is at most 30 days (it reads every target at once). Filters, sort, view and focus are kept in the URL, so a link opens the same view; the public link shows the same Overview.

## Privacy

fyisp has no account and no telemetry, and its data stays on your machine. What others can see:

- **The services it checks** see a TCP connection from your public IP address every 15 seconds (STUN servers: a few tiny packets every 5 seconds). No data is sent, nobody logs in, no cookies.
- **Your DNS resolver** sees fyisp look up each service's name about once a minute, all day. That resolver is usually your provider's, a Pi-hole, or **on a work laptop, your employer's**, whose security tools may log every lookup. This is why the default sticks to call apps, work apps and the big CDNs: lookups for `netflix.com` or `tiktok.com` every minute from a work laptop would look like you stream all day. Streaming (`--profile streaming`) and social sites (part of `--profile common`) are opt-in for that reason.
- **On a work laptop**, keep the default, and run any extra profiles on a home machine instead: a NAS, a Raspberry Pi or a family computer on the same network sees the same connection. If your employer manages the laptop, check its policy before running monitoring tools on it. `fyisp profiles default` lists exactly which names fyisp looks up.
- **A shared report** is public to anyone with its link: the 7-day report and the charts behind it, without addresses, host names or private notes (see [Sharing a public link](#sharing-a-public-link---share)).

## ICMP permissions

fyisp never asks for admin rights. Only the Network path is pinged (the services get TCP connects, which need no permission). On Linux it uses unprivileged ping sockets, which the kernel allows for the groups in `net.ipv4.ping_group_range` (most distributions allow everyone). If fyisp logs `ICMP unavailable`, allow your group:

```sh
sudo sysctl -w net.ipv4.ping_group_range="0 2147483647"
```

or carry on without ICMP: the services and the anycast resolvers are still measured over TCP, but the gateway and the ISP edge are not, so the verdict can no longer tell `lan` from `isp`. macOS and Windows need nothing.

## Sharing a public link (`--share`)

Sharing is **off by default**. Start it with *Make a report* on Home, the share controls in Details, or `fyisp --share`; fyisp then prints exactly what becomes public. The link opens on the report: Home in the third person (*Internet quality report*, with times in absolute form and the time zone), with Details one click away.

- fyisp opens a [Cloudflare quick tunnel](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/do-more-with-tunnels/trycloudflare/) from inside the process (no `cloudflared` install, no Cloudflare account) and prints a link like `https://<random-words>.trycloudflare.com/s/<secret>/`.
- **What is public:** the report, the charts and their read-only data API, under a random 128-bit secret path. Nothing else exists on the public side: no `/metrics`, no settings, no sharing controls, and requests are rate-limited.
- **People with your link can start a trace** (*Investigate*) of your profile's targets. That, and queuing a speed test if you allow it (below), are the only things the public link can change. Only targets in your profile can be traced, never a host or address of the visitor's choosing. Each trace runs 250 rounds (about 10 minutes) and stops by itself. At most 4 run at once, and visitors can hold at most 3 of them, so you always have one. Each visitor can start one every 30 seconds and 10 an hour, and a target whose trace just finished can be traced again from the link only after 5 minutes. The hops it shows are redacted like the rest of the public link.
- **People with your link can queue a speed test** (one server, or a sweep of every server), **only if you turn this on** (`--speedtest-public` or the toggle in Details → Speed; off by default). Tests wait in one queue and run one at a time: yours first, then the scheduled ones, then the link's.
  - Each visitor can have one waiting and start one every 30 seconds (10 an hour).
  - At most 3 sweeps and 12 tests from the link wait at once.
  - The link's tests are limited by time, not data: they keep your line busy for at most 10 minutes an hour and 60 minutes a day, and a full sweep from the link waits 30 minutes after the last one. Then the link says when it can test again. (`--speedtest-public-budget` from v0.7.1 is accepted and ignored, with a warning.)
  - A sweep from the link pauses between servers whenever one of your tests or a scheduled one is waiting.
  - You can cancel or clear them in Details → Speed.
- **What is redacted:** LAN and gateway addresses, hostname, username, file paths and build details never appear on the public side. The public Calls view shows only the top card, the working-hours strip and public notes: no STUN target names and no Teams relay region (which would hint at where you are).
- The tunnel tries QUIC (UDP port 7844) first and switches to HTTP/2 over TCP port 443 after about 30 seconds if UDP is blocked. Force one with `--share-protocol quic` or `--share-protocol http2`.
- The link is random. It changes when fyisp restarts, or when the tunnel has to be re-created after 5 minutes without a connection. The browser on your machine always opens `127.0.0.1`, never the public link.
- Quick tunnels are a free Cloudflare service meant for testing and demos. They come with no uptime guarantee, are rate-limited, and are subject to [Cloudflare's terms](https://www.cloudflare.com/website-terms/). fyisp is not affiliated with Cloudflare.

## Prometheus metrics

The local listener serves `/metrics` (never on the public link). ICMP results use the same `ping_*` metric names, help, types and labels as [network_exporter](https://github.com/syepes/network_exporter). Differences: the `target` label is the hostname (the old stack used the IP it resolved once at startup), and `target_ip` is empty.

The services are measured with TCP only, so `ping_*` now covers the Network path (`Gateway`, `ISP edge` and the three anycast resolvers) plus any ICMP targets of your own `--config` file. The services' latency is `fyisp_tcp_rtt_seconds{name,target}` (the latest connect time) and their loss is `fyisp_probe_lost_total{kind="tcp"}` over `fyisp_probe_samples_total{kind="tcp"}`; point old-stack panels and alerts that used `ping_*` for services at those. fyisp never fills `ping_*` from TCP results: a connect time is not an echo reply. `fyisp_https_rtt_seconds` exists only for `--config` targets that ask for HTTPS.

UDP (STUN) targets never feed `ping_*`. Each burst counts once in `fyisp_probe_samples_total{kind="udp"}` (and `fyisp_probe_lost_total{kind="udp"}` when nothing answered); the latest burst's values are `fyisp_udp_rtt_seconds{name,target}` (median round trip), `fyisp_udp_jitter_seconds` and `fyisp_udp_loss_ratio` (0 to 1), each absent while the latest burst did not measure it.

Speed tests (see [Speed tests](#speed-tests)) add `fyisp_speed_download_bits_per_second{region,server}` and `fyisp_speed_upload_bits_per_second{region,server}`, both from the latest test against that server that measured them (`region` is empty outside mode `regions`). They also add `fyisp_speed_loaded_rtt_seconds{region,server,layer,load}`: the median RTT to your provider's first router (`layer="edge"`) or the gateway (`layer="gateway"`), before the test (`load="idle"`) and during each direction (`load="download"`, `load="upload"`); a lost probe counts as 1 s. `fyisp_speed_tests_total{region,server,outcome}` counts tests by outcome: `ok`, `busy`, `unreachable` or `failed`.

`fyisp_call_mos{app}` is the call-quality score the Calls view shows (MOS, 1 to 4.5; 4.0 and up is good, 3.6 fair, 3.1 poor, below that unusable): the worst minute of the last 5 minutes. `app="path"` is your call path, and `app="meet"` and `app="teams"` are the measured apps (Teams only with the `calls` profile). Each is absent until its STUN servers have 10 bursts in the window. With `--no-udp` there are no `fyisp_udp_*` or `fyisp_call_mos` series.

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

**Upgrading to v0.7** changes the database (a new table for speed tests). Before it does, fyisp copies the database to `fyisp.db.bak-v<old version>` in the data directory. v0.6 refuses to open the upgraded file: to go back to v0.6, stop fyisp and put the backup in place of `fyisp.db` (tests and samples since the upgrade are lost). Speed tests stay off after the upgrade.

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
