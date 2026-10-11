# Plan v2: Call quality for fyisp v0.5.0

> **Historical plan.** The share link it describes (`--share`, the Cloudflare quick tunnel, the public listener and everything served on it) was removed in v0.8.0; see the README's "Sharing" and "Upgrading to v0.8.0".
This revises v1 after review by five reviewers: network/VoIP, Go architecture, UX, security/privacy and test/release. All five said "approve with changes". This version applies their must-fix items and resolves the conflicts between them. The items still open for the owner are at the bottom.

## Goal
Let a remote employee on calls all day answer two questions:
1. "Is my connection good for calls right now?"
2. "Why did my call at 14:14 go bad?"

The answers come from measuring the UDP path that call media actually uses, with tiny STUN probes.

## Endpoints that ship
| Target | Host:port | Profile | Cadence |
|---|---|---|---|
| Meet-STUN | `stun.l.google.com:19302` (fallback `:3478` on the same host when 19302 is blocked) | default, calls | burst every 5 s |
| Cloudflare-STUN | `stun.cloudflare.com:3478` | default, calls | burst every 5 s |
| Cloudflare-UDP53 | `stun.cloudflare.com:53` (STUN on a non-STUN port, used only for "UDP vs STUN-ports blocked") | default, calls | single probe every 15 s |
| Twilio-STUN-global | `global.stun.twilio.com:3478` | default, calls | burst every 5 s |
| Twilio regions (9) | `{us1,us2,ie1,de1,sg1,jp1,au1,br1,in1}.stun.twilio.com:3478` | calls | burst every 30 s |
| Teams-Relay | `worldaz.tr.teams.microsoft.com:3478` | **owner decision (see below)** | burst every 5 s |

**Dropped after review:**
- The undocumented Teams pools `euaz`, `usaz` and `inaz`.
- The hard-coded Meet IP `74.125.247.128`.
- A separate "Signal" row. It folds into "Your call path", and the tooltip says Signal relays via Cloudflare.
- AWS's 19 UDP targets. They're deferred until there's a per-region UDP view, because the Overview has no UDP kind.

**Not measurable:** Zoom, Slack, Discord, WhatsApp and Webex. They are shown as one row, never as five grey chips.

## Measurement (network review)
- **Socket handling:**
  - One long-lived connected `udp4` socket per target. Real calls keep one 5-tuple, and a fresh socket per sample could use about 1,100 NAT/CGNAT mappings and starve the user's traffic.
  - The socket is reopened only when the resolved IP changes or after an error.
- **Bursts:**
  - Each sample is a burst of 5 STUN Binding Requests 20 ms apart on that socket.
  - Per burst we record:
    - **median RTT**;
    - **in-burst jitter**: the mean |ΔRTT| of consecutive replies, which is real packet-scale jitter, close to what a jitter buffer sees;
    - **lost/sent**.
- **Loss and timeouts:**
  - Only timeouts count as loss. ICMP-refused and DNS failures count as "unknown".
  - The udp timeout is 1 s (`udpTimeout`). `--timeout` does not apply to UDP.
- **Teams relay switching:** Teams switches relay by GeoDNS (Sweden ↔ Dublin). Bursts never span an IP change, and the change epoch is recorded, so cross-relay deltas never count as jitter.
- **Labels:** anycast targets (Google, Cloudflare) are labelled "your connection to the nearest edge".
- **Known gap: bufferbloat.** Calls break when the link is busy, and idle probes don't see that. This is documented in the README.

## Score (network review)
- **Formula:** simplified E-model.
  - `eff = rtt/2 + 2·jitter + 10`. In-burst jitter is real, so the 2× weight stays.
  - `R = 93.2 − eff/40` (or `− (eff−120)/10` above 160).
  - Loss penalty: 2.0 per % up to 5%, then 2.5 per %. The softer penalty reflects Opus forward error correction and packet-loss concealment in Meet and Teams.
  - **R is clamped to 0–100.**
  - `MOS = 1 + 0.035R + 7e-6·R(R−60)(100−R)`.
- **Bands:** Good ≥ 4.0, Fair 3.6–4.0, Poor 3.1–3.6, Unusable < 3.1. These are the G.107 satisfaction bands.
- **Jitter caps (added after the netns tests):** the E-model sees jitter only as added delay, so 30 ms of in-burst jitter on a short path still scores Good. After the MOS band, a minute's band is capped by its jitter, following Microsoft's Teams network targets (jitter < 30 ms): ≥ 30 ms → at most Fair, ≥ 50 ms → at most Poor, ≥ 100 ms → Unusable. A cap never improves a band, and the factor is then "jitter". `udp_degraded` uses the same 30 ms (`callq.JitterFairMs`).
- **Validation:** table tests at the G.107 reference points (R 93.2 → 4.41, R 80 → 4.02, R 70 → 3.60, R 60 → 3.10).
- **Scoring window:** each 1-minute bucket is scored separately. Views show the **worst minute** and p95 RTT, not a 5-minute mean, because a mean hides a 30-second stall.

