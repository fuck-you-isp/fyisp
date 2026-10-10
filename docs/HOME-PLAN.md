# fyisp v0.6: Home first (plan v2)

**Released as v0.6.0 (owner decision: pre-1.0, not a major).**

**What the owner asked for (2026-10-10):**
- The default launch is for a home user: a remote employee who wants the basics, especially audio and video calls, to work.
- Every other profile is off by default.
- The default view is friendly to a non-technical person, and the detail lives elsewhere.
- No persona wizard.
- **Greenfield major release:** no backwards compatibility, no migration and no upgrade notices.

**Sources:** five reviews in this folder (ux-research, ux-product, ux-design, ux-eng, ux-content), plus one review round on v1 of this plan. The decisions that settled disagreements are marked ⚖.

## 1. Principles
1. **Answer first.** The page opens with one sentence: "Is my internet OK for calls and work right now?", followed by who is at fault, in plain words.
2. **Measure only what the home user relies on.** False alarms are mostly a target-list problem. In v0.5, 80 of the 91 default targets were cloud regions, which is why a home user's first screen said "Only AWS-me-south-1 looks affected".
3. **Two levels, Home and Details.** Details is today's UI, kept for technical users. There is no third tier and no wizard.
4. **No jargon, units or CLI flags on Home.** ms, MOS, jitter, STUN, UDP, anycast, p95 and `--flags` belong in Details.
5. **Honest blame.** The confidence of the wording matches the evidence everywhere: the answer card, the timeline summary and the report. fyisp never clears someone as a fact, and it says so when the problem is in the home.
6. **Accessible by default.** WCAG 2.2 AA from the first PR.

## 2. What plain `fyisp` measures (the new `default`)
The old 91-target `default.yml` is **deleted**, along with `testdata/network_exporter.yml.template` and its test. The cloud sets remain available as catalog profiles (`clouds`, `aws`, `gcp`, `devclouds`, …).

| Group | Home tile | Targets (exact catalog names) |
|---|---|---|
| **Network path** (built in, unchanged) | the path chain in the answer card | Gateway, ISP edge, Cloudflare DNS 1.1.1.1, Google DNS 8.8.8.8, Quad9 DNS 9.9.9.9 (ICMP; feeds `ping_*`, which stays network_exporter-compatible and unchanged) |
| **Call path** (UDP STUN) | **Calls** | Meet-STUN, Cloudflare-STUN, Cloudflare-UDP53, Twilio-STUN-global |
| **Call & chat apps** | **Calls** | Microsoft-Teams (trace), Zoom, Google-Meet, Slack, Webex, Discord, WhatsApp-Chat |
| **Work apps** | **Work apps** | M365-Outlook, M365-Portal, M365-Login *(new catalog entry: login.microsoftonline.com, TCP connect only, no sign-in, with notes saying why)*, Google-Gmail, Google-Docs, Google-Drive |
| **Websites** | **Websites** | CDN-Cloudflare (trace), CDN-Akamai, CDN-Fastly, CDN-CloudFront, CDN-Google |

- **Size:** 22 targets (18 TCP, 4 UDP) plus 5 path targets, with 2 always-on traces (down from 8).
- **Measured footprint:** the 26-target draft, including streaming, used **0.77% CPU, 70 MB and 4.3 GB/month**, against 2.45% CPU, 76 MB and 9.9 GB/month for the v0.5 default. Without streaming it will be lower. The README row gets re-measured with the 17-minute method, disk included.
- ⚖ **Streaming is out of the default.** Netflix and Disney+ lookups would appear in an employer's DNS logs every minute, the same reason social sites are excluded. It's available with `--profile streaming`.
- **The old DNS group is dropped.** It duplicated the path's TCP probes, and the `dns` verdict still covers DNS failures.
- **Teams-Relay stays opt-in** (owner decision). Teams gets a "By app" row from its TCP target plus the shared call path: "Teams: reachable. Calls use your call path (Good)". This is **new code**: calls.go:461 drops Teams when the relay is missing. It needs a test. The `--profile default,calls` hint is removed from the UI.
- **Categories come from tags.** A fixed `home:calls|work|web` vocabulary is added to the catalog entries above.
  - `profile.Validate` rejects unknown `home:*` values.
  - The "exactly one category" test applies to the catalog and the default only.
  - `--config` and other profiles fall back in this order: `app:*`/`udp-path` → calls; productivity provider → work; CDN/streaming → web; anything else → **Other services**.
