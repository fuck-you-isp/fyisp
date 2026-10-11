# Speed tests: API contract (fyisp v0.7)

iperf3 speed tests over IPv4, on a schedule and on demand. Each test runs download, then upload: 8 TCP streams, an adaptive ramp-up that is left out (until a 100 ms tick stops rising, at most 1 s), then 3 to 8 s measured, stopping once a second is within 5% of the one before. Each direction stops at 100 MB (the test is then "short"), so a test moves at most 200 MB.

Modes:
- **`off`**, the default.
- **`regions`**, the default once speed tests are on (`speedtest.DefaultMode`), hourly: the 9 default regions, each **once an hour**, one test at a time about 400 s apart. The order and the minute differ per install (a saved seed), and each hour's start has ±60 s of jitter. Each region lists same-metro servers from several providers. The first choice rotates per (region, hour of day) by day, so each hour is measured with every provider within N days. `extra` adds opt-in regions.
- **`nearby`**: the nearest server of 3 different providers (same metro preferred), plus 1 automatic far region on a major route (Europe and South America → US East; North America, Africa and the Middle East → UK; Asia and Oceania → US West) and any `extra` far regions, each every 15 minutes. The picks are redone daily.
- **`custom`**: the `--speedtest-server` list, each every 15 minutes. It is offered only with that flag.

Intervals can be changed with `--speedtest=…,every=`. The floor is 15 minutes against public servers; with only the user's own servers there is no floor.

The catalog (`internal/profile/catalog/iperf3/`: 45 servers in per-provider files, plus `regions.yml`) is built into the binary. It changes only with a release.

The Go types live in `internal/web/speed.go`; keep this file in sync with them. Golden examples: `internal/web/testdata/speed/{local,public}.json`.

Routes are relative like every other API: `/api/speed` locally, `/s/<secret>/api/speed` on the share link. Unknown parameters return 400. When fyisp runs without a speed-test source (an old build, or `fyisp-webdev -no-speed`), every route answers 404 and `/api/profile` has `features.speed: false`. Hide Speed then.

## Errors

Every API error is `{"error": "<sentence>", "code": "<word>"}`. Use `code` and never match on the sentence.

