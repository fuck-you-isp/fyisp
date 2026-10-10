# iperf3 speed test servers: operators and use

The record of the public iperf3 servers fyisp's speed tests use, what fyisp sends them and how often, kept before the v0.7 release as the product review asks. The list is built into fyisp (`internal/profile/catalog/iperf3/`) and changes only with a release: there is no remote list and no remote switch. A user can stop testing any server at once with `--speedtest-skip` or the Details toggle.

## What fyisp sends

- **Protocol:** stock iperf3 over TCP, IPv4 only: the control connection, then 8 data streams, download (reverse mode) then upload. The control cookie starts with `fyisp`, so the tests can be told apart in a server's debug log.
- **Per test:** at most 100 MB each way (200 MB a test), 3 to 8 seconds measured each way after a ramp-up of at most 1 second; the client ends the test (TEST_END), which stock servers accept.
- **One at a time:** an install never runs two tests at once, against any server.
- **Busy servers:** a busy or refusing port gets at most 2 more ports, 2 seconds apart (single-port servers: none), then fyisp moves to another provider. A server busy 3 scheduled tests in a row is left alone for 6 hours.
- **From where:** the user's own connection; the server sees the user's public IP address.
- **Not sent:** nothing besides iperf3; no account, no credentials.

## How often, per install

Speed tests are **off by default**. Once a user turns them on:

| Mode | Tests a server gets from one install |
|---|---|
| `regions` (the default) | each region once an hour, rotating between the region's providers: one test every 2 to 3 hours per server in a 2- or 3-provider region, hourly for a single-server region (East Asia, and some opt-in regions) |
| `nearby` | 3 nearby servers and 1 far region, each every 15 minutes |
| `custom` | the servers the user names, at most every 15 minutes (catalog servers) |
| Test now / Test all servers | on demand: one test, or one test of every server; from the share link only if its owner allows it, within a daily data budget and time caps |

The interval can't go below 15 minutes per region or server for catalog servers.

## Operators

| Operator | Servers | Source of the server list | Notes |
|---|---|---|---|
| Leaseweb | 16 | https://speedtest.leaseweb.net/ | Public test servers. |
| OVH | 12 | http://proof.ovh.net/, https://network.ovh.com/ | Link capacities from proof.ovh.net. |
| Clouvider | 9 | https://www.clouvider.com/looking-glass/, YABS's list | |
| InterServer | 3 | https://www.interserver.net/speedtest/ | Dallas, Los Angeles, Secaucus NJ. |
| Hurricane Electric | 1 | https://www.he.net/ | `iperf.he.net`, a single port: never retried on other ports. |
| Scaleway (online.net) | 1 | http://ping.online.net/ | |
| Eranium, Uztelecom, Edgoo | 1 each | YABS's list (https://github.com/masonr/yet-another-bench-script) | |

fyisp uses only the hosts and ports these lists publish. Each server's ports were checked open with one TCP connect (2026-10-10); see each file's `verified` field.

## Owner decision (2026-10-10)

The owner states that the operators' permission is in place, and decided that this project sends them no further notice. If an operator objects: remove the server in the next release, and tell users meanwhile to add it to `--speedtest-skip`. `docs/OPERATORS.md` tells operators how to ask.