## Storage (architecture and test reviews)
- **No blob format change.** A burst writes three sibling series with the same slot:
  - `udp`: median RTT; lost if every packet in the burst was lost;
  - `udp_jitter`: in-burst jitter in the RTT field; gap if fewer than 2 replies;
  - `udp_loss`: lost packets out of 5, as a small number in the RTT field.

  `summary_1h` then gives mean, max and p95 of jitter and loss automatically, so there is no 48-hour cutoff. All three are new `ProbeKind`s (5, 6, 7) that are only ever written together.
- **Store and query changes:**
  - The store `Interval` gets an explicit case for udp kinds (5 s and 30 s).
  - `/api/calls` uses one `Store.Panel` call over the udp keys with 60 s buckets. No per-sample arrays and no 3 s step are needed.
- **Disk:** measured and added to the README cost table. Estimate: about +15% on `default`.

## Code changes the plan missed (architecture review)
- `model.Target.Tags` gets plumbed through, so the app mapping works on `default` too:
  - `toTarget` copies the tags;
  - `default.yml`'s `fileTarget` accepts `tags`;
  - tags are exposed in `/api/profile`.
- Validation and intervals:
  - `Validate` exempts catalog and default udp targets from the 5 s minimum.
  - `toTarget` sets the udp interval.
  - Custom `--config` UDP targets need **≥ 6 s**. There is **no opt-in** for faster probing.
- Verdict and other kind lists:
  - The verdict engine leaves udp out of its `[https,tcp,icmp]` service aggregation, and a test proves it.
  - The new verdict kinds are added everywhere kinds are listed: metrics, `report/text.go`, `app.js`, CSS themes.
- The CNAME region comes from a separate `LookupCNAME` in `resolveLoop`, exposed through a new runner→web state hook.
- `Caps.UDP` is dropped. Opening a socket always succeeds, so it proves nothing.
- **Windows:** Go suppresses ICMP port-unreachable on UDP sockets, so a closed port shows up as a timeout there. Refused-reason tests are Linux/macOS only, behind a build tag.

## Verdicts (network and test reviews)
- **Per-target UDP loss never enters the main verdict.**
- **New `udp_blocked` verdict** (low priority). It fires only when all of these hold:
  - targets from ≥ 2 independent providers have each failed ≥ 10 attempts;
  - TCP and the network path are healthy;
  - the run is past warm-up, after the first UDP success, or after 2 minutes.

  It splits into **"STUN ports blocked"** (port 53 works) and **"UDP blocked"** (port 53 fails too). The wording allows for a firewall on the user's own computer.

  After 6 hours it is demoted to a quiet info state, because hotel and corporate networks block UDP permanently. Calls rows then say "Blocked", not "Unusable".
- **New `udp_degraded` verdict** (low priority): ≥ 2 udp-path providers are Poor for ≥ 3 minutes while TCP is healthy.

## Calls view (UX review)
- **Navigation:**
  - **Calls | Overview | Charts.**
  - Calls is the default view for the `default` and `calls` profiles. This is a per-profile default, not onboarding.
  - Overview stays the default for big profiles.
- **Top card:** one answer, e.g. "● Good for calls · 32 ms · 0% loss · steady".
  - It is computed as the worst of: the verdict layer that affects UDP, `udp_blocked`/`udp_degraded`, and the call-path score.
  - It can never say Good while the banner says ISP outage, and it shows the banner's cause inline.
  - It refreshes every 10–15 s and shows "updated 4 s ago". This is the pre-call check.
- **Working-hours strip:**
  - "Today 09–17: Good 92% · Poor 14:10–14:16".
  - Working hours are Mon–Fri 09–17 in local time. Editing them comes later.
- **"What happened at …?":**
  - Pick a time (default: last 60 min) and get one sentence naming the layer at fault, e.g. "14:10–14:16: 8% loss at your ISP edge; Wi-Fi/router fine".
  - It links to the chart and to "Create evidence report".
  - It reuses the existing incident and blame logic.
- **By app:**
  - Teams and Meet show a state word, RTT and loss. MOS and jitter appear only when the row is expanded.
  - Zoom, Slack, Discord, WhatsApp and Webex share one row: "These apps pick a server per meeting, so they can't be tested ahead of a call. If your call path is Good they usually are too. Websites reachable, 24 ms."
