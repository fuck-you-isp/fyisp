# Call quality

Call audio and video travel over UDP, and a TCP connect says little about them. fyisp therefore also measures the UDP path with tiny STUN requests, the same kind of packet a call app sends before every call, and answers two questions: *is my connection good for calls right now?* and *why did my call at 14:14 go bad?* Back to the [README](../README.md).

## The Calls view

Home's call tiles (Zoom, Microsoft Teams, Google Meet and Slack) are judged on these measurements (see [How fyisp decides](VERDICTS.md#how-fyisp-decides-good-slow-choppy-and-down)). **Technical details** has the full view: **Calls | Overview | Charts** switches between them, and Details opens on Calls whenever the profile has STUN targets (as `default` and `calls` do); otherwise on the charts, or on the Overview when the profile is large.

- **The top card** gives one answer, e.g. *● Good for calls · 32 ms · 0% loss · steady*, refreshed every few seconds with *updated 4 s ago*: the pre-call check. It is the worst of the path verdict (it never says Good while the banner shows an ISP outage, and it names that cause inline), the UDP verdicts below, your call path's score and the measured apps' scores. When it is not Good it says why, e.g. *Poor: 8% loss on your call path (worst minute 14:12)*.
- **Working hours**: a strip for Mon–Fri 09–17 local time, colored per minute (good, fair, poor, unusable, no data), with a summary such as *Today 09–17: Good 92% · Poor 14:10–14:16*. Click a minute to ask about it.
- **What happened at …?**: pick a time (the default is the last 60 minutes) and get one sentence about the 15 minutes either side of it that names the layer at fault, e.g. *14:10–14:16: 8% loss at your ISP edge; Wi-Fi/router fine*. It uses the outage log, so it blames layers the same way the verdicts do. It links to the charts.
- **By app**, over the last 5 minutes: Meet and Teams get a state, round trip and loss from their own STUN servers; expanding a row shows the MOS, jitter, p95 round trip and the last hour. If you remove the Teams relay (see [Which apps are measured](PROFILES.md#which-apps-are-measured)), Teams' row says whether its website answers and that its calls take your call path. Zoom, Slack, Discord, WhatsApp and Webex share one row (see below) that says whether their websites are reachable over TCP.

A STUN server that stops answering while the others still answer is shown as *This test server isn't answering; your other call checks look normal*. After a start the view says *Not enough data yet* until 10 bursts have been measured.

## How it is measured

- Every STUN target gets **one long-lived UDP socket**, as a real call does (one address and port pair, so there is no new NAT mapping per sample). It is reopened only when the server's address changes or after an error.
- Each sample is a **burst of 5 STUN Binding Requests, 20 ms apart** (RFC 5389, 20 bytes each, no attributes; never TURN, never credentials). Bursts run every 5 seconds per target unless the [list below](#stun-servers) says otherwise.
- A burst records the **median round trip**, the **in-burst jitter** (the mean difference between consecutive round trips, close to what a call's jitter buffer sees) and **loss**: requests not answered within 1 s. Only timeouts count as loss. A refused port or a failed DNS lookup is *unknown*, never loss.
- A burst never spans an address change, so a GeoDNS server switching region (the Teams relay) is never counted as jitter.
- Each one-minute bucket is scored on its own, and a window is judged by its **worst minute**: a 5-minute mean would hide a 30-second stall.

### STUN servers

This is the one list of the STUN servers fyisp ships; the catalog files ([`calls-stun.yml`](../internal/profile/catalog/calls-stun.yml), [`twilio.yml`](../internal/profile/catalog/twilio.yml)) are the source of truth. Each server's terms are reviewed in [STUN-TERMS.md](STUN-TERMS.md).

| Target | Server | Every | Profile | Role |
|---|---|---|---|---|
| Meet-STUN | `stun.l.google.com:19302` | 5 s | `default`, `calls` | call path (anycast); Google Meet |
| Meet-STUN-3478 | `stun.l.google.com:3478` | 5 s | `calls` | call path; Google Meet, for networks that block 19302 |
| Cloudflare-STUN | `stun.cloudflare.com:3478` | 5 s | `default`, `calls` | call path (anycast) |
| Cloudflare-UDP53 | `stun.cloudflare.com:53` | 15 s | `default`, `calls` | call path; also tells *STUN ports blocked* from *UDP blocked* |
| Twilio-STUN-global | `global.stun.twilio.com:3478` | 5 s | `default`, `calls` | call path (GeoDNS to the nearest Twilio region) |
| Twilio-us1 … Twilio-in1 | `{us1,us2,ie1,de1,sg1,jp1,au1,br1,in1}.stun.twilio.com:3478` | 30 s | `calls` | fixed regions, a world map of the UDP path (not in the call path) |
| Teams-Relay | `worldaz.tr.teams.microsoft.com:3478` | 5 s | `default`, `calls` | call path (GeoDNS to an Azure region); Microsoft Teams |

## The score

A simplified ITU-T G.107 E-model turns round trip, jitter and loss into an R factor, and R into a mean opinion score (MOS, 1 to 4.5):

```
delay = rtt/2 + 2·jitter + 10                              (one way, ms)
R     = 93.2 − delay/40            (delay < 160 ms)
R     = 93.2 − (delay − 120)/10    (delay ≥ 160 ms)
R    −= 2.0 per % loss up to 5 %, then 2.5 per % above     (R clamped to 0..100)
MOS   = 1 + 0.035·R + 7·10⁻⁶·R·(R − 60)·(100 − R)
```

The loss penalty is softer than G.711's tables because Opus forward error correction and packet-loss concealment (Meet, Teams) hide some loss. The bands are G.107's user-satisfaction bands:

| Band | MOS | R |
|---|---|---|
| Good | 4.0 and up | about 80 and up |
| Fair | 3.6 to 4.0 | 70–80 |
| Poor | 3.1 to 3.6 | 60–70 |
| Unusable | below 3.1 | below 60 |

*Your call path* combines the anycast and GeoDNS STUN servers marked *call path* above: each minute takes the median of the servers' round trip, jitter and loss, so one bad server does not make the path bad.

## Verdicts, metrics and the off switch

- **Verdicts.** `udp_blocked`, `stun_blocked` and `udp_degraded` (see [Whose fault is it?](VERDICTS.md#whose-fault-is-it-verdicts)) fire only when the path and TCP are healthy and at least two independent STUN providers agree, so a single dead STUN server never raises one. After 6 hours a blocked verdict turns quiet: it is shown as information (*It has been this way for hours (common on hotel and office networks)*), and the Calls rows say *Blocked*, not *Unusable*. UDP loss never feeds the main verdict or a service's loss.
- **Metrics.** `fyisp_udp_rtt_seconds`, `fyisp_udp_jitter_seconds` and `fyisp_udp_loss_ratio` per STUN target, and `fyisp_call_mos{app}`; see [API.md](API.md#metrics).
- **`--no-udp`** leaves out every STUN target and hides the Calls view. Nothing else changes.

## Limits

- **Anycast targets measure the path to the nearest edge.** Anycast and GeoDNS servers answer from a data centre near you, not from the meeting's media server or the other people on the call. Most call problems are on your own connection, which is exactly that path; a problem beyond the provider's edge is not seen.
- **Idle probes miss bufferbloat.** Calls often break when the link is busy (an upload, a backup, someone streaming), because queues only fill under load. fyisp's probes are tiny and see a quiet link, so a call can be bad while the strip is green. Mark the call, and compare with the TCP charts at that time.
- **IPv4 only.** STUN is measured over IPv4; a call that goes over IPv6 takes a path fyisp doesn't measure.
- The score is a model, not the app's own rating: expect it to agree on the band, not the decimal.
