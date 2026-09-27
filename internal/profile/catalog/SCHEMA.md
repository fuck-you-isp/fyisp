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
  DynamoDB regional endpoints answer GET /ping ...
targets:
  - name: AWS-us-east-1            # globally unique; [A-Za-z0-9._-], <= 40 chars; keep existing names where they exist
    host: dynamodb.us-east-1.amazonaws.com
    kinds: [https, tcp]            # ONLY kinds verified to work (https/tcp/icmp)
    port: 443                      # optional, default 443
    path: /ping                    # optional https path (small response!), default /; may include a query string
    host_overrides:                # optional: probe some kinds on another host (e.g. ping one, HTTPS another)
      https: other.example.com
    region: us-east-1              # provider's own region/datacenter id
    city: Ashburn                  # best-known physical location (if published)
    country: US                    # ISO 3166-1 alpha-2
    geo: na                        # na | sa | eu | me | af | as | oc  (me = Middle East)
    anycast: false                 # true for anycast/global endpoints (no fixed location)
    tags: []                       # optional, e.g. [voice], [matchmaking]
    verified:                      # from the research agent's own light checks
      date: 2026-09-27
      icmp: true                   # echo reply seen
      tcp: true                    # TCP 443 connect ok
      https: 200                   # status code for GET path (any status counts as reachable)
      bytes: 2                     # response size (keep tiny: < 64 KB)
```

Rules for choosing endpoints:
- Prefer endpoints **meant for latency checks** (published "ping"/speed-test hosts,
  health endpoints, public regional service endpoints) over product/login/API hosts.
- No authentication, no personal data, no endpoints whose terms forbid automated
  checks. fyisp sends 1 HTTPS GET + 1 TCP connect every 15 s and 3 pings every
  15 s per target — keep that acceptable for the operator.
- Regional endpoints must really be **in** that region (not a global anycast or
  CDN front); mark anycast ones `anycast: true`.
- HTTPS responses must be small; set `path` to a tiny resource if `/` is large.
- Record only kinds that actually worked in your checks.