- **Labels:**
  - "Unknown" becomes "Not enough data yet".
  - When a STUN server doesn't answer, the label says "This test server isn't answering, not your connection".
- **"Mark a bad call":**
  - One click saves a **private** note immediately, at now − 2 min, with the stats attached (MOS, RTT, jitter, loss only).
  - A "When?" picker offers now, 15 min ago and 30 min ago.
  - The evidence report is an offered link, not a required step.
- **Phone:** single column, two-line app cards, and a sticky Mark button.
- **Public link:**
  - Shows the top card, the working-hours strip and public notes.
  - Hides the Teams relay region and per-target names.
  - The Mark button is hidden there; the link stays GET-only.

## Privacy and abuse (security review)
- **Public IP (XOR-MAPPED-ADDRESS / MAPPED-ADDRESS):**
  - The parser skips these attributes without decoding them.
  - Raw datagrams are never kept.
  - A test serves a known mapped IP and checks it appears in no sample, `/api/status`, `/api/calls`, `/metrics`, report or log output.
- **Errors:** only the Reason enum is stored or exposed. UDP `err.Error()` strings, which contain addresses, are never logged.
- **Parser hardening:**
  - fixed 548-byte buffer;
  - len ≥ 20;
  - header length ≤ bytes read and a multiple of 4;
  - every attribute bounds-checked, including padding;
  - type and transaction-ID checks;
  - keep reading until the deadline on a mismatch;
  - plus `FuzzParseSTUN`.
- **Custom UDP targets:**
  - at most 32;
  - no private or loopback addresses unless `AllowPrivateIPs`;
  - port ≠ 0;
  - ≥ 6 s interval.
- **Docs:** README and SECURITY say that STUN providers see the user's IP, the same as the TCP targets do.

## Off switch and back-out (test review)
- `--no-udp` drops every udp target and hides Calls. It is tested.
- Back-out never requires a downgrade.

## Tests
- **Unit:**
  - STUN codec and fuzz;
  - fake STUN server: bursts, partial burst loss, transaction-ID mismatch, a spoofed source being ignored;
  - jitter skipped across an IP change;
  - MOS at G.107 points and at the band edges;
  - Calls API states, including "not enough data" vs "server not answering" vs "blocked";
  - `--no-udp`;
  - tags plumbing;
  - Validate exemptions and custom-target limits;
  - a test that the mapped IP never leaks;
  - verdict: udp never affects service loss; `udp_blocked` needs ≥ 2 providers; `udp_blocked` doesn't fire during warm-up; it clears within 2 minutes of recovery; one dead STUN server raises no verdict;
  - opening a v0.4.x database with the new kinds;
  - metrics golden files: `ping_*` byte-identical, plus a new golden for `fyisp_udp_*` and `fyisp_call_mos`.
- **netns harness:**
  - The STUN responder runs inside the existing responder re-exec, on a new 203.0.113.x address.
  - Faults are **UDP-only**, using a prio qdisc plus a `tc filter` on the STUN port, and the nft rules spare DNS port 53.
  - Scenarios:
    - **i-udp-block**: `udp_blocked`, with TCP ok.
    - **i2-stun-ports-block**: STUN ports blocked, UDP 53 fine.
    - **j-udp-jitter**: 30 ms jitter, band drops to Fair or Poor.
    - **k-udp-loss**: 10% loss; assert 5–16%.
  - Every scenario asserts that TCP verdicts stay ok.
- **CI:** real-socket tests against a local responder on Linux, macOS and Windows.
- **Live:**
  - 10-minute runs of `default` and `calls`;
  - light and dark screenshots on desktop and phone;
  - a real Meet or Teams call compared against `chrome://webrtc-internals`, expecting the same band.

## Effort
About 14 focused days, from the architecture estimate plus burst storage.

**Riskiest areas:**
- Windows UDP behaviour.
- The jitter storage path.
- The netns UDP-only shaping.

**Delivery:** one agent team, each piece in its own worktree, built in this order: probe/store → catalog/profile → verdict → API → UI → tests → docs.

## Before release
Check the terms of Google STUN, Cloudflare, Twilio and, if it ships, Microsoft's Teams relays. Drop any target whose terms forbid this use.

## Owner decisions (2026-10-10)
1. **Teams relay: opt-in only.** It is in the `calls` profile at 5 s and not in `default`. The README explains how to add it (`--profile default,calls`); the UI no longer shows the flag (v0.6).
2. **Bursts:** each sample is 5 packets 20 ms apart, stored as three sibling series (`udp`, `udp_jitter`, `udp_loss`).
