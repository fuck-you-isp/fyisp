# For iperf3 server operators

fyisp is a home internet monitor. If a user turns its speed tests on (they are off by default), it runs iperf3 tests against public iperf3 servers, yours possibly among them, to see when the user's connection slows down during the day.

**What you will see:**
- stock iperf3 TCP tests over IPv4: 8 streams, download (reverse) then upload;
- at most 100 MB each way, 3 to 8 seconds measured each way; the client ends the test early with TEST_END;
- the control cookie starts with `fyisp` (visible in `iperf3 -s --debug`);
- one test at a time per install, from the user's home connection.

**How often:** by default each install tests a region once an hour, rotating between the providers of that region, so a server gets roughly one test every 2 to 3 hours from each install that has speed tests on. Some modes test a few nearby servers every 15 minutes. No mode tests a catalog server more often than every 15 minutes.

**When your server is busy** (ACCESS_DENIED, or refusing), fyisp tries at most 2 other ports, then another provider. After 3 busy scheduled tests in a row it leaves your server alone for 6 hours.

**To have your server removed:** contact the fyisp maintainers: **<contact to be filled in by the owner before release>**. Removal ships in the next release; until then users can stop testing it with `--speedtest-skip=<server id>` (the ids are in [`internal/profile/catalog/iperf3/`](../internal/profile/catalog/iperf3/)). The list of operators and what fyisp sends is in [`IPERF3-TERMS.md`](IPERF3-TERMS.md).
