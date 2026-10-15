# What to measure

Which apps Home's tiles check, the built-in profiles, the Overview for large profiles, and the `--config` file. Back to the [README](../README.md).

## Which apps are measured

Home has 11 fixed tiles, always in this order. With the default profile every one is checked:

| Tile | Kind | What fyisp checks (default) |
|---|---|---|
| Zoom | call | its website (`zoom.us`) over TCP; call quality from your [call path](CALL-QUALITY.md#stun-servers) |
| Microsoft Teams | call | its website (`teams.microsoft.com`) over TCP; call quality from the Teams relay that Teams clients use (`worldaz.tr.teams.microsoft.com`) |
| Google Meet | call | its website (`meet.google.com`) over TCP; call quality from `stun.l.google.com`, the STUN server Chrome and Meet use |
| Slack | call | its connection endpoint (`wss-primary.slack.com`) over TCP; call quality from your call path |
| Microsoft 365 | app | Outlook, the Microsoft 365 portal and Microsoft sign-in (`outlook.office365.com`, `www.microsoft365.com`, `login.microsoftonline.com`) |
| Google Workspace | app | Gmail, Google Docs and Google Drive (`mail.google.com`, `docs.google.com`, `drive.google.com`) |
| GitHub | app | `github.com` (the catalog target is named `dev-GitHub`) |
| Dropbox | app | `www.dropbox.com` |
| OneDrive | app | consumer OneDrive's front door (`skyapi.onedrive.live.com`); work OneDrive on your company's SharePoint isn't checked |
| YouTube | stream | `www.youtube.com` |
| Netflix | stream | `www.netflix.com` |

- **Several targets per tile** are judged by majority, so one target that fails alone doesn't make Microsoft 365 or Google Workspace *Down*.
- **Zoom and Slack** (whose huddles run on Amazon Chime), like Discord, WhatsApp and Webex, hand out a media server per meeting, only after you sign in, and those servers ignore STUN without that session. They can't be tested ahead of a call, so their call quality is judged from your general call path (the panel says so); if your call path is Good, they usually are too.
- **The Teams relay** is the Microsoft server Teams calls go through; fyisp sends it tiny call-setup (STUN) packets to judge Teams' call quality. (It is GeoDNS, Azure Traffic Manager, not anycast, and can switch region between lookups; the Calls view shows where it landed, *via Sweden Central*.) Microsoft does not document it as a public STUN service (see [STUN-TERMS.md](STUN-TERMS.md)), but it is in the default. To leave it out, use a `--config` file with `extends: [default]` and `remove: [Teams-Relay]`: Teams is then judged on your call path plus its website.
- **Not on Home:** the networks most websites are served from (Cloudflare, Akamai, Fastly, CloudFront, Google's edge) and Webex, Discord and WhatsApp. They are still checked, count toward the [verdicts](VERDICTS.md#whose-fault-is-it-verdicts), and have their charts under Technical details, as does every target of the other [profiles](#profiles).
- **Tiles your profile doesn't cover** say *Not checked*, with how to turn them on. `--profile calls` alone covers the 4 call tiles; `services-productivity` adds Microsoft 365, Google Workspace, Dropbox and OneDrive, `services-dev` GitHub, and `streaming` YouTube and Netflix.
- **A `--config` file** feeds a tile only through a target's `tile:<id>` tag (`tile:zoom`, `tile:teams`, `tile:meet`, `tile:slack`, `tile:m365`, `tile:gws`, `tile:github`, `tile:dropbox`, `tile:onedrive`, `tile:youtube`, `tile:netflix`), never by name. Start the file from `extends: [default]` (catalog targets keep their tags), or tag your own targets. An `override` that sets `tags:` replaces all of a target's tags, so keep its `tile:` tag. Removing a target (see [Privacy](../README.md#privacy)) turns its tile *Not checked*.

## Profiles

By default fyisp measures **28 targets** a home and a remote worker rely on, plus your own [network path](VERDICTS.md#whose-fault-is-it-verdicts) (router, your provider's first hop, two public DNS resolvers):

| Group in Details | Targets | Home tiles they feed |
|---|---|---|
| Call path | 5 STUN servers ([list](CALL-QUALITY.md#stun-servers)): Google Meet's, Cloudflare's (on its STUN port and on port 53), Twilio's and the Microsoft Teams relay | call quality for Zoom, Microsoft Teams, Google Meet and Slack |
| Call & chat apps | Microsoft Teams, Zoom, Google Meet, Slack, Webex, Discord, WhatsApp | Zoom, Microsoft Teams, Google Meet, Slack |
| Work apps | Outlook, the Microsoft 365 portal, Microsoft sign-in, Gmail, Google Docs, Google Drive, GitHub, Dropbox, OneDrive | Microsoft 365, Google Workspace, GitHub, Dropbox, OneDrive |
| Websites | Cloudflare, Akamai, Fastly, Amazon CloudFront and Google's edge, the networks most websites are served from; YouTube and Netflix | YouTube, Netflix |

Each service gets a TCP connect (handshake, then close; no data, no login) every 15 seconds. The STUN servers get a burst of 5 tiny Binding Requests every 5 seconds ([Call quality](CALL-QUALITY.md)); `--no-udp` leaves them out. A STUN server sees your public IP address, as every other target does. Its answer contains that address too (that is what STUN is for); fyisp skips it without decoding it and never stores, logs or exports it. `fyisp profiles default` lists every target with its host.

**Everything else is opt-in.** `--profile` picks from the built-in **target catalog** (about 1,400 endpoints, one file per provider in [`internal/profile/catalog/`](../internal/profile/catalog/), every endpoint checked by hand). For containers, compose files and NAS templates, the environment variable `FYISP_PROFILE` does the same; `--profile` and `--config` win over it. Likewise `FYISP_LISTEN` and `FYISP_DATA_DIR` set the defaults of `--listen` and `--data-dir`.

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
| `default` | the 28 services above: calls, work apps, websites (used without `--profile`) |
| `calls` | call quality in depth: every [STUN server](CALL-QUALITY.md#stun-servers), including the Teams relay and 9 Twilio regions, plus the call apps over TCP; `--profile default,calls` adds it to the default |
| `streaming` | YouTube, Netflix, Spotify, Disney+, Prime Video, Twitch, Apple TV and Music (YouTube and Netflix are also in the default; see [Privacy](../README.md#privacy)) |
| `common` | everyday services: calls and chat, productivity, streaming, social sites |
| `gaming` | game platforms and cloud gaming (Xbox, PlayStation, Battle.net, GeForce NOW, ...) |
| `hyperscalers`, `devclouds`, `clouds` | AWS, Google Cloud, Azure, Oracle, IBM, Alibaba, Tencent, Huawei / VPS, bare-metal, regional and object-storage clouds (DigitalOcean, Linode, Vultr, Hetzner, OVH, ...) / both; one panel per provider and region |
| `storage` | object storage clouds (Backblaze B2, Wasabi, IDrive e2, R2, ...) plus Hetzner, Linode and OVH object storage |
| `cdn`, `dns`, `dev` | CDN edges, public DNS resolvers (their DNS-over-HTTPS/TLS endpoints), developer services |
| `north-america`, `south-america`, `europe`, `middle-east`, `africa`, `asia`, `oceania` | every catalog target located there (anycast endpoints have no region and are left out) |
| `all` | every target in the catalog |
| `aws`, `gcp`, `azure`, `hetzner`, ... | every provider in the catalog is a profile of its own |

Home's 11 tiles stay the same whatever you measure (see [Which apps are measured](#which-apps-are-measured)); targets of other profiles show under Technical details and count toward the [verdicts](VERDICTS.md#whose-fault-is-it-verdicts). Target names are identities (a name keeps its history), so a target in several profiles is one series.

There is no limit on the number of targets; you can run every profile at once (`--profile all`). Large profiles open Details on the [Overview](#the-overview-large-profiles) instead of hundreds of charts.

Profiles are defined in [`internal/profile/profiles.yml`](../internal/profile/profiles.yml) as selections over the catalog (by provider, provider kind, tag and region); the file documents that format. To change a profile for yourself, use a [`--config` file](#the---config-file).

Missing a provider or region, or found a dead endpoint? Pull requests are welcome; see [CONTRIBUTING.md](../CONTRIBUTING.md#adding-or-fixing-endpoints).

## The Overview (large profiles)

With more than 150 targets and no STUN targets (e.g. `--profile clouds`), **Details** opens on the **Overview** rather than one chart per panel; profiles with STUN targets (`default`, `calls`, `all`) open it on Calls. The **Overview | Charts** switch (with **Calls** first when the profile has STUN targets) changes view on any profile. The Overview shows one probe kind at a time (TCP, unless a `--config` file adds HTTPS or ICMP targets and you switch) over the selected time range:

- **Breadth line**: how many targets are worse than normal at the same time, across how many providers and regions. When a quarter or more of them, across several providers and regions, degrade together, the common factor is your side. It backs up the verdict and never overrides it; in the first day, before targets have a normal, it counts only packet loss.
- **Heatmap**: one row per provider (grouped by kind), one column per region. A cell is colored by how much slower than normal its targets are (median), or by latency until half the targets have a normal (about a day); a red dot marks loss, a red cell with × heavy loss (20% or more), hatching means not measured. Click a cell or a provider to filter the table.
- **Table**: every target with its mean round-trip time over the range, its normal, the ratio, loss and a status (failing: 20% loss or more; lossy: 1% or more; very slow: 3× normal or more; slow: 1.5× or more). Sort by any column, filter by text, region or problems only. Click a row to see that target alone on its chart; Back returns to the Overview.

The Overview's window is at most 30 days (it reads every target at once). Filters, sort, view and focus are kept in the URL, so a link opens the same view.

## The `--config` file

`--config mine.yml` loads your own profile instead of `--profile`. It can start from any profiles and change them:

```yaml
name: mine
extends: [aws, europe]            # or [default]
remove: [AWS-eu-south-2]
groups: [{id: home, title: Home}]
add:
  - {name: nas, host: 192.168.1.10, group: home, kinds: [icmp]}
```

Targets without `kinds` get a TCP connect to `port` (default 443), like every shipped target. Your own targets may also ask for `icmp` (ping, as for the NAS above) or `https` (a GET of `path`, timed to the first response byte); the dashboard then offers a switch between the kinds.

A fuller example, starting from the default:

```yaml
name: mine
extends: [default]
remove: [YouTube, Netflix]        # by target name; `fyisp profiles default` lists them
groups:
  - {id: home, title: Home}       # a new group, or a new title (and order) for an existing id
add:
  - {name: nas, host: 192.168.1.10, group: home, kinds: [icmp]}
  - {name: router-admin, host: 192.168.1.1, group: home, kinds: [https], path: /, interval: 30s}
override:
  - {name: Zoom, interval: 30s}   # change fields of a target by name
```

The top-level fields:

| Field | |
|---|---|
| `name` | a name for your profile |
| `extends` | named profiles to start from (`fyisp profiles` lists them); `add`, `remove` and `override` need it |
| `groups` | new groups (`id`, `title`, optional `order`), or a new title or order for an existing `id` |
| `add` | new targets |
| `remove` | target names to leave out; fyisp then neither checks nor looks them up |
| `override` | changes to targets by `name`: only the fields you give change. Setting `tags` replaces all of a target's tags |
| `targets` | without `extends`: the full target list |
| `path: false` | leave out the built-in Network path group (gateway, ISP edge, two anycast resolvers). Without it fyisp can't tell `lan` from `isp` |

A target's fields:

| Field | |
|---|---|
| `name`, `host`, `group` | required; a name keeps its history, so renaming starts a new series |
| `kinds` | `tcp` (the default), `https`, `icmp` or `udp` (a STUN server: needs a `port` and no other kind) |
| `port` | default 443 |
| `path` | HTTPS only, default `/` |
| `interval` | default `15s`: whole seconds, divisible by 3, dividing an hour evenly (6s, 9s, 12s, 15s, 18s, 24s, 30s, …, 3600s) and at least 5 s. ICMP runs at a third of it, so the NAS above is pinged every 5 seconds. A UDP target's interval only has to divide an hour and be at least 6 s; a file may have at most 32 UDP targets of its own |
| `trace: true` | an always-on traceroute to the target, every 5 seconds (at most 20 targets) |
| `tags` | labels such as `tile:zoom`; a `tile:<id>` tag feeds a Home tile (see [Which apps are measured](#which-apps-are-measured)) |

`@gateway` and `@isp-edge` as a `host` mean your router and your provider's first hop, found at run time; such targets must be `kinds: [icmp]` and may use a 3-second interval. The format is also described in the package comment of [`internal/profile/profile.go`](../internal/profile/profile.go).

In Docker, mount the file and pass `--config` after the image name: `-v "$PWD/mine.yml:/mine.yml:ro" ghcr.io/fuck-you-isp/fyisp --config /mine.yml`.
