# Speed tests: API contract

The routes and types of the speed tests. What they measure and how to use them is in [SPEED-TESTS.md](SPEED-TESTS.md); every other route is in [API.md](API.md).

iperf3 speed tests over IPv4, on a schedule and on demand. Each test runs download, then upload: 8 TCP streams, a ramp-up that is left out (decided afterwards, where the rate levels off), then at least 3 s and up to 10 s: a direction stops early only once its rate has levelled off, at least 3 s have passed since it first came near its peak, and a second is within 5% of the one before. So a direction runs at most about 11 s, ramp-up included; a hard stop (an RST) 2 s after that ends it whatever the server does. There is no byte cap.

Modes:
- **`off`**, the default.
- **`regions`**, the default once speed tests are on (`speedtest.DefaultMode`), hourly: the 15 default regions, each **once an hour**, one test at a time about 240 s apart. The order and the minute differ per install (a saved seed), and each hour's start has ±60 s of jitter. Each region lists same-metro servers from several providers. The first choice rotates per (region, hour of day) by day, so each hour is measured with every provider within N days. `extra` adds opt-in regions.
- **`nearby`**: the nearest server of 3 different providers (same metro preferred), plus 1 automatic far region on a major route (Europe and South America → US East; North America, Africa and the Middle East → UK; Asia and Oceania → US West) and any `extra` far regions, each every 15 minutes. The picks are redone daily.
- **`custom`**: the `--speedtest-server` list, each every 15 minutes. It is offered only with that flag.

Intervals can be changed with `--speedtest=…,every=`. The floor is 15 minutes against public servers; with only the user's own servers on their own network (private, CGNAT, link-local or loopback addresses), 1 minute.

The catalog (`internal/profile/catalog/iperf3/`: 47 servers in per-provider files, plus `regions.yml`) is built into the binary. It changes only with a release.

The Go types live in `internal/web/speed.go`; keep this file in sync with them. Golden example: `internal/web/testdata/speed/local.json`.

Routes are relative like every other API. Unknown parameters return 400. When fyisp runs without a speed-test source (only `fyisp-webdev -no-speed` and tests), every route answers 404 and `/api/profile` has `features.speed: false`. Hide Speed then.

## Errors

Every API error is `{"error": "<sentence>", "code": "<word>"}`. Use `code` and never match on the sentence.

| HTTP | `code` |
|---|---|
| 400 | `bad_request` |
| 401 | `unauthorized` (no or the wrong admin token) |
| 403 | `forbidden` (cross-origin, or a bad CSRF token) |
| 409 | `skipped` (Test now of a server the user skipped) |
| 404 | `not_found` |
| 405 | `method_not_allowed` |
| 421 | `unknown_host` |
| 500 | `error` (a store error) |
| 503 | `unavailable` (the store timed out or failed: retry after `Retry-After` when it is sent, else after a few seconds) |

## Nullables

In the JSON below, `|null` marks fields that are **always present and may be null**. `(omitempty)` marks fields that are **omitted** when they don't apply. Every other field is always present.

## Identity

Servers have these names:
- `id` is the catalog id, e.g. `Leaseweb-NewYork`. It is stable and URL-safe. The user's own servers (`--speedtest-server host:port`) have the id `host:port`, with the label "Your server" (or "Your server 2" and so on).
- `label` is provider and city, e.g. "Leaseweb New York" or "OVH Paris (CCH)".
- `display` is "Region · Provider, City", e.g. "US East · Leaseweb, New York". It falls back to "Provider, City" for servers in no region.

Regions have an `id` (`uk`, `eu-west`, …) and a `title` ("UK", "Western Europe", …).

Wording: `finding.text`, `pattern.text`, row dips and `under_load.text` speak to the owner ("your internet provider").

## `GET api/speed` (summary)

`part=summary` is the default.

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

Caching: none. Poll every 30 s, and use `part=running` or `part=queue` for live state (every 3 s while something runs).

