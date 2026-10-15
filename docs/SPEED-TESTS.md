# Speed tests

fyisp can measure your download and upload speed around the clock, so you can see whether your connection gets slower at certain times of day (the classic evening dip when everyone streams), and whose network the slowdown is in. **It is off by default.** Switch it on with `--speedtest`, or pick a mode in Details → Speed (the choice is saved). A `--speedtest` flag wins: Details then shows its mode but can't change it, and turning tests off there lasts only until the next restart. "Test now" (or *Test speed now* on Home) queues one test, and "Test every server" a sweep of every server, in any mode. Tests run one at a time from a single queue (scheduled ones included), and results you asked for are kept out of the time-of-day patterns.

```sh
fyisp --speedtest                                  # mode regions: 9 regions, each once an hour
fyisp --speedtest=regions,add=canada,us-northwest  # plus opt-in regions
fyisp --speedtest=nearby                           # 3 nearby providers and 1 far region, every 15 minutes
fyisp --speedtest=nearby,far=us-west               # plus another far region
fyisp --speedtest-server 192.168.1.20:5201 --speedtest=every=5m   # your own iperf3 server on your network: down to every=1m
fyisp --speedtest --speedtest-server Leaseweb-Tokyo,OVH-Sydney     # chosen catalog servers (mode custom)
```

**Modes:**
- **`regions`**, the default once switched on: 9 regions (UK, Western Europe, Central Europe, US East, US Central, US West, East Asia, Southeast Asia and Oceania), each tested once an hour.
  - Only one test runs at a time; the regions are spread over the hour, about 400 seconds apart, in an order and at minutes that differ per install, with a minute of jitter on each hour's start.
  - Most regions have servers from several providers in the same city. The provider rotates by hour and day, so every hour of the day is measured with every provider within a few days.
  - Opt-in regions (`add=`): US Northwest, US Mid-Atlantic, US Midwest, US Southeast, Canada, US Bay Area, South America, Central Asia, Netherlands and UK North.
  - Results show whether a dip is your line (every region and provider at once), one provider, one route or one server.
- **`nearby`**: the nearest server of 3 different providers (same city preferred), plus one far region (US East from Europe, London from the US; `far=` adds more), each every 15 minutes. The nearby servers are picked again daily. It is lighter on distance, heavier on frequency.
- **`custom`**: the servers you list with `--speedtest-server`, catalog ids or `host:port` of your own iperf3 server, each every 15 minutes.
- `every=` sets the interval per region or server (at least 15 minutes against public servers; at least 1 minute in mode `custom` when every server is on your own network).

**What is sent, to whom.** Each test is an iperf3 test over IPv4, built into fyisp, so nothing has to be installed:
- download, then upload, each with 8 parallel TCP streams;
- the ramp-up (until the speed levels off, decided afterwards) is left out; a direction runs at most about 11 seconds, ramp-up included;
- then 3 to 10 seconds are measured, stopping once the speed has levelled off and held steady for 3 seconds (a second within 5% of the one before): up to 10 seconds each way, whatever the data.
- speeds are what TCP delivers (goodput): about 4–6% below the line rate your ISP sells, because of packet headers. A 1 Gbit/s plan reads about 940–960 Mbit/s at best.

The servers are public iperf3 servers run by hosting companies and volunteers: Leaseweb (16), OVH (12), Clouvider (9), InterServer (3), Hurricane Electric, Scaleway, Eranium, Uztelecom and Edgoo. All 45 are listed with their sources in [`internal/profile/catalog/iperf3/`](../internal/profile/catalog/iperf3/), with the regions in `regions.yml`. The list is built into fyisp and changes only with a release. Like every probe target, they see your public IP address.

**How much it costs your line.** A speed test fills the line while it runs, by design:
- **Time under test:** mode `regions` keeps the line under test about 5% of the time (9 tests of 10 to 25 seconds an hour); `nearby` about 9%. Details shows the planned share for each mode. fyisp warns when your settings would exceed 25%.
- **Data:** a test moves your line's speed for 4 to 11 seconds each way; there is no byte cap. Mode `regions`, the default, runs 216 tests a day:

  | Your line | Data a month (mode `regions`) |
  |---|---|
  | 25 Mbit/s | about 150 to 450 GB |
  | 100 Mbit/s | about 650 GB to 1.8 TB |
  | 1 Gbit/s | about 6.5 to 18 TB |
  | one "Test every server" sweep | Details estimates it from your recent tests |

  Mode `nearby` uses about 1.8 times as much. Details shows the data used in the last 24 hours and this month, and an estimate for each mode.
