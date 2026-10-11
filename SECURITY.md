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

fyisp has no telemetry and, since v0.8.0, no share link: it talks to its
probe targets and your DNS resolver. Every probe target, TCP or UDP, sees
your public IP address, as any website you visit does.

The STUN servers fyisp measures calls with (listed in the README under
"STUN servers") are no different: they see your
public IP address and the time of each request. A STUN answer also contains
your public address as the server sees it (XOR-MAPPED-ADDRESS); fyisp's parser
skips that attribute without decoding it, keeps no raw datagrams, and never
stores, logs or exports the address (a test checks samples, the API, metrics,
reports and logs for it). Socket errors are stored only as a reason code,
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
"Test now", sweeps, cancelling and the mode choice are write actions behind
the same protections as notes (same origin, CSRF token, admin token). Every
test waits in one queue and exactly one runs at a time: the owner's tests
first, then scheduled ones.

On-demand traces (*Investigate*) need the same origin and the CSRF token
and, where the dashboard has an admin token (a network listener, or any
`--allow-host`), the admin token. On a loopback dashboard without one,
anyone who can open it may start them. The safeguards are:

- only the profile's target names are accepted, never a host or address;
- each trace stops by itself after 250 rounds (about 10 minutes); re-posting
  doesn't extend it;
- at most 4 run at once;
- the per-router probe cap is unchanged;
- the route takes exactly one `target` parameter and no body (a declared
  length, chunking or anything read is refused), is never cached, and its
  responses carry only the trace's state (target name, rounds, times);
- client addresses are never logged.

## Exposing the dashboard

fyisp serves one dashboard, by default on 127.0.0.1:3000. It has no public
or redacted view: whoever can open it sees every address, host name, note
and report. To show your connection to others, send a redacted report
(`fyisp report --redact`). If you put a tunnel or reverse proxy in front of
the dashboard (cloudflared, Tailscale Serve or Funnel, nginx), name it with
`--allow-host`: any `--allow-host` turns the admin token on, even on a
loopback listener. A loopback dashboard without an admin token refuses
write routes and traces from requests that carry proxy headers
(`Forwarded`, `X-Forwarded-For`, `X-Real-Ip`, `Cf-Connecting-Ip`,
`Tailscale-User-Login`), so a proxy that rewrites `Host` to 127.0.0.1 can't
hand its visitors the owner's controls. The README's "Sharing" section has
the details.

In scope, for example:

- a redacted report (`--redact`) that shows what it should leave out:
  private addresses, the ISP edge unmasked, notes not marked for redacted
  reports;
- bypasses of the dashboard's protections (Host allowlist, CSRF,
  `--admin-token`, the proxy-header refusal above), or of the on-demand trace safeguards above (tracing
  anything but a profile target, more than 4 at once, past 250 rounds);
- ways for a remote party to make fyisp run commands, read or write files, or
  exhaust memory or disk;
- supply-chain issues in the release process (launchers, checksums, images).

Out of scope: findings that require local access to the machine running
fyisp, and the security of a tunnel or proxy you put in front of it.