- ⚖ **The `label:` field is cut.** Home shows no target names, so nothing would use it.
- **`FYISP_PROFILE` env var** for NAS and compose templates; the flag wins.
- **Dead code removed:**
  - the `legacy` merge (named.go:364/597);
  - the `extends: [default]` special case (profile.go:247);
  - `shippedUDP`'s default.yml source;
  - the app.js:4055 `callsDefault` name check.

  `DefaultDescription` and the profiles.yml header are rewritten.

## 3. Home: the default view for every profile

```
┌──────────────────────────────────────────────────────────────────┐
│ fyisp                                        [Home | Details]  ◐ │
├──────────────────────────────────────────────────────────────────┤
│ ▌✓ Your internet is fine for calls and work                      │
│ ▌  ✓ Your Wi-Fi/router → ✓ Your internet provider → ✓ Internet   │
│ ▌  Checked a few seconds ago               [What does this mean?]│
│ ▌  ▸ What you can try          (only when not OK)                │
├──────────────────────────────────────────────────────────────────┤
│ [ Mark a bad call ]  (saves "now") · Was it earlier? ▸           │
├─────────────────────┬─────────────────────┬──────────────────────┤
│ ✓ Calls             │ ✓ Work apps         │ ✓ Websites           │
│   Good              │   Working           │   Working            │
├──────────────────────────────────────────────────────────────────┤
│ This week: 2 rough patches, both most likely your internet       │
│ provider (Tue 14:05–14:20, Thu 09:40–09:52).                     │
│ Today  [██████████████▒▒████████░░░░░░░░░░]   (Today | Week)     │
│ ░ fyisp wasn't running · ▒ trouble          How to keep it running│
├──────────────────────────────────────────────────────────────────┤
│ What happened at…?  (A few minutes ago)(An hour ago)(Pick a time)│
│ [ Make a report ]                                                │
└──────────────────────────────────────────────────────────────────┘
```

### Answer card
- It **merges** today's verdict banner and the "Good for calls" card into one card with one state.
- ⚖ **Path labels everywhere:** "Your Wi-Fi/router → Your internet provider → Internet", with "Apps" added as a fourth link only when a service is at fault. The same words are used in the card, the timeline, the report and the public view.
- **State inputs:** the worst of the path verdicts (lan/isp/upstream), the UDP verdicts, `dns`, `no_network`, the call score and the tile states.
- **Relevance rule:** a `service` verdict on a single target doesn't change the headline. It shows on its tile in words, e.g. "Zoom isn't answering. Other call apps are fine." A pattern across services is already escalated by the verdict engine to isp/upstream, and that does change the headline.
- **Copy:** the content-review tables, with confidence tiers ("most likely…", "slow to answer our checks"). There is one fact, stated as an outcome: "Calls would have broken up for about 6 minutes". No packets, no ms and no MOS.
- **Warm-up:** "Checking your connection. First results in about 2 minutes." The card never says "Good" while warming up.
- **Error and empty states:**
  - **Can't reach fyisp:** grey, not red. Keep the last state and say "as of 14:05".
  - **Days before the first run:** "No record yet", not hatched.
  - **Timeline load failure:** offer Retry.
  - **Public link off or expired:** a plain "This report isn't available" page.

### Tiles
- **The three tiles:** Calls (call path plus call & chat apps), Work apps and Websites.
  - A fourth tile, **Other services (N)**, appears only when the profile has uncategorised targets. Under `--profile clouds` it's the main tile.
  - Tiles with no targets are hidden.
- **Words:**
  - The Calls tile uses the call vocabulary: Good, Some glitches, Choppy, Breaking up, Blocked, Checking.
  - Other tiles use Working, Slow, Not working, Checking, and name the failing app when exactly one fails: "Zoom isn't answering".
- ⚖ **Other services counts.** It never says "AWS-me-south-1", but its state is real. "3 of 1,330 not answering" is shown in words on the tile. When most are failing, the verdict engine's upstream or isp verdict drives the headline. So a `--profile clouds` user never sees all-green while their chosen services are down, and a single-region outage doesn't raise an alarm.
- **"Slow"** needs a baseline (about 24 h of data) before it can show.
- **Clicking a tile** deep-links to Details: the Overview filtered by a new `ot=<category>` hash parameter.

