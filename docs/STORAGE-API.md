# Storage tests: API contract

The routes and types of the storage tests: S3-compatible endpoints the owner saves as **targets**, **checks** of them, and **campaigns**: rounds of fresh connections, reused connections and transfers every few minutes, stored with every attempt. Every other route is in [API.md](API.md).

A target is any S3-compatible endpoint: a scheme, a host and an optional port (`https://s3.us-east-1.amazonaws.com`, `https://<account>.r2.cloudflarestorage.com`, `http://192.168.1.20:9000`), a region (signed as typed; default `us-east-1`), an optional bucket and an addressing style. Nothing is specific to a vendor or a region. fyisp uses IPv4 only.

The Go types live in `internal/web/storage.go`, `internal/web/storage_campaigns.go` and `internal/storage`; keep this file in sync with them.

Routes are relative like every other API. Unknown or repeated parameters return 400. Without a storage source (only `fyisp-webdev -storage none` and tests) every route answers 404 and `api/profile` has `features.storage: false`: hide Storage then. What the page shows is in [STORAGE-TESTS.md](STORAGE-TESTS.md).

## Where targets live

Targets are kept in `storage-targets.json` in the data directory (`fyisp paths` lists it), never in the database. The file is readable by fyisp's user only: created `0600` on Unix (a copy with wider permissions is narrowed at start), and with a protected DACL for the owner and SYSTEM on Windows. Every save writes a new file and renames it over the old one. fyisp refuses a symlink, a file owned by another user (Unix), a file over 1 MiB, JSON it can't parse or a format version it doesn't know: storage is then off, every route answers 503 `storage_file`, and the file is never overwritten. Fix or move it and restart fyisp. A target in the file must be as fyisp saves it (an id, a normalized endpoint, `network` `public` or `private`, a unique id and name).

## Keys

Keys belong to a campaign, never to a target, and are write-only: no response, log line, error, export or database row holds them.

- An **on-demand** campaign's keys are held in memory until it ends, is stopped or fyisp exits. After a restart its bucket targets wait for keys (`waiting_keys`, a `keys_lost` notice) and run connection checks only until `campaign/keys` gives them again.
- A **recurring** schedule's keys are saved in the targets file until its end passes (checked while running and at start) or it is removed.
- Keys are bound to the target's endpoint, region, bucket, addressing and CA: a change to any of these deletes them (`keys_cleared`), and *forget keys* does too.

## Errors

`{"error": "<sentence>", "code": "<word>"}`, plus `"field"` naming the refused field where one is. Use `code` and never match on the sentence. Error sentences never quote a body, so they never contain keys.

| HTTP | `code` | When |
|---|---|---|
| 400 | `bad_request` | a bad parameter, body or field (`field` names it: a target's, a campaign's, or `keys`) |
| 400 | `interval_too_short` | a round of these takes longer than the interval (`min_interval_s`) |
| 400 | `keys_refused` | the keys of `targets` were refused (each is tried once before a campaign starts) |
| 400 | `addr_refused` | the endpoint is or resolves to an address fyisp never connects to (`field: "endpoint"`) |
| 401 | `unauthorized` | no or the wrong admin token |
| 403 | `forbidden` | cross-origin, or a bad CSRF token |
| 404 | `not_found` | no such target, campaign or round |
| 405 | `method_not_allowed` | |
| 409 | `name_taken` | another target has that name (names compare without case) |
| 409 | `too_many` | 32 targets already |
| 409 | `changed` | the target changed during the update: read it again |
| 409 | `in_use` | a live campaign uses the target |
| 409 | `active_campaign` | another on-demand campaign is live, or the schedule's window covers the start |
| 409 | `schedule_exists` | a recurring schedule exists |
| 409 | `bad_state` | pause a campaign that isn't running, or resume one that isn't paused |
| 409 | `not_waiting` | keys for a target that isn't waiting for any |
| 413 | `too_large` | the body is over the route's limit |
| 415 | `unsupported_media_type` | a body that is not `application/json` |
| 429 | `busy` | a check, or a campaign's key tries, is running |
| 503 | `storage_file` | the targets file can't be used (above) |
| 500 | `error` | saving the targets file, or reading the campaigns, failed |

## Nullables

