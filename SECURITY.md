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

fyisp has no telemetry and no share link: it talks to its probe
targets and your DNS resolver. Every probe target, TCP or UDP, sees
your public IP address, as any website you visit does.

The STUN servers fyisp measures calls with (listed in the README under
"STUN servers") are no different: they see your
public IP address and the time of each request. A STUN answer also contains
your public address as the server sees it (XOR-MAPPED-ADDRESS); fyisp's parser
skips that attribute without decoding it, keeps no raw datagrams, and never
stores, logs or exports the address (a test checks samples, the API, metrics
and logs for it). Socket errors are stored only as a reason code,
never as text that could contain an address. `--no-udp` turns STUN off.

Speed tests are off unless you switch them on (`--speedtest` or the Details
choice). Then fyisp sends iperf3 traffic over IPv4 to public iperf3 servers in
its catalog (Leaseweb, OVH, Clouvider, InterServer, Hurricane Electric,
Scaleway and others; the README lists them) and to servers you name with
`--speedtest-server`, and to no one else. The catalog is built into the
binary and never fetched. Those servers see your public IP address. Each test
runs at most about 11 seconds each way, ramp-up included, with
no byte cap, and each direction is hard-stopped 2 seconds later even against
a server that ignores the end of the test. Once a day per server, fyisp traceroutes the path to
it to name the first network past your ISP (shown in the dashboard only).
"Test now", sweeps, cancelling, the mode choice and skipped servers are
the speed-test controls: write actions behind the same origin, the CSRF
token and, where the dashboard has one, the admin token. Every
test waits in one queue and exactly one runs at a time: the owner's tests
first, then scheduled ones.

On-demand traces (*Investigate*) are open to anyone who can open the
dashboard, behind the same origin and the CSRF token: no admin token, on
any dashboard, through a proxy or not. A trace re-runs fyisp's own
traceroute against a target the viewer can already see, so it reveals
nothing new. The safeguards are:

- only the profile's target names are accepted, never a host or address;
- each trace stops by itself after 250 rounds (about 10 minutes); re-posting
  doesn't extend it;
- at most 4 run at once;
- at most 12 new traces start in a rolling hour, across all targets and
  viewers (429 `rate_limited`); a request carrying the admin token skips
  this budget, not the 4-at-once cap. So viewers can hold all 4 slots for
  about 31 minutes an hour (12 traces of about 10 minutes, 4 at a time),
  and meanwhile even the token holder gets `busy`;
- the per-router probe cap is unchanged;
- the route takes exactly one `target` parameter and no body (a declared
  length, chunking or anything read is refused), is never cached, and its
  responses carry only the trace's state (target name, rounds, times);
- client addresses are never logged.

Behind a tunnel, client addresses can't be trusted, so these limits (the
hourly budget, the 4 slots, 250 rounds and the per-router cap) are the
only bounds on a visitor or a script that starts traces.

The dashboard's controls:

| Route | Host allowlist | Same origin + CSRF | Admin token | Proxy-header refusal | Other limits |
|---|---|---|---|---|---|
| GET UI, `/api/*`, `/metrics` | yes | - | no | no | read-only |
| POST `/api/speed/run`, `/api/speed/config`, `/api/speed/cancel` | yes | yes | yes, where the dashboard has one | yes, on a dashboard without a token | off for everyone when reachable from elsewhere with `--admin-token=off`; one queue, one test at a time |
| POST `/api/investigate` | yes | yes | not needed (with it, a request skips the hourly budget) | no | the safeguards above |
| POST/DELETE `/api/admin/session` | yes | yes | POST needs it | - | 404 when no token is set |

## Exposing the dashboard

fyisp serves one dashboard, by default on 127.0.0.1:3000. It has no public
or redacted view, and fyisp makes no reports:
whoever can open the dashboard sees every address and host name. To show
your connection to others, send a screenshot of Home, which shows no
addresses, or review a `fyisp export` first (traceroute rows contain hop
addresses). If you put a tunnel or reverse proxy in front of
the dashboard (cloudflared, Tailscale Serve or Funnel, nginx), name it with
`--allow-host`: any `--allow-host` turns the admin token on, even on a
loopback listener, and the token then guards the speed-test controls.
Anyone who can open the dashboard can start traces, within the limits
above. A loopback dashboard without an admin token refuses speed-test
controls from requests that carry proxy headers (`Forwarded`,
`X-Forwarded-For`, `X-Real-Ip`, `Cf-Connecting-Ip`,
`Tailscale-User-Login`), so a proxy that rewrites `Host` to 127.0.0.1 can't
hand its visitors the owner's speed tests. The README's "Remote access" section
has the details.

In scope, for example:

- an address (yours, your router's or your provider's) on Home or in
  *What happened at …?*;
- bypasses of the dashboard's protections (Host allowlist, CSRF, the
  admin token on speed-test controls, the proxy-header refusal for them),
  or of the on-demand trace safeguards above (tracing anything but a
  profile target, more than 4 at once, past 250 rounds, more than 12 new
  traces an hour without the admin token);
- ways for a remote party to make fyisp run commands, read or write files, or
  exhaust memory or disk;
- supply-chain issues in the release process (launchers, checksums, images).

Out of scope: findings that require local access to the machine running
fyisp, and the security of a tunnel or proxy you put in front of it.