### Timeline
- **Week summary sentence on every device,** built with the same confidence wording as the card.
- ⚖ **Strips default to Today on every device.** "Week" is a toggle stored in the hash (`hw=w`).
- **Time when fyisp wasn't running** is hatched and stated in words, e.g. "fyisp wasn't running for most of last week". It links to a plain one-page "How to keep fyisp running" guide and is never counted as bad.
- **Clicking or keyboard-selecting a block** fills in "What happened at…?".
- **Data sources** (backend constraint: per-minute call scores are capped at 48 h):
  - **Today:** per-minute call states.
  - **Week:** stored incidents, plus the not-measured gaps in the hourly summaries.

### Actions
- ⚖ **One time picker.**
  - "Mark a bad call" saves "now" immediately, with Undo, and shows the answer for that minute inline. It says honestly when the connection looked fine ("probably the app or the other person's connection").
  - "Was it earlier?" opens the same presets as "What happened at…?".
  - On phones the button is sticky.
- ⚖ **"Make a report" is the single proof flow.** The report is the public link: a redacted 7-day summary for a support agent or the employer's IT. It warns honestly when most problems were in the home.
  - The live share-link controls move to Details.
  - "Copy a message for support chat" is cut.
- **Write actions under Docker:** the README's Docker command currently loses Mark and Notes. On first start without `--admin-token`, fyisp generates a token and prints a one-time "open this link" URL in the logs, like Jupyter. Without it, Home explains in plain words and never prints flags. *Done (security-reviewed): the token lives in `<data-dir>/admin-token`, the link is `#admin=<token>`, and `/api/status` reports `controls` and `admin` (this browser has the session).*

### Header
- "fyisp" plus a Home | Details switch (`aria-current`) and a theme icon.
- No subtitle; it would repeat the card.
- The profanity stays in the repo and README, not in the UI or the report.
- A dynamic `<title>`: "Fine · fyisp" / "Trouble · fyisp".

## 4. Details
- **Contents:** today's UI (verdict banner, outage log, notes, reports, Calls/Overview/Charts, Network path, Investigate, CSV, ranges, kinds, Normal band and the share controls) moves under Details.
- **Navigation:**
  - Home→Details uses `pushState`, so Back returns to Home.
  - New hash parameters: `v=h`, `hw`, `ot`.
  - Old `v=` values keep working.
- **Copy fixes:**
  - The contradictions go, e.g. "none losing packets" next to a target at 100% loss.
  - Glossary tooltips cover MOS, jitter, loss, RTT, STUN, UDP, anycast, ISP edge and p95.
  - The footer help moves into a `<details>` element.
- **Loading:** charts load only when Details is opened.
- ⚖ **The "What fyisp measures" command panel is cut.** The README covers profiles.

## 5. Public link = the report
It is built from the Home component in a **third-person voice** (one flag on `/api/home`) and contains:
- a neutral title, "Internet quality report";
- the timezone and absolute times;
- counts of provider-side vs home-side problems;
- a problem table that includes home-side incidents.

Details are one click away. Redaction is unchanged: no note text, hosts, relay region or private evidence.

## 6. Backend
- **`/api/home`** in `internal/web/home.go`, on the local and public routes, served in two parts:
  - `?part=live`: state, blame, path, tiles and the headline/why/try copy, **cached for 10 s or less** (the same as PublicCacheTTL);
  - `?part=history`: today and week blocks (incidents, bad-call minutes, not-running spans), **cached for 60 s**.

  Other rules:
  - `tz` is rounded to 15-minute steps within ±14 h, which bounds the number of cache keys on the public link.
  - The copy is generated in Go and golden-tested. The API returns **timestamps, not formatted times**; the client formats them in the user's 12- or 24-hour locale.
  - Refactors: `s.calls()` is extracted from `serveCalls`, and `overviewRows()` from `overview.go`. The `BLAME` map moves from app.js into Go.
  - A `voice=public` flag produces the third-person report copy.
- **Verdict-engine test on the new 22-target default:** `service` vs `upstream` (≥3 bad, ≥50%, more than one group), plus the "Other services" escalation under `clouds`.
- **netns harness:** the existing scenarios also assert the `/api/home` blame (lan / isp / upstream / blocked).
- **README:**
  - a new pitch and quick start, with the profile table led by `default`;
  - the "v0.1 in development" line deleted;
  - the "Which apps are measured" Teams row and the STUN table's Profile column updated;
  - a privacy note that names streaming and social sites as DNS-log exposure, with a pointer to `--profile streaming`;
  - the re-measured footprint row;
  - a short "Keep it running" page in plain words (macOS, Windows, Linux), linked from Home.

