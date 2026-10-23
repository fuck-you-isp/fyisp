# For operators

What fyisp sends to servers it does not run: [iperf3 servers](#for-iperf3-server-operators), [storage endpoints](#for-storage-providers) and [traced hosts and routers](#for-operators-of-traced-hosts-and-routers). STUN servers are in [STUN-TERMS.md](STUN-TERMS.md).

## For iperf3 server operators

fyisp is a home internet monitor. If a user turns its speed tests on (they are off by default), it runs iperf3 tests against public iperf3 servers, yours possibly among them, to see when the user's connection slows down during the day.

**What you will see:**
- stock iperf3 TCP tests over IPv4: 8 streams, download (reverse) then upload;
- each direction runs for at most about 11 seconds each way, ramp-up included (3 to 10 seconds measured once the rate is steady), at the user's line rate. On a 1 Gbit/s line that is up to about 1.4 GB each way per test; see the table in [IPERF3-TERMS.md](IPERF3-TERMS.md#data-per-test). A hard stop 2 seconds after that ends a direction whatever happens; the client ends the test early with TEST_END;
- the control cookie starts with `fyisp` (visible in `iperf3 -s --debug`);
- one test at a time per install, from the user's home connection; only the user starts tests (on a schedule, Test now, or a sweep of every server), never visitors;
- in mode `nearby`, at most once a day, 3 RTT checks of your server: ICMP echoes or, without an answer, TCP connects to port 80, 443 or your first iperf3 port (which iperf3 may log as an error);
- at most once a day per install, just before a test, a TTL-limited ICMP traceroute towards your server.

**How often:** by default each install tests a region once an hour, rotating between the providers of that region, so a server gets roughly one test every 2 to 3 hours from each install that has speed tests on, or one an hour when it is its region's only server. Some modes test a few nearby servers every 15 minutes. No mode tests a catalog server more often than every 15 minutes.

**When your server is busy** (ACCESS_DENIED, or refusing), fyisp tries at most 2 other ports, then another provider. After 3 busy scheduled tests in a row it leaves your server alone for 6 hours.

**To have your server removed:** open an issue at https://github.com/fuck-you-isp/fyisp/issues, or, to ask privately, use GitHub's private reporting on the Security tab (see [SECURITY.md](../SECURITY.md)). Removal ships in the next release, which also drops the server's past results on each install; until then users can stop testing it with `--speedtest-skip=<server id>` (the ids are in [`internal/profile/catalog/iperf3/`](../internal/profile/catalog/iperf3/)). The list of operators and what fyisp sends is in [`IPERF3-TERMS.md`](IPERF3-TERMS.md).

## For storage providers

fyisp's storage tests reach only S3-compatible endpoints that a user saved themselves, with their own account's keys or none: no endpoint is built in. A check, which the user starts by hand, is one request over IPv4 from their home connection: an unsigned `HEAD` of the bucket (or of the endpoint's root), or, with keys, a SigV4-signed `GET ?list-type=2&prefix=fyisp-test/&max-keys=1000` of the bucket (fyisp's own prefix). It sends `User-Agent: fyisp/<version>`, follows no redirect and doesn't retry. One check runs at a time per install.

A user can also run a **campaign** against their endpoints: rounds every few minutes (an interval the user picks, at least 1 minute), for a while or inside a weekly window. Before it starts, each endpoint's keys are tried with one listing. A round sends, per endpoint: with the user's keys, one listing of fyisp's test objects (and an upload of any that are missing: a 1 KiB one and one of the download's size, kept for later rounds); up to 20 requests on fresh connections, a second apart, each after its own DNS lookup; up to 10 on one kept connection, a second apart; and, with keys, one ranged download and one upload of up to 1 GiB each, followed by a DELETE of the upload. A campaign's first round with keys also lists fyisp's prefix once and deletes what a crash left there (at most 1000 objects, one DELETE each). Requests stay under `fyisp-test/<install>/` in the user's own bucket. Nothing is retried, and a round has a time bound (at most 10 minutes). Rounds from one install run one at a time, in the same queue as its speed tests. After a campaign's first round, and after a connection failure, fyisp traceroutes the address that answered (as *Investigate* does, below: about a minute, at most one per /24 every 10 minutes per install).

## For operators of traced hosts and routers

A user's *Investigate* view (traceroutes, like `mtr`) probes the path to one of the targets in their profile, or to any public host that someone with their dashboard's admin token types in, yours possibly among them: hop by hop, one round every 2.5 seconds, for at most 250 rounds (about 10 minutes), then it stops by itself; it can be started again. At most 4 such traces run at once per install, targets and typed hosts together. A typed host must resolve to public IPv4 addresses only, and is looked up about once a minute while it is traced. Always-on traces (`trace: true` in the user's profile) run one round every 5 seconds with ICMP only.

**What you will see:**
- **ICMP** (the default): echo requests with a low TTL, 16 bytes of payload starting with a random tag and `fyisp-tr`.
- **TCP**, if the user picks *TCP 443*: TCP SYNs to port 443 with a low TTL, from an ordinary socket. When one reaches your host the handshake may complete, but fyisp then resets it at once (`SO_LINGER 0`, a RST); a SYN without an answer is reset within about a second, before it would be retransmitted. None is left half-open or open. On Windows each TCP probe is an ICMP echo request and a TCP SYN with the same TTL, so a router gets 2 packets per counted probe. A firewall or IDS may log them as short connection attempts.
- **UDP**, if the user picks *UDP*: datagrams of 8 bytes (`fyisp-tr`) to port 33434 with a low TTL. A host answers them with ICMP port unreachable.

Each probe goes out once per round per hop; the destination gets at most one probe per round, plus up to 5 more past it when it does not answer. **Routers** on the way get at most 1 probe a second from one install, over all of its traces and probe types together (with a burst of 2), and hops that several traces of the same probe type share are probed once per round for all of them.

