# Speed tests: API contract (fyisp v0.7)

Scheduled iperf3 speed tests. Each test runs download, then upload: 8 TCP streams, a 1 s ramp-up that is left out, then 3 to 8 s measured. The measurement stops once a second is within 5% of the one before.

Schedule and modes:
- **Mode `regions`**, the default once speed tests are on: the 9 catalog regions are each tested every 15 minutes, one test at a time, spread evenly about 100 s apart. Each region has a fallback chain across providers.
- **Mode `nearest`**: the 2 nearest servers, alternating, one test every 15 minutes.
- **Mode `custom`**: the `--speedtest-server` list, each server every 15 minutes.
- **Mode `off`** is the default.

The catalog (`internal/profile/catalog/iperf3/`: 41 servers in per-provider files, and `regions.yml`) is data, changed by PR. The Go types live in `internal/web/speed.go`; keep this file in sync with them. Golden examples: `internal/web/testdata/speed/{local,public}.json`.

Routes are relative like every other API: `/api/speed` locally, `/s/<secret>/api/speed` on the share link. Unknown parameters return 400. When fyisp runs without a speed-test source (an old build, `fyisp-webdev -no-speed`), every route answers 404: hide Speed.

## Errors

Every API error (not only these routes) is `{"error": "<sentence>", "code": "<word>"}`. Use `code` and never match on the sentence.