`|null` marks fields that are always present and may be null. Every other field is always present.

## `GET api/storage`

No parameters.

```jsonc
{"at": 1761206400000, "rev": 7, "targets": [Target],
 "live": [Live],                // the live campaigns: at most one on demand and one recurring
 "history": [Campaign],         // the last 10 that ended, newest first
 "notices": [{"code": "schedule_ended"|"keys_lost", "at", "campaign", "targets": [tid]}]}   // the last day's
```

`rev` increases with every change to the targets, their checks or the campaigns, and starts at the clock (unix ms) when fyisp starts, so it doesn't repeat across a restart: poll and redraw when it changes. `live`, `history` and `notices` are left out when fyisp runs without campaigns (tests only).

### Target

```jsonc
{"id": "t_3f9a01c2", "name": "AWS Virginia",
 "endpoint": "https://s3.us-east-1.amazonaws.com", "scheme": "https", "host": "s3.us-east-1.amazonaws.com", "port": 443,
 "region": "us-east-1", "bucket": "fyisp-probe",   // "": connection checks only
 "addressing": "virtual-host",                     // or "path"
 "has_ca": false,                                  // a private CA is trusted instead of the system's
 "network": "public",                              // or "private"
 "keys": {"state": "none", "access_key_last4": null, "session_token": false, "campaign": null},
 "in_use_by": 12|null,                             // the live campaign using it: it can't be deleted
 "last_check": Check|null,
 "created": 1761206400000, "updated": 1761206400000}
```