## 7. Accessibility (built into PR 4, not bolted on later)
- **Live regions:**
  - no `aria-live` on frequently re-rendered nodes;
  - one `role=status` announcer, written only when the state changes;
  - an assertive region for offline or unusable states.
- **Order and focus:**
  - DOM order matches visual order, so the CSS `order:-1` hack goes.
  - A visible focus ring for `summary` and `[role=button]`.
  - `scroll-padding` for the sticky header and the floating button.
- **Never colour alone:** icon plus word everywhere, and a pattern or height on the timeline. The state palette is ordered by lightness and checked for deuteranopia and protanopia.
- **Contrast:**
  - **Text:** at least 4.5:1, with `--accent-fill: #2b70cc` (4.89:1). Text on Home is at least 12 px.
  - **Non-text graphics** (strip fills, "no data", icons): at least 3:1.
- **Timeline:** keyboard operable (arrow keys across blocks), and each block has a name such as "Tue 14:00, choppy, most likely your internet provider".
- **Touch targets:** at least 24 px on Home controls and 44 px on primary actions.
- **Dialogs:** `<dialog>` replaces `prompt`/`confirm`, and the admin token goes in a password field.
- **Explanations:** a "What does this mean?" disclosure (`aria-expanded`) replaces the hover tooltips.
- **Label in name:** the `aria-label` override on the datetime input is removed.

## 8. Delivery
All PRs go into the `feat/home` integration branch. Estimated **16–20 days**:

| # | PR | Contents | Days |
|---|---|---|---|
| 1 | **Profile** | new default.yml (22 targets); M365-Login catalog entry; `home:*` tags plus validation; `FYISP_PROFILE`; delete the old set, its template and test, and the dead code; tests | 1.5 |
| 2 | **/api/home** | live and history parts; `calls()`/`overviewRows()` refactors; blame in Go; copy tables with confidence tiers and public voice; Teams web-only row and test; verdict test on the new default | 3–4 |
| 3 | **core.js split** | shared helpers out of app.js (4,086 lines); no behaviour change | 1 |
| 4 | **Home view** | home.js: answer card, tiles, timeline, actions, report flow, header switch, hash/pushState, deep links, error and empty states, **with the accessibility work above** | 5–6 |
| 5 | **Details and public report** | moved controls, copy fixes, glossary, third-person public view, neutral titles | 2–3 |
| 6 | **Tokens and Details accessibility** | palette/contrast tokens; Details focus, labels and dialogs | 1.5 |
| 7 | **README and footprint** | README rewrite, keep-running page, re-measured footprint; Docker first-run token (if approved) | 1.5 |

**Testing:**
- **fyisp-webdev** mirrors the new default. Its scenarios: good, warming up, Wi-Fi bad, ISP bad, wider internet bad, one app down, calls choppy, UDP blocked, no network, a week with not-running gaps, public report, `--profile clouds` with Other services.
- **Puppeteer matrix:** desktop and phone, light and dark, local and public. Scripted checks:
  - no sideways scroll;
  - the answer visible on a phone without scrolling;
  - 5 or fewer requests on the first Home load;
  - no target names or flags on Home;
  - no serious axe-core violations.
- **Hallway test:** 5 non-technical people, on desktop **and** phone. Pass mark: 4 out of 5 correct within 30 s on each task:
  1. "Was my 2 pm call bad because of my internet?"
  2. "Is it your Wi-Fi or your provider?"
  3. "Find last Tuesday's bad call."

**Release v1.0.0:** the notes say plainly that `default` changed, and how to get the cloud sets (`--profile clouds`, `aws`, …).

## 9. Not in v1.0
- **Possible later additions:**
  - an in-UI "Add services" (additions only, gated like share);
  - the Teams relay in the default via `world.tr.teams.microsoft.com` every 15 s (owner decision);
  - OneDrive/SharePoint, Google Accounts and FaceTime;
  - a VPN provider (never default);
  - a Wi-Fi hint from the interface type;
  - configurable working hours;
  - `fyisp install --user` autostart.
- **Cut from this release:**
  - per-target `label:`;
  - "Copy a message for support chat";
  - the "What fyisp measures" panel;
  - TTY log-level and banner changes;
  - the Docker `--version` fix (tracked separately).
