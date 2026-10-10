# On-demand traces (*Investigate*) API

An on-demand trace follows one profile target's path hop by hop every 2.5 s, for **250 rounds** (about 10.4 minutes), and then stops by itself. Always-on traces (`trace: true` in the profile) are not affected.

## Start

```
POST api/investigate?target=<profile target name>          (local listener)
POST /s/<secret>/api/investigate?target=<…>                 (share link)
```

There is no body. The checks are:

| | Local | Share link |
|---|---|---|
| Host allowlist | yes | (the secret path) |
| Same origin (`Origin` / `Sec-Fetch-Site`) | yes | yes |
| `X-FYISP-CSRF` (page meta) | yes | — (the public page has none) |
| Admin token | **no** | — |
| Per-client limit (`Cf-Connecting-Ip`; IPv6 per /64) | — | 1 new start per 30 s, 10 per hour (only starts that happen count; a full limiter table refuses new clients) |
| Slots | any of the 4 | at most 3 of the 4: the owner always has one |
| Restarting a finished target | at once | after a 5-minute cooldown |

**Request rules:**
- Exactly one `target` parameter and nothing else, with no body: a declared length or chunking is refused before reading, and anything sent anyway is read up to 1 KiB and refused.
- Only profile target names are accepted; never a host or an address.
- At most 4 traces run at once, shared by both listeners.
- Every response has `Cache-Control: no-store`.
- Logs name the target and the error, never the client.

**Same origin behind the tunnel:** cloudflared passes the visitor's Host (the trycloudflare hostname) to the loopback origin unchanged, along with Origin and Sec-Fetch-Site, and Cloudflare's edge sets `Cf-Connecting-Ip`. This was checked on a real quick tunnel by `TestLiveSameOrigin` in `internal/tunnel`. The public origin binds 127.0.0.1 only, which is what makes `Cf-Connecting-Ip` trustworthy as the per-client key.

**What a POST does:**
- **Trace running:** returns its state. It doesn't extend the trace or reset the count, and it isn't counted against the per-client limit.
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
| 403 | `forbidden` | cross-origin; local: missing or bad `X-FYISP-Token` |
| 404 | `unknown_target` | not a profile target name |
| 404 | `unavailable` | tracing is not available |
| 405 | `method_not_allowed` | not POST |
| 429 | `busy` | no free slot (4 in all, 3 for the share link) |
| 429 | `rate_limited` | share link: this client's start limit (`retry_after`) |
| 429 | `cooldown` | share link: this target finished less than 5 minutes ago (`retry_after`) |
| 500 | `failed` | the tracer failed (the local listener adds the reason) |

## Progress

`GET api/trace?target=` carries `"investigation": {…same object…}` while the trace runs and for an hour after it finishes, on both listeners. The UI polls it and never re-posts to keep a trace alive.
