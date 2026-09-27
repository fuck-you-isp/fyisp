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

## Scope

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
