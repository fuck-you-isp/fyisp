# `/api/home`: the Home view's API

Part of the [API index](API.md).

Home answers "is my internet OK, and which apps work": an answer card, the connection strip (router → internet provider → the internet), 11 fixed app tiles with their last 2 hours, the speed, and the problems of the last 7 days. It is built from the tiles engine (`internal/tiles`), not from the store, and every sentence is written on the server (`internal/web/home_copy.go`) and pinned by golden files. The Go types are in `internal/web/home_types.go`; keep the two in sync.

## Request

```
GET api/home?part=now&tz=America/New_York
GET api/home?part=tile&id=zoom&tz=America/New_York
```

| Param | Values | Notes |
|---|---|---|
| `part` | `now`, `tile` | Anything else (or none) returns 400. |
| `id` | a tile id: `zoom`, `teams`, `meet`, `slack`, `m365`, `gws`, `github`, `dropbox`, `onedrive`, `youtube`, `netflix` | Required with `part=tile`, 400 with `part=now`. An unknown id returns 404 "unknown tile"; a tile that is `off` returns 404 "tile not monitored". |
| `tz` | an IANA zone (`Europe/Berlin`), or minutes east of UTC | Times inside sentences are written in this zone, en-US ("10:14 am"). Default: the server's zone. |

Unknown or repeated parameters return 400. Without the tiles engine (only in tests and tools) the route answers 404.

**Caching.** `part=now` is never cached whole: it is rendered from the engine's latest snapshot (every 5 s) on each request, and the part that depends on who asks (the Wi-Fi wording) is decided per request. Only these are reused: the problems block, per snapshot revision, zone and local date; and the stored speed tests, for 60 s or until a test starts or ends. `part=tile` keeps its chart block (`from` … `text`) per tile, zone and 5-minute bucket; its header (`state`, `word`, `reason`, `coverage`, `metrics`) follows the snapshot, so the panel never disagrees with the tile. Poll `now` every 15 s and an open panel every 60 s.

## `part=now`

Abridged from the golden `internal/web/testdata/home/zoom.now.json`:

```jsonc
{
  "at": 1791556200000,
  "last_check": 1791556196000,        // the newest measured sample; 0: none yet
  "stale": false,                     // last_check is 2 minutes old or more
  "watching_since": 1772548200000,    // the store's oldest sample (whole hours; the engine's first data within the first); 0: none
  "footer": "Watching your connection since Mar 3",
  "learning": null,                   // the first day: {"text": "fyisp is still learning what’s usual…"}
  "answer": {
    "key": "app:zoom:1791554700000:1lz2wls",  // the state plus a hash of headline+sub: one key, one text; announce on change
    "tone": "ok",                     // ok | warn | bad | stale | checking | info
    "icon": "good",                   // good | slow | down | none | info
    "tab": "! Zoom down",             // the tab title; add " – fyisp"
    "headline": "Your internet is fine. Zoom isn’t connecting.",
    "sub": "Likely cause: Zoom’s side — every other app is working normally. Teams and Google Meet are working if you need to switch.",
    "day": {"text": "Last 24 hours: 5 problems, 1 likely from your internet provider.", "link": "5 problems"}
  },
  "path": [                           // always 3; state good | slow | down | unknown
    {"id": "router", "label": "Your home router", "state": "good", "sub": "Working normally"},
    {"id": "provider", "label": "Your internet provider", "state": "good", "sub": "Working normally"},   // or "Dropped for 40 s at 10:14 am, fine now"
    {"id": "internet", "label": "The internet", "state": "good", "sub": "Working normally"}
  ],
  "path_hint": "fyisp checks from the box it runs on, up to your router. …",   // "" with the Wi-Fi wording
  "tiles": [ /* always 11, in the order of the id list above */ ],
  "speed": { /* below */ },
  "problems": {"summary": "6 problems: 1 likely from your internet provider, 1 from your home router, 4 from the apps themselves.", "items": [ /* below */ ]}
}
```

