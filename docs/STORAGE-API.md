# Storage tests: API contract

The routes and types of the storage tests: S3-compatible endpoints the owner saves as **targets**, and **checks** of them. Every other route is in [API.md](API.md).

A target is any S3-compatible endpoint: a scheme, a host and an optional port (`https://s3.us-east-1.amazonaws.com`, `https://<account>.r2.cloudflarestorage.com`, `http://192.168.1.20:9000`), a region (signed as typed; default `us-east-1`), an optional bucket and an addressing style. Nothing is specific to a vendor or a region. fyisp uses IPv4 only.

The Go types live in `internal/web/storage.go` and `internal/storage`; keep this file in sync with them.

Routes are relative like every other API. Unknown or repeated parameters return 400. Without a storage source (only `fyisp-webdev -storage none` and tests) every route answers 404 and `api/profile` has `features.storage: false`: hide Storage then. What the page shows is in [STORAGE-TESTS.md](STORAGE-TESTS.md).

## Where targets live

Targets are kept in `storage-targets.json` in the data directory (`fyisp paths` lists it), never in the database. The file is readable by fyisp's user only: created `0600` on Unix (a copy with wider permissions is narrowed at start), and with a protected DACL for the owner and SYSTEM on Windows. Every save writes a new file and renames it over the old one. fyisp refuses a symlink, a file owned by another user (Unix), a file over 1 MiB, JSON it can't parse or a format version it doesn't know: storage is then off, every route answers 503 `storage_file`, and the file is never overwritten. Fix or move it and restart fyisp. A target in the file must be as fyisp saves it (an id, a normalized endpoint, `network` `public` or `private`, a unique id and name).

## Errors

`{"error": "<sentence>", "code": "<word>"}`, plus `"field"` naming the refused field where one is. Use `code` and never match on the sentence. Error sentences never quote a body, so they never contain keys.

| HTTP | `code` | When |
|---|---|---|
| 400 | `bad_request` | a bad parameter, body or field (`field`: `name`, `endpoint`, `region`, `bucket`, `addressing`, `ca_pem`, `keys`) |
| 400 | `addr_refused` | the endpoint is or resolves to an address fyisp never connects to (`field: "endpoint"`) |
| 401 | `unauthorized` | no or the wrong admin token |
| 403 | `forbidden` | cross-origin, or a bad CSRF token |
| 404 | `not_found` | no such target |
| 405 | `method_not_allowed` | |
| 409 | `name_taken` | another target has that name (names compare without case) |
| 409 | `too_many` | 32 targets already |
| 409 | `changed` | the target changed during the update: read it again |
| 413 | `too_large` | the body is over the route's limit |
| 415 | `unsupported_media_type` | a body that is not `application/json` |
| 429 | `busy` | a check is running |
| 503 | `storage_file` | the targets file can't be used (above) |
| 500 | `error` | saving the targets file failed |

## Nullables

`|null` marks fields that are always present and may be null. Every other field is always present.

## `GET api/storage`

No parameters.

```jsonc
{"at": 1761206400000, "rev": 7, "targets": [Target]}
```

`rev` increases with every change to the targets or their checks, and starts at the clock (unix ms) when fyisp starts, so it doesn't repeat across a restart: poll and redraw when it changes.

### Target

```jsonc
{"id": "t_3f9a01c2", "name": "AWS Virginia",
 "endpoint": "https://s3.us-east-1.amazonaws.com", "scheme": "https", "host": "s3.us-east-1.amazonaws.com", "port": 443,
 "region": "us-east-1", "bucket": "fyisp-probe",   // "": connection checks only
 "addressing": "virtual-host",                     // or "path"
 "has_ca": false,                                  // a private CA is trusted instead of the system's
 "network": "public",                              // or "private"
 "last_check": Check|null,
 "created": 1761206400000, "updated": 1761206400000}
```

- `id` is `t_` and 8 hex digits, fixed for the target's life.
- `network` is fixed when the endpoint is saved: `private` when every address its name resolves to is on this network (RFC 1918, carrier-grade NAT 100.64/10 or loopback), `public` when every one is public. A name that resolves to both kinds, or to any other address, is refused (`addr_refused`): link-local (cloud metadata, and Alibaba Cloud's 100.100.100.200), multicast, reserved, "this network" (0/8), the IETF block (192.0.0/24), benchmarking (198.18/15) and documentation ranges. Every later connection checks its address against the saved kind (DNS rebinding) and fails as `addr_refused`. Plain `http://` is allowed only for an address on the LAN itself (RFC 1918 or loopback; not carrier-grade NAT).
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
| `api/storage/target/update?id=` | any of the add fields | 200 `{"target": Target}` |
| `api/storage/target/delete?id=` | none | 200 `{}` |
| `api/storage/check?id=` | none, or `{"keys": Keys}` | 200 `{"result": Check}` |

- **add and update.** `name` is 1 to 64 characters with no control characters (Unicode Cc). `endpoint` is `https://` or `http://`, a host name or an IPv4 address, and an optional port: no user, path, query or fragment. `region` is 1 to 32 letters, digits, `-` or `_` (default `us-east-1`; some servers want `auto` or their own name). `bucket` is 3 to 63 letters, digits, `.`, `-` or `_`, or empty. `addressing` is `virtual-host` (`<bucket>.<host>`, the default) or `path` (`<host>/<bucket>`, needed for an IP address). `ca_pem` is PEM certificates (at most 64 KiB) to trust instead of the system's roots, or empty. The endpoint's name is resolved when it is saved (above).
- **update** changes only the fields given. A change to the endpoint, region, bucket, addressing or CA clears its last check. A target that another request changed while the new endpoint was resolved answers 409 `changed`.
- **delete** removes the target and its last check.
- **check** sends one request and waits for it (at most 20 s). With keys it signs a listing of fyisp's own prefix (`fyisp-test/`) in the bucket, which needs only list permission and reads nothing else; without keys, or with no bucket, it sends an unsigned `HEAD` of the bucket or the endpoint's root. One check runs at a time (429 `busy`). A check stores nothing but its result, in memory, and a check whose request is dropped (the page closed) keeps nothing; the keys sent with it are dropped when it returns.

### Keys

```jsonc
{"access_key": "AKIA…", "secret": "…", "session_token": "…"}   // session_token optional
```

Write-only. `access_key` is 1 to 128 printable characters without spaces, `secret` 1 to 256 printable characters, `session_token` up to 4096 printable characters without spaces, sent only over `https://` endpoints. A check needs only permission to list the bucket under `fyisp-test/`. Use keys scoped to the test bucket, or short-lived session credentials.