- **Fast lines:** a test still speeding up when its 10 seconds end (a far server on a fast line) shows "≥": the speed was at least this. Speed tests show when your speed drops, not whether you get your plan's advertised speed.
- **Metered lines:** leave speed tests off on metered or capped connections.

**Be kind to the servers.** The operators and what fyisp sends them are listed in [IPERF3-TERMS.md](IPERF3-TERMS.md); [OPERATORS.md](OPERATORS.md) is for the operators themselves. Each server port runs one test at a time.
- If a port is busy, or refuses for a moment (a stock iperf3 server re-listens after every test), fyisp tries up to 2 more ports.
- Then it moves to another provider in the same city, then to a nearby city. Single-port servers (Hurricane Electric) are never retried. If nothing answers, it records **"server busy"**, never a slow result.
- No new port or server is tried after 25 seconds of a slot, and a slot never delays the next one.
- A server that was busy 3 scheduled tests in a row rests for 6 hours.
- `--speedtest-skip=<id,…>` (or the toggle per server in Details → Speed) never tests a server again: honour an operator's request without waiting for a release.

**One test at a time.** Scheduled tests, Test now and sweeps share one queue: your own requests first, then scheduled ones. A running test is never interrupted, except when you cancel it. A scheduled test that waited more than 10 minutes is dropped. Details shows the queue with estimated start times.

Please don't run modes more often than every 15 minutes against these servers. For more frequent tests, run your own iperf3 server on your own network (`iperf3 -s` on a NAS or another computer), then `--speedtest-server 192.168.1.20:5201`: servers on your own network may be tested as often as every minute. Your own servers elsewhere (a VPS) keep the 15-minute minimum.

**What you get.** For each test:
- download and upload speed, and how long each was measured;
- a **score** against that server's own normal (the 90th percentile of its last 14 days), so slow and fast servers compare;
- TCP retransmits;
- the region, the server, and the fallback level (`none`, `same_metro` or `nearby_city`);
- whether the server's own link was the limit (a result within 10% of its published capacity);
- **latency under load**: the round-trip time to your ISP's first router (behind your line's queue) and to your own router, in the 2 minutes before the test (from the regular probes) and during its download and upload (pinged every 150 ms); lost pings count. A big jump at the ISP's router is bufferbloat, the reason calls break while someone uploads; a jump at your own router is your home network (often Wi-Fi).
- **connection type** (wired or Wi-Fi, Linux) and, locally, **the first network past your ISP** on the way to each server (a traceroute at most once a day per server).

Details → Speed shows:
- what the slowdown points to: your provider's network (every region and provider dips together, checked against your provider's first router), the link to one provider, one long-distance route, or one server's own busy hours, with the evidence and how sure it is;
- a heatmap of regions × hour of day by score, with how many tests and providers back each hour;
- a time series per region and server, a table, and compare;
- a note when a region mostly used its fallback in the last day.

Home shows your latest result against your usual speed (the median of the last 7 days' tests), e.g. *Your speed: about 930 Mbps down / 940 Mbps up — about your usual speed.* When every region and provider dips at the same time of day, Details → Speed says so, for example *Every region slows from about 7 to 11 pm … most likely your internet provider's own network.* Only scheduled tests count toward patterns: Test now and sweeps don't. `fyisp export --speedtests` writes every test as CSV.

**Probes keep running during a test.** Their results are stored as measured, so a test that slows your line shows up as a latency spike or loss, and can count toward the verdict. The "show speed tests" toggle under each chart (off by default) marks when tests ran, so a spike can be matched to a test. The 2 minutes before a test in *latency under load* come from these probes; during the test fyisp adds its own pings every 150 ms. `/api/speed` reports the share of the last 24 hours spent under test (`test_share_24h`).

Limits: one test at a time; each direction sends data for at most 11 seconds (ramp-up included) and is cut off at 13; no new server is tried after 25 seconds of a slot, so a slot ends within about a minute even in the worst case; IPv4 only.

The routes, types and errors are in [SPEEDTEST-API.md](SPEEDTEST-API.md).
