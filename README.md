# fyisp

Is your internet good enough for calls and work right now, and when it isn't, is it your Wi-Fi or your internet provider? fyisp answers that in plain words and keeps 90 days of evidence: when it went wrong, for how long, and whose fault it likely was.

Run it now, with everything on (every target, calls and the Home tiles, and scheduled speed tests), on Linux or macOS:

```sh
curl --proto '=https' --tlsv1.2 -fsSL https://github.com/fuck-you-isp/fyisp/releases/latest/download/run.sh | sh -s -- --profile all --speedtest
```

Speed tests can use terabytes a month on a fast line: on a metered or capped connection, leave out `--speedtest` (see [Speed tests](#speed-tests)).

fyisp opens the dashboard in your browser; otherwise open the `http://127.0.0.1:3000/#admin=…` link it prints. Run the same command again to [update](#upgrading). Windows, Docker, other ports and options: [Quick start](#quick-start).

fyisp ("F\*\*\* You ISP") is one small program for a home or a remote worker. It runs on your computer (or a NAS or Raspberry Pi), checks your connection every few seconds, and shows the answer at <http://127.0.0.1:3000>:

- **Calls, work apps and websites.** It checks the path that call audio and video take, the call and chat apps (Teams, Zoom, Google Meet, Slack, Webex, Discord, WhatsApp), Microsoft 365, Google Workspace, GitHub, Dropbox and OneDrive, the networks most websites are served from, and YouTube and Netflix.
- **Whose fault it is.** It also checks your own router and your provider's first hop, so it can say *your Wi-Fi or router*, *your internet provider*, *the wider internet* or *one app*.
- **Every app at a glance.** 11 tiles (Zoom, Microsoft Teams, Google Meet, Slack, Microsoft 365, Google Workspace, GitHub, Dropbox, OneDrive, YouTube and Netflix), each *Good*, *Slow* (*Choppy* for calls) or *Down*, with the last 2 hours in 5-minute bars. Drops of 30 seconds or more always show.
- **When it was bad.** The problems of the last 7 days, each with a *likely cause*: your home router (or Wi-Fi), your internet provider, the internet beyond your provider, or the app itself.
- **Evidence for your provider.** Home lists every problem of the last 7 days with its time, length and likely cause, and has no addresses on it, so a screenshot is safe to send. Technical details keeps 90 days of charts, and `fyisp export` writes the raw numbers as CSV (target names, times and results; traceroute rows give the hop number, not its address).
- **Private.** No account, no telemetry, nothing to install to try it. fyisp only talks to the services it checks (and, for traceroutes, the routers on the way), your DNS resolver, and hosts you ask it to trace (plus speed-test servers and Cloudflare, if you turn those on; see [Privacy](#privacy)). Its data stays on your machine.

Technical users get every number under **Technical details**: charts per service, traceroutes, call-quality scores, the outage log, Prometheus metrics, CSV export, and 1,400 more endpoints (clouds, CDNs, gaming, regions of the world) [one flag away](docs/PROFILES.md).

> **The default is for homes and remote workers.** Plain `fyisp` makes 28 checks of calls, work apps and websites, one or more for each Home tile (see [Privacy](#privacy) to leave any out), and opens on the plain-words **Home** view. Cloud regions are one flag away: `--profile clouds` (or `aws`, `gcp`, `hetzner`, … ; see [What to measure](docs/PROFILES.md)).

## Quick start

### Linux and macOS

```sh
curl --proto '=https' --tlsv1.2 -fsSL https://github.com/fuck-you-isp/fyisp/releases/latest/download/run.sh | sh
```

fyisp opens the dashboard in your browser, signed in; otherwise open the link it prints (`http://127.0.0.1:3000/#admin=…`, see [Admin token](#admin-token)). Pass options after `sh -s --`, e.g. `... | sh -s -- --profile streaming`.

Everything on, on port 8080 instead of 3000:

```sh
curl --proto '=https' --tlsv1.2 -fsSL https://github.com/fuck-you-isp/fyisp/releases/latest/download/run.sh | sh -s -- --profile all --speedtest --listen 127.0.0.1:8080
```

Leave out `--listen` for the default port 3000. Keep `127.0.0.1:` to stay local-only; `0.0.0.0:<port>` opens it to your network, behind the [admin token](#admin-token) as always. With `--profile all`, consider `--retention 30d`: 90 days of 1,421 targets takes a lot of disk.

To update, stop fyisp (Ctrl-C) and run the same command again (see [Upgrading](#upgrading)).

The launcher (`run.sh`) is pinned to the release it came from. It downloads the matching binary into a temporary directory, **refuses to run it unless its SHA-256 matches the release's `SHA256SUMS`**, also checks the GitHub build provenance attestation if the [GitHub CLI](https://cli.github.com/) is installed and logged in, runs it, and deletes it when fyisp exits. Nothing is installed, so fyisp stops when you close the terminal: see [Keep it running](#keep-it-running).

### Windows (PowerShell)

```powershell
& ([scriptblock]::Create((irm https://github.com/fuck-you-isp/fyisp/releases/latest/download/run.ps1)))
```

Add fyisp options at the end, e.g. `... ))) --profile all --speedtest --listen 127.0.0.1:8080`. `run.ps1` does the same checks as `run.sh`.

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

Code signing is planned: free Windows signing through the [SignPath Foundation](https://signpath.org/) (for open-source projects), and Apple notarization if there is demand.

### Docker

```sh
docker run -d --name fyisp --restart unless-stopped -p 3000:3000 -v fyisp:/data ghcr.io/fuck-you-isp/fyisp
sleep 5; docker logs fyisp 2>&1 | grep -o 'http://[^ "]*#admin=[0-9a-f]*' | tail -1
```

Open the link that the second command prints, `http://127.0.0.1:3000/#admin=…`, once in your browser: the dashboard needs its admin token. If it prints nothing, fyisp is still starting: run it again. That token is new at every start of the container, which signs every browser out; to keep one, pass your own (below, and [Admin token](#admin-token)).

**On Linux, use host networking for accurate verdicts.** With Docker's default bridge network the container's default gateway is Docker's bridge (`172.17.0.1`), not your home router, so the *Gateway* layer measures the bridge (always fine) and the *ISP edge* found by the path discovery is your router's WAN side or later. A Wi-Fi or router problem then shows up one layer too far out (as *ISP* or *Upstream*). Run the container in the host's network namespace instead:

```sh
docker run -d --name fyisp --restart unless-stopped --network host -v fyisp:/data ghcr.io/fuck-you-isp/fyisp --listen=127.0.0.1:3000
```

With `--network host`, `-p` is ignored: fyisp listens on the host directly (the image's default `FYISP_LISTEN=0.0.0.0:3000` makes it reachable from your network; the `--listen=127.0.0.1:3000` above keeps it on this machine).

**Docker Desktop (macOS, Windows)** runs containers inside a Linux VM: the container's gateway is the VM's virtual network, every probe is NATed by the VM, and `--network host` means the VM's network, not your Mac's or PC's. The charts are still useful, but the Gateway and ISP edge layers (and so the *Wi-Fi/router* vs *ISP* verdicts) are not about your real network there. For layer-accurate verdicts, run the native binary on macOS and Windows (see [Quick start](#quick-start)).

Images are published for `linux/amd64`, `linux/arm64` and `linux/arm/v7`. The container runs as an unprivileged user (65532) from a `scratch` image and keeps its data in the `/data` volume. `-p 3000:3000` makes the UI reachable from your network; use `-p 127.0.0.1:3000:3000` to keep it on this machine. ICMP works without extra capabilities on Docker 20.10+, which allows unprivileged ping sockets inside containers.

fyisp options go **after the image name** (the image sets `FYISP_DATA_DIR=/data` and `FYISP_LISTEN=0.0.0.0:3000`; `--data-dir` and `--listen` after the image name override them):

```sh
# Your own admin token, kept across restarts: open the link this prints
tok=$(openssl rand -hex 16); echo "http://127.0.0.1:3000/#admin=$tok"
docker run -d --name fyisp --restart unless-stopped -p 3000:3000 -v fyisp:/data -e FYISP_ADMIN_TOKEN=$tok ghcr.io/fuck-you-isp/fyisp
```

If you open the UI by a name instead of an IP address (`http://nas.local:3000`, a Tailscale MagicDNS name, a tunnel), add `--allow-host nas.local` (repeatable or comma-separated). Otherwise fyisp refuses the request with *421 unknown host*, which protects against DNS rebinding.

## What you see: Home and Details

![fyisp Home: the answer card, the connection strip from your router to the internet, 11 app tiles with their last 2 hours (Microsoft Teams' panel open, with its 24-hour chart), and the problems of the last 7 days](docs/img/home.png)

**Home** opens first. It answers one question, *is my internet OK for calls and work, and which apps work?*, in plain words. Numbers stay in the tile panels.

- **The answer card**: one sentence, e.g. *Everything you use is working normally.*, *Your internet provider is likely having trouble.*, *Your internet is fine. Zoom isn't connecting.* or, for 30 minutes after a drop, *Working again. Your internet dropped for about 40 seconds at 10:14 am.* The line under it gives the *likely cause* and what it means for you (*Teams and Google Meet are working if you need to switch*). Below that: how many problems the last 24 hours had, and when fyisp last checked. The card's color follows your connection, not the worst app: Zoom down on Zoom's side is a green card. The browser tab's title says the same in a few words (*✓ All good*, *✕ Provider problem*).
- **Your connection**: *Your home router → Your internet provider → The internet*, each with an icon (✓, ! or ✕) and a short line: *Working normally*, *Losing some data since 10:12 am*, *Dropped for 40 s at 10:14 am, fine now*. The first box says *Your Wi-Fi and router* only when fyisp can tell it is looking at the Wi-Fi of the machine you're reading on; otherwise *Your home router* (see [Wi-Fi or router?](docs/VERDICTS.md#wi-fi-or-router)).
- **11 app tiles**: Zoom, Microsoft Teams, Google Meet and Slack (calls); Microsoft 365, Google Workspace, GitHub, Dropbox and OneDrive (apps); YouTube and Netflix (streaming). Each says *Good*, *Slow* (*Choppy* for calls) or *Down*, with *since 10:12 am* when it isn't Good, or *Dropped 40 s at 10:14 am* for 30 minutes after a drop. Under it, 24 bars show the last 2 hours in 5-minute steps: green, yellow (slow for 2 minutes or more), red (down, or a drop of 30 seconds or more) or grey (fyisp wasn't running). A tile your profile doesn't cover says *Not checked* and how to turn it on.
- **Click a tile** for its panel: why it has that word, its numbers against their limits (delay, jitter and loss for calls; the usual time and the *Slow above* limit for the others), and a 24-hour chart with the usual line, the problems and the time fyisp wasn't running. Escape or *Close* returns to the tile. *How fyisp decides Good, Slow, Choppy and Down*, under the tiles, explains the rules (see [How fyisp decides](docs/VERDICTS.md)).
- **Speed** (when [speed tests](#speed-tests) are on): your latest result in Mbps against your usual speed (once there are a few tests), and **Test speed now**, which runs one test against fyisp's built-in servers.
- **Problems in the last 7 days**: a summary by cause (*5 problems: 1 likely from your internet provider, 1 from your home router, 3 from the apps themselves*), then each problem with its time, what happened, its *likely cause* and one line of evidence (*Your router kept answering, but nothing past it did.*). A problem during one of fyisp's own speed tests says so.
- **The footer**: *Watching your connection since …* and **Technical details →**.

<img src="docs/img/home-phone.png" width="260" align="right" alt="fyisp Home on a phone, in the dark theme, during a problem at the internet provider: a red card, the provider marked on the connection strip, call tiles Choppy and the other apps Slow or Down since 8:59 pm">

On a phone the strip stacks and the tiles take 2 columns (right: a problem at the internet provider, in the dark theme).

For its first day fyisp is still learning what's usual for your connection: Home says so and flags only big problems until then. If fyisp hasn't checked for 2 minutes or more (the computer it runs on is asleep, or fyisp stopped), the card turns grey, says since when, and the tiles are dimmed.

**Technical details** (*Technical details →* on Home; the *Home | Details* switch at the top goes back) is the full instrument panel: the verdict and outage log, the [Calls view](docs/CALL-QUALITY.md) with scores per app and *What happened at …?*, the [Overview](docs/PROFILES.md#the-overview-large-profiles), charts for every service, traceroutes (*Investigate*), speed tests and CSV export.

Home's 11 tiles and what each one checks, the 28 default targets and the profiles that add about 1,400 more are in [What to measure](docs/PROFILES.md); how a tile becomes *Slow*, *Choppy* or *Down*, and how fyisp decides whose fault a problem is, in [How fyisp decides](docs/VERDICTS.md).

### Investigate (traceroutes)

*Investigate* follows one target's path hop by hop, like `mtr`. The *ICMP | TCP 443 | UDP* picker chooses the probes: ICMP echo (like `mtr`), TCP SYNs to port 443 (like `mtr -T -P 443`: the same path and treatment as web traffic, useful when routers filter or deprioritise ICMP) or UDP to port 33434 (like `mtr -u`). None needs admin rights. Each probe type is its own trace, with its own numbers; always-on traces use ICMP, and route changes are recorded for ICMP traces only. Each row is a router, with mtr's columns: *Host* (address and reverse DNS name), *ASN*, *Loss%*, *Snt* (probes sent), *Last*, *Avg*, *Best*, *Wrst* and *StDev* (round trips in ms), and a bar from best to worst with a tick at the average. *More columns* adds *P95*, *Javg* (average jitter) and *Jmax* (worst jitter), and the browser remembers the choice. Over ranges longer than 48 hours fyisp has only hourly summaries, so *Last*, *StDev* and *Jmax* are blank.

- **Loss**: the first hop where real loss starts (it continues to the destination) is marked *loss starts here*. A middle hop that drops replies while the destination doesn't is marked *not real — router rate-limiting*. The line above the table says the same in words. See [Hop loss flags](docs/TRACE-API.md#hop-loss-flags).
- **Range**: while a trace runs, and for an hour after it finishes, the numbers cover that trace (*This trace* in the range buttons). Pick another range to see more; *This trace* goes back.
- **Copy report**: the table as `mtr -rwz` text, `mtr -j` JSON or `mtr -C` CSV, for a support ticket or a forum post. The source host is always `fyisp`, never your machine's name or address. The text ends with how fyisp measured: the probe type, the round interval, and at most 1 probe a second per router; for TCP and UDP the JSON also has `type`, `port` and `probe`.
- **Any public host**: type a name or an IPv4 address in the host box to trace it with the picked probe type, like `mtr <host>`. It must resolve to public addresses only: fyisp refuses names and addresses on your LAN (private, loopback, link-local, carrier-grade NAT, multicast and the like) and says why under the box, and stops a trace whose name later resolves to one. The URL (`#t=host:example.com`) reopens it. The same limits apply as for targets; see [Hosts outside the profile](docs/TRACE-API.md#hosts-outside-the-profile).
- **Run it yourself**: the same trace as an `mtr` command for the target (`mtr -rwzbc 100 <host>`, with `-T -P 443` or `-u -P 33434` for TCP and UDP), to compare from Linux or macOS (it may need `sudo`), and `pathping -n <host>` (ICMP only) for Windows.

**Platform notes.** Tracing, in any mode, needs fyisp's ICMP socket to start (see [ICMP permissions](#icmp-permissions)); without it no trace runs, and Investigate shows every probe type as unavailable. The TCP and UDP probes themselves need no ping socket: Linux reads the routers' replies to them from the socket's error queue, and macOS gets them on its unprivileged ICMP socket. On **Windows** the default firewall drops the routers' replies to TCP and UDP probes of a standard user, so a TCP trace sends an ICMP echo request and a TCP SYN to port 443 with the same TTL at every hop and names the routers from the ICMP replies (the view says so), and UDP traces aren't offered. Traces are IPv4 only.

Each round stops after 5 silent hops in a row past the last one that answered (like `mtr --max-unknown`), or past the destination once fyisp knows how far it is.

## Keep it running

fyisp can only tell you about time when it was running. Started from a terminal or the launcher, it stops when you close that window or log out, and Home shows that time as grey bars, and as hatched bands in a tile's chart: *fyisp wasn't running*. To have it start by itself, install the binary once and register it with your system, as below. It then runs in the background whenever you're logged in (or, on Linux and Docker, whenever the machine is on) and you open <http://127.0.0.1:3000> and sign in (see [Admin token](#admin-token)).

A laptop that sleeps still leaves gaps; that's expected and never counted as a problem. For a complete record, run fyisp on something that is always on and on the same network: a NAS, a Raspberry Pi or a home server, with [Docker](#docker) or [systemd](#linux-systemd).

### macOS (LaunchAgent)

In Terminal (Apple silicon; on an Intel Mac use `fyisp-darwin-amd64`):

```sh
mkdir -p ~/.local/bin && cd ~/.local/bin
curl -fsSLO https://github.com/fuck-you-isp/fyisp/releases/latest/download/fyisp-darwin-arm64
curl -fsSLO https://github.com/fuck-you-isp/fyisp/releases/latest/download/SHA256SUMS
shasum -a 256 -c SHA256SUMS --ignore-missing && mv fyisp-darwin-arm64 fyisp && chmod +x fyisp

tok=$(openssl rand -hex 16)
cat > ~/Library/LaunchAgents/io.github.fuck-you-isp.fyisp.plist <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>io.github.fuck-you-isp.fyisp</string>
  <key>ProgramArguments</key><array><string>$HOME/.local/bin/fyisp</string><string>--open-browser=false</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>EnvironmentVariables</key><dict><key>FYISP_ADMIN_TOKEN</key><string>$tok</string></dict>
  <key>StandardOutPath</key><string>$HOME/Library/Logs/fyisp.log</string>
  <key>StandardErrorPath</key><string>$HOME/Library/Logs/fyisp.log</string>
</dict></plist>
PLIST
chmod 600 ~/Library/LaunchAgents/io.github.fuck-you-isp.fyisp.plist
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/io.github.fuck-you-isp.fyisp.plist
echo "Sign in once per browser: http://127.0.0.1:3000/#admin=$tok"
```

fyisp now starts whenever you log in, with the admin token the last line printed (it is also in the plist). Its log is `~/Library/Logs/fyisp.log`. To stop it for good: `launchctl bootout gui/$(id -u)/io.github.fuck-you-isp.fyisp && rm ~/Library/LaunchAgents/io.github.fuck-you-isp.fyisp.plist`. Downloaded with `curl`, the binary carries no quarantine mark, so Gatekeeper doesn't block it.

### Windows (Task Scheduler)

In PowerShell (on an ARM PC use `fyisp-windows-arm64.exe`):

```powershell
$dir = "$env:LOCALAPPDATA\Programs\fyisp"; New-Item -ItemType Directory -Force $dir | Out-Null
$url = "https://github.com/fuck-you-isp/fyisp/releases/latest/download"
Invoke-WebRequest "$url/fyisp-windows-amd64.exe" -OutFile "$dir\fyisp.exe"
Invoke-WebRequest "$url/SHA256SUMS" -OutFile "$dir\SHA256SUMS"
$want = (Select-String -Path "$dir\SHA256SUMS" -Pattern 'fyisp-windows-amd64\.exe$').Line.Split(' ')[0]
if ((Get-FileHash "$dir\fyisp.exe").Hash -ne $want) { Remove-Item "$dir\fyisp.exe"; throw "checksum mismatch" }

$b = [byte[]]::new(16); [Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($b)
$tok = -join ($b | ForEach-Object { $_.ToString('x2') })
$action  = New-ScheduledTaskAction -Execute "powershell.exe" -Argument "-NoProfile -WindowStyle Hidden -Command & '$dir\fyisp.exe' --open-browser=false --admin-token $tok"
$trigger = New-ScheduledTaskTrigger -AtLogOn -User $env:USERNAME
$settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([TimeSpan]::Zero) -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1)
Register-ScheduledTask -TaskName fyisp -Action $action -Trigger $trigger -Settings $settings
Start-ScheduledTask -TaskName fyisp
"Sign in once per browser: http://127.0.0.1:3000/#admin=$tok"
```

fyisp now starts, hidden, whenever you log in, with the admin token the last line printed. The hidden task has no log, so `--tunnel`, whose link is printed only to the log, needs fyisp run from a terminal instead. To stop it for good: `Unregister-ScheduledTask -TaskName fyisp -Confirm:$false`, then end `fyisp.exe` in Task Manager.

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

fyisp now runs whenever the machine is on, from boot. [`packaging/fyisp.service`](packaging/fyisp.service) runs it as a transient unprivileged user (`DynamicUser`) with a read-only system, no access to home directories and a system-call filter; data lives in `/var/lib/fyisp`. Options go in a drop-in (`sudo systemctl edit fyisp`), e.g. `Environment=FYISP_PROFILE=default,streaming`; the comments in the unit show how to pass other flags (e.g. `--speedtest`), keep one admin token across restarts, or grant `CAP_NET_RAW`. With a generated token, the sign-in link of the current run is in the journal: `sudo journalctl -u fyisp | grep -o 'http://[^ "]*#admin=[0-9a-f]*' | tail -1`; with your own, it is `http://127.0.0.1:3000/#admin=<token>`.

### Docker, NAS and Raspberry Pi

The [Docker command](#docker) above already uses `--restart unless-stopped`: the container comes back after a reboot as long as Docker itself starts at boot (the default on Linux; on Docker Desktop, turn on *Start Docker Desktop when you sign in*).

## Call quality

fyisp measures the UDP path that call audio and video take with tiny STUN requests to Google's, Cloudflare's, Twilio's and Microsoft Teams' servers, and scores each minute as a mean opinion score (MOS). The Calls view under Technical details answers *is my connection good for calls right now?* and *why did my call at 14:14 go bad?* Details: [Call quality](docs/CALL-QUALITY.md).

## Speed tests

Speed tests are **off by default**. `--speedtest` (or a mode picked in Details → Speed) tests your download and upload speed against public iperf3 servers in 15 regions (9 of them in the US), each once an hour, so you can see whether your connection slows down at certain times of day, and whose network the slowdown is in. A test fills your line while it runs: on a 100 Mbit/s line that is about 1.1 to 3 TB a month, so leave speed tests off on metered or capped connections. Modes, data use, the servers and how they're treated: [Speed tests](docs/SPEED-TESTS.md).

## What to measure (profiles)

The default measures 28 targets for calls, work apps and websites. `--profile` adds any of about 1,400 more (clouds, CDNs, gaming, regions of the world), and `--config` loads your own file that adds, removes or changes targets. `fyisp profiles` lists the profiles. Details: [What to measure](docs/PROFILES.md).

## Privacy

fyisp has no account and no telemetry, and its data stays on your machine. What others can see:

- **The services it checks** see a TCP connection from your public IP address every 15 seconds (STUN servers: a few tiny packets every 5 seconds). No data is sent, nobody logs in, no cookies. The STUN servers include **Microsoft's Teams relay** (`worldaz.tr.teams.microsoft.com`, UDP 3478): a burst of 5 tiny STUN requests every 5 seconds, so Microsoft sees your address around the clock, as Google, Cloudflare and Twilio already did.
- **Your DNS resolver** sees fyisp look up each service's name about once a minute, all day. That resolver is usually your provider's, a Pi-hole, or **on a work laptop, your employer's**, whose security tools may log every lookup. The default has one target for each of Home's app tiles, including **GitHub, Dropbox, OneDrive, YouTube and Netflix**: lookups for `youtube.com` or `netflix.com` every minute from a work laptop can look like you stream all day, and the Teams relay's name is looked up every minute too. Social sites (part of `--profile common`) and the rest of streaming (`--profile streaming`) stay opt-in.
- **To leave targets out**, start fyisp with `--config mine.yml`, a file that extends the default and removes them by name; fyisp then neither checks nor looks them up (the format is in [What to measure](docs/PROFILES.md#the---config-file)). In Docker, mount the file and pass `--config` after the image name (e.g. `-v "$PWD/mine.yml:/mine.yml:ro" ghcr.io/fuck-you-isp/fyisp --config /mine.yml`).

  ```yaml
  # mine.yml
  name: default-no-streaming
  extends: [default]
  remove: [YouTube, Netflix]      # also possible: dev-GitHub, Dropbox, OneDrive, Teams-Relay
  ```
- **On a work laptop**, keep the default (or the default without YouTube and Netflix, as above), and run any extra profiles on a home machine instead: a NAS, a Raspberry Pi or a family computer on the same network sees the same connection. If your employer manages the laptop, check its policy before running monitoring tools on it. `fyisp profiles default` lists the names fyisp looks up (Google Meet's TCP check goes to `meet.google.com`).
- **Hosts traced on request.** *Investigate* traces any public host someone signed in to your dashboard types in (within its limits: 4 at once, about 10 minutes each). That host and the routers on the way see probes from your public IP address, and your DNS resolver sees the name looked up, about once a minute while the trace runs. Its hop numbers age out with the rest of the data, but the host's name stays in the database's index of series. No other service is involved.
- **Wi-Fi or router.** To word Home's first box, fyisp reads whether its own connection is Wi-Fi (no network name, nothing about other devices) and whether the page is opened on the same machine, which is never stored, logged or returned (see [Wi-Fi or router?](docs/VERDICTS.md#wi-fi-or-router)). The connection type itself is in fyisp's log and in speed-test results.
- **`--tunnel`** (off by default) connects to Cloudflare to publish the dashboard at a random `https://….trycloudflare.com` address (see [Remote access](#remote-access)). Cloudflare sees your public IP address and your machine's local address, and it decrypts the dashboard's traffic at its edge, the admin token included. Your DNS resolver sees the lookups of `api.trycloudflare.com` and Cloudflare's `argotunnel.com` names (or fyisp asks 1.1.1.1 over TLS), and the request for a tunnel goes through `HTTPS_PROXY` when that is set. The dashboard there needs the admin token, as everywhere. [SECURITY.md](SECURITY.md#what-leaves-your-machine) lists everything the tunnel sends.
- **A screenshot or export you send** shows what it says. Home and *What happened at …?* show no addresses; the Details charts, traceroutes and `fyisp export` include your router's and your provider's addresses.

## ICMP permissions

fyisp never asks for admin rights. Only the Network path, ICMP traceroutes (Investigate, and the always-on ones of `trace: true` targets), speed tests' latency under load and your own ICMP targets use ICMP (the services get TCP connects, which need no permission). On Linux it uses unprivileged ping sockets, which the kernel allows for the groups in `net.ipv4.ping_group_range` (most distributions allow everyone). If fyisp logs `ICMP unavailable`, allow your group:

```sh
sudo sysctl -w net.ipv4.ping_group_range="0 2147483647"
```

or carry on without ICMP: the services and the anycast resolvers are still measured over TCP, but the gateway and the ISP edge are not, so the verdict can no longer tell `lan` from `isp`. macOS and Windows need nothing.

## Remote access

To show someone what happened, send a screenshot of Home; for raw data, `fyisp export` (review it first: it lists the targets you measure).

**Over HTTPS from anywhere** (your phone, or your IT team watching the dashboard): start fyisp with `--tunnel` (or set `FYISP_TUNNEL=1`). It opens a Cloudflare quick tunnel from inside fyisp, with nothing to install and no Cloudflare account, and prints the link once it is up:

```sh
fyisp --tunnel
#   Dashboard over HTTPS:  https://calm-river-words-1234.trycloudflare.com/
```

Under Docker, add `--tunnel` after the image name and read the link from the log: `docker logs fyisp 2>&1 | grep trycloudflare`. The name is random and lives as long as fyisp keeps the tunnel: if the tunnel stays down for 5 minutes, fyisp opens a new one and prints its new link, and the link stops working when fyisp stops. fyisp uses QUIC and switches to HTTP/2 by itself when a network blocks UDP. With a generated admin token, fyisp also prints the private sign-in link for that address (to the terminal; under Docker, systemd or launchd that is the log); with your own, open `https://<name>.trycloudflare.com/#admin=<token>` or paste the token into the sign-in box. Quick tunnels are Cloudflare's free service for testing and demos, without an uptime guarantee.

For a name of your own, run a tunnel or a reverse proxy yourself, for example a [cloudflared named tunnel](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/) (`cloudflared tunnel run`) or `tailscale serve 3000`, and add its host name with `--allow-host`:

```sh
fyisp --allow-host fyisp.example.com     # the name the tunnel or proxy uses
```

- The address shows the sign-in box until a browser has the [admin token](#admin-token).
- Anyone you give the token to sees the **whole dashboard as you do**, every address and host name, and can run speed tests and traces (*Investigate*), including of a host they run, which shows them your public IP address. For people outside your household, send a screenshot of Home instead.

## Admin token

Everything fyisp serves except the page itself needs the admin token: the dashboard, its API and `/metrics`, on this machine as much as from your network or through a tunnel. Pass your own with `--admin-token` or `FYISP_ADMIN_TOKEN` (at least 32 letters, digits or `- . _ ~`, e.g. from `openssl rand -hex 16`). Without one, fyisp makes a new random token at every start and logs an "open this link" URL with it, like Jupyter; started from a terminal, it opens that link in your browser.

- Open the link once per browser. The page swaps the token for an HttpOnly cookie and removes it from the address bar. The cookie holds a value derived from the token, not the token, but it signs a browser in by itself until the token changes, and browsers send it to every port of that host name: other web apps on the same host name see it and could use it to open this dashboard. Don't run fyisp next to web apps you don't trust under the same host name. The token is in the part after `#`, which browsers never send to a server, so it doesn't end up in proxy logs or `Referer` headers.
- Opening the dashboard from another machine: replace `127.0.0.1` with the server's address, e.g. `http://192.168.1.10:3000/#admin=…`. A browser without the cookie asks for the token instead.
- To sign every browser out, restart fyisp with a new token. A generated token is new at every start, so a restart always signs everyone out: for a service that restarts (Docker, systemd, a NAS), pass your own.
- Under systemd, keep the token out of the unit, which any user can read: put `FYISP_ADMIN_TOKEN=<token>` in a file only root can read and load it with `EnvironmentFile=` (see the comments in [`packaging/fyisp.service`](packaging/fyisp.service)). On Linux and macOS, `--admin-token` on the command line is visible to other users of the machine (`ps`), and `FYISP_ADMIN_TOKEN` is not; fyisp never prints either one, `-h` included.
- Scripts and Prometheus send it as `Authorization: Bearer <token>` (or `X-FYISP-Admin: <token>`); see [Prometheus metrics](#prometheus-metrics).
- The dashboard is plain HTTP: someone who can watch your network traffic could copy the token or the cookie, as with any HTTP login. Keep it on a network you trust, use `-p 127.0.0.1:3000:3000`, or reach it through `--tunnel` (HTTPS).

*Investigate* traces a profile target or any public host (never an address on your LAN), for at most 250 rounds and 4 at once.

## Upgrading

- **Launcher (`run.sh`, `run.ps1`):** stop fyisp and run the same command again. It always fetches the latest release.
- **Docker:** pull the image and recreate the container; the volume keeps your data:

  ```sh
  docker pull ghcr.io/fuck-you-isp/fyisp
  docker rm -f fyisp
  docker run -d --name fyisp --restart unless-stopped -p 3000:3000 -v fyisp:/data ghcr.io/fuck-you-isp/fyisp
  ```

  (Repeat the options you started it with.)
- **Installed binary** ([Keep it running](#keep-it-running)): download the new `fyisp-<os>-<arch>`, verify it, replace the old file and restart the service.

Upgrading keeps your history. When a release changes the database format, its first start upgrades the database in place and logs `store: database upgraded from schema vN to vM`. A database from before v0.11.0 is the exception: it is replaced with an empty one, and fyisp logs `store: database from an older fyisp (schema vN) replaced with a new empty one`. Going back to an older release never wipes data: fyisp refuses a database from a newer version and leaves it untouched. fyisp makes no backups of its database; to keep one, stop fyisp and copy `fyisp.db` (`fyisp paths` shows where it is).

fyisp does not update itself: run the launcher from a newer release, or replace the binary.

## Prometheus metrics

The dashboard serves `/metrics`: network_exporter-compatible `ping_*` series for the Network path, and fyisp's own series for TCP, UDP, call quality, the verdict, traceroutes and speed tests. Every series is listed in [API.md](docs/API.md#metrics), hop addresses included. It needs the [admin token](#admin-token), as a bearer token:

```yaml
scrape_configs:
  - job_name: fyisp
    authorization:
      credentials_file: /etc/prometheus/fyisp-token   # the token alone, readable by Prometheus
    static_configs:
      - targets: ['127.0.0.1:3000']
```

Scraping from another machine by host name also needs that name in `--allow-host`.

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

To uninstall, stop fyisp, then delete its data directory and the binary. The launchers leave no binary behind; only the data directory remains.

- **Linux:** `rm -rf ~/.local/state/fyisp` (or the directory `fyisp paths` printed).
- **macOS:** `rm -rf ~/Library/Application\ Support/fyisp`.
- **Windows:** `Remove-Item -Recurse "$env:LOCALAPPDATA\fyisp"`.
- **Docker:** `docker rm -f fyisp && docker volume rm fyisp`.
- **systemd:** `sudo systemctl disable --now fyisp && sudo rm /etc/systemd/system/fyisp.service /usr/local/bin/fyisp && sudo rm -rf /etc/systemd/system/fyisp.service.d /var/lib/private/fyisp /etc/fyisp.env`.

## Building from source

Everything builds in Docker; no Go toolchain is needed on your machine.

```sh
docker build --target test .                                         # gofmt check, go vet, go test -race
docker buildx build --target dist --output type=local,dest=dist .    # the 7 release binaries + SHA256SUMS
docker build -t fyisp .                                              # the runtime image
```

[AGENTS.md](AGENTS.md) lists every other target (UI tests, launcher and image tests, regenerating goldens and fixtures) and how the code is laid out.

Hop owners (AS number and name) come from a table built into the binary, [`internal/asn/data/ip2asn-v4.bin`](internal/asn/data), generated from the [iptoasn.com](https://iptoasn.com/) IPv4 data; fyisp never looks them up over the network. The `asndb` target downloads the latest file and regenerates the table; releases do this automatically and fall back to the committed table if iptoasn.com is unreachable.

Add `--build-arg VERSION=v0.1.0` to stamp a version. Releases are built by [`.github/workflows/release.yml`](.github/workflows/release.yml) from a `v*` tag using the same targets, and carry [build provenance attestations](https://docs.github.com/actions/security-for-github-actions/using-artifact-attestations).

### Network-fault tests (Linux, sudo)

`test/netns/run.sh` builds fyisp in Docker, then (with sudo) builds throwaway network namespaces (client ↔ gateway ↔ ISP router ↔ internet), runs fyisp in the client namespace as uid 65532 without capabilities, and injects real faults: dropped and rejected packets, a withdrawn route, loss, delay, DNS and TLS failures, blocked UDP and restarts. It checks that the verdict blames the right layer, and that Home answers correctly in 17 scenarios (TestHome10). A full run takes about 20 minutes; it runs nightly in CI ([`netns.yml`](.github/workflows/netns.yml)), not per pull request. The commands are in [AGENTS.md](AGENTS.md#build-and-test-docker-only).

## More docs

- [What to measure](docs/PROFILES.md): Home's tiles, the profiles, the Overview and the `--config` format
- [How fyisp decides](docs/VERDICTS.md): Good, Slow, Choppy and Down, the verdicts, and Wi-Fi or router
- [Call quality](docs/CALL-QUALITY.md) and [Speed tests](docs/SPEED-TESTS.md)
- [API](docs/API.md): every route, the `/metrics` series and the `fyisp export` columns
- [STUN servers' terms](docs/STUN-TERMS.md), [iperf3 servers' terms](docs/IPERF3-TERMS.md) and [a note for iperf3 operators](docs/OPERATORS.md)
- [SECURITY.md](SECURITY.md), [CONTRIBUTING.md](CONTRIBUTING.md) and [AGENTS.md](AGENTS.md)

## License

MIT, see [LICENSE](LICENSE). fyisp includes third-party code under its own licenses, see [NOTICE](NOTICE). IP-to-ASN data from [iptoasn.com](https://iptoasn.com/) (PDDL).
