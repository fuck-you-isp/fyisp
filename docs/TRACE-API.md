# On-demand traces (*Investigate*) API

Part of the [API index](API.md).

An on-demand trace follows one profile target's path, or the path to [any public host](#hosts-outside-the-profile), hop by hop every 2.5 s, for **250 rounds** (about 10.4 minutes), and then stops by itself. Always-on traces (`trace: true` in the profile) are not affected.

## Probe types

A trace has a mode, which picks its probes. None needs admin rights.

| `mode` | Probes | Like | Hop series kind |
|---|---|---|---|
| `icmp` (default) | ICMP echo requests | `mtr` | `trace` |
| `tcp` | TCP SYNs to port 443, closed with a reset | `mtr -T -P 443` | `trace_tcp` |
| `udp` | UDP datagrams to port 33434; the destination answers "port unreachable" | `mtr -u` | `trace_udp` |

The destination port is fixed per trace, but the source port changes with every probe, so routers that balance per flow (ECMP) can still show several addresses at one hop, as with `mtr`. Each mode of a target is its own trace: its own session, slot and start in the budget, numbers and route. Always-on traces are ICMP. Hops are shared only between traces of the same mode, but the cap of 1 probe a second per router counts every mode. Route changes are recorded for ICMP traces only; the route of a TCP or UDP trace is kept in memory while fyisp runs, so after a restart its hop numbers have no addresses until it is traced again.

`GET api/trace` lists what this machine supports in `modes`:

```json
"modes": [{"mode":"icmp","available":true},
          {"mode":"tcp","port":443,"available":true,"hops_via":"icmp"},
          {"mode":"udp","port":33434,"available":false,"reason":"windows_firewall"}]
```

