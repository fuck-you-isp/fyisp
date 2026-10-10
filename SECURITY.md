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
choice). Then fyisp sends iperf3 traffic over IPv4 to public iperf3 servers in
its catalog (Leaseweb, OVH, Clouvider, InterServer, Hurricane Electric,
Scaleway and others; the README lists them) and to servers you name with
`--speedtest-server`, and to no one else. The catalog is built into the
binary and never fetched. Those servers see your public IP address. Each test
moves at most 200 MB.

The mode choice, cancelling and clearing the queue are write actions behind
the same protections as notes (same origin, CSRF token, admin token). "Test
now" and "Test all servers" are also open to share-link visitors, so anyone
with the link can make your line run speed tests. Those requests need the
same origin and a token served with the shared page, are rate-limited per
visitor (by `Cf-Connecting-Ip`, IPv6 by /64), and wait in the one queue: one
waiting request per visitor, at most 3 waiting sweeps, the owner's requests
first, one test at a time. Stop sharing to close it. The share link shows
server labels (provider and city) but never hosts, addresses or ports, shows
your own servers only as "Your server", and never shows visitors' addresses
or data use.

The public share link hides the STUN target names and the Teams relay region
(the Azure region fyisp's relay lookup landed in, which hints at where you
are), along with the other redactions listed in the README.


In scope, for example:

- anything reachable through the public share link (`--share`) that is not
  meant to be public: private addresses, settings, local-only endpoints,
  unpublished reports or notes;
- bypasses of the local dashboard's protections (Host allowlist, CSRF,
  `--admin-token`);
- ways for a remote party to make fyisp run commands, read or write files, or
  exhaust memory or disk;
- supply-chain issues in the release process (launchers, checksums, images).

Out of scope: the quick tunnel's availability (it depends on Cloudflare's free
trycloudflare service) and findings that require local access to the machine
running fyisp.
