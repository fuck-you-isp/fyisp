# STUN endpoints: terms-of-service check

Checked on 2026-10-10 before the call-quality release (v0.5.0), as the plan's "Before release" step asks ([CALL-QUALITY-PLAN.md](CALL-QUALITY-PLAN.md)). This is a reading of the providers' public docs and terms, not legal advice. The owner decides what ships.

**What fyisp sends.** RFC 5389 STUN Binding Requests only: 20 bytes, no attributes, no credentials, never TURN, never an allocation. Each sample is a burst of 5 requests 20 ms apart: every 5 s per target (15 s for Cloudflare port 53, 30 s for the Twilio regions), around the clock, from the user's own machine. That is about 1 request per second per target per install, or roughly 8 KB/s per provider for every 1,000 installs. A WebRTC client sends a similar burst at the start of every call.

## Result

| Endpoint | Profile | Verdict | Reason |
|---|---|---|---|
| Google `stun.l.google.com:19302` (and `:3478`) | default (`:3478` in calls) | **OK with caveat** | Google's own WebRTC docs and samples use it as the default STUN server; there's no statement on public use, limits or an SLA. |
| Cloudflare `stun.cloudflare.com:3478` and `:53` | default | **OK** | It's documented as "free and unlimited", and both ports are listed; no account is needed. |
| Twilio `global.stun.twilio.com:3478` | default | **OK with caveat** | Twilio documents STUN as free and shows the host with no credentials. Its AUP bans "automated means … to gain access to or use the Services", which is aimed at bots but is literally broad. |
| Twilio `{us1,us2,ie1,de1,sg1,jp1,au1,br1,in1}.stun.twilio.com:3478` | calls | **Do not ship** | These hostnames aren't documented. The regions page lists only IP ranges, and region pinning is documented only by **edge name** (`frankfurt.stun.twilio.com`). The `us1`-style codes are retired legacy region IDs, and `in1` is not even on that legacy list. |
| Microsoft Teams `worldaz.tr.teams.microsoft.com:3478` | calls only (opt-in) | **Do not ship** | The hostname isn't documented; only `world.tr.teams.microsoft.com`, which is a CNAME to it, is named. Microsoft documents the relays for licensed Microsoft 365 use by its own clients, not as a public STUN service, and its relay test tool "is not intended for load/stress testing". |

> **Do not ship:**
> - **The 9 Twilio regional hosts** in the `calls` profile (`us1` … `in1`.stun.twilio.com).
> - **The Microsoft Teams relay** `worldaz.tr.teams.microsoft.com` (opt-in, `calls` only).
>
> I didn't change the catalog or profiles; the owner decides. The team lead's recommendation is to ship Google and Cloudflare only, and drop Twilio and Teams. If Twilio regions are kept, the documented edge names (`ashburn`, `umatilla`, `dublin`, `frankfurt`, `singapore`, `tokyo`, `sydney`, `sao-paulo`, `mumbai`.stun.twilio.com) would make them **OK with caveat**, the same as global. Each would have to be re-verified first.

Also for the owner (not part of the STUN feature): the TCP targets `Teams-Relay-EU` (`euaz.tr.teams.microsoft.com`) and `Teams-Relay-US` (`usaz.tr.teams.microsoft.com`) have been in the catalog since 2026-09-27. Microsoft doesn't name these pools anywhere; the plan already dropped them as UDP targets for that reason. Consider dropping them or relabelling them as undocumented.

## Google: `stun.l.google.com`

**Verdict: OK with caveat.** Google publishes this host as the default STUN server in its own WebRTC getting-started guide and in the official WebRTC samples, and Chrome and Meet use it. No Google page says that third parties may use it, sets a rate limit or offers an SLA. Google's general terms forbid only abuse and disruption, and their "automated means" clause is about content and robots.txt, which doesn't fit STUN.