- `hops_via: "icmp"` (Windows): the default firewall drops the routers' replies to a standard user's TCP probes, so every probe is an ICMP echo request and a TCP connect to the port with the same TTL, sent together. The routers are named from the ICMP replies; the destination counts as reached when the connect completes or is refused, with the connect's round trip.
- `reason` of a mode that isn't available: `windows_firewall` (UDP on Windows, for the same reason), `os` (this OS can't send it without admin rights) or `icmp_unavailable` (fyisp could not open its ICMP socket, so the tracer isn't running and every mode, ICMP included, has it).

Linux reads the routers' replies to TCP and UDP probes from each probe socket's error queue (`IP_RECVERR`), so those probes need no ping socket. macOS gets them on its unprivileged ICMP socket and matches them by the quoted header. Tracing in any mode still needs the ICMP socket to start (see [ICMP permissions](../README.md#icmp-permissions)): without it no trace runs, and every mode in `modes` is unavailable with `icmp_unavailable`. IPv4 only.

## Start

```
POST api/investigate?target=<profile target name>[&mode=icmp|tcp|udp]
POST api/investigate?host=<name or IPv4 address>[&mode=icmp|tcp|udp]
```

There is no body. The checks are:

| Check | |
|---|---|
| Host allowlist | yes |
| Admin token | yes (401 `unauthorized` without) |
| Same origin (`Origin` / `Sec-Fetch-Site`) | yes |
| `X-FYISP-Token` (CSRF, page meta) | yes |
| Slots | at most 4 traces at once |
| Public addresses only | a `host` must resolve to public IPv4 addresses only (see [below](#hosts-outside-the-profile)) |

**Request rules:**
- Exactly one `target` or `host` parameter, at most one `mode` (default `icmp`) and nothing else, with no body: a declared length or chunking is refused before reading, and anything sent anyway is read up to 1 KiB and refused.
- `target` is a profile target name, or the name of a host's trace (`host:example.com`), which is the same as `host=example.com`.
- There is no port parameter: TCP traces always go to port 443, UDP traces to port 33434.
- Every response past the host check has `Cache-Control: no-store`.
- Logs name the target and the error, never the client.

**What a POST does:**
- **Trace running:** returns its state. It doesn't extend the trace or reset the count, and it uses no budget.
- **Trace finished:** starts a fresh 250 rounds.
- Only starts that succeed use the budget; refused ones (no slot, unknown target, a refused host, a tracer error) don't.

Client addresses behind a tunnel can't be trusted, so the budget, the slots, the 250 rounds, the public-address check and the tracer's per-router probe cap are the only bounds on a script that starts traces.

**Response 200:**

```json
{"target":"Microsoft-Teams","mode":"tcp","state":"running","rounds_done":37,"rounds_max":250,
 "interval_ms":2500,"started_at":1760103900000}
```

`state` is `running` or `finished`; `finished_at` (ms) is present when finished.

**Errors:** `{"error": "<words for people>", "code": "<stable>", "retry_after": <seconds, with Retry-After>}`. Match on `code`, never on the words.

| Status | `code` | When |
|---|---|---|
| 400 | `bad_request` | a parameter other than one `target` or `host` and one `mode` (a `port` too), an empty one, an unknown `mode`, or a body |
| 400 | `bad_host` | `host` is not a DNS name or an IPv4 address (an IPv6 address, a port, a scheme or a path included) |
| 400 | `unsupported_mode` | this machine can't trace with `mode` (see `modes`) |
| 401 | `unauthorized` | no or the wrong admin token (checked first, after the `Host`) |
| 403 | `forbidden` | cross-origin; missing or bad `X-FYISP-Token` |
| 403 | `not_public` | `host` is, or resolves to, an address that isn't public (the address is not named) |
| 404 | `unknown_target` | not a profile target name |
| 404 | `unresolved` | `host` could not be resolved to an IPv4 address (no address, a lookup failure or a timeout) |
| 404 | `unavailable` | tracing is not available |
| 405 | `method_not_allowed` | not POST |
| 421 | `unknown_host` | the `Host` isn't allowed (open the dashboard's own address or its `--tunnel` link, or add `--allow-host`) |
| 429 | `busy` | no free slot (4 in all; `retry_after`) |
| 500 | `failed` | the tracer failed (with the reason) |

## Hosts outside the profile

`host=` traces any public host, like `mtr` does whatever you type. The checks, in order:

1. **Syntax.** A DNS name (letters, digits and hyphens; at most 253 characters; not all digits in the last label) or an IPv4 address. It is lowercased and loses a trailing dot, so `Example.COM.` and `example.com` are one trace. IPv6 is not traced.
2. **The slots**, before any lookup.
3. **Public addresses only.** The name is resolved with the system resolver, IPv4 only, as targets are. Every address must be public: not private (RFC 1918), loopback, link-local, carrier-grade NAT (100.64/10), multicast, broadcast, "this network", reserved, benchmarking (198.18/15) or documentation space. One such address refuses the host. This keeps a token holder behind your tunnel from mapping your LAN; the Network path already shows your router.

The trace is named `host:<host>` (`"target"` in the response): `GET api/trace?target=host:example.com&mode=` and `api/trace/panel` read it like a target's, with `host` set to it. Profile target names can't start with `host:`. The tracer re-resolves the name every minute, as it does for targets, and checks every answer again: if the name later resolves to an address that isn't public (DNS rebinding), the trace stops (`finished`) and nothing is sent there.

A host's hop numbers are stored like a target's, under its name, and age out with the rest of the data; the name itself stays in the database's index of series. Its route is kept in memory, never stored: while the trace runs and for an hour after, so after that (or a restart) its hop numbers have no addresses. No route changes are recorded for it.

## Progress

The dashboard posts by itself when Investigate opens a profile target without an always-on trace. Opening a host's trace (`#t=host:…`, e.g. from a link someone sent) never starts one: you click *Trace*.

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

Each round probes TTL 1 upwards until the destination answers. Like `mtr --max-unknown`, it stops after 5 TTLs in a row stay silent past the last answer, or past the destination's TTL once that is known. TTLs not probed have no sample that round.

## Hop loss flags

Each hop in `GET api/trace`'s `hops` has two flags that say whether its loss is real. fyisp sets them over the requested range. The destination is the deepest hop with probes in the range.

| Flag | Set when |
|---|---|
| `loss_continues` | the hop loses at least 5% of probes, and the destination loses at least half as much. Packets are really dropped at this hop or before it |
| `rate_limited` | the hop loses at least 5% of probes, but the destination loses less than half as much. The router rate-limits its replies to trace probes, and forwarded traffic is not affected |

Loss below 5% sets neither flag. The destination's own loss, when 5% or more, sets `loss_continues`.

*Investigate* labels hops only from these flags: *loss starts here* on the first hop with `loss_continues` (one marker, not one per flagged hop), and *not real — router rate-limiting* on each hop before the destination with `rate_limited`. Other hops show only their loss. The summary names the first hop before the destination with `loss_continues` as where real loss starts. It blames the destination only when the destination has `loss_continues` and no hop before it does. When no hop is flagged but the destination loses probes, it says there is too little loss to tell where it starts.