| HTTP | `code` |
|---|---|
| 400 | `bad_request` |
| 403 | `forbidden` (controls disabled, cross-origin, bad CSRF token or admin token) |
| 404 | `not_found` |
| 405 | `method_not_allowed` |
| 409 | `conflict` |
| 421 | `unknown_host` |
| 403 | `disabled` (share link: the owner hasn't turned on its tests) |
| 429 | `rate_limited` (with `retry_after`, seconds, and `Retry-After`); `queue_full`, `budget_exhausted` (with `budget` and `resets_at`), `sweep_cooldown` (with `available_at`) (share link: see Queue) |
| 503 | `unavailable` (retry after `Retry-After`) |

## Nullables

In the JSON below, `|null` marks fields that are **always present and may be null**. `(local only)` and `(omitempty)` mark fields that are **omitted** when they don't apply. Every other field is always present.

## Identity and the share link

Servers have three names:
- `id` is the catalog id, e.g. `Leaseweb-NewYork`. It is stable, URL-safe, and the same on both listeners, so `#v=speed&srv=<id>` works on both.
- `label` is public-safe: provider and city, e.g. "Leaseweb New York".
- `host` and the ports are **local only**, along with every test's `port`.

The user's own servers (`--speedtest-server host:port`) have the id `host:port` locally. On the share link they appear as `custom-1`, `custom-2`, … with the label "Your server" (or "Your server 2" and so on), and the share link accepts those ids as well. Regions have an `id` (`uk`, `eu-west`, …) and a `title` ("UK", "Western Europe", …).

## `GET api/speed` (summary)

```
GET api/speed?days=7&tz=120[&exclude_fallback=1]
```

| Param | Values | Default | Notes |
|---|---|---|---|
| `days` | `7`, `30` | `7` | The span of the rows. The pattern is always judged over 30 days. |
| `tz` | minutes east of UTC, integer from -840 to 840 | `0` | Rounded to 15 min and echoed. Hours of day are local to it. Send `-new Date().getTimezoneOffset()`. |
| `exclude_fallback` | `0`, `1` | `0` | `1` leaves fallback tests out of `per_region`. |

**No full test lists here.** The summary carries rows and the latest test only; use `?server=` or `?region=` (below) for tests. With no tests yet it is still 200, with `latest: null`, rows with `n: 0`, and `pattern: null`.

Caching: 30 s on the share link; none locally. Poll every 30 s, and use `part=running` (below) for the "testing now" line, or `part=queue` (Queue) for the queue.

```jsonc
{
  "at": 1760104800000, "tz": 120, "days": 7, "exclude_fallback": false,
  "mode": "regions",              // "off" | "regions" | "nearest" | "custom"
  "modes": ["off", "regions", "nearest"],  // offered here; "custom" only with --speedtest-server
  "enabled": true,                // mode != "off"
  "configurable": true,           // may use run and config (local, controls allowed); always false on the share link
  "interval_s": 900,              // each region or server is tested once per interval
  "spacing_s": 100,               // between tests
  "planned_share": 0.15,          // share of time the line is planned to be under test
  "warning": null,                // |null: a sentence when planned_share > 0.25
  "masked_share_24h": 0.143,      // share of the last 24 h actually under test (masked)
  "next_at": 1760104860000,       // |null: the next scheduled test; null when off or while running
  "next": {"region": "uk", "server": "Clouvider-London"},   // |null; region |null outside mode regions
  "running": { ...Running },      // |null
  "servers": [ ...Server ],       // every catalog server, then the own servers
  "regions": [ {"id": "uk", "title": "UK", "chain": ["Clouvider-London", "Leaseweb-London", "OVH-London"]} ],
  "per_region": [ ...Row ],       // the heatmap rows: every region, in catalog order
  "per_server": [ ...Row ],       // servers with tests in the span, catalog order then own servers
  "latest": { ...Test },          // |null: the newest test of any kind
  "pattern": {                    // |null: a clear time-of-day dip of your own line (same rule as Home)
    "text": "Evenings are slower: about 40% below your daytime speed.",
    "level": "info",
    "hours": [19, 20, 21, 22],    // local hours of the dip, in order (may wrap: [23, 0, 1])
    "drop": 0.4                   // rounded to 0.1
  }
}
```

### Running

```jsonc
{
  "started": 1760104790000,
  "expected_end": 1760104810000,  // a typical test's end; it can run up to 45 s
  "direction": "download",        // "picking" (nearest: RTT checks) | "download" | "upload"
  "server": {"id": "Leaseweb-NewYork", "label": "Leaseweb New York"},   // |null while picking
  "region": "us-east"             // |null outside mode regions
}
```

### Server

```jsonc
{
  "id": "Leaseweb-NewYork", "label": "Leaseweb New York", "provider": "Leaseweb",
  "city": "New York", "country": "US", "geo": "na", "region": "North America",  // region: the geo's title (not a test region)
  "rtt_ms": 82.4,                 // |null: the last RTT check (nearest mode, Test now); null when never checked
  "picked": false,                // one of the 2 nearest
  "custom": false,                // the user's own server
  "host": "speedtest.nyc1.us.leaseweb.net",   // local only
  "ports": "5201-5210"                        // local only
}
```

### Row (`per_region[]`, `per_server[]`, and `row` of a single view)

```jsonc
{
  "id": "us-east", "label": "US East",   // region title, or server label
  "latest_at": 1760104790000,     // |null
  "n": 96,                        // ok tests
  "busy": 2, "unreachable": 0, "failed": 1,
  "fallback": 7,                  // ok tests on a fallback server (regions)
  "down_median_bps": 181000000,   // |null without ok tests (all *_bps below likewise)
  "down_p10_bps": 150000000,      // the slow end
  "up_median_bps": 38000000, "up_p10_bps": 35000000,
  "best_down_bps": 196000000,     // p90 of the downloads: the row's "best"
  "dip": { ...pattern },          // |null: this row's own time-of-day dip, same shape as pattern
  "hours": [                      // always 24 entries, local hours
    {"hour": 0, "n": 4, "down_median_bps": 188000000, "up_median_bps": 38000000,
     "down_norm": 0.96}           // down median ÷ best_down_bps, 0.01 steps; |null when the medians are
  ],
  "fallback_share_24h": 0.12,     // regions only (omitempty): share of the last 24 h's ok tests on a fallback server
  "fallback_note": "UK mostly used a fallback server in the last 24 hours: its first choice was busy or not answering."   // regions only, omitempty: when the share is over 0.5
}
```

**Sample size.** An hour cell needs `n ≥ 3` tests for its medians; otherwise they are null. In mode `regions` a region gets 4 tests per hour of day per day, so after one day nearly every cell is filled. Server rows in `regions` mode fill only for the servers in use. Show cells with n < 3 as "filling in".

### Test

```jsonc
{
  "id": 412,
  "start": 1760104790000, "end": 1760104808000,   // the window: probes in it (+2 s) are not judged
  "trigger": "schedule",          // "schedule" | "manual" (Test now) | "sweep"
  "on_demand": false,             // trigger "manual" or "sweep": left out of rows, hours, dips and patterns (still in test lists and latest)
  "outcome": "ok",                // "ok" | "busy" | "unreachable" | "failed"
  "region": "us-east",            // |null outside mode regions
  "server": "Leaseweb-NewYork", "label": "Leaseweb New York",   // the server actually used
  "fallback": false,              // not the region's first choice
  "port": 5203,                   // local only
  "attempts": 2,                  // connection attempts over every server tried
  "down_bps": 182345678,          // |null when not measured
  "up_bps": 38123456,             // |null
  "down_retrans": 112, "up_retrans": null,      // |null when unknown
  "idle_gateway_ms": 1.1, "loaded_gateway_ms": 9.8,   // |null: router RTT 2 min before / during
  "idle_call_ms": 14.2, "loaded_call_ms": 61.0        // |null: call path (STUN) before / during
}
```

**Outcomes.**
- `busy`: every port tried, and every server of the chain within the slot's budget, was running someone else's test. Never draw it as a slow or zero result; show a gap labelled "server busy".
- `unreachable`: no server of the chain accepted a connection (timeouts, DNS failures, or refusals on every port).
- `failed`: anything else.

When a download worked but the upload failed, `down_bps` is set and `outcome` is the upload's; no fallback follows.

**Fallback.** For each server in the chain:
- busy or refused tries 2 more ports, 2 s apart;
- unreachable moves to the next server at once.

There is no new attempt after 25 s, and a slot is cut off at 45 s. A slot never pushes later slots back; a late one is skipped.

## `GET api/speed?server=<id>` or `?region=<id>`

One server's or region's view. It takes `days`, `tz` and `exclude_fallback` (the last only with `region`). An unknown id returns 404.

```jsonc
{
  "at": ..., "tz": 120, "days": 7, "exclude_fallback": false,
  "kind": "region",               // "region" | "server"
  "row": { ...Row },
  "tests": [ ...Test ],           // every test in the span, oldest first (a region: ≤ 30 × 96)
  "windows": [ {"start": ..., "end": ...} ]
}
```

For compare, make one request per server or region (up to 4).

## `GET api/speed?part=windows&from=...&to=...`

```json
{"at": 1760104800000, "windows": [{"start": 1760104790000, "end": 1760104808000}]}
```

`from` and `to` take the same forms as `/api/panel`: `now`, `now-6h`, `now-7d` or unix ms, up to 90 days apart. The defaults are `now-30m` and `now`. Shade these on every time chart. Probe samples that ran inside a window, or within 2 s after it, are stored as "not measured", so charts show a gap. The verdict, call minutes, Home and history never count them.

## `GET api/speed?part=running`

A light poll for the "testing now" line: `{"at": ..., "running": { ...Running } | null}`.

## Queue

Every test goes through one process-wide queue, and exactly one runs at a time:
- **Test now:** one server, a region's chain, or the nearest servers.
- **Sweep:** every catalog server and the user's own servers, one test each, about 45.
- **Scheduled slots:** a slot is enqueued when it falls due. It is **dropped** if it is still waiting 10 minutes later, and a slot already waiting is never stacked.

The order is by class, FIFO within each: the owner's entries (the local dashboard) first, then schedule slots, then the share link's. Nothing interrupts the running test. A public sweep **yields** between its tests whenever an owner entry or a schedule slot is waiting, and then resumes at the head of the share link's entries. Schedule slots therefore never starve behind public sweeps.

There is no cooldown. Sweep and Test now results are stored with `on_demand: true` and kept out of the patterns.

**The share link's tests are an owner opt-in.** They are off by default, turned on with `--speedtest-public` or the Details toggle (`POST api/speed/config {"public": true}`), and the choice is saved. Turning them off drops the share link's waiting entries.

**The share link has a data budget:** at most `--speedtest-public-budget` (default 50 GB) over a rolling 24 hours.
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

A new public sweep can start at the earliest 30 minutes after the last one ended (429 `sweep_cooldown` with `available_at`); single tests are not affected. A capped share link gets 429 `budget_exhausted` with `"budget": "time"` and `resets_at`. Visitors therefore can't blank out the monitoring.

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
  "tests": [412, 413]           // the stored tests so far
}
```

On the share link, `who` is `you` only for that visitor's own entries; everyone else's, the owner's included, are `someone`. Locally, the owner's are `you` and the share link's are `someone`. Targets and servers use the share link's ids (`custom-1`), and no host, port or address appears.

### `GET api/speed?part=entry&id=<id>`

One entry: waiting, running, or finished less than an hour ago. Otherwise 404.

### `GET api/speed?part=sweep`

The last finished sweep and its tests: `{"at": ..., "entry": { ...Entry } | null, "tests": [ ...Test ]}`.

### `POST api/speed/run?kind=single[&id=<server or region id>]` or `?kind=sweep`

"Test now" (no `id`: the nearest servers) or a sweep. There is no body; `kind` defaults to `single`. The share link's own-server ids (`custom-1`) are accepted.

The response is `202 {"entry": { ...Entry }, "duplicate": false}`. `duplicate: true` means the same requester (the owner, or the same share-link visitor) already had an equal entry waiting or running, and this is that entry. An unknown id returns 404 `not_found`.

**Local:** the same protections as notes:
- same origin;
- `X-FYISP-Token`;
- the admin token or session cookie when one is set.

It works in every mode, `off` included.

**Share link:** `POST /s/<secret>/api/speed/run?…` needs no token. It has these protections:
- **Opt-in:** 403 `disabled` while the owner hasn't turned it on.
- **Budget:** 429 `budget_exhausted` with `budget` (`data` or `time`), `resets_at` (ms) and `retry_after` once the data budget or the test time is spent.
- **Sweep gap:** 429 `sweep_cooldown` with `available_at` and `retry_after` for a sweep within 30 minutes of the last public one.
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
{"mode": "regions"}            // and/or
{"public": true}               // let the share link queue tests (saved; --speedtest-public sets it at start)
```

