# `/api/home`: the Home view's API

> **v0.9.0:** the new Home reads [`/api/home2`](#v2-apihome2-the-new-home-v090) (below), which replaces this API in the switch-over release; until then both are served, and the page falls back to this one when `api/home2?part=now` answers 404.

Home is built from one endpoint with two parts:
- **`live`** is the answer, the path, the tiles and the call apps. Poll it every 15 s.
- **`history`** is the timeline, the problem table and today's calls. Poll it every 60 s, and again when the span changes.

The roll-up and every string of copy are made on the server. That way the page uses the same words everywhere, and Go tests pin them (`internal/web/home*_test.go`, with golden files).

The Go types are in `internal/web/home_types.go`; keep the two in sync.

## Request

```
GET api/home?part=live&tz=120
GET api/home?part=history&span=w&tz=120
GET api/home?part=at&at=1760103900000      ("What happened at…?", "I just had a bad call")
```

The route is relative, like every other API.

| Param | Values | Default | Notes |
|---|---|---|---|
| `part` | `live`, `history`, `at` | `live` | Anything else returns 400. |
| `at` | Unix ms, within the last 90 days and not in the future | – | Required with `part=at`, 400 elsewhere. |
| `span` | `d` (today), `w` (the last 7 local days, today included) | `d` | `history` only; on `live` it returns 400. |
| `tz` | minutes **east** of UTC, an integer from -840 to 840 | `0` | Send `-new Date().getTimezoneOffset()`. The server rounds it to the nearest 15 minutes and echoes the result in `tz`. A non-integer or out-of-range value returns 400. One offset is used for the whole span, so across a DST change the day edges of older days are off by an hour; this is accepted. `live` ignores `tz` but accepts it. |

Unknown parameters return 400, like the other routes.

**Caching:**
- `live` is not cached.
- `history` is cached for 60 s, keyed by `span` and `tz`.
- Every response carries `at`, the server time it was computed at. Use it for "Checked … ago". Don't use the client's clock.

**Errors:** `{"error": "..."}` with 400, 404, 429 or 503, as for the other routes. On 503, retry after `Retry-After`.

## Vocabulary

### `level`: severity (picks the icon and colour)

| Value | Meaning | Icon |
|---|---|---|
| `good` | fine | ✓ |
| `warn` | some trouble | ! |
| `bad` | broken | ✕ |
| `info` | a known, long-standing condition (a quiet UDP block): neutral, no alarm | i |
| `checking` | warming up or not enough data | … |

### `who`: where the problem is

| Value | From | Default label |
|---|---|---|
| `none` | nothing is wrong | – |
| `home` | verdict `lan`; `no_network` is `offline` | Your Wi-Fi or router |
| `provider` | verdict `isp` | Your internet provider |
| `beyond` | verdict `upstream` | Further out on the internet |
| `dns` | verdict `dns` | Looking up names |
| `app` | verdict `service` on a Home tile's targets; one call app worse than the path | {App} |
| `calltraffic` | verdicts `udp_blocked`, `stun_blocked`, `udp_degraded`; a bad call-path score with no layer verdict | The connection calls use | Call traffic |
| `offline` | verdict `no_network` | No internet connection | The monitoring device was offline |
| `unknown` | no verdict source | – | – |

### `confidence` (with `who` home, provider, beyond, dns)

| Value | When | Wording |
|---|---|---|
| `sure` | The layer verdict is corroborated by impact the user notices: `services_loss` ≥ 5 %, the call state is poor or unusable, or `udp_degraded` coincides. | a plain statement: "The problem is with your internet provider." |
| `likely` | The path layers alone. | "most likely…" |
| `weak` | Only the router or the provider's first hop is lossy or slow while every app and the call path are fine. Hops often rate-limit pings. | "…is slow to answer our checks. Your calls and apps aren't affected." `level` is then `info`, not `warn`. |

### Call states (`answer.calls`, `tiles[calls].state`, `calls.state`, `apps[].state`)

`good`, `fair`, `poor`, `unusable`, `blocked`, `not_enough_data`, `server_not_answering`; for apps that are only reachable over the web: `reachable`, `not_answering`.

| State | `level` | Label | Tile headline |
|---|---|---|---|
| `good` | good | Good | Good for calls |
| `fair` | warn | Some glitches | Mostly fine for calls |
| `poor` | bad | Choppy | Calls may be choppy |
| `unusable` | bad | Breaking up | Calls will break up |
| `blocked` | warn, or `info` when quiet | Blocked | This network blocks call traffic |
| `not_enough_data` | checking | Not checked yet | Checking… |
| `server_not_answering` | info | Test servers down | Can't test calls right now |
| `reachable` | good | Reachable | – |
| `not_answering` | warn | Not answering | – |

### Tile states (`work`, `web`, `other`)

| State | `level` | Label | Rule |
|---|---|---|---|
| `working` | good | Working | every target healthy |
| `mostly` | warn | Mostly working | exactly 1 target unhealthy out of ≥ 3 |
| `slow` | warn | Slow | ≥ 2 targets (or the only 1–2) far above their normal; only once baselines exist, otherwise they count as working |
| `down` | bad | Not working (most get nothing through), else Unreliable | every measured target failing, or ≥ 2 failing and at least half of them (13 of 38 is `slow`, labelled "Some aren't working" / "Some aren't answering") |
| `checking` | checking | Checking… | fewer than 5 samples on most targets |

"Unhealthy" is the Overview's rule: failing (≥ 20 % loss), lossy, very slow or slow. With a single affected target out of ≥ 3, Home says "mostly working" and doesn't raise the answer.

### Tiles

Tiles come in a fixed order: `calls`, `work`, `web`, `other`. A tile with no targets is left out. Categories come from `profile.HomeCategory(target)` (`internal/profile/home.go`):
- `calls`: `udp-path` STUN targets and call apps' TCP targets (`app:*`);
- `work`, `web`: the `home:work` and `home:web` tags, else the productivity provider (work) or a CDN or the streaming provider (web);
- otherwise `other`.

The path group (gateway, ISP edge, anycast resolvers) is never in a tile.

**Relevance rule:**
- A `service` verdict changes the answer only if one of its targets is in a `calls`, `work` or `web` tile, and then only as much as the tiles it touches: a call app always makes it `warn`; one target of a tile that is `mostly` working leaves the answer `good` with `answer.hint` naming it ("M365 Outlook isn't answering from here. That's most likely a problem on their side.").
- When only `other` targets are affected, the answer stays `good` and `other.line` carries a muted line ("1 other service isn't working well from here."). The `other` tile never names targets (`names` and `affected` are empty); its note counts them: "3 of 1,330 aren't answering from here." It is shown whenever the profile has such targets (e.g. `--profile default,aws`), but it never raises the answer.
- **Exception:** when the profile has no targets in the Home tiles (e.g. `--profile clouds`), the `other` tile *is* the user's set. It then escalates like any tile, and the answer follows it: `warn` "Some services aren't answering", or `bad` when it is `down`.

## `part=live`

```jsonc
{
  "part": "live",
  "at": 1760104800000,
  "answer": {
    "level": "bad",                  // good | warn | bad | info | checking
    "who": "provider",               // see "who"
    "confidence": "sure",            // sure | likely | weak (with home, provider, beyond, dns)
    "kind": "isp",                   // raw verdict kind, "unknown" without a verdict source
    "calls": "poor",                 // the call state that fed the answer, if any
    "app": "",                       // with who=app: "teams", "meet", "zoom", ...; or a tile target's display name
    "since": 1760103660000,          // 0 when nothing is wrong or unknown
    "headline": "Calls may be choppy",
    "cause": "The problem is with your internet provider. Your Wi-Fi and router are fine.",
    "hint": "",
    "try": ["Restarting your router sometimes helps.", "If it keeps happening, make a report and send it to your provider."],
    "fact": "In the worst minute, about 1 in 7 bits of sound were lost.",
    "fact_at": 1760104320000,        // the worst minute; the client writes "In the worst minute (5:42)…"
    "announce": "Calls may be choppy. The problem is with your internet provider."
  },
  "path": [
    { "id": "device",   "label": "This device",    "state": "ok",  "words": "OK" },
    { "id": "router",   "label": "Your router",    "state": "ok",  "words": "OK",
      "tooltip": "The box that connects your home to the internet." },
    { "id": "provider", "label": "Your provider",  "state": "bad", "words": "Dropping data", "blamed": true,
      "tooltip": "The first piece of your internet provider's equipment, just outside your home." },
    { "id": "internet", "label": "The internet",   "state": "bad", "words": "Dropping data" },
    { "id": "apps",     "label": "Apps & websites","state": "warn","words": "2 apps have problems" }
  ],
  "tiles": [
    { "id": "calls", "title": "Calls", "level": "bad", "state": "poor", "label": "Calls may be choppy",
      "names": ["Meet", "Teams", "Zoom"], "more": 4, "ok": 6, "total": 7, "details": "#v=calls" },
    { "id": "work", "title": "Work apps", "level": "warn", "state": "mostly", "label": "Mostly working",
      "note": "M365 Outlook isn't answering from here. That's usually a problem on their side.",
      "names": ["M365 Outlook", "M365 Portal", "M365 Login"], "more": 3, "affected": ["M365 Outlook"],
      "ok": 5, "total": 6, "details": "#v=o&ot=work" },
    { "id": "web", "title": "Websites", "level": "good", "state": "working", "label": "Working",
      "names": ["CDN Cloudflare", "CDN Akamai", "CDN Fastly"], "more": 2, "ok": 5, "total": 5, "details": "#v=o&ot=web" }
  ],
  "other": null,                     // or { "affected": 1, "total": 84, "line": "1 other service isn't answering from here." }
  "calls": {
    "state": "poor",
    "delay": "short",                // very_short (<50 ms) | short (<150) | noticeable (<300) | long
    "dropouts": "frequent",          // none (0) | rare (<1 %) | some (<5 %) | frequent
    "smoothness": "uneven",          // steady (<10 ms jitter) | bit_uneven (<30) | uneven
    "words": "Delay short · Dropouts frequent · Smoothness uneven",
    "worst_at": 1760104320000
  },
  "apps": [
    { "app": "meet",  "title": "Google Meet", "level": "bad",  "state": "poor", "label": "Choppy" },
    { "app": "teams", "title": "Teams",       "level": "bad",  "state": "reachable", "label": "Reachable",
      "note": "Calls use your call path (Choppy)." },
    { "app": "zoom",  "title": "Zoom",        "level": "good", "state": "reachable", "label": "Reachable",
      "note": "Zoom picks a server when each meeting starts, so we can't test it ahead of time." }
  ],
  "steps": []                        // warming up only, see below
}
```

**Notes:**
- `names` and `affected` are **display names**, never hosts, and there is no `label:` field. In order of preference:
  - **the table in `home_copy.go`:** "Outlook", "Microsoft 365", "Microsoft sign-in", "Gmail", "WhatsApp" and so on. A CDN is "websites using Akamai", which sentences turn into "Some websites (those using Akamai) aren't answering".
  - **the call app's title.**
  - **the target name with `-` replaced by a space.**

  Under the Websites tile, `names` drop the "websites using" part ("Cloudflare", "Akamai"). The calls tile lists app titles.
- `details` is `#v=calls` for the calls tile and `#v=o&ot=<tile id>` for the others (the Overview filtered to that Home category). `/api/profile` gives every non-path target its category as `"home": "calls"|"work"|"web"|"other"` (`profile.HomeCategory`), so the client never repeats the rules.
- `fact` is set only when the call path or an app had a scored bad minute in the last 5 minutes. It never holds a time: show `fact_at` in the viewer's locale.
- `since` is the verdict's `since`, or the first minute of the current bad call run.
- `calls` is omitted when the profile has no `udp-path` targets; the Calls tile then has only web-reachable apps, and its state is `reachable` or `not_answering`.
- `apps` lists every call app that has a target in the profile, in the fixed order teams, meet, zoom, slack, discord, whatsapp, webex. Teams without its relay (a profile that removes or lacks Teams-Relay), and Meet or Teams while their own call servers aren't answering but others are, are `reachable`/`not_answering`, with a `note` naming the shared call path's state ("Its own call server isn't answering; your other call servers: Good." in the second case).
- **Warming up** (`answer.level = "checking"`):
  - Every tile is `checking`.
  - `fact`, `calls.words` and every number are omitted.
  - `steps` lists `router`, `provider`, `internet` and `apps`, with `done` set once that layer has samples:

    ```jsonc
    "steps": [ {"id":"router","label":"Your router","done":true}, {"id":"provider","label":"Your provider","done":true},
               {"id":"internet","label":"The internet","done":false}, {"id":"apps","label":"Apps & websites","done":false} ]
    ```

  Tiles flip from `checking` one by one as they get data; they don't wait for the verdict.

**Path node `state`:**
- `ok`, `warn` (dropping some data, or slow), `bad` (not answering, or heavy loss);
- `unknown` (no recent checks; the ISP edge not discoverable: "Can't see your provider from here");
- `affected` (a node past the culprit that shows the trouble but isn't to blame: neutral, "Affected"; "Can't check while offline" when offline);
- `finding` (discovering).

`device` is always `ok`. `apps` sums the tiles: `warn` if any tile is `warn`, and `bad` if most Home targets are failing.

## `part=history`

```jsonc
{
  "part": "history",
  "at": 1760104800000,
  "tz": 120,
  "span": "w",
  "from": 1759528800000,             // local midnight 6 days ago
  "to": 1760104800000,               // now
  "first_data": 1759600000000,       // first measurement in the store; 0 when none
  "days": [
    {
      "date": "2026-10-04",
      "from": 1759528800000, "to": 1759615200000,
      "work_from": 0, "work_to": 0,  // weekend: no working hours
      "runs": [], "off": [], "no_record": true
    },
    {
      "date": "2026-10-06",
      "from": 1759701600000, "to": 1759788000000,
      "work_from": 1759734000000, "work_to": 1759762800000,   // Mon-Fri 09:00-17:00 local
      "runs": [
        { "from": 1759749660000, "to": 1759750560000, "level": "bad", "who": "provider", "kind": "isp",
          "label": "Your provider", "source": "incident", "incident": 41,
          "text": "The problem was most likely with your internet provider. Your Wi-Fi and router looked fine." }
      ],
      "off": [ { "from": 1759701600000, "to": 1759730400000 } ]   // fyisp wasn't running
    },
    {
      "date": "2026-10-10",          // today: "to" is now
      "from": 1760047200000, "to": 1760104800000,
      "work_from": 1760079600000, "work_to": 1760108400000,
      "runs": [
        { "from": 1760103660000, "to": 1760104800000, "level": "bad", "who": "provider", "kind": "isp",
          "label": "Your provider", "source": "incident", "incident": 44, "ongoing": true },
        { "from": 1760090100000, "to": 1760090280000, "level": "warn", "who": "calltraffic", "kind": "fair",
          "label": "Call traffic", "source": "calls" }
      ],
      "off": []
    }
  ],
  "summary": "2 rough patches this week, both from your internet provider.",
  "counts": { "problems": 2, "home": 0, "provider": 2, "beyond": 0, "dns": 0, "app": 0, "calltraffic": 0, "offline": 0, "services": 0, "call_patches": 1 },
  "problems": [
    { "id": 44, "from": 1760103660000, "ongoing": true, "kind": "isp", "who": "provider",
      "where": "Your provider's network (past your router)", "peak_loss": 0.62,
      "details": "#v=calls&from=1760101860000&to=1760104800000&o=1" },
    { "id": 41, "from": 1759749660000, "to": 1759750560000, "kind": "isp", "who": "provider",
      "where": "Your provider's network (past your router)", "peak_loss": 1,
      "details": "#v=calls&from=1759747860000&to=1759752360000&o=1" }
  ],
  "work": { "measured": 412, "bad": 9, "good": 0.978 }      // today in working hours; span d and w
}
```

**Notes:**
- **`days`:**
  - One per local day, oldest first: 1 for `span=d`, 7 for `span=w`.
  - Days are cut at local midnight using the rounded `tz`.
  - Today's `to` is `at`.
- **`runs`** are problem stretches, never "good" stretches; the client draws everything else as fine (or off):
  - **`source: "incident"`:** outage-log incidents (any day). `level` is `bad` for no_network, lan, isp, upstream and an unusable call; `warn` otherwise. An incident that crosses midnight appears on both days, clipped.
  - **`source: "calls"`:** **today only**. Runs of poor/unusable (`bad`) or fair (`warn`) call minutes that no incident covers. Call scores are per minute and only exist for the last 48 h.
- **`off`:** spans with no measurement at all ("fyisp wasn't running"). They are never counted as bad.
  - Today they are minute-accurate.
  - Older days use the hourly tier: a partly measured hour counts as running.
  - Days, or parts of days, before `first_data` are not in `off`. A whole day before it has `no_record: true`; draw "No record yet", not hatching.
- **`runs[].text`** is the stretch in a past-tense sentence for its tooltip, with no times: an incident's place ("The problem was most likely your Wi-Fi or router."), or, for a calls-only run, "Calls had some glitches. Nothing on your side looked wrong." / "Calls were choppy. …".
- **`counts.services`** counts incidents about targets outside the Home tiles (e.g. AWS regions under `--profile aws`). They are worded "online services", never apps or websites: the summary says "with online services", the label and `where` say "Some online services". `counts.app` counts the others.
- **`counts.call_patches`** counts today's calls-only runs. They are not in `problems`, but the summary mentions them.
- **`summary`** is one sentence with no times, e.g.:
  - "No problems today." / "No problems in the last 7 days."
  - "1 problem today, from your internet provider."
  - "3 problems this week: 2 from your internet provider, 1 in your home network."
  - "Calls had 1 rough patch today, with nothing wrong on your side." (calls-only)
  - "1 problem today, from your internet provider. Calls also had 2 other rough patches today."

  Show the run times from `runs`/`problems` next to it.
- **`counts`:** incidents in the span by `who`; `problems` is the total.
- **`problems`:** the outage log in Home's words, newest first, at most 50.
  - Home-network incidents are listed too.
  - `where` is the `who` in a short phrase. `details` opens the Details Calls view zoomed to the incident ±30 min, with the outage log open.
- **`work`:** today's working hours (Mon–Fri 09:00–17:00 local) so far. `good` is the share of scored minutes that were good or fair. It is omitted on weekends, before 09:00, and without `udp-path` targets.

## `part=at`

"What happened around this time?": at ± 15 minutes (up to now; at least 5 minutes). It is used by "What happened at…?" and by the result of "I just had a bad call". It is cached like `live`.

```jsonc
{
  "part": "at",
  "at": 1760104800000,               // server time of the answer
  "time": 1760103900000,             // the picked time
  "from": 1760103000000, "to": 1760104800000,
  "measured": true,                  // the call path was scored in at least one minute of the window
  "calls": "poor",                   // the worst scored minute's call state
  "bad_from": 1760103360000,         // the span of the minutes in that state, when it is not good
  "bad_to": 1760103900000,
  "level": "bad", "who": "provider",
  "kind": "isp",                     // the incident that explains it, if any
  "headline": "Calls would have been choppy",
  "cause": "The problem was most likely with your internet provider. Your Wi-Fi and router looked fine.",
  "fact": "In the worst minute, about 1 in 7 bits of sound were lost.",
  "fact_at": 1760103720000
}
```

**Headlines:**
- `good`: "Your connection was good for calls then"
- `fair`: "Mostly fine for calls, with a few glitches"
- `poor`: "Calls would have been choppy"
- `unusable`: "Calls would have broken up"
- `blocked`: "This network was blocking call traffic"
- nothing scored: "We weren't checking at that time", or, with an incident, "We weren't checking calls then, but we did see a problem"

**Causes:**
- **An incident in the window:** the worst relevant incident names the place, in the past tense: "…most likely your Wi-Fi or router", "…most likely with your internet provider", "…further out on the internet" and so on.
- **Good, and no incident:** the honest "If the call was bad, the problem was probably the app or the other person's connection."
- **Bad, and no incident:** "We didn't see a cause on your side: your Wi-Fi, router and provider looked fine."

Write the times from `bad_from`/`bad_to` (or `time`) yourself.

## Removed in v0.8.0

The share link's third-person voice is gone: the `voice` parameter (now a 400, like any unknown parameter) and the `voice` field of every part, the share link's weekly report (the same `part=history&span=w` read in that voice), and the `sides` and `coverage` fields of `part=history`, which only that report read. `answer.try` and every copy field speak to the owner. For something to send to others, build a redacted evidence report (`fyisp report --redact`, or Reports in the dashboard).

## Rules for Home's words

**A path verdict names the place in the headline:**
- `lan`: "There's a problem in your home network"
- `isp`: "Your internet provider is having trouble" ("may be" when only likely)
- `no_network`: "You're offline"
- `upstream` and `dns`: their own headlines

What it does to calls is the Calls tile's job. During such a problem, call apps that are only measured by their website show `cant_tell` ("Can't tell right now"), never Reachable or Not answering.

**The `apps` path node follows the tiles:** "OK", or which tiles have trouble ("Calls, work apps and websites affected").

**A block lasting `verdict.UDPQuietAfter` (6 h) or more is information:** its runs are `info` and it isn't counted in `counts` or the summary.

**One minute, one figure:** live and `part=at` both use whole-minute windows and the call path's worst minute, and TestHomeGolden checks it.

**ux-content's rules (applied at 527ed09 review):**
- **Culprit sub-label:** a blamed node carries `culprit`: "Problem is here" when sure (and for the device when offline), else "Problem is probably here".
- **Offline:** the headline is "No internet connection" and the device node is the culprit. Every other node, every tile and every app row say "Can't check while offline" (state or level `affected`; apps `cant_tell`).
- **Tiles during a path problem:** a tile whose trouble the path problem explains has level `affected` and label "Affected by the problem above", with no note. The Calls tile keeps its call words, except when offline.
- **Warming up:** the Calls tile and the app rows say "Checking…"; the cause is "First results in about 2 minutes."
- **App rows:**
  - Teams without its relay: "Same call quality as your other apps: Good."
  - Apps that choose a server per call have no note of their own. `apps_note` covers them in one line: "Zoom, Slack, Discord, WhatsApp and Webex choose a server when each call starts, so we can't test them ahead of time."
- **Never absolve as fact:** "look fine", "looks fine from here"; history runs and the problem table say "Most likely your provider", "Most likely your home network (Wi-Fi or router)". Incidents keep no confidence tier, so "most likely" is always used for the places in history.
- **Summaries:**
  - "1 problem today, most likely from your internet provider."
  - "1 problem today, with Outlook." (one app named when every app problem was about it)
  - "Calls had 1 rough patch today. We didn't see a cause on your side."
  - "No new problems today." while a known issue lasts.
- **Known issue:** `history.known` = {since, text: "Some call connections have been blocked since"}. The client appends the formatted `since`. The quiet calls tile says "Some call connections blocked".
- **Quantities:**
  - Counts, not "many" ("13 of 38 aren't answering reliably from here."); "Many…" only when a tile is down (at least half).
  - Websites "aren't loading reliably"; everything else "aren't answering reliably".
- **One app of a working tile:** the cause reads "Calls and websites are working. Outlook isn't answering from here. That's most likely a problem on their side." That is the tile's own note, so the verb is the same.

**Recovery:**
- Live tiles and call states leave out the minutes of an outage that already ended and that the verdict explained (lan, isp, upstream, dns and no_network for the tiles; those and the UDP verdicts for calls), plus one minute of grace, and the minutes before the incident started in which the verdict was still building up: 1 minute for the path kinds, 3 for a block, 4 for degraded calls. Right after an outage the apps and calls are not blamed for its samples, and blocked minutes are never scored as bad calls.
- `answer.recovered` (ms) is when such an outage ended, while the answer is good: "Back to normal since 3:12 PM".

**Weak tier:** only when the loss stays on the blamed hop. Every known layer past it must lose < 2 %; without that evidence the tier is `likely`. TCP retransmits hide loss from apps, so `services_loss` alone never makes it weak.

**The headline never repeats the cause.**
- The headline says what the user notices: "Your internet is having trouble", "Calls will break up", "Outlook and Gmail aren't answering".
- The cause says where the problem is, or whose it is: "The problem is with your internet provider…", "Your connection looks fine, so this is most likely a problem on their side."

TestHomeCopyWords and TestHomeGolden fail when the two share a run of three words.


None of these may appear in any copy field: STUN, UDP, TCP, ICMP, anycast, MOS, jitter, p95, probe, target, baseline, verdict, profile, gateway, ISP edge, ms, `×N`, or a CLI flag. `DNS` appears only in the owner's tips ("If you changed your DNS settings…"). A golden test scans every copy field of every scenario for these words.

## Development: scenarios and golden files

`internal/web/homescen` builds the Home scenarios: the default profile (`--profile aws` for `clouds-other`), a week of stored samples, and a verdict with its outage log. It covers good, warming, wifi, isp, upstream, one-app-down, calls-choppy, udp-blocked, no-network, week-with-gaps, clouds-other and teams-relay-down.

- **`cmd/fyisp-webdev -home <scenario>`** serves the **real** `/api/home` (and the rest of the dashboard) over that data, and keeps adding samples as time passes.
- **`internal/web/testdata/home/<scenario>.json`** pins every scenario: live, today, the week, and `part=at` 5 minutes ago, on Friday 2026-10-09 at 14:30 (UTC+2). This is where the copy is reviewed.
  - To rewrite the files, run `go test ./internal/web -run TestHomeGolden -update`, then review the diff.
  - The test also runs every copy field through the banned-word rules.

---

## v2: `/api/home2`, the new Home (v0.9.0)

The new Home answers "is my internet OK, and which apps work": an answer card, the connection strip (router → internet provider → the internet), 11 fixed app tiles with their last 2 hours, the speed, and the problems of the last 7 days. It is built from the tiles engine (`internal/tiles`), not from the store, and every sentence is written on the server (`internal/web/home2_copy.go`) and pinned by golden files. The Go types are in `internal/web/home2_types.go`. When v1 is removed, `/api/home2` becomes `/api/home`.

### Request

```
GET api/home2?part=now&tz=America/New_York
GET api/home2?part=tile&id=zoom&tz=America/New_York
```

| Param | Values | Notes |
|---|---|---|
| `part` | `now`, `tile` | Anything else (or none) returns 400. |
| `id` | a tile id: `zoom`, `teams`, `meet`, `slack`, `m365`, `gws`, `github`, `dropbox`, `onedrive`, `youtube`, `netflix` | Required with `part=tile`, 400 with `part=now`. An unknown id returns 404 "unknown tile"; a tile that is `off` returns 404 "tile not monitored". |
| `tz` | an IANA zone (`Europe/Berlin`), or minutes east of UTC | Times inside sentences are written in this zone, en-US ("10:14 am"). Default: the server's zone. |

Unknown or repeated parameters return 400. Without the tiles engine the route answers 404, and the page keeps the old Home.

**Caching.** `part=now` is never cached whole: it is rendered from the engine's latest snapshot (every 5 s) on each request, and the parts that depend on who asks (the Wi-Fi wording, `can_control`) are decided per request. Only these are reused: the problems block, per snapshot revision, zone and local date; and the stored speed tests, for 60 s or until a test starts or ends. `part=tile` keeps its chart block (`from` … `text`) per tile, zone and 5-minute bucket; its header (`state`, `word`, `reason`, `coverage`, `metrics`) follows the snapshot, so the panel never disagrees with the tile. Poll `now` every 15 s and an open panel every 60 s.

### `part=now`

```jsonc
{
  "v": 2, "at": 1791556200000,
  "last_check": 1791556196000,        // the newest measured sample; 0: none yet
  "stale": false,                     // last_check is 2 minutes old or more
  "watching_since": 1772548200000,    // the store's oldest sample (whole hours; the engine's first data within the first); 0: none
  "footer": "Watching your connection since Mar 3",
  "learning": null,                   // the first day: {"until": ms, "text": "fyisp is still learning what’s usual…"}
  "monitored": 11,                    // tiles that are not off
  "answer": {
    "key": "app:zoom:1791554700000:1xwl0ja",  // the state plus a hash of headline+sub: one key, one text; announce on change
    "tone": "ok",                     // ok | warn | bad | stale | checking | info
    "icon": "good",                   // good | slow | down | none | info
    "tab": "! Zoom down",             // the tab title; add " – fyisp"
    "headline": "Your internet is fine. Zoom isn’t connecting.",
    "sub": "Likely cause: Zoom’s side — every other app is working normally. Teams and Google Meet are working if you need to switch.",
    "day": {"text": "Last 24 hours: 5 short problems, 1 likely from your internet provider.", "link": "5 short problems"}
  },
  "path": [                           // always 3; state good | slow | down | unknown
    {"id": "router", "label": "Your home router", "state": "good", "sub": "Working normally"},
    {"id": "provider", "label": "Your internet provider", "state": "good", "sub": "Dropped for 40 s at 10:14 am, fine now"},
    {"id": "internet", "label": "The internet", "state": "good", "sub": "Working normally"}
  ],
  "path_hint": "fyisp checks from the box it runs on, up to your router. …",   // "" with the Wi-Fi wording
  "tiles": [ /* always 11, in the order of the id list above */ ],
  "speed": { /* below */ },
  "problems": {"summary": "6 problems: 1 likely from your internet provider, 1 from your home router, 4 from the apps themselves.", "items": [ /* below */ ]}
}
```

**`answer`** follows the engine's connection state, first match: stale; checking (the engine is replaying, the verdict is warming up, or no covered tile has its first word yet); a path verdict (`lan`, `isp`, `upstream`, `no_network`; tone `bad`); a connection drop in progress (`drop`); `dns`; `udp_blocked` (calls lost their fast path, not their calls: their tiles are Choppy, not Down; a block known for 6 hours or more is `info`, with icon `info` and tab "ⓘ Calls limited"); `udp_degraded`; a broad slowdown (`broad`); problems with some apps and no path verdict (`app`: green when the apps' side is to blame, amber (`warn`) when the cause is the connection, which the strip then marks, or the route calls take; a recent connection problem is appended to `sub` as "Separately, your internet dropped … and is back now."); "Working again" for 30 minutes after a connection problem ended (`working_again`, with the variants "dropped for about 40 seconds at 10:14 am", "was down for about 2 minutes (10:20–10:22 am)" and "had trouble for about 16 minutes (10:02–10:18 am)"); good. `headline` and `sub` never hold a relative time, and the key carries a hash of them, so they never change under one `key` ("Started at 10:12 am." becomes "Started yesterday at 10:12 am." after midnight: a new key). `day` is null while checking and on the first day; it says "N short problems" unless one goes on or lasted over 30 minutes ("N problems").

**`path`.** The blamed node is `down` when it lost everything ("Not answering since …"), else `slow` ("Losing some data since …"); the nodes past it are `slow` ("Hard to reach through your provider"). While stale every node is `unknown` ("Last known: working"). With no problem, the newest connection drop of the last 30 minutes shows on the node where it happened. A provider fyisp hasn't found yet is `unknown`: "Can’t see your provider from here".

**Wi-Fi wording.** The first node is "Your Wi-Fi and router" (and `path_hint` is "") only when fyisp's own uplink is Wi-Fi (Linux), fyisp does not run in a container, and the request comes from the same machine: from a loopback address or one of the host's own, without a proxy header (`Forwarded`, `X-Forwarded-For`, `X-Real-Ip`, `Cf-Connecting-Ip`, `Tailscale-User-Login`), and not by an `--allow-host` name. Otherwise it is "Your home router", with the hint. The API returns only the words: no address, no interface and no flag; nothing about the viewer is logged or cached. A forwarder that adds no header (`ssh -L`, socat) reads as the same machine; only the wording is affected.

**`tiles[]`:**

```jsonc
{"id": "zoom", "name": "Zoom", "kind": "call",   // call | app | stream
 "state": "down",                                  // good | slow | down | none | off
 "word": "Down",                                   // Good | Slow | Choppy (call tiles) | Down | No data | Not checked
 "since": 1791554700000,                           // null when good, none or off
 "note": "since 10:05 am",                         // or "Dropped 40 s at 10:14 am" (a good tile, 30 minutes after a drop), or ""
 "aria": "Zoom: Down, since 10:05 am",
 "reason": "Not connecting since 10:05 am. Likely cause: Zoom’s side — everything else is normal.",
 "bars_summary": "Last 2 hours: down or dropped in 6 of 24 five-minute periods.",
 "bars": [{"from": 1791547500000, "state": "good", "note": ""} /* ×24: 5 minutes each, the last the current one */]}
```

- A tile's `reason` names a likely cause only when it is on the app's side (the app, the call path, a speed test). A router or provider cause is said once, by the card and the strip.
- Bar `state`: good | slow | down | none. Bar `note`: "Dropped 40 s", "Down since 10:05 am", "Choppy 9:41–9:45 am — likely Slack’s side", "Slow during a speed test", or "" (the client then writes the word). The client writes the bar's times.
- An `off` tile (the profile has no target for it) has `"bars": []`, an `enable_hint` ("To check GitHub, add services-dev to fyisp’s --profile setting (FYISP_PROFILE in Docker).", or with a `--config` file "To check GitHub, tag a target in your --config file tile:github, or add extends: default to it.") and no `since`. It never counts in `monitored`, the card, the tab or the problems.
- While checking, every tile that isn't off is `none` with 24 `none` bars.

**`speed`:**

```jsonc
{"available": true,                                // speed tests are part of this fyisp
 "latest": {"at": 1791547200000, "down_mbps": 930, "up_mbps": 940,
            "text": "Your speed: about 930 Mbps down / 940 Mbps up — about your usual speed.",
            "note": "Today 8:00 am"},              // ", during the problem" when it ran during the card's bad problem; null: none in 7 days
 "empty_text": "No speed test yet. The first one runs within the hour.",
 "usual": {"down_mbps": 925, "up_mbps": 938},      // the median of the OK tests of 7 days, the latest left out, 3 or more; else null
 "running": null,                                  // or {"entry": 12, "expected_end": ms}: a test running now, from any source
 "can_control": true}                              // this request may start a test without the admin token
```

Without a usual the text has no comparison ("Your speed: about 930 Mbps down / 940 Mbps up."). Slower than 60% of the usual in either direction is "much slower than usual (usually about 925 / 938 Mbps)", slower than 85% "a bit slower than usual (…)", faster by a quarter in both "faster than usual". There is no internet-plan setting and no plan text.

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

### `part=tile`

```jsonc
{"v": 2, "at": 1791556200000, "id": "zoom", "state": "down", "word": "Down",
 "reason": "Not connecting since 10:05 am. Likely cause: Zoom’s side — everything else is normal.",
 "coverage": "Call quality is judged from the general call path; fyisp can’t test Zoom’s own servers before a meeting.",
 "metrics": [{"label": "Delay", "value": "21 ms", "limit": "choppy over 150 ms", "bad": false},
             {"label": "Jitter", "value": "2 ms", "limit": "over 30 ms", "bad": false},
             {"label": "Packet loss", "value": "0%", "limit": "2% or more", "bad": false}],
 "from": 1791470100000, "step": 300000,
 "ms": [18.2, 17.9, null /* ×288: 5-minute medians of the last 24 hours, the last the current bucket; null: no data */],
 "usual_ms": 18,                  // null while learning
 "bands": [{"from": ms, "to": ms, "kind": "problem", "text": "Down since 10:05 am — likely Zoom’s side"},
           {"from": ms, "to": ms, "kind": "off", "text": "fyisp wasn’t running 3:05–3:50 am"}],
 "text": "Usually about 18 ms. … Down since 10:05 am — likely Zoom’s side."}   // the chart's text alternative
```

- `metrics`: call tiles show delay, jitter and loss over the last 3 minutes against their limits (`—` without data; none for a tile judged on TCP only). When they are the general call path's (Zoom, Slack), a first row `{"label": "General call path:"}` says so, and `legend` names the chart's line ("How quickly the general call path answers (lower is better)"; absent otherwise). App tiles show `Usual` and `Slow above`; stream tiles those and "Only marked Down if it stops loading"; app and stream tiles show none while learning.
- `coverage` says what the figures are judged on when it isn't the app's own servers: the general call path (Zoom, Slack; Teams and Meet without their own STUN targets), "Can connect; call quality isn’t checked on this setup." (no UDP at all), and OneDrive's consumer front door. Otherwise "".
- What the chart plots: app and stream tiles the median of their endpoints' TCP connect times; call tiles their UDP delay (their own STUN targets, else the shared path; TCP when there is no UDP). `off` bands are fyisp not running (on the first day: "Not watched before 9:50 am"). A connection problem that didn't touch the tile has no band on it; an ongoing band is worded with the tile's word. While checking, `text` is "fyisp is still checking.".

### Rules for the new Home's words

Pinned by `TestHome2Golden` (`checkHome2Copy`) over every scenario:
- "internet provider", never "ISP"; US spelling; typographic apostrophes (’).
- No "ms", "jitter" or "loss" outside a panel's `metrics` (and its chart's `text`); no "plan" or "fiber" anywhere.
- A cause is always "Likely cause", never "Cause:".
- `headline` and `sub` hold no relative time ("ago", "just now"); "all N apps" uses `monitored`.
- Tile reasons don't repeat a router or provider cause.