**Caveats:**
- Treat it as best effort.
- fyisp already shows a dead server as "This test server isn't answering, not your connection" and never raises a verdict for one dead provider.
- Port 3478 on this host isn't named anywhere. Meet's network guide only lists UDP 3478 and 19302–19309 as ports to open.

**Sources:**
- https://webrtc.org/getting-started/peer-connections: `const configuration = {'iceServers': [{'urls': 'stun:stun.l.google.com:19302'}]}`
- https://github.com/webrtc/samples/blob/gh-pages/src/content/peerconnection/trickle-ice/js/main.js: the default server `{"urls":["stun:stun.l.google.com:19302"]}`
- https://knowledge.workspace.google.com/admin/meet/prepare-your-network-for-meet-meetings-and-live-streams: "For audio and video, set up outbound UDP ports 3478 and 19302–19309."
- https://policies.google.com/terms: "You must not abuse, harm, interfere with, or disrupt our services or systems". The automated-access clause covers "using automated means to access content from any of our services in violation of the machine-readable instructions on our web pages (for example, robots.txt files…)".

## Cloudflare: `stun.cloudflare.com`

**Verdict: OK.** Cloudflare explicitly offers the STUN service free and unlimited, with no account or credentials. Its network allowlist lists both 3478/udp and 53/udp, and also says not to rely on 53 alone. That matches how fyisp uses port 53: only to tell "STUN ports blocked" from "UDP blocked".

The terms that likely apply are Cloudflare's website and online-services terms, which cover free services offered without a subscription (1.1.1.1 is their example). This is an inference, because the STUN service isn't named there. Those terms forbid use that could "overburden" the service; about 1 request/s per install is far from that.

**Sources:**
- https://developers.cloudflare.com/realtime/turn/faq/: "Cloudflare's STUN service at `stun.cloudflare.com` is free and unlimited."
- https://developers.cloudflare.com/realtime/turn/: "STUN over UDP | stun.cloudflare.com | 3478/udp"
- https://developers.cloudflare.com/realtime/realtimekit/network-allowlist/: "STUN over UDP | `stun.cloudflare.com` | `3478/udp` | `53/udp`" and "Do not rely only on `53/udp`, because Internet service providers and browsers can block this port."
- https://www.cloudflare.com/website-terms/: "You may not use the Websites or Online Services in any manner that could damage, disable, overburden, disrupt or impair" them.

## Twilio: `global.stun.twilio.com` and the regional hosts

**Global: OK with caveat.** Twilio lists STUN as free in every region. Its API docs return `stun:global.stun.twilio.com:3478` with no username or credential, while the TURN entries carry both. No page says tokenless STUN is allowed for non-customers, and none forbids it either.

The caveats are in the terms:
- **The AUP, which applies to anyone using the Services:** under "No Service Integrity Violations … Harmful Code or Bots", it lists "using automated means, such as bots, to gain access to or use the Services." In context this targets harmful bots, but read literally it covers any automated use.
- **The ToS:** "intended for business use".

If Twilio objects, the target can be removed in a release without breaking anything, because the call path is the median over several providers.

**Regional hosts: Do not ship.** Twilio's docs pin a region by replacing the `global` prefix with an **edge location name**, for example `frankfurt.stun.twilio.com`. The STUN regions page lists 9 edges: sydney, sao-paulo, frankfurt, mumbai, dublin, tokyo, singapore, ashburn and umatilla. The `us1`/`ie1`/... codes appear only on Twilio's legacy-regions page, which says they "have been replaced by descriptive Edge Location names". That page has no `in1` entry; India is the `mumbai` edge. The codes resolve today, but nothing documents them for STUN. If they were renamed to the documented edge names, they would be **OK with caveat**, on the same terms as global.