- `200 {"mode": "regions", "next_at": 1760104920000, "public": true}`, with the fields it changed; `next_at` is null for `off`.
- `400` for a mode not in `modes`, for neither field, or for any other field.

The choice is saved and survives restarts. A `--speedtest…` flag on the command line overrides it at start. Switching to a mode schedules the first test about 2 minutes later.

## `GET api/status`

`speed_masked_share_24h` (omitempty) is the share of the last 24 hours under test. Probes in that time are stored as "not measured".

## `api/home` (live part)

`HomeLive` gains an optional `speed`:

```json
"speed": {"text": "Evenings are slower: about 40% below your daytime speed.", "level": "info"}
```

It is present only when there is a clear pattern in the user's own line. Each test is taken as a fraction of its region's (or server's) p90, so far servers that always read lower neither hide nor fake a dip. Fallback tests are left out. Send `tz` on `live` too, since the wording depends on local hours.

## Metrics (`/metrics`)

These give the latest result per region and server, set after every test:

| Metric | Labels | |
|---|---|---|
| `fyisp_speed_download_bits_per_second` | `region`, `server` | gauge (`region` empty outside mode regions) |
| `fyisp_speed_upload_bits_per_second` | `region`, `server` | gauge |
| `fyisp_speed_loaded_rtt_seconds` | `region`, `server`, `layer` (`gateway`, `call`), `load` (`idle`, `loaded`) | gauge |
| `fyisp_speed_tests_total` | `region`, `server`, `outcome` | counter |

## Export

`fyisp export --speedtests` writes CSV with these columns: `id,start,end,trigger,outcome,region,server,fallback,port,attempts,down_bps,up_bps,down_retrans,up_retrans,idle_gateway_ms,loaded_gateway_ms,idle_call_ms,loaded_call_ms`.
