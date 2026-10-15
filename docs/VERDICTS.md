# How fyisp decides

How Home words its tiles, how the verdict picks whose fault a problem is, and when Home says *Wi-Fi*. Back to the [README](../README.md).

## How fyisp decides Good, Slow, Choppy and Down

This is what *How fyisp decides* under Home's tiles says, with the numbers behind it.

- **Calls** (Zoom, Teams, Meet, Slack) are judged on their UDP call path over the last 3 minutes. They are **Choppy** when the delay is over 150 ms, the jitter over 30 ms, or 2% or more of packets are lost (in at least 2 bursts: one unlucky burst isn't enough), the points where people start to notice on a call. **Down** means a delay over 400 ms, 10% loss or more in each of the last 2 minutes, or not connecting (their website or call server stops answering). Too few answers is *not enough data*, never a problem.
- **Everything else** is **Slow** when a check takes more than twice its usual time *and* at least 30 ms longer, or when a quarter or more of its checks in a minute get no answer. **Down** means it isn't answering: most checks fail. Slowness alone never makes an app *Down*, YouTube and Netflix included.
- **YouTube and Netflix** buffer ahead, so slowness never marks them Down; only failing to load does. If they buffer, test your speed.
- **Drops of 30 seconds or more always show**, even when everything recovers: the bar turns red, the tile notes *Dropped 40 s at 10:14 am* for 30 minutes (the most recent drop only), and the drop is listed under problems. A drop counts only when most of an app's checks fail together, so one flaky server can't set it off; shorter blips leave nothing behind. When your whole connection dropped, the card says *Working again* with the likely cause for 30 minutes.
- **Words don't flicker.** For slowdowns, a tile's word changes once 3 of the last 5 minutes are bad, and goes back to *Good* after 5 good minutes in a row. Outages show at once and clear as soon as the checks answer again.
- **"Usual"** is learned from your own connection over the last day, per target, leaving out bad minutes. For the first day fyisp uses fixed limits (*Slow* above 300 ms). A lasting route change is learned again after about an hour; a new router or network (a laptop that moved) starts over.
- **Likely cause** comes from comparing your router, your internet provider's first stop, the internet beyond it and all the apps at the same moment. It's a strong hint, not proof. Problems during one of fyisp's own speed tests are put down to the test.

The card follows the [verdict](#whose-fault-is-it-verdicts) (the last minute, every 5 seconds) and the tiles are judged minute by minute, so for a minute or two they can disagree.

## Whose fault is it? (verdicts)

Above the charts, fyisp shows one plain-English verdict, re-evaluated every 5 seconds over the last minute of probes, and keeps an outage log of every period that was not *ok*. A target is unhealthy when it loses at least 20% of its probes (and at least 2 in the minute: a single lost connect out of four is noise) or its median latency is far above its usual (the last 7 days at this hour once known, otherwise the last 30 minutes). The first matching kind wins:

| Kind | Meaning |
|---|---|
| `no_network` | This device has no working connection (no route, every layer unreachable). |
| `lan` | The *Gateway* (your router) is unhealthy: Wi-Fi, cable or the router itself. |
| `isp` | The router is fine, the *ISP edge* (first public hop past it) is not: your ISP's access network. |
| `upstream` | Router and ISP edge are fine, but the anycast resolvers or most monitored services (in more than one group) fail: beyond your ISP. |
| `dns` | The path works, but at least 30% of the services can't be resolved. |
| `service` | Only some services are unhealthy: their problem, not yours. |
| `udp_blocked` | Everything above is fine, but UDP (STUN) to at least two independent providers has timed out 10 bursts in a row, port 53 included: calls fall back to slower TCP relays. Also a firewall on your own computer. |
| `stun_blocked` | As `udp_blocked`, but STUN on UDP port 53 still answers: the call ports are blocked, UDP itself is not. |
| `udp_degraded` | Everything above is fine, but the UDP path to at least two STUN providers has lost at least 5% of packets or jittered by 30 ms or more for 3 minutes. |
| `ok` / `warming_up` | Everything looks fine / the first minute after start. |

The UDP kinds wait 2 minutes after start (or for the first STUN answer), so a network that blocks UDP from the start is still caught, and a STUN server that is down alone never raises them. Hotel and office networks often block UDP for good: after 6 hours `udp_blocked`/`stun_blocked` is marked quiet and shown as information.

Loss on an inner link shows on every layer beyond it, so the verdict blames the innermost lossy layer: the gateway or ISP edge is blamed once it has lost at least 5 probes and 10% of them and the layers beyond it lose at least half as much. A layer further out is blamed only when its loss is significantly higher than that of the layers inside it (a two-proportion test on the minute's counts), so a few unlucky probes can't point at the wrong layer.

A new problem must hold for about 10 seconds before it is shown, and the verdict returns to *ok* only after a full minute without problems. Verdict summaries never contain addresses or host names. Every host name is looked up again every 60 seconds. After one failed lookup fyisp keeps probing the last good address; after two in a row (about 70 seconds into a resolver outage) the host's probes count as lost with reason *DNS*, as a real application would fail too, until a lookup succeeds again.

The layers come from the built-in **Network path** group, shown first: the *Gateway* and the *ISP edge* (found automatically from the routing table and a short traceroute; pinged every second) and two public anycast resolvers (1.1.1.1, 8.8.8.8; ICMP and TCP). A profile passed with `--config` keeps this group unless it sets `path: false` at the top level (`--no-path` does the same for `--profile`); without it fyisp can still tell `dns`, `service` and `upstream` apart, but not `lan` from `isp`.

## Wi-Fi or router?

fyisp measures from the machine it runs on, so it can only speak for that machine's own Wi-Fi. Home's first box says *Your Wi-Fi and router* only when all of these hold; otherwise it says *Your home router*, with a hint that fyisp can't see the Wi-Fi of the device you're reading on:

- **fyisp's own connection is Wi-Fi.** This is detected on Linux only (from `/sys/class/net`). macOS and Windows report it as unknown.
- **fyisp is not in a container.** Inside Docker (and Podman, systemd-nspawn, LXC, Kubernetes) the network fyisp sees is the container's, so it always uses the router wording. fyisp counts as containerized when `/.dockerenv`, `/run/.containerenv` or `/run/systemd/container` exists or `$container` is set (the release image sets `container=oci`).
- **You are looking at the page on that same machine:** the request comes from a loopback address or one of the machine's own addresses, carries no proxy header (`Forwarded`, `X-Forwarded-For`, `X-Real-Ip`, `Cf-Connecting-Ip`, `Tailscale-User-Login`), and doesn't use an `--allow-host` name. Through a reverse proxy, a tunnel (cloudflared, Tailscale Serve) or from another device on your network, Home uses the router wording.

A forwarder that adds no proxy header (`ssh -L`, `socat`) looks like the same machine, so a page opened through one says *Your Wi-Fi and router*. Only the wording changes; the measurements and the verdicts are the same. Whether you're on the same machine is decided per request and never stored, logged or cached, and the API returns only the resulting words, not your address.