```jsonc
{
  "at": 1760104800000, "tz": 120, "tz_name": "Europe/Berlin", "days": 7, "exclude_fallback": false,
  "mode": "regions",              // "off" | "regions" | "nearby" | "custom"
  "modes": ["off", "regions", "nearby"],   // offered here; "custom" only with --speedtest-server
  "enabled": true,                // mode != "off"
  "set_by_flag": false,           // a --speedtest flag chose the mode at start (the Details choice is overridden until restart)
  "interval_s": 3600,             // each region or server is tested once per interval
  "spacing_s": 240,               // between scheduled tests
  "planned_share": 0.083,         // share of time the line is planned to be under test
  "warning": null,                // |null: a sentence when planned_share > 0.25
  "plans": [                      // every offered mode except off, for the mode picker
    {"mode": "regions", "tests_per_hour": 15, "share": 0.083, "est_gb_month": 1512}
  ],                              // est_gb_month: tests per hour × 720 × the median data of recent tests (150 MB before any)
  "test_share_24h": 0.08,         // share of the last 24 h actually under test (each test plus 2 s)
  "bytes_month": 86580579678,
  "bytes_24h": 3120000000,        // the last 24 hours (bytes_month: data used by speed tests since the 1st of this month, UTC)
  "connection": {"type": "wired", "source": "detected"},   // type: "wired" | "wifi" | "unknown", from the newest test that knows
  "next_at": 1760104860000,       // |null: the next scheduled slot; null when off
  "sweep_bytes": 9200000000, "sweep_servers": 47, // "Test every server": its estimated data (each server's recent median, else this line's median rate both ways for 10 s) and the servers it tests
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
  "under_load": { ...UnderLoad }
}
```

### Running

The queue's running entry in short (the full entry is in `part=queue`):

```jsonc
{
  "entry": 17,                    // the queue entry
  "kind": "single",               // "single" | "sweep" | "scheduled"
  "started": 1760104790000,
  "direction": "download",        // "picking" (nearby: RTT checks) | "download" | "upload"
  "server": {"id": "Leaseweb-NewYork", "label": "Leaseweb New York"},   // |null while picking
  "step": 1, "steps": 1           // a sweep: the server under test, 1-based, of steps
}
```

### `GET api/speed?part=running`

`{"at": 1760104800000, "running": { ...Running } | null}`. In memory, so it is never cached (`Cache-Control: no-store`); poll every 3 s while a test runs. It takes no other parameter.

### Server

```jsonc
{
  "id": "Leaseweb-NewYork", "label": "Leaseweb New York", "display": "US East · Leaseweb, New York",
  "provider": "Leaseweb", "city": "New York", "country": "US",
  "geo": "na", "area": "North America",   // the continent and its title
  "region": "us-east",            // |null: the first test region it belongs to
  "rtt_ms": 82.4,                 // |null: the last RTT check (mode nearby, Test now of the nearest); null when never checked
  "picked": false,                // one of the nearby picks
  "custom": false,                // the user's own server
  "capacity_mbps": 10000,         // |null: the server's published link capacity, where known
  "skipped": false,               // the user skipped it: never tested (config {"skip": [...]})
  "skip_locked": false,           // skipped by --speedtest-skip: the toggle can't undo it
  "rest_until": null,             // |null: resting after BusyRun (3) busy scheduled tests in a row, for 6 h
  "host": "speedtest.nyc1.us.leaseweb.net",   // (omitempty)
  "ports": "5201-5210"                        // (omitempty)
}
```

### Region

```jsonc
{
  "id": "us-east", "title": "US East",
  "servers": ["Leaseweb-NewYork", "Clouvider-NewYork"],   // same metro, rotated per hour and day
  "nearby": ["Leaseweb-Washington"],   // the nearby-city fallback, tried last
  "default": true,                // false: opt-in (extra)
  "providers": ["Leaseweb", "Clouvider"]
}
```