- `id` is `t_` and 8 hex digits, fixed for the target's life.
- `network` is fixed when the endpoint is saved: `private` when every address its name resolves to is on this network (RFC 1918, carrier-grade NAT 100.64/10 or loopback), `public` when every one is public. A name that resolves to both kinds, or to any other address, is refused (`addr_refused`): link-local (cloud metadata, and Alibaba Cloud's 100.100.100.200), multicast, reserved, "this network" (0/8), the IETF block (192.0.0/24), benchmarking (198.18/15) and documentation ranges. Every later connection checks its address against the saved kind (DNS rebinding) and fails as `addr_refused`. Plain `http://` is allowed only for an address on the LAN itself (RFC 1918 or loopback; not carrier-grade NAT).
- `keys.state` is `none`, `memory` (an on-demand campaign holds them) or `saved` (a recurring schedule saved them), `campaign` the one holding them. `access_key_last4` is the access key's last 4 characters, or null for keys of 4 characters or fewer. `session_token` says whether a session token came with them.
- `last_check` is kept in memory only: a restart, or a change to the target's endpoint, region, bucket, addressing or CA, clears it.

### Check

```jsonc
{"at": 1761206400000, "class": "ok", "group": "ok", "keys": "work",
 "hint": null, "http_status": 200, "s3_code": null,
 "dns_ms": 3.1, "connect_ms": 11.8, "tls_ms": 24.0, "ttfb_ms": 31.2, "during_test": false}
```

- `class` is how the request ended, `group` its group: `ok`; `network` (`dns_fail`, `dns_no_ipv4`, `connect_timeout`, `connect_refused`, `connect_reset`, `connect_unreachable`, `tls_timeout`, `tls_reset`, `tls_error`, `reset_before_response`, `timeout_response`, `conn_closed`, `reset_mid_transfer`, `stall`, `short_body`, `timeout_total`); `provider` (`http_5xx`, `throttled`); `config` (`http_auth`, `clock_skew`, `tls_cert`, `http_redirect`, `http_4xx`, `addr_refused`).
- `keys`: `work` (the signed request was accepted), `refused` (it got `http_auth` or `clock_skew`) or `none` (sent unsigned, or the check failed before an answer).
- `hint` names a likely fix of a config failure, or null: `region` (the server wants another region), `path_style` (a bucket name with dots breaks virtual-host TLS), `endpoint` (a redirect: another endpoint or region), `clock` (this machine's clock is off), `ca` (an unknown certificate authority).
- `http_status` and `s3_code` (the error body's `<Code>`) are null without a response. The timings are null for steps the request never reached.
- `during_test`: a speed test was running when the check started or ended, so its timings may be high.
- A check cut off by its 20 s bound is `timeout_total`.

## Writes

Every write is a POST behind the admin token, the same origin and the CSRF token (`X-FYISP-Token`). A body is `application/json` (else 415) with no unknown fields; targets take at most 96 KiB, checks 16 KiB. Routes without a body refuse one.

| Route | Body | Answer |
|---|---|---|
| `api/storage/target/add` | `{"name", "endpoint", "region"?, "bucket"?, "addressing"?, "ca_pem"?}` | 201 `{"target": Target}` |
| `api/storage/target/update?id=` | any of the add fields | 200 `{"target": Target, "keys_cleared": bool}` |
| `api/storage/target/delete?id=` | none | 200 `{}`; 409 `in_use` |
| `api/storage/target/forget-keys?id=` | none | 200 `{"target": Target}` |
| `api/storage/check?id=` | none, or `{"keys": Keys}` | 200 `{"result": Check}` |

- **add and update.** `name` is 1 to 64 characters with no control characters (Unicode Cc). `endpoint` is `https://` or `http://`, a host name or an IPv4 address, and an optional port: no user, path, query or fragment. `region` is 1 to 32 letters, digits, `-` or `_` (default `us-east-1`; some servers want `auto` or their own name). `bucket` is 3 to 63 letters, digits, `.`, `-` or `_`, or empty. `addressing` is `virtual-host` (`<bucket>.<host>`, the default) or `path` (`<host>/<bucket>`, needed for an IP address). `ca_pem` is PEM certificates (at most 64 KiB) to trust instead of the system's roots, or empty. The endpoint's name is resolved when it is saved (above).
- **update** changes only the fields given. A change to the endpoint, region, bucket, addressing or CA deletes the keys campaigns hold for it (`keys_cleared`) and clears its last check. A target that another request changed while the new endpoint was resolved answers 409 `changed`.
- **delete** removes the target, its keys and its last check; a target a live campaign uses can't be deleted.
- **forget-keys** deletes the keys any campaign holds for the target: that campaign's transfers to it stop until it gets keys again.
- **check** sends one request and waits for it (at most 20 s). With keys (the body's, else the ones a campaign holds) it signs a listing of fyisp's own prefix (`fyisp-test/`) in the bucket, which needs only list permission and reads nothing else; without keys, or with no bucket, it sends an unsigned `HEAD` of the bucket or the endpoint's root. One check runs at a time (429 `busy`). A check stores nothing but its result, in memory, and a check whose request is dropped (the page closed) keeps nothing; the keys sent with it are dropped when it returns.

### Keys

```jsonc
{"access_key": "AKIA…", "secret": "…", "session_token": "…"}   // session_token optional
```

Write-only. `access_key` is 1 to 128 printable characters without spaces, `secret` 1 to 256 printable characters, `session_token` up to 4096 printable characters without spaces, sent only over `https://` endpoints. A check needs only permission to list the bucket under `fyisp-test/`; a campaign's rounds also get, put and delete objects there. Use keys scoped to the test bucket, or short-lived session credentials.

## Campaigns

A campaign runs **rounds** every `interval_s` against 1 to 8 targets. Each round waits its turn in the speed-test queue (behind the owner's tests, beside the schedule's, never stopping a running test) and then runs, in order:

1. **seed** (bucket targets with keys): a listing of `fyisp-test/<install>/seed/`, and the objects the round reads put when missing: a 1 KiB one for the checks and one of `down_bytes` for the download (`seed/<size>`: one per download size ever used). They stay for later rounds and campaigns; the seed upload isn't counted in a round's bytes or the estimate.
2. **cold** ×`cold` (0 to 20): a fresh name lookup, connection and TLS handshake per request, a second apart, the targets interleaved. A request is an unsigned `HEAD`, or with keys a signed `GET` of the 1 KiB seed.
3. **warm** ×`warm` (0 to 10): the same requests on one kept connection, a second apart. A closed connection ends the phase (`conn_closed`): it is never reopened.
4. **down** and **up** (bucket targets with keys): a download of `down_bytes`, then an upload of `up_bytes` to `fyisp-test/<install>/<campaign>/<round>-<target>` and its delete, each on a fresh connection (0 to 1 GiB each).

Nothing is retried; every attempt of a round is stored (a key try before a start, and a sweep, aren't). A target whose keys, clock or settings are refused leaves the round (`skips`). A round has a time bound (twice its estimate, and at least its estimate plus 2 minutes, at most 10 minutes); targets it cut off are skipped `round_cap`. Before each campaign's first round with keys for a target (and again after a sweep that failed), fyisp deletes what an earlier run left under `fyisp-test/<install>/` but the seeds: one listing of at most 1000 objects, then a DELETE each, bounded at 10 minutes. Transfers are marked like speed tests (charts and Home attribute probes during them). A round the owner cancels in the queue is stored as `cancelled`.

A round that can't run is stored as **skipped**: `paused` (also a round already waiting when the campaign was paused), `busy` (the campaign's last round still waits), `stale` (it waited in the queue over 10 minutes; the queue drops waiting entries between tests), `no_keys` (no target can run: none has keys and the round has only transfers), `other_campaign` (another campaign's round waits or runs). Time fyisp wasn't running, or a clock that jumped ahead (a machine that slept), is stored as a **gap**; missed rounds are never made up.

**On demand:** from `start_at` (default now, at most 30 days ahead), `count` rounds (at most 1000) or for `duration_s` (at most 7 days); `run` is one round now, first in the queue. **Recurring:** on `days` (1 Monday … 7 Sunday) from `from` to `to` (`HH:MM`; an end at or before the start crosses midnight, and the window belongs to its start's day) in the IANA zone `tz`, until `until` (optional). Windows keep their wall-clock times across DST changes (a window over one is an hour shorter or longer). One on-demand campaign and one recurring schedule may be live at a time; an on-demand campaign can't start inside the schedule's window.

### Campaign

```jsonc
{"id": 12, "shape": "on_demand"|"recurring", "state": "scheduled"|"running"|"paused"|"done"|"stopped",
 "stop_reason": "owner"|"finished"|"until"|"removed"|null,
 "created": ms, "start_at": ms|null, "ends_at": ms|null, "count": 24|null,
 "interval_s": 300, "cold": 10, "warm": 5, "down_bytes": 67108864, "up_bytes": 16777216,
 "targets": [{"id": "t_3f9a01c2", "name": "AWS Virginia", "mode": "bucket"|"conn"}],   // as it started
 "schedule": {"days": [1, 2, 3, 4, 5], "from": "19:00", "to": "23:00", "tz": "America/New_York", "until": ms|null, "keys_saved": true}|null,
 "rounds_done": 9, "bytes_down": 604241920, "bytes_up": 150994944}
```

`mode` is `bucket` for a target given keys (transfers), `conn` otherwise. `ends_at` is a duration campaign's planned end, or when any campaign ended.

### Live

A Campaign plus `in_window` (recurring: inside a window now; null on demand), `next_at` (the next round due, or null), `running` (a round runs now) with its `phase` (`seed`, `cold`, `warm`, `down`, `up`) and `target` (null for `cold`, which interleaves them), `queue_position` (its round waits in the queue) and `waiting_keys` (bucket targets with no keys held).

## Campaign reads

| Route | Answer |
|---|---|
| `GET api/storage?part=live` | `{"at", "rev", "live": [Live]}` (cheap: poll it while a round runs) |
| `GET api/storage?part=campaigns[&before=<id>][&limit=1…50]` | `{"campaigns": [Campaign], "next_before": id|null}`, newest first |
| `GET api/storage?part=campaign&id=` | `{"campaign": Campaign, "rounds": [Round], "cells": {tid: "oof…"}, "notes": {tid: {"<round index>": Note}}, "throughput": {tid: Series}}` |
| `GET api/storage?part=round&id=` | `{"round": Round, "attempts": {tid: [Attempt]}}`, in the order they ran |
| `GET api/storage?part=attempts&campaign=[&target=][&class=][&phase=][&failed=1][&before=<id>][&limit=1…500, default 50]` | `{"items": [Attempt], "next_before": id|null, "total": n}`, newest first; 404 for an unknown campaign |
| `GET api/storage?part=estimate&targets=t_1,t_2&keyed=t_1&shape=…&interval_s=&cold=&warm=&down_bytes=&up_bytes=[&count=\|&duration_s=][&days=1,2&from=HH:MM&to=HH:MM]` | `{"per_round": {"down", "up"}, "rounds": n|null, "total": {…}|null, "per_window": {…}|null, "per_30d": {…}|null, "round_s_est", "min_interval_s"}` (bytes; `keyed` are those of `targets` that will get keys; seeding isn't counted) |

- **Round** = `{"id", "seq"|null, "due", "start"|null, "end"|null, "queue_wait_ms"|null, "outcome": "ok"|"slow"|"failed"|"config"|"cancelled"|"skipped"|"gap", "skip_reason"|null, "skips": {tid: why}, "gw_loss"|null, "edge_loss"|null, "bytes_down", "bytes_up"}`, oldest first. `seq` is the round's number (on demand: its slot, so missed ones leave numbers out); a gap has none, and its `due`..`end` is the time fyisp wasn't running. `outcome`: `failed` when any attempt failed in the network or provider groups, `config` when only config classes or refusals, `slow` when a transfer stalled 2 s or more.
- **cells** has one letter per round and target, `cells[tid].length == rounds.length`: `o` ok, `s` slow, `f` failed, `a` config (keys, clock or settings refused), `k` no keys (connection checks only, or skipped `no_keys`), `b` didn't run (skipped, or the target was left out), `g` gap, `c` cancelled. A **Note** for each `f`, and each `a` with failed attempts: `{"class": its most frequent failure, "failed", "of": attempts, "status": "403 AccessDenied"|""}`. Every round of the campaign is returned (a schedule kept for 90 days can have thousands).
- **Series** (bucket targets): `{"at": [ms], "down_bps": [n|null], "up_bps": [n|null]}`, one point per round with a transfer.
- **Attempt** = `{"id", "round", "target", "phase", "op": "get"|"head"|"put"|"delete"|"list", "n", "at", "class", "group", "http_status", "s3_code", "request_id", "host_id", "request_id_header", "dns_ms", "connect_ms", "tls_ms", "ttfb_ms", "total_ms", "bytes", "goodput_bps", "stall_ms", "edge_prefix", "tls_version", "tcp_rtt_ms", "retrans", "mss", "skew_s"}`, all nullable but the first eight. `request_id` and `host_id` are the provider's (for failures, from `request_id_header`). `edge_prefix` is the /24 of the provider's address: never a full address, never one of this machine's, and never one in your own internet provider's network as fyisp's built-in table names it, or of no network it knows (it could be your own public address; best effort: a provider that gives you addresses from another of its networks isn't recognised). `request_id` and `host_id` are checked against a pattern that refuses dotted addresses.

Campaigns, rounds and attempts age out with the rest of the data (`--retention`).

## Campaign writes

| Route | Body | Answer |
|---|---|---|
| `api/storage/run` | `{"targets": [tid], "keys": {tid: Keys}}` | 202 `{"campaign": Campaign}`: one round now (10 cold, 5 warm, with keys a 64 MiB download and a 16 MiB upload) |
| `api/storage/campaign/start` | `{"targets", "shape", "interval_s", "cold", "warm", "down_bytes", "up_bytes", "start_at"?, "count"?, "duration_s"?, "days"?, "from"?, "to"?, "tz"?, "until"?, "keys": {tid: Keys}}` | 201 `{"campaign": Campaign}` |
| `api/storage/campaign/stop?id=` | none | 200 `{"campaign"}`: ends it now (a recurring one is removed, `stop_reason: removed`), cancels its round and deletes its keys |
| `api/storage/campaign/pause?id=`, `…/resume?id=` | none | 200 `{"campaign"}`: rounds due while paused are skipped `paused` |
| `api/storage/campaign/keys?id=` | `{"keys": {tid: Keys}}` | 200 `{}`: keys for targets in `waiting_keys` |

- Keys given with `run`, `start` and `keys` are tried once per target first, all at once (a signed listing): any refused answers 400 `keys_refused` with `targets`, and nothing starts; a target that doesn't answer keeps its keys (its rounds say what happens). One set of tries runs at a time (429 `busy`).
- `interval_s` is 60 s to 24 h, and at least the round's estimate (`min_interval_s`, rounded up to a minute). A recurring window is at least one interval long.
