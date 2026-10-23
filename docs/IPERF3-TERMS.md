# iperf3 speed test servers: operators and use

The record of the public iperf3 servers fyisp's speed tests use, what fyisp sends them and how often. The list is built into fyisp (`internal/profile/catalog/iperf3/`) and changes only with a release: there is no remote list and no remote switch. A user can stop testing any server at once with `--speedtest-skip` or the Details toggle.

## What fyisp sends

- **Protocol:** stock iperf3 over TCP, IPv4 only: the control connection, then 8 data streams, download (reverse mode) then upload. The control cookie starts with `fyisp`, so the tests can be told apart in a server's debug log.
- **Per test:** each direction runs until its rate levels off and holds steady, from 3 up to 10 seconds measured, at most about 11 seconds each way, ramp-up included. The client ends the test (TEST_END), which stock servers accept. This applies to every test. A hard stop (an RST) 2 seconds after the time is up ends a direction whatever the server does.
- **One at a time:** an install never runs two tests at once, against any server.
- **Busy servers:** a busy or refusing port gets at most 2 more ports, 2 seconds apart (single-port servers: none), then fyisp moves to another provider. A server busy 3 scheduled tests in a row is left alone for 6 hours.
- **From where:** the user's own connection; the server sees the user's public IP address.
- **Besides the tests:** in mode `nearby` (and for Test now of the nearest servers), at most once a day, an RTT check of every catalog server: 3 checks each, 8 servers at a time, each an ICMP echo or, without an answer, a TCP connect to port 80, 443 or, last, the first iperf3 port. And at most once a day per tested server, just before a test, a TTL-limited ICMP traceroute towards it. No account, no credentials.

## How often, per install

Speed tests are **off by default**. Once a user turns them on:

| Mode | Tests a server gets from one install |
|---|---|
| `regions` (the default) | each region once an hour, rotating between the region's providers: one test every 2 to 3 hours per server in a 2- or 3-provider region, hourly for a single-server region (East Asia, US Northwest, US Southeast, and some opt-in regions). No server is in two default regions |
| `nearby` | 3 nearby servers and 1 far region, each every 15 minutes |
| `custom` | the servers the user names, at most every 15 minutes (catalog servers) |
| Test now / Test every server | on demand, by whoever has the install's admin token: one test, or one test of every server |

The interval can't go below 15 minutes per region or server for catalog servers.

## Data per test

A test moves what the user's line can carry for up to about 11 seconds each way. Near servers usually level off and stop after 3 to 4 seconds measured, so about 4 to 5 seconds each way. The worst case is the full 11 seconds.

| User's line (each way) | Per direction: typical (≈ 4.5 s) | Per direction: worst case (11 s) | Per test, symmetric line: worst case |
|---|---|---|---|
| 100 Mbit/s | 56 MB | 140 MB | 280 MB |
| 300 Mbit/s | 170 MB | 410 MB | 830 MB |
| 500 Mbit/s | 280 MB | 690 MB | 1.4 GB |
| 1 Gbit/s | 560 MB | 1.4 GB | 2.8 GB |
| 2 Gbit/s | 1.1 GB | 2.8 GB | 5.5 GB |
| 2.5 Gbit/s | 1.4 GB | 3.4 GB | 6.9 GB |
| 5 Gbit/s | 2.8 GB | 6.9 GB | 14 GB |
| 10 Gbit/s | 5.6 GB | 14 GB | 28 GB |

Notes:
- **Asymmetric lines** (most cable and DSL) move far less upstream. A 1000/50 line moves at most about 1.4 GB down plus 70 MB up.
- **Distant servers** (150–250 ms away) usually top out near 1–1.7 Gbit/s per test, whatever the user's line. That's the default TCP buffer sizes on both ends. So from a multi-gigabit home they see less than the table.
- **A server's link caps a test too.** A server on a 1 Gbit/s link moves at most about 1.4 GB per direction per test.

**Per install, per server**, in the default `regions` mode, a server gets one test every 2 to 3 hours (in a single-server region, hourly). On a 1 Gbit/s symmetric line that is up to:
- about 22–34 GB a day (8–12 tests);
- about 66 GB a day for a single-server region (24 tests).

Typical early-stopped tests are about 40% of these figures.

## Operators

| Operator | Servers | Source of the server list | Notes |
|---|---|---|---|
| Leaseweb | 18 | https://speedtest.leaseweb.net/ (no longer resolves), https://iperf3serverlist.net/ | Public test servers. Seattle and Phoenix are listed only on iperf3serverlist.net. |
| OVH | 12 | http://proof.ovh.net/, https://network.ovh.com/ | Link capacities from proof.ovh.net. |
| Clouvider | 11 | https://www.clouvider.com/looking-glass/, YABS's list, https://iperf3serverlist.net/ | Chicago and Phoenix are listed only on iperf3serverlist.net. |
| Hurricane Electric | 1 | https://www.he.net/ | `iperf.he.net`, a single port: never retried on other ports. In the default US Bay Area region, so a busy port falls back to Leaseweb San Francisco at once. |
| NOCIX | 1 | https://iperf3serverlist.net/, http://speedtest.nocix.net/ | Kansas City. |
| Scaleway (online.net) | 1 | http://ping.online.net/ | |
| Eranium, Uztelecom, Edgoo | 1 each | YABS's list (https://github.com/masonr/yet-another-bench-script) | |

fyisp uses only the hosts and ports these lists publish. Each server's ports were checked open with one TCP connect (2026-10-10; the Seattle, Phoenix, Chicago Clouvider and NOCIX servers 2026-10-22); see each file's `verified` field.

## Owner decision (2026-10-10, extended 2026-10-22)

The owner states that the operators' permission is in place, including for the servers added on 2026-10-22 (NOCIX Kansas City, Leaseweb Seattle and Phoenix, Clouvider Chicago and Phoenix), and decided that this project sends them no further notice. If an operator objects: remove the server in the next release, and tell users meanwhile to add it to `--speedtest-skip`. `docs/OPERATORS.md` tells operators how to ask.
