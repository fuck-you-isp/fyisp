# On-demand traces (*Investigate*) API

Part of the [API index](API.md).

An on-demand trace follows one profile target's path hop by hop every 2.5 s, for **250 rounds** (about 10.4 minutes), and then stops by itself. Always-on traces (`trace: true` in the profile) are not affected.

## Probe types

A trace has a mode, which picks its probes. None needs admin rights.

| `mode` | Probes | Like | Hop series kind |
|---|---|---|---|
| `icmp` (default) | ICMP echo requests | `mtr` | `trace` |
| `tcp` | TCP SYNs to port 443, closed with a reset | `mtr -T -P 443` | `trace_tcp` |
| `udp` | UDP datagrams to port 33434; the destination answers "port unreachable" | `mtr -u` | `trace_udp` |

The port is fixed per trace, so routers that balance by port (ECMP) keep the trace on one path. Each mode of a target is its own trace: its own session, slot and start in the budget, numbers and route. Always-on traces are ICMP. Hops are shared only between traces of the same mode, but the cap of 1 probe a second per router counts every mode. Route changes are recorded for ICMP traces only; the route of a TCP or UDP trace is kept in memory while fyisp runs, so after a restart its hop numbers have no addresses until it is traced again.

`GET api/trace` lists what this machine supports in `modes`:

```json
"modes": [{"mode":"icmp","available":true},
          {"mode":"tcp","port":443,"available":true,"hops_via":"icmp"},
          {"mode":"udp","port":33434,"available":false,"reason":"windows_firewall"}]
```

- `hops_via: "icmp"` (Windows): the default firewall drops the routers' replies to a standard user's TCP probes, so the routers are found with ICMP echo requests and only the destination is probed with a TCP connect to the port, with the connect's round trip.
- `reason` of a mode that isn't available: `windows_firewall` (UDP on Windows, for the same reason) or `os` (this OS can't send it without admin rights).

Linux reads the routers' replies from each probe socket's error queue (`IP_RECVERR`; no ping sockets needed). macOS gets them on its unprivileged ICMP socket and matches them by the quoted header. IPv4 only.

## Start

```
POST api/investigate?target=<profile target name>[&mode=icmp|tcp|udp]
```

There is no body. Anyone who can open the dashboard may start a trace: there is no admin token check. The checks are:

| Check | |
|---|---|
| Host allowlist | yes |
| Same origin (`Origin` / `Sec-Fetch-Site`) | yes |
| `X-FYISP-Token` (CSRF, page meta) | yes |
| Slots | at most 4 traces at once |
| Start budget | at most 12 new traces in a rolling hour, across all targets and viewers. A request carrying the admin token (`X-FYISP-Admin` or the session cookie) skips the budget, not the slots |

**Request rules:**
- Exactly one `target` parameter, at most one `mode` (default `icmp`) and nothing else, with no body: a declared length or chunking is refused before reading, and anything sent anyway is read up to 1 KiB and refused.
- Only profile target names are accepted; never a host or an address.
- Every response past the host check has `Cache-Control: no-store`.
- Logs name the target and the error, never the client.

**What a POST does:**
- **Trace running:** returns its state. It doesn't extend the trace or reset the count, and it uses no budget.
- **Trace finished:** starts a fresh 250 rounds.
- Only starts that succeed use the budget; refused ones (no slot, unknown target, a tracer error) don't.

Client addresses behind a tunnel can't be trusted, so the budget, the slots, the 250 rounds and the tracer's per-router probe cap are the only bounds on a script that starts traces.

**Response 200:**

```json
{"target":"Microsoft-Teams","mode":"tcp","state":"running","rounds_done":37,"rounds_max":250,
 "interval_ms":2500,"started_at":1760103900000}
```

`state` is `running` or `finished`; `finished_at` (ms) is present when finished.

**Errors:** `{"error": "<words for people>", "code": "<stable>", "retry_after": <seconds, with Retry-After>}`. Match on `code`, never on the words.