### Development: scenarios, goldens and fixtures

- **`internal/web/homescen` (`V2Names`, `BuildV2`)** writes each scenario as the engine's snapshot, charts and problems, plus the speed tests, the network path and the viewer: the mockup's states (good, blip, isp, zoom, zoom-working-again, learning, stale), every other row of the card, the Working-again variants, drops and call quality, who is looking (wifi-same-machine, wifi-remote-viewer, container-unknown, edge-unknown), coverage (calls-profile, no-streaming, custom-config), history (week-with-problems, not-running), speed (speed-running, speed-no-usual, speed-done, speedtest-labelled) and the end of daylight saving time (dst).
- **`internal/web/testdata/home2/<scenario>.now.json`** and **`<scenario>.tile-<id>.json`** pin them on Friday 2026-10-09 at 10:30 am in New York (dst: Sunday 2026-11-01), as the scenario's viewer gets them through the real handler. Rewrite them with `go test ./internal/web -run TestHome2Golden -update` and review the diff.
- **`test/ui/fixtures`** are made from those files (`docker buildx build --target uifixtures --output type=local,dest=. .`), so the Playwright specs test the page against what the server says.
- **`cmd/fyisp-webdev -home2 <scenario>`** serves a scenario through the real `/api/home2`, moving with the clock; the rest of the dashboard runs on the fake probes, and speed tests are the fake scheduler's. `test/ui/specs/home-webdev.spec.js` opens every scenario that way.

### Removed in v0.9.0 (at the switch-over)

When `/api/home2` becomes `/api/home`: `part=live`, `part=history` and `part=at`; the v1 fields (`answer.who`, `confidence`, `kind`, `try`, `fact`, `fact_at`, the 5-node path, `apps`, `calls`, `other`, `steps`, `history.*`); "Mark a bad call", "What happened at…?", the timeline and the Home report button (reports stay under Technical details); the speed line by time of day and every plan setting.
