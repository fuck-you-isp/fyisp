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
| Per-client limit (`Cf-Connecting-Ip`) | — | 1 new start per 30 s, 10 per hour |

Only profile target names are accepted (404 otherwise); never a host or an address. At most 4 traces run at once, shared by both listeners.

**What a POST does:**
- **Trace running:** returns its state. It doesn't extend the trace or reset the count, and it isn't counted against the per-client limit.
- **Trace finished:** starts a fresh 250 rounds.

**Response 200:**

```json
{"target":"Microsoft-Teams","state":"running","rounds_done":37,"rounds_max":250,
 "interval_ms":2500,"started_at":1760103900000}
```

`state` is `running` or `finished`; `finished_at` (ms) is present when finished.

**Errors:**
- 400: missing or extra parameters.
- 403: cross-origin, or a missing or bad CSRF token (local).
- 404: unknown target, or tracing unavailable.
- 405: not POST.
- 429 with `Retry-After`: more than 4 running ("too many on-demand traces running"), or the per-client limit.
- 500: the tracer failed. The local listener includes the reason; the share link doesn't.

## Progress

`GET api/trace?target=` carries `"investigation": {…same object…}` while the trace runs and for an hour after it finishes, on both listeners. The UI polls it and never re-posts to keep a trace alive.