**`answer`** follows the engine's connection state, first match: stale; checking (the engine is replaying, the verdict is warming up, or no covered tile has its first word yet); a path verdict (`lan`, `isp`, `upstream`, `no_network`; tone `bad`); a connection drop in progress (`drop`); `dns`; `udp_blocked` (calls lost their fast path, not their calls: their tiles are Choppy, not Down; a block known for 6 hours or more is `info`, with icon `info` and tab "ⓘ Calls limited"); `udp_degraded`; a broad slowdown (`broad`); problems with some apps and no path verdict (`app`: green when the apps' side is to blame, amber (`warn`) when the cause is the connection, which the strip then marks, or the route calls take; a recent connection problem is appended to `sub` as "Separately, your internet dropped … and is back now."); "Working again" for 30 minutes after a connection problem ended (`working_again`, with the variants "dropped for about 40 seconds at 10:14 am", "was down for about 2 minutes (10:20–10:22 am)" and "had trouble for about 16 minutes (10:02–10:18 am)"); good. `headline` and `sub` never hold a relative time, and the key carries a hash of them, so they never change under one `key` ("Started at 10:12 am." becomes "Started yesterday at 10:12 am." after midnight: a new key). `day` is null while checking and on the first day; it says "N short problems" unless one goes on or lasted over 30 minutes ("N problems").

**`path`.** The blamed node is `down` when it lost everything ("Not answering since …"), else `slow` ("Losing some data since …", or "Hard to reach since …" for the internet); the nodes past it are `slow` ("Hard to reach through your router" or "Hard to reach through your provider"). With no network at all the router is `down` ("No network connection since …") and the others `unknown` ("Can’t check while offline"). Calls losing data at a layer no verdict blames mark it `slow` ("Losing some call data since …" when only calls suffer). A layer that is slow without a verdict reads "Having some trouble"; while checking every node is `unknown` ("Checking…"). While stale every node is `unknown` ("Last known: working"). With no problem, the newest connection drop of the last 30 minutes shows on the node where it happened. A layer fyisp can't see is `unknown`: "Can’t see your router from here", "Can’t see your provider from here" or, for the internet, "Not checked yet".

**Wi-Fi wording.** The first node is "Your Wi-Fi and router" (and `path_hint` is "") only when fyisp's own uplink is Wi-Fi (Linux), fyisp does not run in a container, and the request comes from the same machine: from a loopback address or one of the host's own, without a proxy header (`Forwarded`, `X-Forwarded-For`, `X-Real-Ip`, `Cf-Connecting-Ip`, `Tailscale-User-Login`), and not by an `--allow-host` name or the `--tunnel` name (the tunnel connects from this machine). Otherwise it is "Your home router", with the hint. The API returns only the words: no address, no interface and no flag; nothing about the viewer is logged or cached. A forwarder that adds no header (`ssh -L`, socat) reads as the same machine; only the wording is affected.

**`tiles[]`:**

```jsonc
{"id": "zoom", "name": "Zoom", "kind": "call",   // call | app | stream
 "state": "down",                                  // good | slow | down | none | off
 "word": "Down",                                   // Good | Slow | Choppy (call tiles) | Down | No data | Not checked
 "since": 1791554700000,                           // null when good, none or off
 "note": "since 10:05 am",                         // or "Dropped 40 s at 10:14 am" (a good tile, 30 minutes after a drop), or ""
 "aria": "Zoom: Down, since 10:05 am",
 "reason": "Not connecting since 10:05 am. Likely cause: Zoom’s side — the general call path and everything else are normal.",
 "bars_summary": "Last 2 hours: down or dropped in 6 of 24 five-minute periods.",
 "bars": [{"from": 1791549300000, "state": "good", "note": ""} /* ×24: 5 minutes each, the last the current one */]}
```