| HTTP | `code` |
|---|---|
| 400 | `bad_request` |
| 403 | `forbidden` (controls disabled, cross-origin, bad CSRF token or admin token); `disabled` (the share link's tests are off); `speed_off` (speed tests are off) |
| 409 | `skipped` (Test now of a server the user skipped) |
| 404 | `not_found` |
| 405 | `method_not_allowed` |
| 421 | `unknown_host` |
| 429 | `queue_full`; `rate_limited`, `budget_exhausted`, `time_cap` and `cooldown` (with `retry_after`, the last three with `resets_at`) |
| 503 | `unavailable` (retry after `Retry-After`) |

## Nullables

In the JSON below, `|null` marks fields that are **always present and may be null**. `(local only)` and `(omitempty)` mark fields that are **omitted** when they don't apply. Every other field is always present.

## Identity and the share link

Servers have these names:
- `id` is the catalog id, e.g. `Leaseweb-NewYork`. It is stable, URL-safe, and the same on both listeners.
- `label` is provider and city, e.g. "Leaseweb New York" or "InterServer Secaucus NJ".
- `display` is the public label "Region · Provider, City", e.g. "US East · Leaseweb, New York". It falls back to "Provider, City" for servers in no region.
- `host` and the ports are **local only**, along with every test's `port`.

The user's own servers (`--speedtest-server host:port`) have the id `host:port` locally. On the share link they appear as `custom-1`, `custom-2`, … with the label "Your server" (or "Your server 2" and so on), and the share link accepts those ids as well.

Regions have an `id` (`uk`, `eu-west`, …) and a `title` ("UK", "Western Europe", …). Neither a client address nor data use (`bytes_month` is 0) ever appears on the share link.

Wording: `finding.text`, `pattern.text`, row dips and `under_load.text` speak to the owner ("your internet provider"). On the share link the same fields carry the public wording ("the internet provider").

## `GET api/speed` (summary)

```
GET api/speed?days=7&tz=120[&exclude_fallback=1]
```

| Param | Values | Default | Notes |
|---|---|---|---|
| `days` | `7`, `30` | `7` | The span of the rows. The pattern and the finding are judged over 30 days. |
| `tz` | an IANA zone (`Europe/Berlin`), or minutes east of UTC (-840 to 840) | the server's zone | Send `Intl.DateTimeFormat().resolvedOptions().timeZone`: hours of day are then local and right on both sides of a DST change. Minutes are a fixed offset (rounded to 15). Echoed as `tz` (minutes east of UTC now) and `tz_name` (the zone; "" for minutes). Home's `tz` takes the same forms. |
| `exclude_fallback` | `0`, `1` | `0` | `1` leaves fallback tests out of the region rows. |
| `row`, `region`, `server` | an id | | One row's view (below). |

The summary carries rows and the latest test only; use `?row=` for tests. With no tests yet it is still 200, with `latest: null`, rows with `n: 0`, `pattern: null` and `finding.case: "not_enough_data"`.

Caching: 30 s on the share link; none locally. Poll every 30 s, and use `part=running` or `part=queue` for live state (every 3 s while something runs).

```jsonc
{
  "at": 1760104800000, "tz": 120, "tz_name": "Europe/Berlin", "days": 7, "exclude_fallback": false,
  "mode": "regions",              // "off" | "regions" | "nearby" | "custom"
  "modes": ["off", "regions", "nearby"],   // offered here; "custom" only with --speedtest-server
  "enabled": true,                // mode != "off"
  "set_by_flag": false,           // a --speedtest flag chose the mode at start (the Details choice is overridden until restart)
  "extra": ["canada"],            // the opt-in (regions) or extra far (nearby) regions
  "configurable": true,           // may use config, cancel and clear (local, controls allowed); always false on the share link
  "interval_s": 3600,             // each region or server is tested once per interval
  "spacing_s": 400,               // between scheduled tests
  "planned_share": 0.038,         // share of time the line is planned to be under test
  "warning": null,                // |null: a sentence when planned_share > 0.25
  "plans": [                      // every offered mode except off, for the mode picker
    {"mode": "regions", "tests_per_hour": 9, "share": 0.038, "est_gb_month": 907.2}
  ],                              // est_gb_month: tests per hour × 720 × the median data of recent tests (150 MB before any)
  "masked_share_24h": 0.037,      // share of the last 24 h actually under test (masked)
  "bytes_month": 86580579678,
  "bytes_24h": 3120000000,        // the last 24 hours; 0 on the share link     // data used by speed tests since the 1st of this month (UTC); 0 on the share link
  "connection": {"type": "wired", "source": "detected"},   // type: "wired" | "wifi" | "unknown", from the newest test that knows
  "next_at": 1760104860000,       // |null: the next scheduled slot; null when off
  "next_row": "uk",               // |null: its row id
  "running": { ...Running },      // |null
  "rows": [ ...Row ],             // the current mode's scheduled rows, in plan order (empty when off)
  "per_region": [ ...Row ],       // every default region, plus scheduled or tested opt-in regions, catalog order
  "per_server": [ ...Row ],       // servers that are scheduled or have tests in the span
  "servers": [ ...Server ],       // every catalog server, then the own servers
  "regions": [ ...Region ],       // every region, default and opt-in
  "latest": { ...Test },          // |null: the newest test of any kind
  "pattern": { ...Pattern },      // |null: a clear time-of-day dip of all scheduled tests' scores pooled (fallback tests left out)
  "finding": { ...Finding },      // what a dip points to
  "coverage": [ {"hour": 0, "n": 4, "providers": 2} ],   // 24 entries: scheduled ok tests and providers per local hour, in the span
  "under_load": { ...UnderLoad },
  "edge_hours": [                 // |null without an ISP edge target: the access network's control
    {"hour": 21, "n": 28, "rtt_ms": 30.2, "loss": 0.01}   // 24 local hours: idle RTT to the ISP edge (median over days of the hour's mean; tests are masked out) and loss over 30 days; rtt_ms and loss |null when n is 0
  ]
}
```

### Running

The queue's running entry in short (the full entry is in `part=queue`):

```jsonc
{
  "entry": 17,                    // the queue entry
  "kind": "single",               // "single" | "sweep" | "scheduled"
  "started": 1760104790000,
  "expected_end": 1760104810000,  // the queue's estimate; a test can run up to 45 s
  "direction": "download",        // "picking" (nearby: RTT checks) | "download" | "upload"
  "server": {"id": "Leaseweb-NewYork", "label": "Leaseweb New York"},   // |null while picking
  "step": 1, "steps": 1           // a sweep: the server under test, 1-based, of steps
}
```

### Server

```jsonc
{
  "id": "Leaseweb-NewYork", "label": "Leaseweb New York", "display": "US East · Leaseweb, New York",
  "provider": "Leaseweb", "city": "New York", "country": "US",
  "geo": "na", "area": "North America",   // the continent and its title
  "region": "us-east",            // |null: the first test region it belongs to
  "metro": "new-york",            // "" for own servers
  "rtt_ms": 82.4,                 // |null: the last RTT check (mode nearby, Test now of the nearest); null when never checked
  "picked": false,                // one of the nearby picks
  "custom": false,                // the user's own server
  "single_port": false,           // one port only (HE Fremont): busy moves on, no port retries
  "capacity_mbps": 10000,         // |null: the server's published link capacity, where known
  "skipped": false,               // the user skipped it: never tested (config {"skip": [...]})
  "skip_locked": false,           // skipped by --speedtest-skip: the toggle can't undo it
  "rest_until": null,             // |null: resting after BusyRun (3) busy scheduled tests in a row, for 6 h
  "host": "speedtest.nyc1.us.leaseweb.net",   // local only
  "ports": "5201-5210"                        // local only
}
```

### Region

```jsonc
{
  "id": "us-east", "title": "US East",
  "servers": ["Leaseweb-NewYork", "InterServer-Secaucus", "Clouvider-NewYork"],   // same metro, rotated per hour and day
  "nearby": ["Leaseweb-Washington"],   // the nearby-city fallback, tried last
  "default": true,                // false: opt-in (extra)
  "providers": ["Leaseweb", "InterServer", "Clouvider"]
}
```

### Row (`rows[]`, `per_region[]`, `per_server[]`, and `row` of a single view)

Only **scheduled** tests count in rows. Test now and sweeps are stored and listed, but they never enter rows, patterns or the finding.

```jsonc
{
  "id": "us-east", "kind": "region",   // "region" | "server"
  "label": "US East",             // region title, or server label
  "why": "added",                 // omitempty: why it is scheduled ("added", "nearest Clouvider", "far: US East", …)
  "scheduled": true,              // a row of the current mode
  "servers": ["Leaseweb-NewYork", "InterServer-Secaucus", "Clouvider-NewYork", "Leaseweb-Washington"],
  "latest_at": 1760104790000,     // |null
  "n": 96,                        // ok tests
  "busy": 2, "unreachable": 0, "failed": 1,
  "fallback": 7, "fallback_share": 0.07,   // ok tests not on the first choice
  "fallback_share_24h": 0.12,     // |null: regions, the last 24 h
  "capped_by_server": 0,          // ok tests within 10% of the server's capacity
  "server_now": "Leaseweb-NewYork",   // the server of the newest ok test ("" without)
  "down_median_bps": 181000000,   // |null without ok tests (every *_bps below likewise)
  "down_p10_bps": 150000000,      // the slow end
  "up_median_bps": 38000000, "up_p10_bps": 35000000,
  "best_down_bps": 196000000,     // p90 of the downloads
  "score_median": 0.97,           // |null: the median score (below)
  "dip": { ...Pattern },          // |null: this row's download dip by score
  "up_dip": { ...Pattern },       // |null: its upload dip
  "hours": [                      // always 24 entries, local hours
    {"hour": 0, "n": 4, "n_fallback": 0, "n_failed": 0, "n_capped": 0,
     "down_median_bps": 188000000, "up_median_bps": 38000000,   // |null with fewer than 3 ok tests
     "score_median": 0.96, "up_score_median": 0.98,             // |null likewise; leaves out server-capped tests
     "servers": {"Leaseweb-NewYork": 2, "Clouvider-NewYork": 2}}   // ok tests per server
  ],
  "note": null,                   // |null: regions, when over half of the last 24 h's tests used a fallback
  "learning": false               // tests but no scores yet: a server needs 20 first-choice tests over 14 days
}
```

**Scores.** A test's score is its speed divided by that server's baseline. The baseline is the p90 of the server's first-choice scheduled ok tests over the 14 days before the test, leaving out tests the server capped; it needs at least 20 such tests, so the score is null for a new server's first days. Scores make servers of different speeds comparable, and the heatmap and every pattern use them. A cell needs `n ≥ 3`; show fewer as "filling in". In mode `regions` a region gets one test per hour of day per day.

### Pattern

```jsonc
{
  "text": "Evenings are slower: about 40% below your daytime speed.",
  "level": "info",
  "hours": [19, 20, 21, 22],      // local hours of the dip, in order (may wrap: [23, 0, 1])
  "drop": 0.4,                    // rounded to 0.1
  "up": false                     // an upload dip
}
```

### Finding

What the slowdown points to. It is computed in Go and reused by Details, Home and the report.

```jsonc
{
  "case": "isp",                  // "not_enough_data" | "nothing" | "mild" | "mixed" | "isp" | "provider" | "route" | "server"
  "tier": "points_to",            // omitempty: "points_to" | "most_likely" | "early_sign" (how sure)
  "text": "Every region slows from about 7 to 11 pm, with every provider: about 40% below normal. Your provider's first router also answers slower then (25 ms instead of 8 ms), so it's in its local network. That points to your internet provider's own network.",
  "text_public": "… That points to the internet provider's own network.",   // the share link's and report's wording
  "evidence": "Based on 648 tests over 9 days; each evening hour was tested at least 9 times across 3 providers.",
  "caveat": "Wi-Fi, or other people at home using the internet at that time, can cause this too.",   // omitempty: on Wi-Fi, or when the router itself slows
  "caveat_public": "Wi-Fi, or people in the home using the internet heavily at those hours, can look the same.",   // omitempty: the share link's and report's voice (the share link's `caveat` is this already)
  "hours": [19, 20, 21, 22], "regions": ["uk", "us-east"], "providers": ["Clouvider", "Leaseweb"],
  "servers": [],                  // case server: the server
  "network": "AS3356 LEVEL3",     // omitempty, local wording only: the first network past the ISP the slow tests share (handoff)
  "regions_judged": 9, "providers_judged": 4,   // omitempty (isp): how many could tell; "Most regions (5 of 9)" unless all dipped
  "far_regions": ["oceania"],     // omitempty (isp): regions too far to show the line's speed (p90 under 60% of the best region's), left out
  "slow_days": 17, "day_count": 21,   // omitempty: the dip hours were slower on 17 of 21 days with tests
  "far_evening": false,           // omitempty (route, provider): the dip hours are evening at the far end too, so its own busy time fits as well
  "bridged": false,               // omitempty: most tests ran behind another router (container, VM): no "points to", and a caveat
  "drop": 0.4, "up": false,
  "days": 9, "tests": 648         // days and scored scheduled tests behind it
}
```

The cases:
- `isp`: every region dips at the same hours with every provider (a provider that stays flat rules it out). The cross-destination comparison is the main signal; the ISP edge's idle RTT by hour (`edge_hours`) only supports it when it rises at those hours. A flat edge proves nothing: lines with active queue management keep it flat while congested.
- `provider`: only one provider's servers dip, in more than one region; or (`network` set, `providers` several) only the servers reached through one network past the ISP (the tests' `handoff`) dip, the others stay flat. `text_public` never names the network.
- `route`: only one region dips, whichever provider.
- `server`: only one server.

