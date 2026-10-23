# fyisp API

Every route the dashboard serves, the `/metrics` series and the `fyisp export` CSV columns. Four areas have their own detail docs: [HOME-API.md](HOME-API.md) (Home), [SPEEDTEST-API.md](SPEEDTEST-API.md) (speed tests), [STORAGE-API.md](STORAGE-API.md) (storage tests) and [TRACE-API.md](TRACE-API.md) (*Investigate*). Everywhere else the Go types in `internal/web` are the reference; this page names them.

## Conventions

- **Paths are relative** to the dashboard's address (`api/status`, not `/api/status`), as the UI calls them.
- **Hosts.** A request whose `Host` is not the dashboard's own address, an `--allow-host` name or the current `--tunnel` name gets 421 `unknown_host`, on every route.
- **Parameters.** Unknown or repeated parameters return 400, on every route except `/`, which ignores them.
- **`from` and `to`** take `now`, `now-<n><s|m|h|d>` (e.g. `now-6h`) or unix milliseconds. `to` is clamped to now, and a span longer than the route's limit is shortened from the start.
- **Times in responses** are unix milliseconds, except RFC 3339 in `api/status`, `api/verdict`, `api/incidents` and `api/trace`'s `route.since` and `changes[].at`.
- **Errors** are `{"error": "<sentence>", "code": "<word>"}`. Match on `code`, never on the sentence: `bad_request` (400), `unauthorized` (401, no or the wrong admin token), `forbidden` (403), `not_found` (404), `method_not_allowed` (405), `unknown_host` (421), `unavailable` (503, retry after `Retry-After`) and `error` (500). Some routes add their own codes; their detail docs list them.
- **Caching.** JSON responses carry `Cache-Control: no-store`; static files carry an ETag and `no-cache`.
- **The admin token.** Every route but `/`, `static/*` and `api/admin/session` needs the [admin token](../README.md#admin-token): `Authorization: Bearer <token>`, `X-FYISP-Admin: <token>`, or the session cookie that `api/admin/session` sets. Without it they answer 401 `unauthorized` with `WWW-Authenticate: Bearer`.
- **Writes** (every `POST` and `DELETE`) also need the same origin and the page's CSRF token in `X-FYISP-Token`. [SECURITY.md](../SECURITY.md#what-leaves-your-machine) has the table of checks per route.

## Routes

| Method | Path | Purpose | Detail |
|---|---|---|---|
| GET | `/` | the dashboard page (with the CSRF token in a meta tag) | |
| GET | `static/…` | the dashboard's scripts, styles and images | |
| GET | `api/status` | version, start time, ICMP support, warm-up and store health | [below](#get-apistatus) |
| GET | `api/profile` | the profile: groups, targets and which features are on | [below](#get-apiprofile) |
| GET | `api/home` | Home: the answer card, connection strip, tiles, speed and problems | [HOME-API.md](HOME-API.md) |
| GET | `api/panel`, `api/panel.csv` | one group's chart data, as JSON or CSV | [below](#get-apipanel-and-apipanelcsv) |
| GET | `api/verdict` | the current verdict | [below](#get-apiverdict-and-apiincidents) |
| GET | `api/incidents` | the outage log | [below](#get-apiverdict-and-apiincidents) |
| GET | `api/baselines` | each series' normal and how it compares now | [below](#get-apibaselines) |
| GET | `api/overview` | the Overview: every target of one probe kind | [below](#get-apioverview) |
| GET | `api/calls` | the Calls view | [below](#get-apicalls) |
| GET | `api/trace` | a traced target's route, hop statistics and route changes | [below](#get-apitrace-and-apitracepanel) |
| GET | `api/trace/panel` | per-hop chart data for a traced target | [below](#get-apitrace-and-apitracepanel) |
| POST | `api/investigate` | start an on-demand trace of a profile target or any public host | [TRACE-API.md](TRACE-API.md) |
| GET | `api/speed` | speed tests: summary, rows, windows, queue, running test | [SPEEDTEST-API.md](SPEEDTEST-API.md) |
| POST | `api/speed/run`, `api/speed/cancel`, `api/speed/config` | the speed-test controls | [SPEEDTEST-API.md](SPEEDTEST-API.md) |
| GET | `api/storage` | storage tests: the targets, their keys' state and last checks, the live campaigns, history and notices; `?part=` live, campaigns, campaign, round, attempts, estimate | [STORAGE-API.md](STORAGE-API.md) |
| POST | `api/storage/target/add`, `…/update`, `…/delete`, `…/forget-keys`, `api/storage/check` | add, change and remove storage targets; check one | [STORAGE-API.md](STORAGE-API.md) |
| POST | `api/storage/run`, `api/storage/campaign/start`, `…/stop`, `…/pause`, `…/resume`, `…/keys` | storage campaigns | [STORAGE-API.md](STORAGE-API.md) |
| POST, DELETE | `api/admin/session` | swap the admin token for a cookie, or sign out | [below](#post-and-delete-apiadminsession) |
| GET | `/metrics` | Prometheus metrics | [below](#metrics) |

### GET `api/status`

No parameters. `Status` in `internal/web/web.go`:

```json
{"version": "v0.11.0", "started": "2026-10-15T13:39:54Z", "caps": {"icmp": "udp"}, "targets": 28, "ready": 28,
 "store": {"file_bytes": 1048576, "oldest": "…", "last_flush": "…", "buffered_bytes": 4096}}
```

`caps.icmp` is `udp`, `raw`, `iphlpapi` or `unavailable` (then `icmp_hint` says how to enable it). `store` may add `last_flush_err`, `dropped` and `rejected`.

### GET `api/profile`

No parameters. `profileJSON` in `internal/web/api.go`: `name`, `features` (`trace`, `baselines`, `speed`, `storage`), `groups` (`id`, `title`) and `targets` (`name`, `group`, `kinds`, and when they apply `layer`, `trace`, the provider and location fields and `tags`).

### GET `api/panel` and `api/panel.csv`

`group` (required), `from` (default `now-30m`), `to` (default `now`), `points` (10 to 2000, default 1000). The span is at most 90 days. The JSON (`panelData.JSON` in `internal/web/api.go`) has `group`, `tier`, `from`, `to`, `now`, `start`, `step` and `len`, and per series `target`, `kind` (`hop` for trace series) and the bucket arrays `mean`, `min`, `max`, `n`, `lost`, `gap` and `lost_by` (per loss reason). The CSV is the same data as a download. An unknown group is 404; a query that takes too long is 503 with `Retry-After: 2`.

### GET `api/verdict` and `api/incidents`

`api/verdict` takes no parameters and returns the current verdict (`verdictJSON` in `internal/web/verdict.go`): `kind` (see [How fyisp decides](VERDICTS.md#whose-fault-is-it-verdicts)) and, when they apply, `since`, `summary`, `targets`, `evidence`, `slow` (targets slower than their normal) and `quiet`.

`api/incidents` takes `from` and `to` (default the last 30 minutes, at most 90 days) and returns at most 500 incidents, newest first: `[{"id", "start", "end", "kind", "summary", "targets", "peak_loss"}]`. `end` is absent while an incident lasts.

### GET `api/baselines`

`group` (optional: without it, every series) and `at` (default `now`). Returns `{"group", "at", "series": [{"target", "kind", "median_ms", "p95_ms", "loss", "samples", "hour_of_day", "now_ms", "ratio_now"}]}` (`baselinesJSON` in `internal/web/baselines.go`). 404 when baselines are off.

### GET `api/overview`

`from`, `to` (default the last 30 minutes, at most 30 days) and `kind` (`tcp`, the default, `https` or `icmp`). Returns `{"from", "to", "kind", "capped", "summary", "rows"}`: `summary` counts targets, slow, lossy and failing ones, and the providers and regions affected; each row has `target`, `state`, `now_ms`, `n`, `lost`, `loss`, `reason` and, once known, `normal_ms`, `normal_p95_ms`, `ratio` and `hour_of_day` (`overviewJSON` in `internal/web/overview.go`).

### GET `api/calls`

`from`, `to` (default the last 5 minutes, at most 48 hours). Returns `{"from", "to", "start", "step", "capped", "state", "quiet", "cause", "path", "apps"}`: the top state and why (`cause`), your call path's scores per minute, and one row per app (`callsJSON` in `internal/web/calls.go`). The scoring is in [Call quality](CALL-QUALITY.md).

### GET `api/trace` and `api/trace/panel`

`api/trace` takes `target` (a profile target, or a host's trace such as `host:example.com`, see [TRACE-API.md](TRACE-API.md#hosts-outside-the-profile)), `mode` (`icmp`, the default, `tcp` or `udp`: which trace, see [TRACE-API.md](TRACE-API.md#probe-types)), `from` and `to` (default the last 30 minutes) and returns the `mode` and its `port` (TCP, UDP), the modes this machine supports (`modes`, each with `available` and, when off, a `reason`: `windows_firewall`, `os` or `icmp_unavailable`, see [TRACE-API.md](TRACE-API.md#probe-types)), the target's bare `host` name or address (never a port or path; left out for the gateway and the ISP edge), the always-on round interval `interval_ms`, its current `route` (`since`, `hops`), per-hop statistics (`hops`: `n`, `lost`, `loss`, `min`, `mean`, `max`, `p95`, `jitter`, `last`, `stdev` and `jmax`, see [TRACE-API.md](TRACE-API.md#hop-statistics), and the loss flags `loss_continues` and `rate_limited`, see [TRACE-API.md](TRACE-API.md#hop-loss-flags)), the route `changes` in the range, newest first (ICMP only), and the `investigation` of that mode while one runs or for an hour after (`traceJSON` in `internal/web/trace.go`). A hop has `hop` and, when known, `ip`, `rdns`, `asn` and `owner`.

`api/trace/panel` takes `target`, `mode`, `hops` (e.g. `1,4,12`; default every hop of the current route), `from`, `to` and `points`, and answers in `api/panel`'s format with one series per hop (kind `trace`, `trace_tcp` or `trace_udp`).

Both answer 404 when tracing is off or the target is neither a profile target nor a valid host's trace name (`host:<host>`).

### POST and DELETE `api/admin/session`

No parameters and no body; it needs no session, so a browser can sign in. `POST` with the admin token in `X-FYISP-Admin` sets an HttpOnly, SameSite=Strict cookie that stands for it from then on (an HMAC of the token, not the token; also `Secure` when the page is HTTPS: through `--tunnel`, an `https://` Origin or `X-Forwarded-Proto: https`), and answers 403 for a wrong token; `DELETE` clears the cookie. Both return `{"admin": true|false}`.

## Metrics

`/metrics` serves Prometheus metrics. ICMP results use the same `ping_*` metric names, help, types and labels as [network_exporter](https://github.com/syepes/network_exporter). Differences: the `target` label is the host name (network_exporter uses the IP it resolved once at startup), and `target_ip` is empty.

The services are measured with TCP only, so `ping_*` covers the Network path (`Gateway`, `ISP edge` and the two anycast resolvers) plus any ICMP targets of your own `--config` file. The services' latency is `fyisp_tcp_rtt_seconds{name,target}` (the latest connect time) and their loss is `fyisp_probe_lost_total{kind="tcp"}` over `fyisp_probe_samples_total{kind="tcp"}`; point network_exporter panels and alerts that used `ping_*` for services at those. fyisp never fills `ping_*` from TCP results: a connect time is not an echo reply. `fyisp_https_rtt_seconds` exists only for `--config` targets that ask for HTTPS.

UDP (STUN) targets never feed `ping_*`. Each burst counts once in `fyisp_probe_samples_total{kind="udp"}` (and `fyisp_probe_lost_total{kind="udp"}` when nothing answered); the latest burst's values are `fyisp_udp_rtt_seconds{name,target}` (median round trip), `fyisp_udp_jitter_seconds` and `fyisp_udp_loss_ratio` (0 to 1), each absent while the latest burst did not measure it.

`fyisp_call_mos{app}` is the call-quality score the Calls view shows (MOS, 1 to 4.5; 4.0 and up is good, 3.6 fair, 3.1 poor, below that unusable): the worst minute of the last 5 minutes. `app="path"` is your call path, and `app="meet"` and `app="teams"` are the measured apps (Teams only while the Teams relay is measured, as in `default` and `calls`). Each is absent until its STUN servers have 10 bursts in the window. With `--no-udp` there are no `fyisp_udp_*` or `fyisp_call_mos` series.

Every series (`internal/metrics`):

| Metric | Labels | |
|---|---|---|
| `ping_up`, `ping_targets` | | network_exporter compatibility |
| `ping_status`, `ping_rtt_seconds`, `ping_rtt_snt_count`, `ping_rtt_snt_fail_count`, `ping_rtt_snt_seconds`, `ping_loss_percent` | `name`, `target`, `target_ip` (`ping_rtt_seconds` also `type`) | ICMP targets |
| `fyisp_tcp_rtt_seconds`, `fyisp_https_rtt_seconds` | `name`, `target` | gauge: the latest connect time or HTTPS round trip; absent while the latest probe failed |
| `fyisp_probe_samples_total` | `name`, `kind` | counter: probes completed |
| `fyisp_probe_lost_total` | `name`, `kind`, `reason` | counter: probes lost |
| `fyisp_udp_rtt_seconds`, `fyisp_udp_jitter_seconds`, `fyisp_udp_loss_ratio` | `name`, `target` | gauge: the latest STUN burst |
| `fyisp_call_mos` | `app` | gauge: call quality, as above |
| `fyisp_verdict` | `kind` | gauge: 1 for the current verdict, 0 for the others |
| `fyisp_layer_healthy` | `layer` (`gateway`, `isp-edge`, `anycast`) | gauge: whether a path layer looks healthy over the last minute; absent while unknown |
| `fyisp_rtt_vs_normal` | `name`, `kind` | gauge: median RTT over the last minute divided by the series' normal; absent while no normal is known |
| `fyisp_trace_hops` | `name` | gauge: hops in a traced target's current route |
| `fyisp_hop_info` | `name`, `hop`, `ip`, `asn`, `owner` | gauge, one per hop of a traced target's route. It puts hop addresses on `/metrics` |
| `fyisp_route_changes_total` | `name` | counter: route changes since fyisp started |
| `fyisp_speed_download_bits_per_second`, `fyisp_speed_upload_bits_per_second` | `region`, `server` | gauge: the latest test against that server that measured it (`region` is empty outside mode `regions`) |
| `fyisp_speed_loaded_rtt_seconds` | `region`, `server`, `layer` (`edge`, `gateway`), `load` (`idle`, `download`, `upload`) | gauge: the median RTT to your provider's first router or your gateway, before the test and during each direction; a lost probe counts as 1 s |
| `fyisp_speed_tests_total` | `region`, `server`, `outcome` (`ok`, `busy`, `unreachable`, `failed`, `cancelled`) | counter: tests by outcome |

The Go runtime's `go_*` and `process_*` series are there too.

## `fyisp export`

`fyisp export` writes the stored samples as CSV to stdout, or to a file with `-o` (created with mode 0600). It reads what the running fyisp has written to disk, which it does every minute. Flags: `--from` and `--to` (RFC 3339, `YYYY-MM-DD`, or an age like `24h` or `7d`; default all the data), `--target` and `--kind` (`https`, `tcp`, `icmp`, `trace`, `trace_tcp`, `trace_udp`, `udp`, `udp_jitter`, `udp_loss`), each repeatable or comma-separated, and `--data-dir`. In Docker: `docker exec fyisp /fyisp export --from 7d > fyisp.csv`.

**Samples**, one row per stored slot:

```
target,kind,ts,rtt_ms,lost,reason,hop
github,tcp,2026-09-26T12:00:15.000Z,,1,timeout,
github,trace,2026-09-26T12:00:00.000Z,8.2,0,,3
```

`ts` is the slot start in UTC, with milliseconds. `rtt_ms` is set only for measured slots. `lost` is 0 or 1, and empty for slots that weren't measured (fyisp was stopped, the machine slept), which are not losses. `reason` is why a probe was lost. `hop` is a traceroute hop's TTL (kinds `trace`, `trace_tcp` and `trace_udp`) and empty for every other kind.

**Speed tests** (`--speedtests`), one row per test, oldest first:

```
id,start,end,trigger,outcome,region,server,fallback,fallback_level,port,attempts,down_bps,up_bps,down_retrans,up_retrans,down_s,up_s,capped_by_server,bytes,score_down,score_up,conn,handoff_asn,isp_asn,edge_idle_p50_ms,edge_idle_loss,edge_down_p50_ms,edge_down_p95_ms,edge_down_loss,edge_up_p50_ms,edge_up_p95_ms,edge_up_loss,gw_down_p50_ms,gw_down_p95_ms,gw_up_p50_ms,gw_up_p95_ms
```

`start` and `end` are in the samples' time format; `outcome` is `ok`, `busy` (the server ran another test: not a slow result), `unreachable`, `failed` or `cancelled`. Speeds are in bits per second, durations in seconds, latencies in milliseconds (lost probes count as 1 s), losses as a fraction, and a column is empty when it wasn't measured. The `edge_*` and `gw_*` columns are [latency under load](SPEED-TESTS.md) to your provider's first router and your own router. The meaning of each field is in [SPEEDTEST-API.md](SPEEDTEST-API.md#test).
