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

fyisp has no telemetry. It talks to its probe targets, your DNS resolver and,
only while sharing, Cloudflare's quick tunnel. Every probe target, TCP or UDP,
sees your public IP address, as any website you visit does.

The STUN servers fyisp measures calls with (listed in the README under
"STUN servers") are no different: they see your
public IP address and the time of each request. A STUN answer also contains
your public address as the server sees it (XOR-MAPPED-ADDRESS); fyisp's parser
skips that attribute without decoding it, keeps no raw datagrams, and never
stores, logs or exports the address (a test checks samples, the API, metrics,
reports and logs for it). Socket errors are stored only as a reason code,
never as text that could contain an address. `--no-udp` turns STUN off.

Speed tests are off unless you switch them on (`--speedtest` or the Details
choice). Then fyisp sends iperf3 traffic to public iperf3 servers in its
catalog (Leaseweb, OVH, Clouvider, Scaleway and others; the README lists them)
and to servers you name with `--speedtest-server`, and to no one else. Those
servers see your public IP address. "Test now", sweeps, cancelling and the
mode choice are write actions behind the same protections as notes (same
origin, CSRF token, admin token). People with the share link may queue a
test or a sweep of every server (`POST /s/<secret>/api/speed/run`), but
never change the mode or cancel anything. Every test waits in one queue and
exactly one runs at a time, with the owner's tests first. The share link's
entries are capped: one waiting entry per visitor, 3 waiting sweeps and 12
waiting entries in all. Each visitor (`Cf-Connecting-Ip`, IPv6 per /64)
may start 1 per 30 s and 10 per hour. That route is same-origin only,
takes only `kind` and `id` and no body, and is never cached. The share link
shows server labels (provider and city) but never hosts, addresses or ports,
shows your own servers only as "Your server", and shows the queue's other
entries only as "someone".

The public share link hides the STUN target names and the Teams relay region
(the Azure region fyisp's relay lookup landed in, which hints at where you
are), along with the other redactions listed in the README.

On-demand traces (*Investigate*) can be started by anyone who can open the
dashboard, without the admin token, and by anyone with the share link: `POST
/s/<secret>/api/investigate` is the public listener's only write route. The
safeguards are:

- only the profile's target names are accepted, never a host or address;
- each trace stops by itself after 250 rounds (about 10 minutes); re-posting
  doesn't extend it;
- at most 4 run at once, shared by the local and public listeners, of which
  public starts may hold 3 (the owner always has a slot);
- a target finished less than 5 minutes ago can't be restarted from the
  public link;
- the per-router probe cap is unchanged;
- public starts are same-origin only (checked behind a real quick tunnel) and
  limited per client by `Cf-Connecting-Ip` (IPv4 per address, IPv6 per /64;
  1 per 30 s, 10 per hour), on top of the public rate limits; that key is
  trustworthy because the public origin listens on 127.0.0.1 only; when the
  limiter's table is full it prunes, then refuses new clients rather than
  forgetting everyone;
- the route takes exactly one `target` parameter and no body (a declared
  length, chunking or anything read is refused), is never
  cached, and its responses carry only the trace's state (target name,
  rounds, times); hop addresses stay redacted as on every public trace view;
- client addresses are never logged.

The local route keeps the Host allowlist, the same-origin check and the CSRF
token.


In scope, for example:

- anything reachable through the public share link (`--share`) that is not
  meant to be public: private addresses, settings, local-only endpoints,
  unpublished reports or notes;
- bypasses of the local dashboard's protections (Host allowlist, CSRF,
  `--admin-token`), or of the on-demand trace safeguards above (tracing
  anything but a profile target, more than 4 at once, past 250 rounds);
- ways for a remote party to make fyisp run commands, read or write files, or
  exhaust memory or disk;
- supply-chain issues in the release process (launchers, checksums, images).

Out of scope: the quick tunnel's availability (it depends on Cloudflare's free
trycloudflare service) and findings that require local access to the machine
running fyisp.