**Sources:**
- https://www.twilio.com/en-us/stun-turn/pricing: STUN "Globally available in all regions", "Free"
- https://www.twilio.com/docs/stun-turn: a sample token response with `"urls": "stun:global.stun.twilio.com:3478"` and no credentials
- https://www.twilio.com/docs/stun-turn/api: "Each URI begins with the prefix `global`. Replace this prefix with the Twilio region/edge location name that you wish to pin to … for example `frankfurt`", with the example `stun:frankfurt.stun.twilio.com:3478`
- https://www.twilio.com/docs/stun-turn/regions: the 9 edge names with IP ranges, including "India `mumbai`"; no hostnames
- https://www.twilio.com/docs/global-infrastructure/edge-locations/legacy-regions: us1→ashburn, us2→umatilla, ie1→dublin, de1→frankfurt, sg1→singapore, jp1→tokyo, au1→sydney, br1→sao-paulo; "These have been replaced by descriptive Edge Location names"
- https://www.twilio.com/en-us/legal/aup: "Do not violate the integrity of the Services, including: … Harmful Code or Bots. … or using automated means, such as bots, to gain access to or use the Services."
- https://www.twilio.com/en-us/legal/tos: "THE SERVICES ARE INTENDED FOR BUSINESS USE OR USE IN CONNECTION WITH AN INDIVIDUAL'S TRADE, CRAFT, OR PROFESSION ONLY."

## Microsoft Teams: `worldaz.tr.teams.microsoft.com` (opt-in)

**Verdict: Do not ship.** This is the closest call in this review; the arguments both ways are below.

**For it:**
- Every Teams client sends STUN to the Transport Relays at call setup.
- Microsoft requires admins to open UDP 3478 (STUN) to them.
- Microsoft's own network connectivity test traceroutes to `world.tr.teams.microsoft.com`, the documented name; `worldaz` is the name it CNAMEs to.
- Microsoft's Teams Network Assessment Tool "streams packets to the nearest edge site and back" to measure exactly this path.
- Neither the Product Terms nor the Services Agreement mentions STUN or probing. They forbid unauthorized access, disruption and circumventing restrictions, and a Binding Request is the unauthenticated part of the protocol, with no allocation.

**Against it:**
- Microsoft documents the relays for **customers' own clients**, not as a public STUN service.
- `worldaz` is not named anywhere.
- The Network Assessment Tool page says it "is not intended for load/stress testing". About 1 request/s isn't load testing, but it does show that Microsoft cares how its relays are exercised.

**If the owner keeps it anyway:**
- Keep it opt-in, as it is today: only in `calls`, never in `default`.
- Probe the documented `world.tr.teams.microsoft.com` (the same pool) instead of `worldaz`.
- Consider a slower cadence.

**Sources:**
- https://learn.microsoft.com/en-us/microsoftteams/microsoft-teams-online-call-flows: "Open connectivity to TCP ports 80 and 443, and to UDP ports 3478 (STUN), 3479 (Audio), 3480 (Video), and 3481 (sharing/VBSS) are required."
- https://learn.microsoft.com/en-us/microsoft-365/enterprise/urls-and-ip-address-ranges: ID 11, 52.112.0.0/14, 52.122.0.0/15, 2603:1063::/38 on UDP 3478–3481, "reachable for customers using Microsoft 365 plans"
- https://learn.microsoft.com/en-us/microsoft-365/enterprise/office-365-network-mac-perf-onboarding-tool: "a traceroute to *world.tr.teams.microsoft.com*"
- https://www.microsoft.com/en-us/download/details.aspx?id=103017: "Test the connection to a Microsoft Teams relay by streaming packets to the nearest edge site and back"; "This tool is not intended for load/stress testing."
- https://www.microsoft.com/licensing/terms/product/ForOnlineServices/all: no one may use an Online Service "to try to gain unauthorized access to or disrupt any service, device, data, account or network"
- https://www.microsoft.com/en-us/servicesagreement: "Don't circumvent any restrictions on access to, usage, or availability of the Services."

## Not checked
- Whether the Twilio edge names (`ashburn.stun.twilio.com`, ...) answer STUN today. The earlier research says they resolve to the same pools, but they must be re-verified before any rename.
- No provider was contacted. A short note to Cloudflare and Twilio before a wide release would turn "no clause against it" into permission.