### Row (`rows[]`, `per_region[]`, `per_server[]`, and `row` of a single view)

Only **scheduled** tests count in rows. Test now and sweeps are stored and listed, but they never enter rows, patterns or the finding.

```jsonc
{
  "id": "us-east", "kind": "region",   // "region" | "server"
  "label": "US East",             // region title, or server label
  "scheduled": true,              // a row of the current mode
  "servers": ["Leaseweb-NewYork", "Clouvider-NewYork", "Leaseweb-Washington"],
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
  "n_lower_bound": 12,            // omitted when 0: ok tests whose download was a lower bound (left out of the *_bps above)
  "ramp_capped_share": 0.4,       // omitted when 0: their share of the ok tests
  "too_fast": true,               // omitted when false: every server of the row is too fast to measure from here (below); out of the patterns
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

What the slowdown points to. It is computed in Go and reused by Details and Home.

```jsonc
{
  "case": "widespread",           // "not_enough_data" | "nothing" | "mild" | "mixed" | "widespread" | "provider" | "route" | "server" | "too_fast" (every server too fast to measure from here)
  "tier": "points_to",            // omitempty: "points_to" | "most_likely" | "early_sign" (how sure)
  "text": "Every region slows from about 7 to 11 pm, with every provider we could test: tests read about 40% below the same servers' usual results.",
  "evidence": "Based on 648 tests on 9 days between Tue 1 Sep and Wed 9 Sep; each evening hour was tested at least 9 times across 3 providers.",
  "coverage": "Tests ran on 5 of the last 7 days.", // omitted when every day of the window has tests; or "Speed tests started on Mon 6 Oct, so this covers 3 days, not 7."
  "slowest": [{"server": "OVH-Singapore", "n": 9, "display": "Southeast Asia · OVH, Singapore", "pct": 52, "median_bps": 310000000, "usual_bps": 600000000}], // omitted when none: up to 3 servers slowest in the dip's hours (≥ 3 results there), nearest first in the widespread and mild cases, else slowest first
  "via": "DE-CIX",                // omitted when none: the internet exchange the slow tests' route crosses
  "caveat": "Wi-Fi, or other people at home using the internet at that time, can cause this too.",   // omitempty: on Wi-Fi, or when the router itself slows
  "hours": [19, 20, 21, 22], "regions": ["uk", "us-east"], "providers": ["Clouvider", "Leaseweb"],
  "servers": [],                  // case server: the server
  "network": "AS3356 LEVEL3",     // omitempty, local wording only: the first network past the ISP the slow tests share (handoff)
  "regions_judged": 15, "providers_judged": 4,  // omitempty (widespread): how many could tell; "Most regions (8 of 15)" unless all dipped
  "far_regions": ["oceania"],     // omitempty (widespread): regions too far to show the line's speed (p90 under 60% of the best region's), left out
  "slow_days": 17, "day_count": 21,   // omitempty: the dip hours were slower on 17 of 21 days with tests
  "far_evening": false,           // omitempty (route, provider): the dip hours are evening at the far end too, so its own busy time fits as well
  "bridged": false,               // omitempty: most tests ran behind another router (container, VM): no "points to", and a caveat
  "drop": 0.4, "up": false,
  "days": 9, "tests": 648         // days and scored scheduled tests behind it
}
```

The cases:
- `widespread`: most regions dip at the same hours with every provider (a provider that stays flat rules it out). It states the pattern and names no cause: speed tests alone can't tell your provider's network from your home's traffic or the far ends' busy hours. Whether your provider is at fault is the verdict's job, from latency and loss ([VERDICTS.md](VERDICTS.md)).
- `provider`: only one provider's servers dip, in more than one region; or (`network` set, `providers` several) only the servers reached through one network past the ISP (the tests' `handoff`) dip, the others stay flat.
- `route`: only one region dips, whichever provider.
- `server`: only one server.

- `mild`: a 10–25% dip, below what the cases need: "a little slower", pointing nowhere.
- `mixed`: some regions dip, but not in a pattern that points to one place.
- `nothing`: no slowdown of 25% or more; the text gives the worst hour's actual distance from normal.

Tiers: `points_to` needs 7 days, full coverage, a wired connection not behind another router, and the dip on at least half the days; `most_likely` 5 days and full coverage; `early_sign` otherwise. The widespread case always carries the household caveat (other people's traffic on the line looks the same). `not_enough_data` says how many of 3 days are in, or how many hours are covered.

### UnderLoad

The latency under load over the span, one sentence per layer, from the median over tests of each phase's p50.

```jsonc
{
  "text": ["Delay through your provider rises from 8 ms to 48 ms while uploading (+40 ms): noticeable on calls.", "…"]
}
```

`edge` is the provider's first router, and `gateway` is the user's router. Both are probed every 150 ms during each direction; a lost probe counts as the 1 s timeout in the percentiles.

### Test

```jsonc
{
  "id": 412,
  "start": 1760104790000, "end": 1760104808000,   // the test's window (charts shade it; Home blames problems that overlap it on the test)
  "trigger": "schedule",          // "schedule" | "manual" (Test now) | "sweep"
  "on_demand": false,             // not "schedule": never in rows, patterns or the finding
  "outcome": "ok",                // "ok" | "busy" | "unreachable" | "failed" | "cancelled"
  "region": "us-east",            // |null when the test was for no region
  "server": "Leaseweb-NewYork", "label": "Leaseweb New York", "display": "US East · Leaseweb, New York",
  "provider": "Leaseweb", "city": "New York",
  "fallback": false,
  "fallback_level": "none",       // "none" | "same_metro" | "nearby_city"
  "port": 5203,                   // (omitempty)
  "attempts": 2,                  // connection attempts over every server tried
  "down_bps": 182345678, "up_bps": 38123456,   // |null when not measured
  "down_lower_bound": true, "up_lower_bound": true, // omitted when false: still speeding up at the 10 s cap (below)
  "cancelled_by": "owner",        // a cancelled test the owner stopped; omitted otherwise (a shutdown)
  "down_lb_counted": true,        // (omitempty) present, true, only for a lower bound that reached normal (counts as "at least normal"); a lower bound without it was left out ("not counted"); likewise up_lb_counted
  "down_s": 4.0, "up_s": 3.2,                  // seconds measured (0 when not measured)
  "capped_by_server": false,      // a direction within 10% of the server's published capacity
  "score": 0.94, "up_score": 0.97,             // |null: against the server's baseline
  "bytes": 146800640,             // data used, both directions, ramp-up included
  "down_retrans": 112, "up_retrans": null,     // |null when unknown
  "latency": {                    // |null: latency under load, see below
    "edge":    {"idle": Stats, "down": Stats, "up": Stats},   // the ISP edge: behind the line's queue
    "gateway": {"idle": Stats, "down": Stats, "up": Stats}    // the router: the home network
  },
  "conn": "wired",                // "wired" | "wifi" | "unknown": the interface the test ran on
  "handoff": {"asn": 3356, "name": "LEVEL3", "isp_asn": 3320, "at": "2026-10-10T13:30:00Z", "via": "DE-CIX"}   // (omitempty); at is RFC 3339; via (omitempty)
}
```

**Latency under load.** Bufferbloat sits in the queue at the line's bottleneck (the modem's upload buffer, the ISP's CMTS, OLT or BNG), past the router: the ISP edge (the first hop past the router) is behind it, the router's LAN address is not. During each direction fyisp pings both every 150 ms (1 s timeout). `idle` is the 2 minutes before the test, from the continuous probes; it is `null` with fewer than 20 probes then, and in fyisp's first 2 minutes after starting (path discovery runs then). Each `Stats` is `{"n": 60, "lost": 3, "loss": 0.05, "p50_ms": 140, "p95_ms": 1000}`, or `null` with fewer than 3 probes. **A lost probe counts as loss and, in `p50_ms`/`p95_ms`, as 1000 ms**: under heavy bloat most probes time out, so a p95 of 1000 means "at least 1 s". On lines with active queue management (fq_codel, CAKE) the edge stays flat even when the line is congested.

**One estimator for every direction.** Whatever ended a direction (its 10 s or an early stop), its rate is measured from where its rate levels off: the 100 ms ticks (each rate over its actual length), smoothed over 3 ticks, from the first one at 90% of the peak to the last byte (uploads count what the server acknowledged, on Linux). When it was still speeding up at the limit, the result is a **lower bound**: leaving out the first tick (connection start, a shaper's burst) and smoothing over at least one RTT on long paths, no two near-peak ticks at least an RTT apart, nor a peak an RTT before the end: the speed was at least this. A lower bound never enters the server's normal (`Baseline`). Per server and direction over 14 days, a **coverage guard** decides the rest: when the server has lower bounds and its other results cover fewer than 12 local hours, or more than half of them fall within any 6 hours, they are no sample of its normal (on a fast line the daytime reads as lower bounds, leaving the slow evenings), so the server is **too fast to measure from here**: left out of the time-of-day patterns, the rows' score medians and the finding (its tests are still listed and exported). Otherwise a lower bound that already reaches the server's normal counts as normal (score 1), and one below it is left out (`*_lb_counted` says which); a lower bound never reads as slow. When every server is too fast, the finding's case is `too_fast` ("Your line is too fast for these tests to compare times of day: …"), never "nothing"; when some are, the evidence ends "k of n servers were too fast to measure from here and are left out."

With the environment variable `FYISP_TICKS` set, fyisp logs each direction's ticks (`at_ms`, `bytes`, `rtt_ms`) and the verdict, for checking the estimator on a real line.

**Bytes.** `bytes` is what the test moved on the wire, every attempt included, also when it failed or was cancelled (then it's what moved until the cancel, and a cancel during setup still counts the few hundred bytes of the handshake). On Linux each stream counts what the kernel saw (received, read or still buffered, for downloads; acknowledged for uploads) when that's more than what fyisp read or wrote; other systems count what fyisp read or wrote. `bytes_24h` and `bytes_month` add up these numbers.

**Handoff**: the first network outside your ISP on the path to the server (`asn`, `name`), after your ISP's `isp_asn`, from a traceroute at most once a day per server (`at`, RFC 3339). Omitted when unknown. The traceroute runs just before the test (bounded to 10 s; without an answer the test goes ahead without a handoff), to the address the test then dials. When the path crosses an internet exchange on fyisp's built-in list (DE-CIX, AMS-IX, LINX and others), the handoff is the network behind it, and `via` names the exchange (`asn` 0 when the path ends there). `via` is kept in memory only, so it fills in within a day of a restart.

**Outcomes.**
- `busy`: every port and server tried within the slot's budget was running someone else's test. Never draw it as a slow or zero result; show a gap labelled "server busy".
- `unreachable`: no server accepted a connection (timeouts, DNS failures, or refusals on every port).
- `failed`: anything else.
- `cancelled`: stopped mid-test (the owner's cancel, or shutdown). Stored without speeds only so its window stays explained (charts shade it; Home never calls it "not running"); never a result, never busy or failed, never linked to a sweep. `end` includes AbortTail. A cancel that lands after the test finished (while its result is being stored) doesn't cancel it: the test is stored as its real result. A test is always stored, whatever happens to its entry.

When a download worked but the upload failed, `down_bps` is set and `outcome` is the upload's.

**Fallback.** A busy or refusing port gets up to 2 more ports. After that, fyisp moves to the next same-metro provider (`same_metro`), then the nearby city (`nearby_city`). Unreachable servers are skipped at once. Single-port servers get no port retries. There is no new attempt after 25 s, and a slot is cut off at 62 s (an attempt started at 25 s still gets both directions). A slot never pushes later slots back; a late one is skipped.

## `GET api/speed?row=<id>` (or `server=`, `region=`)

One region's or server's view. It takes `days`, `tz` and `exclude_fallback`. An unknown id returns 404.

```jsonc
{
  "at": ..., "tz": 120, "days": 7, "exclude_fallback": false,
  "kind": "region",               // "region" | "server"
  "row": { ...Row },
  "tests": [ ...Test ],           // every test in the span (any trigger), oldest first, the newest 5000 at most
  "windows": [ {"start": ..., "end": ..., "origin": "scheduled"} ]   // origin: scheduled | owner
}
```

For compare, make one request per row (up to 4).

## `GET api/speed?part=windows&from=...&to=...`

```json
{"at": 1760104800000, "windows": [{"start": 1760104790000, "end": 1760104808000, "origin": "owner"}]}
```

`from` and `to` take the same forms as `/api/panel`: `now`, `now-6h`, `now-7d` or unix ms, up to 90 days apart. The defaults are `now-30m` and `now`. Shade these on every time chart. Probe samples inside a window are stored as measured, so a test that loads the line shows up as a spike or loss. Home blames a problem that overlaps a window (or the 2 s after it) on the test (`cause_kind` `speedtest`). The windows include tests of servers since removed from the catalog, whose results are gone from every other part of this API.

## The queue

Every test goes through one process-wide queue, and exactly one runs at a time:
- **Test now:** one server, a region's chain, or the nearest servers.
- **Sweep:** every catalog server and the user's own servers, one test each, about 47.
- **Scheduled slots:** a slot is enqueued when it falls due. It is **dropped** if it is still waiting 10 minutes later, and a slot already waiting is never stacked.

The order is by class, FIFO within each: the owner's entries (Test now, sweeps) first, then schedule slots. Nothing interrupts the running test.

There is no cooldown. Sweep and Test now results are stored with `on_demand: true` and kept out of the patterns.

### `GET api/speed?part=queue`

In memory, so it is never cached (`Cache-Control: no-store`). It's cheap; poll every 3 s.

```jsonc
{
  "at": 1760104800000,
  "mine": 17,                   // |null: the owner's newest waiting or running entry
  "running": { ...Entry },      // |null
  "waiting": [ { ...Entry } ],  // in run order
  "last_sweep": { ...Entry }    // |null: the last finished sweep
}
```

### Entry

```jsonc
{
  "id": 17,
  "kind": "sweep",              // "single" | "sweep" | "scheduled"
  "who": "you",                 // "you" | "scheduled"
  "origin": "owner",            // "owner" | "scheduled"
  "state": "running",           // "waiting" | "running" | "done" | "cancelled" | "dropped"
  "target": {"id": "us-east", "label": "US East"},   // |null: the nearest servers, a sweep, or a slot
  "position": 0,                // waiting: 1 is next; otherwise 0
  "enqueued_at": 1760104700000,
  "started_at": 1760104790000,  // |null
  "finished_at": null,          // |null
  "est_start": 1760104790000,   // estimates at about 20 s per test; once finished, the real times
  "est_finish": 1760105690000,
  "step": 3, "steps": 47,       // "server 3 of 47"; a single is 1 of 1
  "server": {"id": "Leaseweb-NewYork", "label": "Leaseweb New York"},   // |null: running only
  "direction": "download",      // |null: running only ("picking" | "download" | "upload")
  "tests": [412, 413],          // the stored tests so far
  "stopped_reason": null,       // finished before its last test: "cancelled" | "unreachable" (a sweep stopped after servers in a row didn't answer); else null
  "cancelled_by": null,         // cancelled by the owner: "owner"; else null (also a shutdown)
  "results": [                  // a sweep's servers in order (part=entry and part=sweep only)
    {"server": {"id": "Leaseweb-Amsterdam", "label": "Leaseweb Amsterdam"}, "display": "Netherlands · Leaseweb, Amsterdam",
     "region": "netherlands", "provider": "Leaseweb",   // region |null: the server is in no region
     "status": "ok", "test": 412, "down_bps": 412000000, "up_bps": 38000000},
    {"server": {"id": "OVH-Erith", "label": "OVH Erith"}, "status": "busy", "test": 413, "down_bps": null, "up_bps": null},
    {"server": {"id": "Leaseweb-NewYork", "label": "Leaseweb New York"}, "status": "testing", "test": null, "down_bps": null, "up_bps": null},
    {"server": {"id": "Clouvider-NewYork", "label": "Clouvider New York"}, "status": "waiting", "test": null, "down_bps": null, "up_bps": null}
  ]
}
```

`results[].status` is `waiting`, `testing`, the stored outcome (`ok`, `busy`, `unreachable`, `failed`), `cancelled` (the server whose test a cancel stopped mid-run), or `skipped` (never reached because the sweep stopped early: see `stopped_reason`). A finished sweep never shows `waiting`. `down_bps` and `up_bps` are set for `ok` only.

### `GET api/speed?part=entry&id=<id>`

One entry: waiting, running, or finished less than an hour ago. Otherwise 404.

### `GET api/speed?part=sweep`

`{"at": ..., "running": { ...Entry } | null, "entry": { ...Entry } | null, "tests": [ ...Test ]}`: `running` is a sweep under way, with its results so far; `entry` and `tests` are the last sweep that ended, whether it ran every server, stopped early, or was cancelled (`stopped_reason` says which; servers it never reached are `skipped`).

### `POST api/speed/run?kind=single[&id=<server or region id>]` or `?kind=sweep`

"Test now" (no `id`: the nearest servers) or a sweep. There is no body; `kind` defaults to `single`.

The response is `202 {"entry": { ...Entry }, "duplicate": false}`. `duplicate: true` means the owner already had an equal entry waiting or running, and this is that entry. An unknown id returns 404 `not_found`.

It has the write checks:
- the admin token (as on every route but the page);
- same origin;
- `X-FYISP-Token`.

It works in every mode, `off` included.

### `POST api/speed/cancel?id=<id>`

The same write checks as `run`. It cancels a waiting entry, or the running one, whose test stops without a result. It returns `200 {"entry": { ...Entry }}`, or 404.

## `POST api/speed/config`

```
POST api/speed/config
{"mode": "regions", "extra": ["canada"]}   // and/or
{"skip": ["Leaseweb-Tokyo"]}   // servers never to test (replaces the saved list; --speedtest-skip adds to it)
```

- `200 {"mode": "regions", "next_at": 1760104920000}`, with the fields it changed; `next_at` is null for `off`.
- `400` for a mode not in `modes`, an unknown region in `extra` (or `extra` without `mode`), for neither field, or for any other field.

`extra` is the opt-in regions (mode `regions`) or extra far regions (mode `nearby`). The choices are saved and survive restarts. A `--speedtest…` flag on the command line overrides them at start (`set_by_flag`). Switching to a mode schedules the first test about 2 minutes later.

## `GET api/profile`

`features.speed` is true when speed tests are available (any mode).

## `api/home`

`part=now`'s `speed` block is the latest OK test of the last 7 days against the usual (the median of the ones before it), whether tests are scheduled, and a test running now; see [HOME-API.md](HOME-API.md#partnow). Probes during a test are stored as measured, so a tile's bars keep their colour; a problem that overlaps a test (or the 2 s after it, 5 s after a cut-short test) is put down to the test (`cause_kind` `speedtest`, bar note "<Slow|Choppy|Down> during a speed test") rather than to the provider.

## Metrics and export

The `fyisp_speed_*` series on `/metrics` and the `fyisp export --speedtests` columns are in [API.md](API.md#metrics) and [API.md](API.md#fyisp-export).
