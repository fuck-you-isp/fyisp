# Speed tests: API contract (fyisp v1.1)

Scheduled iperf3 speed tests (see the brief): download then upload, 8 streams, 10 s each with the first second excluded, against the two servers of the catalog list nearest by RTT, alternating. Opt-in: off unless started with `--speedtest` or switched on in Details. The Go types live in `internal/web/speed.go`; keep the two in sync.

Routes are relative like every other API: `/api/speed` locally, `/s/<secret>/api/speed` on the share link. Errors are `{"error": "..."}` with 400, 403, 404, 409, 429 or 503, as elsewhere. Unknown parameters return 400.

When fyisp runs without a speed-test source (an old build, `fyisp-webdev -no-speed`), every route answers 404: hide the Speed tab.

## `GET api/speed`

```
GET api/speed?days=7&tz=120
GET api/speed?part=windows&from=now-6h&to=now
```

| Param | Values | Default | Notes |
|---|---|---|---|
| `part` | `summary`, `windows` | `summary` | |
| `days` | `7`, `30` | `7` | `summary` only. The span of `tests`, `windows` and `by_hour`. |
| `tz` | minutes **east** of UTC, integer -840..840 | `0` | `summary` only. Rounded to 15 min and echoed. Hours of day in `by_hour` and `pattern` are local to it. Send `-new Date().getTimezoneOffset()`. |
| `from`, `to` | as `/api/panel` (`now-6h`, unix ms, RFC 3339) | `now-30m`, `now` | `windows` only, at most 90 days. |

Caching: `summary` 30 s, `windows` 60 s, on both listeners. Every response has `at` (server time, unix ms).

### `part=summary`

```jsonc
{
  "at": 1760104800000,
  "tz": 120,
  "days": 7,
  "enabled": true,              // the schedule runs (flag or Details toggle)
  "configurable": true,         // this viewer may toggle it and run "Test now" (local listener, controls allowed); false on the share link
  "interval_s": 900,            // schedule interval (15 min, or more; less only with a custom server)
  "next_at": 1760105612000,     // next scheduled test (jitter included), null when disabled or running
  "running": {                  // null when idle
    "started": 1760104790000,
    "direction": "download",    // "download" | "upload" | "picking" (measuring RTT to the servers)
    "server": "Clouvider-London"   // omitted on the share link
  },
  "custom_server": "iperf.example.net:5201",   // --speedtest-server, else omitted; never on the share link
  "servers": [                  // the catalog list with the last RTT check; omitted on the share link
    {"name": "Clouvider-London", "city": "London", "country": "GB", "rtt_ms": 12.4, "picked": true},
    {"name": "Eranium-Amsterdam", "city": "Amsterdam", "country": "NL", "rtt_ms": 18.9, "picked": true},
    {"name": "Leaseweb-Singapore", "city": "Singapore", "country": "SG", "rtt_ms": null, "picked": false}
  ],
  "latest": { ...Test },        // the newest test of any outcome, null if none ever
  "tests": [ ...Test ],         // every test in the last `days` days, oldest first (≤ 30 × 96)
  "by_hour": {
    "hours": [                  // always 24 entries, hour 0..23 local
      {"hour": 0, "n": 9, "down_median_bps": 412000000, "down_p10_bps": 380000000,
                          "up_median_bps": 41000000, "up_p10_bps": 39000000},
      {"hour": 1, "n": 0, "down_median_bps": null, "down_p10_bps": null, "up_median_bps": null, "up_p10_bps": null}
      // ...
    ],
    "dip_hours": [19, 20, 21, 22]   // hours to highlight (see pattern), [] if none
  },
  "pattern": {                  // null unless there is a clear pattern (same rule as Home)
    "text": "Evenings are slower: about 40% below your daytime speed.",
    "level": "info",
    "dip_from": 19, "dip_to": 23,   // local hours, dip_to exclusive (wraps past midnight: 22 → 2)
    "drop": 0.4                      // download, dip hours vs the other hours, rounded to 0.1
  },
  "windows": [ {"start": 1760104790000, "end": 1760104823000} ]   // test windows in the span, oldest first
}
```

