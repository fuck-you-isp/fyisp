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
