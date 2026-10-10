# Target catalog format

Each file in this directory describes one provider (a cloud, CDN, service or
game platform): every endpoint fyisp may probe, with verified metadata.
Profiles (`--profile aws,gaming,europe`) are *selections* over the catalog,
defined in `../profiles.yml`; targets are never duplicated between files.

```yaml
provider: aws                      # file-unique id, lowercase [a-z0-9-]
display: Amazon Web Services       # human name (panel titles)
kind: hyperscaler                  # hyperscaler | cloud | cdn | dns | service | game | gaming-platform
category: []                       # optional finer tags, e.g. [storage], [voice], [fps]
sources:                           # where the endpoint list comes from (URLs), for re-verification
  - https://docs.aws.amazon.com/general/latest/gr/ddb.html
notes: >-                          # why these endpoints; rate/ToS notes; caveats
  DynamoDB regional endpoints accept TCP on 443 in every region ...
targets:
  - name: AWS-us-east-1            # globally unique; [A-Za-z0-9._-], <= 40 chars; keep existing names where they exist
    host: dynamodb.us-east-1.amazonaws.com
    kinds: [tcp]                   # [tcp] once a TCP connect worked; [udp] for a STUN server (see below); [] while unreachable (kept, never probed)
    port: 443                      # optional TCP port, default 443; required for udp
    interval: 30s                  # udp targets only: default 5s; whole seconds dividing 1h
    host_overrides:                # optional: connect to another host than `host`
      tcp: other.example.com
    region: us-east-1              # provider's own region/datacenter id
    city: Ashburn                  # best-known physical location (if published)
    country: US                    # ISO 3166-1 alpha-2
    geo: na                        # na | sa | eu | me | af | as | oc  (me = Middle East)
    anycast: false                 # true for anycast/global endpoints (no fixed location)
    tags: []                       # optional, e.g. [voice], [matchmaking]; home:calls|work|web (see below)
    verified:                      # from your own light checks (free-form, never used for probing)
      date: 2026-09-27
      tcp: true                    # TCP connect to `port` ok
```

Catalog targets are probed with a **TCP connect only** (handshake, then an
immediate close; no data is sent), every 15 s. The loader rejects any other
kind. Fields from when the catalog also used HTTPS and ICMP are still
accepted and validated, but not used: `path` (an HTTPS path), `https` and
`icmp` keys in `host_overrides`, and `icmp`/`https`/`bytes` in `verified`.
Existing entries keep them as a record of those checks; new entries don't
need them. (Local `--config` profile files can still ask for `https` or
`icmp` on their own targets, see package profile.)

A `home:<category>` tag puts a target in a tile of the Home view: `home:calls`
(the call path and call and chat apps), `home:work` (work apps) or `home:web`
(websites). At most one per target, and no other values (the loader rejects
them). Targets of the default profile must have one; other targets without
one are placed by their other tags and provider (`app:*` and `udp-path` →
calls, services-productivity → work, CDNs and streaming → web), else under
"Other services".

Rules for choosing endpoints:
- Prefer endpoints **meant for latency checks** (published "ping"/speed-test hosts,
  health endpoints, public regional service endpoints) over product/login/API hosts.
- The endpoint must accept TCP connections on `port` (default 443).
- No authentication, no personal data, no endpoints whose terms forbid automated
  checks. fyisp opens 1 TCP connection every 15 s per target — keep that
  acceptable for the operator.
- Regional endpoints must really be **in** that region (not a global anycast or
  CDN front); mark anycast ones `anycast: true`.
- Set `kinds: [tcp]` only when a TCP connect actually worked in your checks.

## UDP (STUN) targets

A target with `kinds: [udp]` measures the UDP path call audio and video use.
fyisp sends **STUN Binding Requests only** (RFC 5389: 20 bytes, no
attributes, no credentials): every `interval` (default 5 s) a burst of 5
requests 20 ms apart on one long-lived socket, and stores the median
round-trip time, the jitter and the loss. It never sends TURN requests,
never allocates a relay, and never decodes the public address the server
reports back.

```yaml
  - name: Cloudflare-STUN
    host: stun.cloudflare.com
    kinds: [udp]                   # alone: a UDP target has no other kind
    port: 3478                     # required
    interval: 5s                   # optional, default 5s; e.g. 30s for regional targets
    region: global
    anycast: true
    tags: [udp-path]               # udp-path: counts for "your call path"; app:<name> maps it to an app
    verified: {date: 2026-10-10, stun: 2/2, rtt_ms: 1.4}
```

Rules for STUN targets:
- **Documented public STUN servers only:** a vendor that publishes the host
  for clients to use without credentials (Google's `stun.l.google.com`,
  Cloudflare, Twilio) or that its own clients and network test tools use
  unauthenticated (the Teams Transport Relay). No community STUN lists.
- **No TURN, no relays that need credentials,** and no media ports that only
  work inside a call.
- `kinds: [udp]` only after a Binding Request got a Binding Success in your
  own checks; keep checks to a few requests per endpoint.
- `tags`: `udp-path` for every STUN target; `app:teams`, `app:meet`, ... when
  it stands for one app's media path. TCP targets of call apps carry the same
  `app:*` tags, which the `calls` profile selects.
- Keep the load small: 5 tiny packets per interval. Regional targets use
  `interval: 30s`.