`by_hour` and `pattern` use tests with `outcome` `ok` only. An hour needs `n ≥ 3` for its medians (else `null`). `p10` is the 10th percentile (the slow end). Bits per second throughout; format as Mbit/s.

**Pattern rule** (also Home's line): over the last 30 days whatever `days` is asked for, at least 12 hours with `n ≥ 3`; a run of ≥ 2 consecutive hours whose download median is ≤ 75 % of the median of the other hours. `drop` is 1 − (median of the run's medians ÷ the others' median). The wording names the run by its middle hour: 18–23 "Evenings", 0–5 "Nights", 6–11 "Mornings", 12–17 "Afternoons"; "below your daytime speed" for evenings and nights, else "below your usual speed".

### Test

```jsonc
{
  "id": 412,
  "start": 1760104790000, "end": 1760104823000,  // the window: probes in it are not judged
  "trigger": "schedule",        // "schedule" | "manual" (Test now)
  "outcome": "ok",              // "ok" | "busy" | "unreachable" | "failed"
  "server": "Clouvider-London", // omitted on the share link
  "city": "London",             // omitted on the share link
  "port": 5203,                 // omitted on the share link
  "attempts": 2,                // connection attempts, 1..3 per direction summed
  "down_bps": 412345678,        // null when not measured
  "up_bps": 40123456,           // null when not measured
  "down_retrans": 112,          // null when unknown
  "up_retrans": null,
  "idle_gateway_ms": 1.1,       // median RTT to your router in the 2 minutes before the test; null if unknown
  "loaded_gateway_ms": 9.8,     // ... during the test
  "idle_call_ms": 14.2,         // call path (STUN) median before
  "loaded_call_ms": 61.0        // ... during
}
```

Outcomes: `busy` means every attempt found the server running someone else's test. It is never a slow result, so draw it as a gap with a "server busy" label, not as zero. `unreachable` means the server did not accept a connection. `failed` covers anything else, such as a timeout or a protocol error. When download worked but upload did not, `down_bps` is set and `outcome` is the upload's.

### `part=windows`

```json
{"at": 1760104800000, "windows": [{"start": 1760104790000, "end": 1760104823000}]}
```

Shade these on every time chart. Probe samples inside a window are stored as "not measured", so charts show a gap there. The verdict, call minutes, Home and history never count them.

## `POST api/speed/run` (local only)

"Test now". Same protections as notes: same origin, `X-FYISP-CSRF`, and the admin token or session cookie when one is set. No body.

- `202 {"started": 1760104800000}`: started. Poll `summary` until `running` is null.
- `409 {"error": "a speed test is already running"}`.
- `403` when controls are not allowed (see notes).

It works even while the schedule is off. Manual tests are not rate-limited beyond one at a time.

## `POST api/speed/config` (local only)

Switches the schedule on or off. The setting is saved and survives restarts; `--speedtest` on the command line switches it on at every start. Same protections as `run`.

```
POST api/speed/config
{"enabled": true}
```

`200 {"enabled": true, "next_at": 1760105612000}`. A body with any other field, or a missing `enabled`, returns 400.

## `api/home` (live part)

`HomeLive` gains an optional `speed`:

```json
"speed": {"text": "Evenings are slower: about 40% below your daytime speed.", "level": "info"}
```

It is present only when `pattern` (above) is non-null. The text is the same as `pattern.text`. No other numbers go on Home.

## Metrics (`/metrics`)

Latest result per server, set after every test:

| Metric | Labels | |
|---|---|---|
| `fyisp_speed_download_bits_per_second` | `server` | gauge; absent until a test measured it |
| `fyisp_speed_upload_bits_per_second` | `server` | gauge |
| `fyisp_speed_loaded_rtt_seconds` | `server`, `layer` (`gateway`, `call`), `load` (`idle`, `loaded`) | gauge |
| `fyisp_speed_tests_total` | `server`, `outcome` | counter |