| Status | `code` | When |
|---|---|---|
| 400 | `bad_request` | a parameter other than one `target` and one `mode`, an empty `target`, an unknown `mode`, or a body |
| 400 | `unsupported_mode` | this machine can't trace with `mode` (see `modes`) |
| 403 | `forbidden` | cross-origin; missing or bad `X-FYISP-Token` |
| 404 | `unknown_target` | not a profile target name |
| 404 | `unavailable` | tracing is not available |
| 405 | `method_not_allowed` | not POST |
| 421 | `unknown_host` | the `Host` isn't allowed (open the dashboard's own address, or add `--allow-host`) |
| 429 | `busy` | no free slot (4 in all; `retry_after`) |
| 429 | `rate_limited` | 12 traces started in the last hour (`retry_after`: until the oldest leaves the hour) |
| 500 | `failed` | the tracer failed (with the reason) |

## Progress

The dashboard posts by itself when Investigate opens only for the owner: when `/api/status` has `"can_control": true` (this request may use the speed-test controls, per request: false through a proxy on a token-less dashboard) or the browser holds an admin token pasted earlier, which it then sends as `X-FYISP-Admin`. Everyone else clicks *Trace*.

`GET api/trace?target=&mode=` carries `"investigation": {…same object…}` of that mode while the trace runs and for an hour after it finishes. The UI polls it and never re-posts to keep a trace alive.

## Hop statistics

`GET api/trace` sends, per hop, the numbers `mtr` shows. Times are milliseconds.

| Field | mtr | What it is |
|---|---|---|
| `n`, `lost` | `Snt` is `n + lost` | answered and unanswered probes; slots that weren't measured are neither |
| `loss` | `Loss%` | `lost / (n + lost)`, a fraction |
| `last` | `Last` | the newest answered round trip in the range |
| `mean`, `min`, `max` | `Avg`, `Best`, `Wrst` | |
| `stdev` | `StDev` | the sample standard deviation of the answered round trips (0 for one) |
| `p95` | | the 95th percentile |
| `jitter` | `Javg` | the mean difference between consecutive answered round trips |
| `jmax` | `Jmax` | the largest such difference |

A slot that wasn't measured (fyisp stopped, no trace running) breaks the run of consecutive round trips for `jitter` and `jmax`; a lost probe doesn't. For ranges up to 48 hours the numbers come from every sample. Longer ranges use the hourly summaries: `p95` is then the probe-weighted 95th percentile of the hourly ones, `jitter` is 0, and `last`, `stdev` and `jmax` are `null`. They are also `null` for a hop without answers.

`host` is the bare name (or address) the target's trace goes to, for running `mtr` yourself: never a scheme, port, path or query. It is left out for the gateway and the ISP edge. `mode` and `port` say which trace the numbers are of. `interval_ms` is the always-on traces' round interval; an on-demand trace reports its own in `investigation`.

Each round probes TTL 1 upwards until the destination answers. Like `mtr --max-unknown`, it stops after 5 TTLs in a row past the last answer stay silent, but never before the destination's TTL once that is known. TTLs not probed have no sample that round.

## Hop loss flags

Each hop in `GET api/trace`'s `hops` has two flags that say whether its loss is real. fyisp sets them over the requested range. The destination is the deepest hop with probes in the range.

| Flag | Set when |
|---|---|
| `loss_continues` | the hop loses at least 5% of probes, and the destination loses at least half as much. Packets are really dropped at this hop or before it |
| `rate_limited` | the hop loses at least 5% of probes, but the destination loses less than half as much. The router rate-limits its replies to trace probes, and forwarded traffic is not affected |

Loss below 5% sets neither flag. The destination's own loss, when 5% or more, sets `loss_continues`.

*Investigate* labels hops only from these flags: *loss starts here* on the first hop with `loss_continues` (one marker, not one per flagged hop), and *not real — router rate-limiting* on each hop before the destination with `rate_limited`. Other hops show only their loss. The summary names the first hop before the destination with `loss_continues` as where real loss starts. It blames the destination only when the destination has `loss_continues` and no hop before it does. When no hop is flagged but the destination loses probes, it says there is too little loss to tell where it starts.