- A tile's `reason` names a likely cause only when it is on the app's side (the app, the call path, a speed test). A router or provider cause is said once, by the card and the strip.
- Bar `state`: good | slow | down | none. Bar `note`: "Dropped 40 s", "Down since 10:05 am", "Choppy 9:41–9:45 am — likely Slack’s side", "<Slow|Choppy|Down> during a speed test", or "" (the client then writes the word). The client writes the bar's times.
- An `off` tile (the profile has no target for it) has `"bars": []`, an `enable_hint` ("To check GitHub, add services-dev to fyisp’s --profile setting (FYISP_PROFILE in Docker).", or with a `--config` file "To check GitHub, add the tag tile:github to a target in your --config file, or base the file on extends: [default].") and no `since`. It never counts in "all N apps", the card, the tab or the problems.
- While checking, every tile that isn't off is `none` with 24 `none` bars.

**`speed`:**

```jsonc
{"available": true,                                // speed tests are part of this fyisp
 "latest": {"at": 1791547200000,
            "text": "Your speed: about 930 Mbps down / 940 Mbps up — about your usual speed.",
            "note": "Today 8:00 am"},              // ", during the problem" when it ran during the card's bad problem; null: none in 7 days
 "empty_text": "No speed test yet. The first one runs within the hour.",   // "No speed test yet." while speed tests are off or their schedule is paused
 "running": null}                                  // or {"entry": 12}: a test running now, from any source
```

The usual is the median of the OK tests (both directions measured) of 7 days, the latest left out, once 3 or more of them precede the latest. Without a usual the text has no comparison ("Your speed: about 930 Mbps down / 940 Mbps up."). Each direction is judged against its own usual: under 60% is "much slower", under 85% "a bit slower", over 125% "faster". When both directions agree the sentence says it once ("much slower than usual (usually about 925 / 938 Mbps)"); otherwise it names each one ("download faster, upload much slower than usual (usually about 938 Mbps up)"). Only a slower direction gets its usual in brackets. There is no internet-plan setting and no plan text.

**`problems.items[]`** (ongoing first, then newest first; at most 100 of the last 7 days; from the same engine tick as the tiles):

```jsonc
{"id": "t-1791554700000-zoom",   // "c-<startms>" for the connection, "t-<startms>-<tiles>" otherwise: stable across restarts
 "from": 1791554700000, "to": 0, "ongoing": true,
 "level": "down",                 // slow | down
 "when": "Today since 10:05 am (still happening)",   // "Today 9:41–9:45 am", "Today 10:14 am (about 40 seconds)", "Today 1:50 am EDT–1:10 am EST"
 "what": "Zoom not connecting.",
 "cause": "Zoom’s side",          // shown as "Likely cause: <cause>."
 "cause_kind": "app",             // provider | home | app | speedtest | other
 "evidence": "Your internet and all other apps are normal."}
```

The summary counts `provider` ("likely from your internet provider"), `home` ("from your home router", or "from your home network" when fyisp itself was offline), `app` ("from the apps themselves") and the rest ("with other causes"), so the counts add up. A blocked fast path is `slow`, with "Likely cause: a firewall or VPN on this network blocking call traffic". While checking it is "Checking…"; with no problem "No problems in the last 7 days." (on the first day "No problems since fyisp started at 9:50 am.").

## `part=tile`

Abridged from the golden `internal/web/testdata/home/zoom.tile-zoom.json`:

```jsonc
{"at": 1791556200000, "id": "zoom", "state": "down", "word": "Down",
 "reason": "Not connecting since 10:05 am. Likely cause: Zoom’s side — the general call path and everything else are normal.",
 "coverage": "Call quality is judged from the general call path; fyisp can’t test Zoom’s own servers before a meeting.",
 "metrics": [{"label": "General call path:", "value": ""},
             {"label": "Delay", "value": "21 ms", "limit": "choppy over 150 ms", "bad": false},
             {"label": "Jitter", "value": "2 ms", "limit": "over 30 ms", "bad": false},
             {"label": "Packet loss", "value": "0%", "limit": "2% or more", "bad": false}],
 "legend": "How quickly the general call path answers (lower is better)",
 "from": 1791470100000, "step": 300000,
 "ms": [17.6, 17.2, null /* ×288: 5-minute medians of the last 24 hours, the last the current bucket; null: no data */],
 "usual_ms": 18,                  // null while learning
 "bands": [{"from": ms, "to": ms, "kind": "problem", "text": "Down since 10:05 am — likely Zoom’s side"},
           {"from": ms, "to": ms, "kind": "off", "text": "fyisp wasn’t running 3:05–3:50 am"}],
 "text": "Usually about 18 ms. … Down since 10:05 am — likely Zoom’s side."}   // the chart's text alternative
```

- `metrics`: call tiles show delay, jitter and loss over the last 3 minutes against their limits (`—` without data; none for a tile judged on TCP only). When they are the general call path's (Zoom, Slack), a first row `{"label": "General call path:", "value": ""}` says so, and `legend` names the chart's line ("How quickly the general call path answers (lower is better)"; absent otherwise). App tiles show `Usual` and `Slow above`; stream tiles those and "Only marked Down if it stops loading"; app and stream tiles show none while learning.
- `coverage` says what the figures are judged on when it isn't the app's own servers: the general call path (Zoom, Slack; Teams and Meet without their own STUN targets), "Can connect; call quality isn’t checked on this setup." (no UDP at all), and OneDrive's consumer front door. Otherwise "".
- What the chart plots: app and stream tiles the median of their endpoints' TCP connect times; call tiles their UDP delay (their own STUN targets, else the shared path; TCP when there is no UDP). `off` bands are fyisp not running (on the first day: "Not watched before 9:50 am"). A connection problem that didn't touch the tile has no band on it; an ongoing band is worded with the tile's word. While checking, `text` is "fyisp is still checking.".

## Rules for Home's words

Pinned by `TestHomeGolden` (`checkHomeCopy`) over every scenario:
- "internet provider", never "ISP"; US spelling; typographic apostrophes (’).
- No "ms", "jitter" or "loss" outside a panel's `metrics` (and its chart's `text`); no "plan" or "fiber" anywhere.
- A cause is always "Likely cause", never "Cause:".
- `headline` and `sub` hold no relative time ("ago", "just now"); "all N apps" counts the tiles that are not off.
- Tile reasons don't repeat a router or provider cause.

## Development: scenarios, goldens and fixtures

- **`internal/web/homescen` (`Names`, `Build`)** writes each scenario as the engine's snapshot, charts and problems, plus the speed tests, the network path and the viewer: the mockup's states (good, blip, isp, zoom, zoom-working-again, learning, stale), every other row of the card, the Working-again variants, drops and call quality, who is looking (wifi-same-machine, wifi-remote-viewer, container-unknown, edge-unknown), coverage (calls-profile, no-streaming, custom-config), history (week-with-problems, not-running), speed (speed-running, speed-no-usual, speed-done, speedtest-labelled) and the end of daylight saving time (dst).
- **`internal/web/testdata/home/<scenario>.now.json`** and **`<scenario>.tile-<id>.json`** pin them on Friday 2026-10-09 at 10:30 am in New York (dst: Sunday 2026-11-01), as the scenario's viewer gets them through the real handler. Rewrite them with `go test ./internal/web -run TestHomeGolden -update` and review the diff.
- **`test/ui/fixtures`** are made from those files by `test/ui/fixtures/gen.js`, which the Dockerfile's `uitest-run` stage runs before the specs, so the Playwright specs test the page against what the server says.
- **`cmd/fyisp-webdev -home <scenario>`** serves a scenario through the real `/api/home`, moving with the clock; the rest of the dashboard runs on the fake probes, and speed tests are the fake scheduler's. `test/ui/specs/home-webdev.spec.js` opens every scenario that way.