- `mild`: a 10–25% dip, below what the cases need: "a little slower", pointing nowhere.
- `mixed`: some regions dip, but not in a pattern that points to one place.
- `nothing`: no slowdown of 25% or more; the text gives the worst hour's actual distance from normal.

Tiers: `points_to` needs 7 days, full coverage, a wired connection not behind another router, the dip on at least half the days, and in the ISP case a drop of at least 30% with the ISP edge rising too; `most_likely` 5 days and full coverage; `early_sign` otherwise. The ISP case always carries the household caveat (other people's traffic on the line looks the same). `not_enough_data` says how many of 3 days are in, or how many hours are covered.

### UnderLoad

The latency under load over the span: per layer, the median over tests.

```jsonc
{
  "layers": [
    {"layer": "edge", "idle_ms": 8.2, "down_ms": 21, "up_ms": 48, "loss_down": 0.033, "loss_up": 0},   // ms |null
    {"layer": "gateway", "idle_ms": 1.1, "down_ms": 1.4, "up_ms": 1.5, "loss_down": 0, "loss_up": 0}
  ],
  "text": ["Delay through your provider rises from 8 ms to 48 ms while uploading (+40 ms): noticeable on calls.", "…"]
}
```

`edge` is the provider's first router, and `gateway` is the user's router. Both are probed every 150 ms during each direction; a lost probe counts as the 1 s timeout in the percentiles.

### Test

```jsonc
{
  "id": 412,
  "start": 1760104790000, "end": 1760104808000,   // the window: probes in it (+2 s) are not judged
  "trigger": "schedule",          // "schedule" | "manual" (Test now) | "sweep" | from the share link "public" | "public_sweep"
  "on_demand": false,             // not "schedule": never in rows, patterns or the finding
  "outcome": "ok",                // "ok" | "busy" | "unreachable" | "failed" | "cancelled"
  "region": "us-east",            // |null when the test was for no region
  "server": "Leaseweb-NewYork", "label": "Leaseweb New York", "display": "US East · Leaseweb, New York",
  "provider": "Leaseweb", "city": "New York",
  "fallback": false,
  "fallback_level": "none",       // "none" | "same_metro" | "nearby_city"
  "port": 5203,                   // local only
  "attempts": 2,                  // connection attempts over every server tried
  "down_bps": 182345678, "up_bps": 38123456,   // |null when not measured
  "down_short": false, "up_short": false,      // the direction hit the 100 MB cap: measured for less time
  "capped_by_budget": false,      // down_short || up_short: show the speed as "≥ X"
  "down_s": 4.0, "up_s": 3.2,                  // seconds measured (0 when not measured)
  "capped_by_server": false,      // a direction within 10% of the server's published capacity
  "score": 0.94, "up_score": 0.97,             // |null: against the server's baseline
  "bytes": 146800640,             // data used, both directions, ramp-up included
  "down_retrans": 112, "up_retrans": null,     // |null when unknown
  "idle_gateway_ms": 1.1, "loaded_gateway_ms": 1.5,   // |null: latency.gateway in short (idle p50; the worse of down/up p50)
  "latency": {                    // |null: latency under load, see below
    "edge":    {"idle": Stats, "down": Stats, "up": Stats},   // the ISP edge: behind the line's queue
    "gateway": {"idle": Stats, "down": Stats, "up": Stats}    // the router: the home network
  },
  "conn": "wired",                // "wired" | "wifi" | "unknown": the interface the test ran on
  "handoff": {"asn": 3356, "name": "LEVEL3", "isp_asn": 3320, "at": 1760090000000}   // local only, |null
}
```

**Latency under load.** Bufferbloat sits in the queue at the line's bottleneck (the modem's upload buffer, the ISP's CMTS, OLT or BNG), past the router: the ISP edge (the first hop past the router) is behind it, the router's LAN address is not. During each direction fyisp pings both every 150 ms (1 s timeout). `idle` is the 2 minutes before the test, from the continuous probes. Each `Stats` is `{"n": 60, "lost": 3, "loss": 0.05, "p50_ms": 140, "p95_ms": 1000}`, or `null` with fewer than 3 probes. **A lost probe counts as loss and, in `p50_ms`/`p95_ms`, as 1000 ms**: under heavy bloat most probes time out, so a p95 of 1000 means "at least 1 s". On lines with active queue management (fq_codel, CAKE) the edge stays flat even when the line is congested.

**Handoff** (local only): the first network outside your ISP on the path to the server (`asn`, `name`), after your ISP's `isp_asn`, from a traceroute at most once a day per server (`at`). `null` when unknown.

**Outcomes.**
- `busy`: every port and server tried within the slot's budget was running someone else's test. Never draw it as a slow or zero result; show a gap labelled "server busy".
- `unreachable`: no server accepted a connection (timeouts, DNS failures, or refusals on every port).
- `failed`: anything else.
- `cancelled`: stopped mid-test (the owner's cancel, or shutdown). Stored without speeds only so its window stays explained (charts shade it; Home never calls it "not running"); never a result, never busy or failed, never linked to a sweep. `end` includes AbortTail. A cancel that lands after the test finished (while its result is being stored) doesn't cancel it: the test is stored as its real result. A test is always stored, whatever happens to its entry.

When a download worked but the upload failed, `down_bps` is set and `outcome` is the upload's.

**Fallback.** A busy or refusing port gets up to 2 more ports. After that, fyisp moves to the next same-metro provider (`same_metro`), then the nearby city (`nearby_city`). Unreachable servers are skipped at once. Single-port servers get no port retries. There is no new attempt after 25 s, and a slot is cut off at 45 s. A slot never pushes later slots back; a late one is skipped.

## `GET api/speed?row=<id>` (or `server=`, `region=`)

One region's or server's view. It takes `days`, `tz` and `exclude_fallback`. An unknown id returns 404, and on the share link own servers are found only by `custom-N`.

```jsonc
{
  "at": ..., "tz": 120, "days": 7, "exclude_fallback": false,
  "kind": "region",               // "region" | "server"
  "row": { ...Row },
  "tests": [ ...Test ],           // every test in the span (any trigger), oldest first, the newest 5000 at most
  "windows": [ {"start": ..., "end": ..., "origin": "scheduled"} ]   // origin: scheduled | owner | public
}
```

For compare, make one request per row (up to 4).

## `GET api/speed?part=windows&from=...&to=...`

```json
{"at": 1760104800000, "windows": [{"start": 1760104790000, "end": 1760104808000, "origin": "public"}]}
```

`from` and `to` take the same forms as `/api/panel`: `now`, `now-6h`, `now-7d` or unix ms, up to 90 days apart. The defaults are `now-30m` and `now`. Shade these on every time chart. Probe samples that ran inside a window, or within 2 s after it, are stored as "not measured", so charts show a gap. The verdict, call minutes, Home and history never count them, and history never calls them "fyisp wasn't running".

## The queue

Every test goes through one process-wide queue, and exactly one runs at a time:
- **Test now:** one server, a region's chain, or the nearest servers.
- **Sweep:** every catalog server and the user's own servers, one test each, about 45.
- **Scheduled slots:** a slot is enqueued when it falls due. It is **dropped** if it is still waiting 10 minutes later, and a slot already waiting is never stacked.

The order is by class, FIFO within each: the owner's entries (the local dashboard) first, then schedule slots, then the share link's. Nothing interrupts the running test. A public sweep **yields** between its tests whenever an owner entry or a schedule slot is waiting, and then resumes at the head of the share link's entries. Schedule slots therefore never starve behind public sweeps.

There is no cooldown for the owner or for single tests; the only wait is the 30-minute gap between public sweeps (below). Sweep and Test now results are stored with `on_demand: true` and kept out of the patterns.

**The share link's tests are an owner opt-in.** They are off by default, turned on with `--speedtest-public` or the Details toggle (`POST api/speed/config {"public": true}`), and the choice is saved. Turning them off drops the share link's waiting entries.

**The share link has a data budget:** at most `--speedtest-public-budget` (default 10 GB) over a rolling 24 hours.
- It counts every byte the share link's tests put on the wire: every attempt, the ramp-up, and tests that were cut short.
- It is saved across restarts.
- Once spent:
  - new share-link requests get 429 `budget_exhausted` with `resets_at`, the moment enough use ages out;
  - waiting share-link entries are dropped when their turn comes;
  - a running share-link sweep stops after its current test.
- The owner and the schedule are never limited by it.

**The share link has test-time caps.** Probes are masked during every test, so its tests may keep the line under test for at most:
- 10 minutes in any hour;
- 60 minutes in any 24 hours.

A new public sweep can start at the earliest 30 minutes after the last one ended (429 `cooldown` with `resets_at`); one already waiting stays waiting until then while other entries go by, and its `est_start` says when. The end time is saved, so a restart doesn't reset the gap. Single tests are not affected. Test time is charged while a test runs, every 15 s, and counts whether the test finishes, fails or is cancelled. A capped share link gets 429 `time_cap` with `resets_at`. Visitors therefore can't blank out the monitoring.

### `GET api/speed?part=queue`

In memory and per viewer, so it is never cached (`Cache-Control: no-store`). It's cheap; poll every 3 s.

```jsonc
{
  "at": 1760104800000,
  "max_waiting": 12,            // the share link's waiting entries, in all
  "full": false,                // the share link can't add any now
  "sweeps_full": false,         // 3 share-link sweeps are waiting
  "mine": 17,                   // |null: the viewer's newest waiting or running entry (share link: this visitor; local: the owner)
  "running": { ...Entry },      // |null
  "waiting": [ { ...Entry } ],  // in run order
  "last_sweep": { ...Entry },   // |null: the last finished sweep
  "public_enabled": true,       // the owner lets the share link queue tests
  "public_start": true,         // the share link may start one now: enabled, budget and test time left, room in the queue (show its button only then)
  "budget": {                   // the share link's rolling 24-hour data budget
    "limit_bytes": 50000000000, "used_bytes": 1200000000, "remaining_bytes": 48800000000,
    "resets_at": null,          // |null: when spent, when enough use ages out
    "time_hour_s": 240, "time_day_s": 960,          // the share link's test time in the last hour and day
    "time_hour_max_s": 600, "time_day_max_s": 3600,
    "time_resets_at": null,     // |null: when a capped one frees up
    "sweep_available_at": null  // |null: a public sweep ran recently; the next from then
  }
}
```

### Entry

```jsonc
{
  "id": 17,
  "kind": "sweep",              // "single" | "sweep" | "scheduled"
  "who": "you",                 // "you" | "someone" | "scheduled": relative to the viewer; never an address
  "origin": "owner",            // local only: "owner" | "public" | "scheduled"
  "state": "running",           // "waiting" | "running" | "done" | "cancelled" | "dropped"
  "target": {"id": "us-east", "label": "US East"},   // |null: the nearest servers, a sweep, or a slot
  "position": 0,                // waiting: 1 is next; otherwise 0
  "enqueued_at": 1760104700000,
  "started_at": 1760104790000,  // |null
  "finished_at": null,          // |null
  "est_start": 1760104790000,   // estimates at about 20 s per test; once finished, the real times
  "est_finish": 1760105690000,
  "step": 3, "steps": 45,       // "server 3 of 45"; a single is 1 of 1
  "server": {"id": "Leaseweb-NewYork", "label": "Leaseweb New York"},   // |null: running only
  "direction": "download",      // |null: running only ("picking" | "download" | "upload")
  "tests": [412, 413],          // the stored tests so far
  "wait_reason": null,          // waiting: "cooldown" = a share-link sweep held until est_start (30 min after the last one); else null
  "stopped_reason": null,       // finished before its last test: "cancelled" | "unreachable" (a sweep stopped after servers in a row didn't answer) | "limit" (the share link's budget or time ran out, or it was turned off); else null
  "results": [                  // a sweep's servers in order (part=entry and part=sweep only)
    {"server": {"id": "Leaseweb-Amsterdam", "label": "Leaseweb Amsterdam"}, "status": "ok", "test": 412,
     "down_bps": 412000000, "up_bps": 38000000},
    {"server": {"id": "OVH-Erith", "label": "OVH Erith"}, "status": "busy", "test": 413, "down_bps": null, "up_bps": null},
    {"server": {"id": "Leaseweb-NewYork", "label": "Leaseweb New York"}, "status": "testing", "test": null, "down_bps": null, "up_bps": null},
    {"server": {"id": "Clouvider-NewYork", "label": "Clouvider New York"}, "status": "waiting", "test": null, "down_bps": null, "up_bps": null}
  ]
}
```

`results[].status` is `waiting`, `testing`, the stored outcome (`ok`, `busy`, `unreachable`, `failed`), or `skipped`: cancelled mid-test, or never reached because the sweep stopped early (see `stopped_reason`). A finished sweep never shows `waiting`. `down_bps` and `up_bps` are set for `ok` only.

On the share link, `who` is `you` only for that visitor's own entries; everyone else's, the owner's included, are `someone`. Locally, the owner's are `you` and the share link's are `someone`. Targets and servers use the share link's ids (`custom-1`), and no host, port or address appears.

### `GET api/speed?part=entry&id=<id>`

One entry: waiting, running, or finished less than an hour ago. Otherwise 404.

### `GET api/speed?part=sweep`

`{"at": ..., "running": { ...Entry } | null, "entry": { ...Entry } | null, "tests": [ ...Test ]}`: `running` is a sweep under way, with its results so far; `entry` and `tests` are the last sweep that ended, whether it ran every server, stopped early, or was cancelled (`stopped_reason` says which; servers it never reached are `skipped`).

### `POST api/speed/run?kind=single[&id=<server or region id>]` or `?kind=sweep`

"Test now" (no `id`: the nearest servers) or a sweep. There is no body; `kind` defaults to `single`. The share link's own-server ids (`custom-1`) are accepted.

The response is `202 {"entry": { ...Entry }, "duplicate": false}`. `duplicate: true` means the same requester (the owner, or the same share-link visitor) already had an equal entry waiting or running, and this is that entry. An unknown id returns 404 `not_found`.

**Local:** the same protections as notes:
- same origin;
- `X-FYISP-Token`;
- the admin token or session cookie when one is set.

It works in every mode, `off` included.

**Share link:** `POST /s/<secret>/api/speed/run?…` needs no token. It has these protections:
- **Speed tests on:** 403 `speed_off` while the mode is `off` (part=queue `speed_on: false`).
- **Opt-in:** 403 `disabled` while the owner hasn't turned it on.
- **Budget:** 429 `budget_exhausted` (data) or `time_cap` (test time) with `resets_at` (ms) and `retry_after` once either is spent.
- **Sweep gap:** 429 `cooldown` with `resets_at` and `retry_after` for a sweep within 30 minutes of the last public one.
- **Request:** same origin only; only `kind` and `id`, each at most once; no body (a declared length or chunking is refused); every response `no-store`.
- **Queue limits (429 `queue_full`, its `error` says which):**
  - one waiting entry per visitor;
  - at most 3 waiting share-link sweeps;
  - at most 12 waiting share-link entries in all.
- **Start limit (429 `rate_limited`, with `retry_after`):** per visitor, 1 new entry per 30 s and 10 per hour. Visitors are `Cf-Connecting-Ip`, with IPv6 grouped by /64. A duplicate or a refused request doesn't count.

### `POST api/speed/cancel?id=<id>` or `?public=1` (local only)

These use the same protections as notes (admin-gated):
- `id`: cancels a waiting entry, or the running one, whose test stops without a result. It returns `200 {"entry": { ...Entry }}`, or 404.
- `public=1` (the owner's "cancel queue"): cancels every waiting share-link entry. It returns `200 {"cancelled": 3}`.

## `POST api/speed/config` (local only)

```
POST api/speed/config
{"mode": "regions", "extra": ["canada"]}   // and/or
{"public": true}               // let the share link queue tests (saved; --speedtest-public sets it at start)
{"skip": ["Leaseweb-Tokyo"]}   // servers never to test (replaces the saved list; --speedtest-skip adds to it)
```

- `200 {"mode": "regions", "next_at": 1760104920000, "public": true, "limits": {"budget_bytes": 10000000000, "time_hour_max_s": 600, "time_day_max_s": 3600}}`, with the fields it changed; `next_at` is null for `off`. `limits` comes with `public`: the share link's caps, for the toggle's confirmation (the same values as part=queue `budget`).
- `400` for a mode not in `modes`, an unknown region in `extra` (or `extra` without `mode`), for neither field, or for any other field.

`extra` is the opt-in regions (mode `regions`) or extra far regions (mode `nearby`). The choices are saved and survive restarts. A `--speedtest…` flag on the command line overrides them at start (`set_by_flag`). Switching to a mode schedules the first test about 2 minutes later.

## `GET api/status`

`speed_masked_share_24h` (omitempty) is the share of the last 24 hours under test. Probes in that time are stored as "not measured".

## `GET api/profile`

`features.speed` is true when speed tests are available (any mode).

## `api/home`

`HomeLive.speed` (omitempty) is the speed line:

```json
"speed": {"text": "Evenings are slower: about 40% below your daytime speed (around 7 to 11 pm). That's still plenty for video calls.", "kind": "pattern", "impact": "ok"}
```

- `kind`: `warmup` (the first days: "Speed: checking at different times of day. We need about 3 days to see a pattern."), `pattern`, or `steady` (no time of day stands out).
- `impact`: `ok`, or `video_risk` when the dip hours' download may be too slow for video calls.
- It is absent when speed tests are off, while the answer card shows a problem (warn or bad), when the dip is narrow (fewer than half the regions or fewer than 2 providers), and when the last day's tests mostly failed.
- Send `tz` on `live` too: the wording depends on local hours. The share link says "the daytime" instead of "your daytime".

`HomeAt` ("What happened at") gains `speed_test_overlap_s` and `speed_note` (both omitempty) when a speed test ran in the window. At 10 s or more of overlap, the note names the test as the most likely cause.

## Metrics (`/metrics`)

These give the latest result per region and server, set after every test:

| Metric | Labels | |
|---|---|---|
| `fyisp_speed_download_bits_per_second` | `region`, `server` | gauge (`region` empty for tests for no region) |
| `fyisp_speed_upload_bits_per_second` | `region`, `server` | gauge |
| `fyisp_speed_loaded_rtt_seconds` | `region`, `server`, `layer` (`edge`, `gateway`), `load` (`idle`, `download`, `upload`) | gauge: median RTT |
| `fyisp_speed_tests_total` | `region`, `server`, `outcome` | counter |

## Export

`fyisp export --speedtests` writes CSV with these columns: `id,start,end,trigger,outcome,region,server,fallback,fallback_level,port,attempts,down_bps,up_bps,down_retrans,up_retrans,down_s,up_s,down_capped_by_bytes,up_capped_by_bytes,capped_by_server,bytes,score_down,score_up,idle_gateway_ms,loaded_gateway_ms,conn,handoff_asn,isp_asn,edge_idle_p50_ms,edge_idle_loss,edge_down_p50_ms,edge_down_p95_ms,edge_down_loss,edge_up_p50_ms,edge_up_p95_ms,edge_up_loss,gw_down_p50_ms,gw_down_p95_ms,gw_up_p50_ms,gw_up_p95_ms`.
