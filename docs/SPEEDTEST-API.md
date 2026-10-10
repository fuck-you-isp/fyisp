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
| 409 | `running` (a speed test runs or is queued); `conflict` elsewhere |
| 421 | `unknown_host` |
| 429 | `rate_limited` |
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

Caching: 30 s on the share link; none locally. Poll every 30 s, and use `part=running` (below) for the "testing now" line.

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
  "trigger": "schedule",          // "schedule" | "manual"
  "outcome": "ok",                // "ok" | "busy" | "unreachable" | "failed"
  "region": "us-east",            // |null outside mode regions
  "server": "Leaseweb-NewYork", "label": "Leaseweb New York",   // the server actually used
  "fallback": false,              // not the region's first choice
  "port": 5203,                   // local only
  "attempts": 2,                  // connection attempts over every server tried
  "down_bps": 182345678,          // |null when not measured
  "up_bps": 38123456,             // |null
  "down_retrans": 112, "up_retrans": null,      // |null when unknown
  "idle_gateway_ms": 1.1, "loaded_gateway_ms": 9.8,   // |null: latency.gateway in short (idle p50; the worse of down/up p50)
  "latency": {                    // |null: latency under load, see below
    "edge":    {"idle": Stats, "down": Stats, "up": Stats},   // the ISP edge: behind the line's queue
    "gateway": {"idle": Stats, "down": Stats, "up": Stats}    // the router: the home network
  },
  "conn": "wired",                // "wired" | "wifi" | "unknown": the interface the test ran on
  "handoff": {"asn": 3356, "name": "LEVEL3", "isp_asn": 3320, "at": 1760090000000}   // local only, |null
}
```

**Latency under load.** Bufferbloat sits in the queue at the line's bottleneck (the modem's upload buffer, the ISP's CMTS, OLT or BNG), past the router: the ISP edge (the first hop past the router) is behind it, the router's LAN address is not. During each direction fyisp pings both every 150 ms (1 s timeout). `idle` is the 2 minutes before the test, from the continuous probes. Each `Stats` is `{"n": 60, "lost": 3, "loss": 0.05, "p50_ms": 140, "p95_ms": 1000}`, or `null` with fewer than 3 probes. **A lost probe counts as loss and, in `p50_ms`/`p95_ms`, as 1000 ms**: under heavy bloat most probes time out, so a p95 of 1000 means "at least 1 s". The edge is missing when it can't be found from here (`edge` phases null).

**Handoff** (local only): the first network outside your ISP on the path to the server (`asn`, `name`), after your ISP's `isp_asn`, from a traceroute at most once a day per server (`at`). Same-metro servers that share it share a path. `null` when unknown (no public hops with an ASN, or the path stays in your ISP's network).

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

## `POST api/speed/run[?id=<server or region id>]` (local only)

"Test now". It uses the same protections as notes: same origin, `X-FYISP-Token` (the page's CSRF token), and the admin token or session cookie when one is set. There is no body.

The test targets depend on `id`:
- no `id`: the 2 nearest servers;
- a server id: that server only;
- a region id: that region's chain.

Share-link ids (`custom-1`) are accepted.

Responses:
- `202 {"started": 1760104800000}`;
- `409` with code `running`;
- `404` for an unknown id;
- `403` when controls are not allowed.

It works in every mode, `off` included.

## `POST api/speed/config` (local only)

```
POST api/speed/config
{"mode": "regions"}
```

- `200 {"mode": "regions", "next_at": 1760104920000}`, with `next_at` null for `off`.
- `400` for a mode not in `modes`, or for a body with any other field.

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
| `fyisp_speed_loaded_rtt_seconds` | `region`, `server`, `layer` (`edge`, `gateway`), `load` (`idle`, `download`, `upload`) | gauge (median; lost probes count as 1 s) |
| `fyisp_speed_tests_total` | `region`, `server`, `outcome` | counter |

## Export

`fyisp export --speedtests` writes CSV with these columns: `id,start,end,trigger,outcome,region,server,fallback,port,attempts,down_bps,up_bps,down_retrans,up_retrans,idle_gateway_ms,loaded_gateway_ms,conn,handoff_asn,isp_asn,edge_idle_p50_ms,edge_idle_loss,edge_down_p50_ms,edge_down_p95_ms,edge_down_loss,edge_up_p50_ms,edge_up_p95_ms,edge_up_loss,gw_down_p50_ms,gw_down_p95_ms,gw_up_p50_ms,gw_up_p95_ms`.
