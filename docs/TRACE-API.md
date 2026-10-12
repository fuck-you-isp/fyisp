# On-demand traces (*Investigate*) API

An on-demand trace follows one profile target's path hop by hop every 2.5 s, for **250 rounds** (about 10.4 minutes), and then stops by itself. Always-on traces (`trace: true` in the profile) are not affected.

## Start

```
POST api/investigate?target=<profile target name>
```

There is no body. The checks are:

| Check | |
|---|---|
| Host allowlist | yes |
| Same origin (`Origin` / `Sec-Fetch-Site`) | yes |
| `X-FYISP-Token` (CSRF, page meta) | yes |
| Admin token (`X-FYISP-Admin` or the session cookie) | where the dashboard has one: `--listen` beyond loopback (unless `--admin-token=off`) or any `--allow-host` |
| Proxy headers | a loopback dashboard without an admin token refuses a request carrying `Forwarded`, `X-Forwarded-For`, `X-Real-Ip`, `Cf-Connecting-Ip` or `Tailscale-User-Login`: its controls work only from the same machine |
| Slots | at most 4 traces at once |

**Request rules:**
- Exactly one `target` parameter and nothing else, with no body: a declared length or chunking is refused before reading, and anything sent anyway is read up to 1 KiB and refused.
- Only profile target names are accepted; never a host or an address.
- Every response has `Cache-Control: no-store`.
- Logs name the target and the error, never the client.

**What a POST does:**
- **Trace running:** returns its state. It doesn't extend the trace or reset the count.
- **Trace finished:** starts a fresh 250 rounds.

**Response 200:**

```json
{"target":"Microsoft-Teams","state":"running","rounds_done":37,"rounds_max":250,
 "interval_ms":2500,"started_at":1760103900000}
```

`state` is `running` or `finished`; `finished_at` (ms) is present when finished.

**Errors:** `{"error": "<words for people>", "code": "<stable>", "retry_after": <seconds, with Retry-After>}`. Match on `code`, never on the words.

| Status | `code` | When |
|---|---|---|
| 400 | `bad_request` | a parameter other than one `target`, or a body |
| 403 | `forbidden` | cross-origin; missing or bad `X-FYISP-Token`; admin token required; proxy headers on a loopback dashboard without a token |
| 404 | `unknown_target` | not a profile target name |
| 404 | `unavailable` | tracing is not available |
| 405 | `method_not_allowed` | not POST |
| 429 | `busy` | no free slot (4 in all; `retry_after`) |
| 500 | `failed` | the tracer failed (with the reason) |

## Progress

`GET api/trace?target=` carries `"investigation": {…same object…}` while the trace runs and for an hour after it finishes. The UI polls it and never re-posts to keep a trace alive.

## Removed in v0.8.0

With the share link: `POST /s/<secret>/api/investigate`, the per-client start limit, the 5-minute restart cooldown (codes `rate_limited` and `cooldown`), the owner's reserved slot, and the redacted hop view (the `masked` field; hops always carry their address, private ones with `"private": true`).
