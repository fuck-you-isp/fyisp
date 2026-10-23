# Security policy

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub's
[private vulnerability reporting](https://github.com/fuck-you-isp/fyisp/security/advisories/new)
("Report a vulnerability" on the Security tab). Please do not open a public
issue for security problems.

Include what you found, how to reproduce it, the fyisp version
(`fyisp --version`) and your platform. We aim to acknowledge reports within a
few days and will keep you updated until a fix is released.

## Supported versions

Only the latest release receives security fixes.

## What leaves your machine

fyisp has no telemetry: it talks only to its probe targets (and, by
traceroute, the routers on the way to them), public hosts a signed-in
viewer asks *Investigate* to trace (below), your DNS resolver, if you turn
speed tests on, iperf3 servers, and, with `--tunnel`, Cloudflare (below).
Every probe target, TCP or UDP, sees your public IP address, as any
website you visit does.

The STUN servers fyisp measures calls with (listed in
[docs/CALL-QUALITY.md](docs/CALL-QUALITY.md#stun-servers)) are no different: they see your
public IP address and the time of each request. A STUN answer also contains
your public address as the server sees it (XOR-MAPPED-ADDRESS); fyisp's parser
skips that attribute without decoding it, keeps no raw datagrams, and never
stores, logs or exports the address (a test checks samples, the API, metrics
and logs for it). Socket errors are stored only as a reason code,
never as text that could contain an address. `--no-udp` turns STUN off.

Speed tests are off unless you switch them on (`--speedtest` or the Details
choice). Then fyisp sends iperf3 traffic over IPv4 to public iperf3 servers in
its catalog (Leaseweb, OVH, Clouvider, Hurricane Electric,
Scaleway and others; [docs/SPEED-TESTS.md](docs/SPEED-TESTS.md) lists them) and to servers you name with
`--speedtest-server`, and to no one else. The catalog is built into the
binary and never fetched. Those servers see your public IP address. Each test
runs at most about 11 seconds each way, ramp-up included, with
no byte cap, and each direction is hard-stopped 2 seconds later even against
a server that ignores the end of the test. Once a day per server, fyisp traceroutes the path to
it to name the first network past your ISP (kept locally: shown in Details →
Speed and in `fyisp export --speedtests`). In mode `nearby` it also checks
the round-trip time to every catalog server once a day, to pick the nearest.
"Test now", sweeps, cancelling, the mode choice and skipped servers are
the speed-test controls: write actions behind the admin token, the same
origin and the CSRF token. Every
test waits in one queue and exactly one runs at a time: the owner's tests
first, then scheduled ones.

With `--tunnel` (off by default) fyisp runs Cloudflare's tunnel code
(cloudflared, linked in) to publish the dashboard at a random
`https://<name>.trycloudflare.com` address. It asks `api.trycloudflare.com`
for a tunnel (through `HTTPS_PROXY` when that is set), looks up Cloudflare's edge servers in DNS
(`_v2-origintunneld._tcp.argotunnel.com` and the servers it names, falling
back to DNS over TLS at 1.1.1.1; and `cfd-features.argotunnel.com`, again
every hour) and keeps a connection to them on port 7844 (QUIC over UDP, or
HTTP/2 over TCP). Cloudflare sees your public IP address and, when the
tunnel registers, the machine's local address, the cloudflared version,
the operating system and a random connector ID; if Cloudflare asks for a
trace of a request, cloudflared also sends it the machine's host name. It
terminates HTTPS, so it sees the dashboard's traffic: the admin token
(in the sign-in request, and in scripts' `Authorization` headers) and the
session cookie, which signs a browser in on its own. The `#admin=` part of
a link never leaves the browser. The
tunnel publishes the dashboard only: cloudflared's private-network
routing (connections to other addresses the edge could ask for) is
refused, so it never reaches your LAN, and fyisp pins the tunnel's
configuration, so the edge can't push other ingress rules or routing to
it. The dashboard behind it needs the
admin token like everywhere else.
cloudflared's own logs are capped at its info level and stripped of
headers and IP addresses before they reach fyisp's log (at debug it logs
every request with its headers, which carry the visitor's address);
addresses are also masked in what cloudflared's HTTP/2 server and quic-go
write through Go's standard log, and in the tunnel's own errors. Nothing
else of cloudflared runs: no metrics, management or diagnostics server,
no updater, no error reporting.

On-demand traces (*Investigate*) need the admin token, the same origin and
the CSRF token. A trace runs fyisp's traceroute against a profile target or
any public host the token holder names, as `mtr` would from this machine:
so whoever has the token can make this machine send trace probes to public
hosts of their choice, within the limits below, and see the path from your
connection to them (and, tracing a host they run, your public IP address).
It can't reach your LAN. The safeguards are:

- a profile target name, or a host (a DNS name or an IPv4 address) whose
  every IPv4 address is public: names are resolved with the system
  resolver first, and a host that is or resolves to a private, loopback,
  link-local, carrier-grade NAT (100.64/10), multicast, broadcast,
  reserved, benchmarking or documentation address is refused without
  naming the address. IPv6 is not traced. The tracer checks every later
  resolution too and stops the trace if the name starts resolving to such
  an address (DNS rebinding);
- the ports are fixed: TCP traces go to port 443 only, UDP traces to
  33434; there is no port parameter, so it can't scan ports;
- each trace stops by itself after 250 rounds (about 10 minutes); re-posting
  doesn't extend it;
- at most 4 run at once (429 `busy`), checked before a host is looked up;
- the per-router probe cap is unchanged;
- the route takes exactly one `target` or `host` parameter, an optional
  `mode` (`icmp`, `tcp` or `udp`) and no body (a declared length,
  chunking or anything read is refused), is never cached, and its
  responses carry only the trace's state (target name, mode, rounds,
  times);
- a host's hop numbers are stored under its name (`host:example.com`)
  and age out with the rest of the data, but the name stays in the
  database's index of series; its route stays in memory;
- client addresses are never logged.

Client addresses are never used to limit anyone: behind a tunnel they
can't be trusted.

The dashboard's controls:

| Route | Host allowlist | Admin token | Same origin + CSRF | Other limits |
|---|---|---|---|---|
| GET the page (`/`) and `static/*` | yes | no | - | the same files for everyone |
| GET `/api/*`, `/metrics` | yes | yes (401 without) | - | read-only |
| POST `/api/speed/run`, `/api/speed/config`, `/api/speed/cancel` | yes | yes | yes | one queue, one test at a time |
| POST `/api/investigate` (`target=` or `host=`, optional `mode=`) | yes | yes | yes | the safeguards above: public hosts only, fixed ports, 4 at once, 250 rounds |
| POST/DELETE `/api/admin/session` | yes | POST exchanges it for the cookie | `Sec-Fetch-Site` only (the token header and DELETE need a CORS preflight, never granted) | - |

The admin token is `--admin-token` or `$FYISP_ADMIN_TOKEN` (at least 32
characters), else a new random one at every start; it is sent as
`X-FYISP-Admin`, `Authorization: Bearer` or the HttpOnly, SameSite=Strict
session cookie that the "open this link" URL sets (it holds an HMAC of
the token, not the token, and is `Secure` when the page is HTTPS: through
`--tunnel`, or behind a proxy, an `https://` Origin or `X-Forwarded-Proto:
https`), and compared in constant time. A generated token is never stored, so a restart revokes every
session; a token passed in is never logged.

## Exposing the dashboard

fyisp serves one dashboard, by default on 127.0.0.1:3000. It has no public
or redacted view, and fyisp makes no reports:
whoever has the admin token sees every address and host name. To show
your connection to others, send a screenshot of Home, which shows no
addresses, or review a `fyisp export` first (it lists the targets you
measure; traceroute rows give the hop number, not its address). Every
route but the page needs the admin token, wherever the dashboard is reached
from: 127.0.0.1, your network, a reverse proxy or a tunnel. If you put a
tunnel or reverse proxy in front of the dashboard (cloudflared, Tailscale
Serve or Funnel, nginx), name it with `--allow-host`. `--tunnel` does that
for its quick tunnel: the dashboard accepts the tunnel's random name (over
HTTPS only; after a replacement tunnel, its new name), and the session
cookie set through the tunnel is `Secure`. Its link (without the token) is printed and logged
once the tunnel has connected and again for each replacement tunnel.
A generated token is printed in fyisp's log at every start as the "open
this link" URL, so anyone who can read the logs (`journalctl`,
`docker logs`) can sign in until fyisp restarts. To revoke every browser
session, restart fyisp with a new token. The README's
[Remote access](README.md#remote-access) and
[Admin token](README.md#admin-token) sections have the details.

In scope, for example:

- an address (yours, your router's or your provider's) on Home or in
  *What happened at …?*;
- bypasses of the dashboard's protections (Host allowlist, the admin
  token, CSRF), or of the on-demand trace safeguards above (tracing
  anything but a profile target or a public address, another port, more
  than 4 at once, past 250 rounds);
- ways for a remote party to make fyisp run commands, read or write files, or
  exhaust memory or disk;
- supply-chain issues in the release process (launchers, checksums, images).

Out of scope: findings that require local access to the machine running
fyisp, and the security of a tunnel or proxy you put in front of it.
