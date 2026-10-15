# On-demand traces (*Investigate*) API

An on-demand trace follows one profile target's path hop by hop every 2.5 s, for **250 rounds** (about 10.4 minutes), and then stops by itself. Always-on traces (`trace: true` in the profile) are not affected.

## Start

```
POST api/investigate?target=<profile target name>
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
- Exactly one `target` parameter and nothing else, with no body: a declared length or chunking is refused before reading, and anything sent anyway is read up to 1 KiB and refused.
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
{"target":"Microsoft-Teams","state":"running","rounds_done":37,"rounds_max":250,
 "interval_ms":2500,"started_at":1760103900000}
```

`state` is `running` or `finished`; `finished_at` (ms) is present when finished.

**Errors:** `{"error": "<words for people>", "code": "<stable>", "retry_after": <seconds, with Retry-After>}`. Match on `code`, never on the words.

| Status | `code` | When |
|---|---|---|
| 400 | `bad_request` | a parameter other than one `target`, an empty `target`, or a body |
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

`GET api/trace?target=` carries `"investigation": {…same object…}` while the trace runs and for an hour after it finishes. The UI polls it and never re-posts to keep a trace alive.
