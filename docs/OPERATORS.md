# For operators

What fyisp sends to servers it does not run: [iperf3 servers](#for-iperf3-server-operators) and [traced hosts and routers](#for-operators-of-traced-hosts-and-routers). STUN servers are in [STUN-TERMS.md](STUN-TERMS.md).

## For iperf3 server operators

fyisp is a home internet monitor. If a user turns its speed tests on (they are off by default), it runs iperf3 tests against public iperf3 servers, yours possibly among them, to see when the user's connection slows down during the day.

**What you will see:**
- stock iperf3 TCP tests over IPv4: 8 streams, download (reverse) then upload;
- each direction runs for at most about 11 seconds each way, ramp-up included (3 to 10 seconds measured once the rate is steady), at the user's line rate. On a 1 Gbit/s line that is up to about 1.4 GB each way per test; see the table in [IPERF3-TERMS.md](IPERF3-TERMS.md#data-per-test). A hard stop 2 seconds after that ends a direction whatever happens; the client ends the test early with TEST_END;
- the control cookie starts with `fyisp` (visible in `iperf3 -s --debug`);
- one test at a time per install, from the user's home connection; only the user starts tests (on a schedule, Test now, or a sweep of every server), never visitors;
- in mode `nearby`, at most once a day, 3 RTT checks of your server: ICMP echoes or, without an answer, TCP connects to port 80, 443 or your first iperf3 port (which iperf3 may log as an error);
- at most once a day per install, just before a test, a TTL-limited ICMP traceroute towards your server.

**How often:** by default each install tests a region once an hour, rotating between the providers of that region, so a server gets roughly one test every 2 to 3 hours from each install that has speed tests on. Some modes test a few nearby servers every 15 minutes. No mode tests a catalog server more often than every 15 minutes.

**When your server is busy** (ACCESS_DENIED, or refusing), fyisp tries at most 2 other ports, then another provider. After 3 busy scheduled tests in a row it leaves your server alone for 6 hours.

**To have your server removed:** open an issue at https://github.com/fuck-you-isp/fyisp/issues, or, to ask privately, use GitHub's private reporting on the Security tab (see [SECURITY.md](../SECURITY.md)). Removal ships in the next release; until then users can stop testing it with `--speedtest-skip=<server id>` (the ids are in [`internal/profile/catalog/iperf3/`](../internal/profile/catalog/iperf3/)). The list of operators and what fyisp sends is in [`IPERF3-TERMS.md`](IPERF3-TERMS.md).

## For operators of traced hosts and routers

A user's *Investigate* view (traceroutes, like `mtr`) probes the path to one of the targets in their profile: hop by hop, one round every 2.5 seconds, for at most 250 rounds (about 10 minutes), then it stops by itself. At most 4 such traces run at once per install, and at most 12 start per hour. Always-on traces (`trace: true` in the user's profile) run one round every 5 seconds with ICMP only.

**What you will see:**
- **ICMP** (the default): echo requests with a low TTL, 16 bytes of payload starting with a random tag and `fyisp-tr`.
- **TCP**, if the user picks *TCP 443*: TCP SYNs to port 443 with a low TTL, from an ordinary socket. The ones that reach your host open no connection: fyisp closes each socket with a reset (`SO_LINGER 0`) within about a second, before the SYN would be retransmitted, so no half-open or established connection is left behind. A firewall or IDS may log them as short connection attempts.
- **UDP**, if the user picks *UDP*: datagrams of 8 bytes (`fyisp-tr`) to port 33434 with a low TTL. A host answers them with ICMP port unreachable.

Each probe goes out once per round per hop; the destination gets at most one probe per round, plus up to 5 more past it when it does not answer. **Routers** on the way get at most 1 probe a second from one install, over all of its traces and probe types together (with a burst of 2), and hops that several traces share are probed once per round for all of them.

