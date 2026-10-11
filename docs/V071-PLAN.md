# fyisp v0.7.1: plan (FINAL)

**Status:** FINAL, 2026-10-11. The execution team (cq-core, ux-eng, cq-ui) starts from this file.

**Who and where:**
- **Planning team:** ux-product (lead), ux-speed (UX), cq-core (backend).
- **Reviewed by:** ux-content, rev-security, go-review and net-isp (`v071-plan-review.md`). Every review **must** is folded in, plus the shoulds the lead accepted.
- **Branch:** `release/v0.7.1`; worktree `fyisp-wt/v071`, cut from main (v0.7.0, 42f8db9).
- **Kind:** a **patch release**: fixes and polish only. Nothing new to switch on, no new data source, **no schema change**. Every new field lives in the existing `Extra` JSON or in API output only.

**Labels:**
- **[M]** must; **[S]** should.
- **Owners:**
  - **cq-core:** `internal/speedtest` (except the queue), `internal/report`, `internal/verdict`, `cmd`, the Dockerfile;
  - **ux-eng:** `internal/speedtest/queue.go`;
  - **cq-ui:** `internal/web/static`.

---

## 0. What changed from the draft

1. **Decisions** (from the lead):
   - **Q2:** a public budget under 210 MB **turns share-link tests off with a warning**. It is never raised, and fyisp never refuses to start.
   - **Report time zone:** the owner's local IANA zone, printed with every time and date. This replaces "UTC only".
   - **PR order:** A3 → A1 → A2.
2. **A1:**
   - adds a hard receive-side **RST backstop** at MaxBytes + 5%;
   - **one live `fits()`** for admit, dequeue and every sweep step, including the running test's live charge;
   - `resets_at` = the latest of the byte fit, the time caps and the sweep gap;
   - the property test runs against a **non-compliant** fake server.
3. **A2 is rewritten:**
   - **Dropped:** the 0.5 s guard. It censored by speed (net-isp M1, go-review M1).
   - **New estimator:** a **smoothed 3-tick peak** with the window ending at the last byte's timestamp. It's a pure function with unit tests on synthetic tick series. Uploads use **acked** bytes.
   - **Still-rising results** are kept as **lower bounds** with censored scoring ("not slow").
   - **v0.7.0 capped rows** are skipped in the baseline, the by-hour stats and the finding.
   - **New acceptance case:** a 2 Gbit/s line with a 30% evening cut.
4. **A3** reads the TCP_INFO bytes in `close()` on **every** run (max of application and kernel counts, never their sum), not only on cancel.
5. **A4** counts **established** connections only.
6. **A5:**
   - fires +2 min on **turn-on or mode change only**, never on restart;
   - the shifted test is an extra run of slot *i*, then the grid resumes;
   - the idle baseline needs ≥ 20 samples;
   - the size is now 1 day.
7. **A6 and A7** pick up rev-security's and ux-content's conditions: `via` is local-only, the wording comes from S11, and the deferred traceroute targets the dialled IP, one at a time.
8. **Copy:** every ux-content must and should is in, with exact strings (B1–B10, C2, C3).
9. **Public origin:** rev-security asked that the share link stop telling tests apart, so `origin` and `trigger` are neutralised on the public listener (B5).
10. **C1** is now **ENV defaults plus a bare ENTRYPOINT**. `report` and `export` stay read-only.
11. **New B10:** the "≥" noise rule (ux-content M8, from Q4).

---

## 1. Scope: in

### A. Data and measurement

#### A3. [M] Honest byte counts on every run (cq-core) — *merge first*
**Problem.**
- Byte counts are taken at the application level.
- Downloads undercount by the receive buffers (measured at 1.08–1.16× the cap per direction).
- A cancel mid-data closes the streams before anything is counted.

**Fix.**
- In each stream's `close()`, on every run, read `TCP_INFO` **before** `SetLinger(0)` and `Close()`: `tcpi_bytes_received` for download, `tcpi_bytes_acked` for upload.
- Each stream's bytes are `max(appBytes, tcpBytes)`. Never add the two.
- `abort()` uses the same path, from the `AfterFunc` goroutine under `t.mu`.
- Non-Linux builds keep the application counts. The docs say so.

**Acceptance.**
1. Normal run against the fake server: the stored `bytes` ≥ the payload the fake sent minus at most 1 MB, and never the sum of the two counts.
2. A cancel mid-download stores `bytes` within 1 MB of what the fake sent.
3. A cancel during setup stores `bytes > 0` (a few hundred bytes of cookie and JSON).
4. `bytes_24h` and the public ledger use these counts.

#### A1. [M] The share-link budget is a hard limit (ux-eng: queue; cq-core: backstop) — *after A3*
**Backstop (cq-core, `iperf.go`).** In each direction, once the received bytes (the A3 kernel count where available) reach `MaxBytes × 1.05` (105 MB), close the data streams with an RST. This bounds a server that ignores TEST_END or keeps sending, so **one test is ≤ 210 MB** for any server.

**One fit check (ux-eng, `queue.go`).**
- `fits()` lives in `publicLocked`. It is the only place that decides:
  - admitting a share-link request (otherwise 429);
  - dequeuing a share-link entry in `Run`;
  - starting each step of a share-link sweep in `execute`.
- It passes only when **all** of these hold:
  - `limit − used − live ≥ 210 MB`, where `live` is the running public test's bytes charged so far (charged live, like test time since 679d64a);
  - the hour and day time caps leave room;
  - for sweeps, the 30-minute gap is over.
- A sweep that fails `fits()` before a step stops with `stopped_reason: "limit"`. The steps it completed stay.

**`resets_at`.** The **latest** of these three:
- when enough old usage ages out (oldest first) for 210 MB to fit;
- when the time caps free up;
- when the sweep gap ends (sweeps only).

`retry_after` agrees with it.

**Budget below the minimum (decision Q2).**
- **At start:** a `--speedtest-public-budget` under 210 MB **doesn't stop fyisp**. Share-link tests are treated as **off**, with one WARN log line: `share-link speed tests are off: the budget (150 MB) is below the 210 MB one test needs`.
- **The link:** it answers 403 `disabled` with `reason: "budget_too_small"`.
- **Details:** the share-link toggle shows "Off: the daily allowance (150 MB) is smaller than one test (about 210 MB). Raise it to at least 210 MB to allow tests from your share link."
- **Never** raised, and never a crash loop.

**Docs.** README, SECURITY.md and SPEEDTEST-API.md say: "a test from the share link starts only when a whole test (up to 210 MB) still fits in what's left".

**Acceptance (unit, fake clock).**
1. A 300 MB budget with 105 MB used: the next request gets 429 `budget_exhausted`, and `resets_at` = when 210 MB fits.
2. Three visitors are queued with 250 MB left: **only one runs**. The others are dropped at dequeue with `budget_exhausted`.
3. A sweep with 250 MB left runs one step, then stops with `limit`.
4. Byte fit and time cap disagree, so `resets_at` is the later of the two. A retry at `resets_at` is admitted.
5. The ledger stays ≤ the limit across 1,000 randomised sequences, run **against a fake server that ignores TEST_END**.
6. Budget 150 MB: fyisp starts, logs the WARN, the link returns 403 `disabled`/`budget_too_small`, and Details shows the sentence above.

#### A2. [M] Fast lines: one steady-rate estimator, lower bounds kept honest (cq-core) — *after A1*
**Estimator.** It applies to every capped direction, and is optional for uncapped ones.
- **Ticks:** take the 100 ms ticks, smoothed by a 3-tick moving average. Download ticks come from received bytes; **upload ticks from per-tick `tcpi_bytes_acked`** (Linux), never written bytes.
- **Peak:** the highest smoothed tick.
- **Window:** from the first smoothed tick ≥ 90% of the peak, to the **actual timestamp of the last byte** (not a tick boundary).
- **Rate:** bytes in the window ÷ the window's duration.
- **Stored** in `Extra`:
  - `rate_method: "peak90"`, or `"whole"` as the fallback;
  - `win_ms`;
  - `lower_bound`.

**Rules.**
- **Fewer than 3 ticks in the window:** the result is a **lower bound** (`lower_bound: true`).
- **Still rising at the cap** (the smoothed peak is in the last 2 ticks, so no plateau was seen): the result is a **lower bound**.
- **No tick qualifies:** use the whole run, with `rate_method: "whole"`.
- **Non-Linux uploads:** the v0.7.0 method, with `rate_method: "whole"`.

**Scoring lower bounds (censored).**
- If `x / baseline ≥ 1`, the result counts as a full-speed sample (score 1.0) in the by-hour stats and the finding. Otherwise it is left out.
- A lower bound **never** enters `Baseline()`, and is never shown or counted as slow.

**v0.7.0 rows.**
- A row with `down_short` (or `up_short`) and no `rate_method` in `Extra`: that direction is **skipped** in `Baseline()`, `HourMedians` and `FindFinding`.
- It is still shown and exported. Nothing is rewritten.

**Far regions on fast lines.** When most of a region's tests are lower bounds, its row and the report say: "**Too fast to measure from this far within the 100 MB limit.**" It is not shown as "learning".

**Acceptance.**
1. **Pure-function unit tests** of the estimator on synthetic tick series:
   - slow start then plateau: the rate is the plateau within 3%;
   - a single high spike tick: ignored by the smoothing;
   - still rising at the end: `lower_bound`;
   - 1-tick and 2-tick runs: `lower_bound`;
   - no qualifying tick: `"whole"`;
   - a deep upload send buffer, with written ≫ acked in the first ticks: uses acked.
2. **One coarse timing test** with the fake server. No other timing-sensitive asserts, to avoid flakes under `-race`.
3. **Under the cap** (100 Mbit/s): by-hour, finding and baseline outputs are byte-identical to v0.7.0 (goldens).
4. **A 2 Gbit/s line with a 30% evening cut** (fixture plus a netns or fake-server case):
   - the evening pattern is detected;
   - the baseline comes from daytime tests;
   - far servers are mostly lower bounds and never read as slow.
5. **`finding_test`** on a 1 Gbit/s line where near regions dip and far regions are lower bounds: the case is `isp`.
6. **A mixed fixture** of v0.7.0 capped rows plus new rows: the old capped directions are absent from `Baseline()`, the medians and the finding; the other old rows still count.

**UI and copy for A2** (cq-ui, with ux-content S10 adapted to lower bounds):
- **"% of normal" cell:**
  - for a lower bound at or above normal: "**at least normal**";
  - for one below it: "**not counted**" (muted, never an error colour);
  - with the ▸ More sentence as the cell's `title` and screen-reader text.
- **▸ More:** "**Still speeding up when it reached its 100 MB limit, so the speed was at least this. It isn't used for this server's normal, and it only counts toward the time-of-day pattern when it already matches the normal.**"
- **"Problems only":** never includes these rows.

#### A4. [S] Prove "at most 9 connections" (cq-core; ux-product for the script)
- **Fake server:** counts connections **accepted and not yet closed** (its own close tracked) across download, relisten, a busy retry and upload. Assert ≤ `Streams + 1`.
- **Acceptance script:** `ss -tn state established dst <ip>` only (TIME-WAIT and FIN-WAIT are legitimate). A 10th **established** connection is the bug.

#### A5. [S] The first test about 2 minutes after turning on (cq-core, 1 day)
**When.** Only on an **explicit turn-on or mode change**, never on a restart. A restart resumes the install's phase grid. (net-isp's "or no scheduled test in the last interval" is not taken; the lead decided "never on restart".)

**How.**
- The +2 min test is an **extra run of the next grid slot *i***.
- After it, the grid continues from slot *i+1*, skipping any grid slot less than `spacing/2` from it. So no two tests are ever closer than `spacing/2`.
- Rotation is chosen by the slot's nominal time, so it is unaffected.

**Idle baseline.**
- The idle median needs **≥ 20** idle ISP-edge (and gateway) samples in the 2 minutes. Otherwise the idle is stored as missing, never as a 3-sample median.
- The first 2 minutes after process start are never "idle" (path discovery and the first traceroutes run then).

**Acceptance.**
1. Turn-on at a random phase: `next_at` = now + 120 s ± 1 s.
2. After the extra test, the next tests follow the grid; none is closer than `spacing/2`.
3. A restart, or a turn-on while already on: no off-grid test.
4. A test right after a process start stores the idle as missing, and its under-load text says so.
5. In webdev, `api/speed.next_at` matches the first test's start within 2 s.

#### A6. [S] Look through internet exchanges in the handoff (cq-core)
**List.**
- A constant list of about 15 IX route-server and LAN ASNs: DE-CIX 6695; AMS-IX 1200; LINX 5459 and 8714; France-IX 51706; NL-ix 34307; Netnod 8674; SIX 33108; JPNAP 7527; HKIX 4635; IX.br 26162; …
- Each entry is **verified against PeeringDB by hand** before committing; the source is noted in a comment.
- No runtime fetch, no rDNS.

**Lookup.**
- `handoffOf` treats hops in these ASNs like ASN 0: it skips them, and the peer is the next hop with an ASN.
- The IX's name is returned as `via` in the API **only**, inside the existing `if !n.public` block. It isn't stored.
- There's no schema change. The handoff is re-traced per server daily, so `via` fills within a day.

**Finding wording** (local; ux-content S11):
- **Peer known:**
  - "the link between your internet provider and **Leaseweb**, where they meet at **DE-CIX (an internet exchange)**";
  - for a transit peer: "…and **Lumen (AS3356)**, meeting at DE-CIX (an internet exchange)".
- **Peer unknown:** "…the route through **DE-CIX (an internet exchange)** to these servers". It never names the exchange as the party at fault, and uses the tier's verb ("may be" when early).
- **Format:** everywhere in the UI, "{Name} (AS{n})", never "AS{n} {NAME}" in upper case.
- **Public voice:** no `via`, no IX name, no AS numbers. The finding names the peer and never the IX.

**Acceptance.**
1. A handoff test with the path ISP → DE-CIX LAN hop → Leaseweb gives ASN 60781 with `via: "DE-CIX"`.
2. A path that ends at the IX hop gives the peer null with `via: "DE-CIX"`, and the finding uses the "route through" wording.
3. `SchemaVersion()` is unchanged.
4. The public golden grep for `"via"`, `DE-CIX` and `AS[0-9]` in `text_public` returns 0.

#### A7. [S] The daily handoff traceroute doesn't sit between a test and its save (cq-core)
**Fix.** Save first. Then run the traceroute (when due) and update that row's handoff fields.

**Conditions** (rev-security):
- It targets the **IP the test dialled** (`Result.IP`, already checked by `Server.check`) and never re-resolves the name.
- **At most one** deferred traceroute at a time.
- It is cancelled on shutdown.
- The late update never fails loudly and never resurrects a row that retention has pruned (update-if-exists).

**Acceptance.**
1. With a 5 s fake traceroute, the row is saved and the next queue entry starts within 1 s of the test's end.
2. The handoff fields are filled once the traceroute finishes.
3. Two quick tests start only one traceroute at a time.
4. A shutdown during a traceroute exits cleanly.

---

### B. Speed UI, report and copy (exact strings)

#### B1. [M] Confirmation before "Test every server" (cq-ui)
**Element.**
- A `<dialog>` opened from the button.
- `aria-labelledby` points to the title and **`aria-describedby` to the body** (ux-content M1).
- Focus starts on **Cancel**. Esc means Cancel.
- DOM order is [Cancel] [Test {N} servers], with the confirm button primary-styled.
- The numbers are **not** a live region.

**Title.** "Test every server now?"

**Body (owner).**
- "One test on each of the {N} servers, one after another." N excludes skipped servers.
  - With skipped servers, add "{k} servers you skipped won't be tested." ("1 server you skipped won't be tested.")
- "Takes about {M} minutes and uses about {G} GB of data."
  - M is whole minutes, at least 1, plural-correct.
  - G is this line's estimate when known, else **N × 200 MB** (the cap, about 9 GB for 45 servers), never a hard-coded 8.
- "Your line is busy most of that time, so calls may stutter."
- "The results are a snapshot. They don't change the hourly pattern."
- With speed tests off: "Speed tests are off. This runs once and doesn't turn them on."

**Body (share link, M3, rev-security 4).**
- The first line is as for the owner, with no skipped-servers line.
- "The owner's line is busy most of that time, so their calls may stutter."
- "This uses the owner's shared allowance: {used} of {limit} used in the last 24 hours."
  - The numbers come from `part=queue` budget fields, never computed in the client.
  - Use the same unit on both sides (GB to one decimal, e.g. "2.1 GB of 10 GB").
- When `limit − used` < G: "**About {left} is left, so the test will stop partway (after about {s} of {N} servers).**"
- "If the owner needs the line, your sweep pauses between servers and carries on afterwards."
- "The results are a snapshot. They don't change the hourly pattern."

**Buttons.** [Cancel] [Test {N} servers]. With N = 1, the button reads "Test 1 server".

**Focus after closing (M2).**
- After confirming, focus moves to the queue heading `#sp-q-h` (it has `tabindex=-1`).
- After Cancel or Esc, focus returns to "Test every server".

**Disabled button (S2).**
- When the button is disabled (cooldown, budget, full queue), the dialog never opens.
- The reason stays as visible text next to the button, and the button's `aria-describedby` points at it.
- When the reason changes, it is written once to the existing `role=status`.
- No rate note in the dialog (Q3).

**Server checks stay authoritative.** The dialog is UX only.

**No dialog** for single-server Test now. The inline sweep text stays, but its "about 8 GB" uses the same G rule.

**Acceptance.**
1. Clicking sends **no** request until confirm. Cancel and Esc send nothing (checked in the network log).
2. Screen readers (NVDA or VoiceOver) read the title, then the body.
3. `document.activeElement` is never `<body>` after confirm or cancel.
4. The share-link dialog shows live allowance numbers and the "stop partway" line when they apply.
5. axe reports no serious issues; it works keyboard-only; there is no sideways scroll at 390 px; light and dark both pass.

#### B2. [M] Report "Speed through the day": facts that agree, in the owner's time zone (cq-core: Go; cq-ui: speedreport.js)
**Time zone (decision 4).**
- **Which zone:** the report (local and public) uses the **owner's IANA zone**:
  - the zone the owner's browser last sent on a local Home, Details or Speed load (already sent as `tz`), stored in settings;
  - else the server's `TZ` / `time.Local`;
  - else UTC.
- **Where it shows:** every time and date prints the zone, e.g. "8–11 pm EDT (UTC−4)". A date range carries it once: "(Mon 6 Oct to Wed 8 Oct, times in EDT)".
- **What uses it:** day counts, the dip's hours and the part-of-day words. `FindFinding` gets `Loc`, and `PooledPattern` gets the zone, not `time.UTC`.
- **Privacy:** the share link reveals the owner's zone name. That's acceptable: the report's times of day already imply it. This note is for rev-security's awareness.

**Days.**
- `f.Days` = the number of **distinct days in that zone with at least one scheduled test**, within the window.
- The card and the evidence line use the same number.
- Pluralise "1 day", "1 test", "1 time", "1 provider" and "1 region".

**Evidence line** (Go, ux-content M5):
- "Based on {n} tests on {d} days between {first} and {last}; each {part} hour was tested at least {k} times across {p} providers."
- `{part}` is "evening", "night" and so on, in the owner's zone.

**When d < 7** (M6):
- If the first test came after the window started: "Speed tests started on {weekday, date}, so this covers {d} days, not 7." (**Owner voice only**; rev-security 5 keeps the start date out of the public voice.)
- Otherwise (gaps): "Tests ran on {d} of the last 7 days."

**Early tier** (net-isp S12): when the finding's tier is early, add "**Early: {d} days of tests. A week gives a clearer picture.**" Where the per-evening consistency count exists, also add "Slower on {x} of {y} evenings."

**The UI bullet** (S3) no longer prints its own count: "**Tests went to {r} regions and {p} hosting providers. {b} couldn't run, usually because the server was busy.**"

**Acceptance.**
1. webdev `-backfill 72h`: the card says "3 days" everywhere, and the string "7 days" doesn't appear in it.
2. Golden tests: test days 1, 2, 4 give "3 days between {day 1} and {day 4}".
3. A fixture in a non-UTC zone (America/New_York, an 8–11 pm EDT dip): the card and the finding say "evening" and "8–11 pm EDT (UTC−4)", never "night" or a UTC hour.
4. Both voices are checked.

#### B3. [M] Report: the slowest servers by name (cq-ui; cq-core for public names)
**Heading.** "Slowest servers from {hours}" (S4).

**Rows.**
- Up to 3 **servers** with ≥ 3 non-fallback scheduled tests in the dip hours.
- The label is the public `display`, taken from `speedNames(public=true)`. The owner's own servers appear as "Your server N" and never by host, label or IP.
- Line text (net-isp S10): "{display}: {pct}% of its usual result (median {x} vs usual {y} Mbit/s)".
- Each row has an `aria-label` with commas, e.g. "Southeast Asia, OVH, Singapore: 52% of its usual result, median 310 versus usual 600 megabits per second" (S4).

**Order** (net-isp S11):
- in the `isp` and `mild` cases: nearest region first;
- in the `route`, `provider` and `server` cases: slowest first.

**When none qualify:** the block is hidden, never empty.

**Leaks** (rev-security 5). The public grep covers:
- the API JSON;
- the public evidence text;
- the public report page `/s/<secret>/r/<id>`.

It looks for catalog hostnames, `"port"`, `"host"`, `--speedtest-server` hosts, custom labels, `custom-` local IDs, `"via"`, `DE-CIX` and `AS[0-9]`. The result must be **0**.

#### B4. [S] Report table: headers and screen readers (cq-ui)
- **Headers:** "Download from {region} (median)" and "% of normal (all {r} regions)".
- **Footnote** (S5 and net-isp S13): "**Download speeds are from the nearest region. Farther regions are usually slower because of distance, not your provider, so every region is compared by % of normal.**"
- **Sub-row:** the "└" is `aria-hidden`. The row header reads "{Region}, slower hours: 9 PM to 1 AM", with the region part visually hidden.
- **▼:** in an `aria-hidden` span; the word "slower" carries the meaning. The bare ▼ in the Download column is removed.
- **Acceptance:** a screen-reader spot check (NVDA or VoiceOver) never reads "box drawings…" or "black down-pointing triangle".

#### B5. [M] Chart legend for speed-test windows, and neutral origins on the share link (cq-ui; cq-core for the API)
**Legend.** It appears only when the chart's range contains a window.
- A muted grey tick: "**speed test running (left out of these numbers)**".
- On the **owner's view only**, when a share-link window is in range, an accent-blue **dashed** tick (a second cue, not colour alone; M7): "**speed test someone started from your share link**".
- Keep the "not measured" hatch for when fyisp wasn't running. Tick and hatch look different.

**API** (rev-security 3). On the public listener:
- `part=windows` `origin` is the neutral `"test"`.
- `speedTestJSON.trigger` is `"test"` (or omitted) for every test.

The live queue's "someone" stays as it is.

**Acceptance.**
1. The legend appears only with windows in range.
2. Each swatch is ≥ 3:1 against the chart background (light and dark), **and** the two entries differ by shape.
3. The share link never shows the blue item.
4. A public golden grep for `"public"`, `public_sweep` and `"manual"` in `public.json` returns 0.

#### B6. [S] The "first test" note (cq-ui; matches A5)
Wording by range:
- under 1 minute: "The first test starts in under a minute." This branch wins below 1, so the note never says "½ minute".
- otherwise: "The first test starts at about {clock} (in {n} minute|minutes)." Whole minutes only (S6).
- running: "The first test is running now."
- no `next_at`: "The first test starts within the hour."

The note is announced once via `role=status`. The header's "next test {clock}" uses the same clock format.

#### B7. [S] Cancelled tests (cq-ui; cq-core if a Go count includes them)
- **Result cell:** "Cancelled", or, when a direction finished, "Cancelled after download ({x} Mbit/s down)". It uses a neutral icon and colour, not ×/red.
- **▸ More** (S7):
  - cancelled by the owner: "Stopped by you at {time}.";
  - cancelled by the visitor who asked for it: "Stopped at {time} by the person who asked for it.";
  - unknown, or on the share link: "**Cancelled at {time}.**"
- **Sweep live list:** "not reached (test cancelled)".
- **Never a problem:** cancelled tests are left out of:
  - "Problems only";
  - the Busy and Failed counts;
  - the report's "couldn't run" count;
  - the failure-rate notice;
  - heatmap cells and patterns.

  Verify each; several already hold.

#### B8. [S] The share link's Speed tab asks for the summary once (cq-ui)
- `report.js` → `SpeedReport.load()` and `speed.js refresh()` send the same `api/speed?days=7&tz=…`.
- Fix: load the report's summary only when the report view is showing, or share one in-flight promise per URL.
- **Acceptance:** a HAR shows exactly one summary request per refresh.

#### B9. [S] Home warm-up line without the ↘ icon (cq-ui)
- `kind: "warmup"` gets no icon. The pattern line keeps ↘ or !.

#### B10. [S] "≥" without the noise (cq-ui; ux-content M8, Q4)
**Column rule.**
- When **every** shown row in a direction is capped: drop the per-cell "≥". The column header becomes "**Down (Mbit/s, at least)**" / "**Up (Mbit/s, at least)**".
- In mixed tables, keep "≥" per cell.

**One note above the table**, replacing today's line under it:
- all capped: "**Your line is faster than these tests can fully measure: each one stops after 100 MB in each direction. Treat the numbers as 'at least'.**"
- mixed: "**≥ means the test stopped at its 100 MB limit, so the speed was at least this.**"

**Screen readers.** The visible "≥" is `aria-hidden`, with sr-only "at least ".

**Report and share link.** Instead of per-row marks, one sentence in the method bullet: "On this line every test reached its 100 MB limit, so speeds are lower limits."

---

### C. CLI, copy and docs

#### C1. [S] Subcommands work in the Docker image (cq-core)
**Fix** (go-review S3, the lead's decision 7).
- **Image:** `ENV FYISP_DATA_DIR=/data FYISP_LISTEN=0.0.0.0:3000`, and `ENTRYPOINT ["/fyisp"]`.
- **fyisp:**
  - reads `FYISP_DATA_DIR` and `FYISP_LISTEN` as defaults; flags win;
  - `report`, `export` and `paths` honour `FYISP_DATA_DIR`;
  - `report` and `export` keep `OpenReadOnly` (no lock, no writes);
  - `paths` and `export` never print or include `admin-token`.
- Existing compose files with `command: [--speedtest]` and the README's `docker run … IMG --share` keep working.

**Acceptance.**
1. `docker run --rm IMG version` prints `fyisp v0.7.1`; `IMG profiles` lists profiles.
2. `docker run --rm -v vol:/data IMG report -o /dev/stdout` works next to a running container on the same volume, without taking its lock.
3. `IMG --speedtest` and `IMG --share` start the server as before.
4. Unit tests: env defaults, flags beating env, and an unknown word still errors.

#### C2. [S] Verdict copy never states health as fact (cq-core)
- `rules.go:330` and `:338`: "the router is fine" → "**the router looks fine**".
- `home_copy.go:354` tooltip: "If they're fine, the internet itself is fine." → "**If they answer normally, the wider internet is working.**"
- The outage log keeps old summaries word for word.
- **Acceptance:** `grep -rnE '\b(is|are) fine\b' internal/verdict internal/web/*.go` returns only the present-state headlines "Your internet is fine" and "This connection is fine". The verdict goldens are updated.

#### C3. [S] The dev server's demo verdicts are the engine's sentences (cq-core)
`cmd/fyisp-webdev/verdict.go` uses these (S9):

| Verdict | Text |
|---|---|
| ISP | "Your ISP's network is dropping packets: the router looks fine, the first hop past it loses 38%." |
| Service | "2 services look affected: Discord and Webex." |
| OK | "Everything looks fine." |
| Upstream | "Problems beyond your ISP: 3 of 3 public DNS anycast servers are unreachable." |
| DNS | "Name lookups are failing: 12 of 20 services can't be found, but the network itself looks fine." |
| No network | "This device has no working network connection." |
| LAN | "Your Wi-Fi or router is dropping 22% of packets." |

**Acceptance:** grep in `cmd/fyisp-webdev` for `fault`, `"is fine"`, `"are fine"` and `not yours` returns 0.

#### C4. [S] docs/CALL-QUALITY-PLAN.md: an outdated line (cq-core)
The line "The Calls view tells users how to add it (`--profile default,calls`)" becomes "The README explains how to add it (`--profile default,calls`); the UI no longer shows the flag (v0.6)."

#### C5. [M] The acceptance script (ux-product)
`v071-acceptance.md`, from `v07-acceptance.md`:
- **Fixes:**
  - `docker run --rm $RC version` (C1);
  - the admin header on `configurable`;
  - §2.5 runs on a writable copy;
  - the §8 grep excludes the corrected cooldown sentence;
  - §5.1 counts `ss -tn state established` (A4).
- **New checks:** A1 (the fit, the 3-visitor drop, a sub-minimum budget), A2 (lower-bound rows, the 2 Gbit stand-in), A5, A6 (`via` within a day, when a path crosses an exchange), B1–B3, B5 (the legend and the neutral public `origin`), B8, B10 and C1.

**Acceptance:** the script runs top to bottom on the RC without manual edits.

#### C6. [S] Phone screenshots at 390 px (cq-ui)
- Retake q-pub-*-phone and q-q-public-phone (they were taken at 1280).
- Check the v0.7.0 phone cards (the Every server and sweep tables, 9a38332) at 390 px, light and dark: no sideways scroll.

---

## 2. Deferred (not in v0.7.1)

| Item | Where | Why |
|---|---|---|
| PeeringDB-based IX and peering data (`ixpfx`/`netixlan`; "public peering / direct / transit") | v0.8 | It's a new embedded data source. **Gate:** the PeeringDB licence for redistribution inside a binary (CAIDA's data is non-commercial). A6 is the stopgap. |
| Monthly data limit (the PO's item) | v0.8 | It's a new setting with UI and API: a feature. The "used today / this month" figures and the capped-line warning already ship. |
| Raising the 100 MB per-direction cap | v0.8, with real data | It's a product lever: about 2.5× the data on fast lines, and it changes the data promise users accepted. A2 handles fast lines within the current cap. |
| A storable `via_ix` column (schema v6) | v0.8 | v0.7.1 keeps `via` in memory and the API only. |
| Firing +2 min after a restart when no test ran for a full interval (net-isp S6, alternative) | Not planned | The lead decided "never on restart". |
| IPv6 tests, CGNAT detection, modem stats, an M-Lab control, a remote server list | v0.8+ | Features (po-v07 "later"). |
| Live macOS verification of the upload byte fix (98f1e77) | Opportunistic | It has a unit test and macOS CI, but no live run against a real server. Do it if a Mac is available during acceptance. |

**Already shipped in v0.7.0** (checked in the code on main; not in this plan):
- "Done: …, The upload didn't finish."
- The sweep's "N of M servers gave a result; …".
- The "Other places" grouping.
- The warm-up hides the slower-hours bracket.
- The over-budget wording.
- "Included" / "Skipped".
- The share-link checkbox label.
- The "≥" explanation (B10 replaces its wording).
- The "by day" provider bars.
- Home's "most likely your internet provider" clause.
- "looks fine from here" on Home.
- The phone cards (to be rechecked in C6).

---

## 3. Order and PR breakdown

All PRs go into `release/v0.7.1`. Merge them in this order; work can start in parallel.

| PR | Branch | Contents | Owner | Size | Merge after |
|---|---|---|---|---|---|
| 1 | `fix/v071-bytes` | A3 (honest counts), A4 (connection test) | cq-core | 0.5 d | — |
| 2 | `fix/v071-budget` | A1: the backstop (cq-core, `iperf.go`) + `fits()` / `resets_at` / sub-minimum handling (ux-eng, `queue.go`) + Details off-reason copy (cq-ui) + docs lines | ux-eng + cq-core | 1 d | 1 |
| 3 | `fix/v071-estimator` | A2: the estimator, lower bounds, censored scoring, skipping old rows, "too fast to measure", the 2 Gbit fixtures; its UI cells and ▸ More copy | cq-core (+ cq-ui for cells) | 2 d | 2 |
| 4 | `fix/v071-schedule` | A5 (first test), A7 (traceroute off the save path), A6 (IX list and wording) | cq-core | 1.5 d | 3 |
| 5 | `fix/v071-report` | B2 (zone, days, evidence), B3, B4; settings for the owner's zone | cq-core + cq-ui | 1.5 d | 3 |
| 6 | `fix/v071-speed-ui` | B1, B5 (legend + public API neutralising), B6, B7, B8, B9, B10, C6 | cq-ui (+ cq-core for the B5 API) | 2 d | 2, 4 |
| 7 | `fix/v071-cli-copy` | C1, C2, C3, C4 | cq-core | 0.5 d | — |
| 8 | `docs/v071` | README, SECURITY.md and SPEEDTEST-API.md for A1, A2, A5, A6, B2 (zone), B5, C1; the release notes; C5 | ux-product (+ owners' review) | 0.5 d | 1–7 |

**Total:** about 9.5 dev-days, roughly 4 calendar days with three owners in parallel.

**Release gate.** A3, A1, A2, B1, B2, B3, B5 and C5 hold the release. Any **S** item that isn't ready by the RC moves to v0.7.2 and is listed in the release notes' known limits where users would notice.

---

## 4. Tests

### Unit and integration (`docker build --target test`)
- **A3:** normal, mid-data cancel and setup cancel byte counts (max, never sum); non-Linux falls back to the application counts.
- **A1:**
  - the backstop against a fake server that ignores TEST_END;
  - `fits()` at admit, at dequeue (the 3-visitor case) and between sweep steps;
  - `resets_at` as the latest of the three, with the fake clock;
  - the sub-minimum budget (starts, WARN, 403 `budget_too_small`);
  - the property test (1,000 randomised sequences, ledger ≤ limit) against the non-compliant fake.
- **A2:**
  - the estimator as a pure function (slow start, spike, still rising, 1–2 ticks, no qualifying tick, a deep upload buffer);
  - one coarse timing test;
  - goldens equal to v0.7.0 below the cap;
  - the 2 Gbit evening-cut fixture;
  - `finding_test` with near dips and far lower bounds (`isp`);
  - the mixed v0.7.0/new fixture.
- **A4:** peak accepted-not-closed ≤ 9.
- **A5:** turn-on, mode change, restart, the grid hand-over (≥ `spacing/2`), and idle samples < 20 giving missing.
- **A6:** the DE-CIX paths (peer found; peer null); the schema is unchanged; the public grep.
- **A7:** a slow traceroute doesn't delay the save or the next entry; one at a time; shutdown.
- **B2:**
  - evidence goldens for gap days, plurals, an America/New_York fixture and both voices;
  - the owner zone fallback chain (browser zone → `TZ` → UTC).
- **B3 and B5:** public goldens and greps (hosts, ports, custom servers, `via`, IX names, AS numbers, `public`, `public_sweep`, `manual`).
- **C1:** env defaults, flags win, subcommand dispatch, `report` read-only beside a running server.
- **C2:** the verdict goldens and the "(is|are) fine" grep.
- **UI (cq-ui's check scripts):**
  - B1 dialog states, focus, `aria-describedby` and the share-link lines;
  - B5 legend presence and shapes;
  - B6 branches;
  - B8 request count;
  - B9;
  - B10 column rule.

### netns harness (`test/netns/run.sh`)
- All 28 scenarios pass again.
- **New:** a fast shaped line (as fast as the lab allows, ideally 2 Gbit/s) with a scheduled 30% cut in the "evening" on a fake clock. The pattern is detected, the baseline comes from uncut tests, and far-path lower bounds never read as slow.

### Live acceptance (`v071-acceptance.md`, about 2 hours)
1. **Upgrade a v0.7.0 volume** (with tests and capped rows in it) to the RC: no `.bak` file (no schema change), and old rows display. Old capped rows show as before, but don't count in the heatmap or the finding.
2. **CLI:** `docker run --rm $RC version` and `profiles`; `report` beside the running container (C1).
3. **Turn on regions:** the first test runs 2 min ± 15 s later, and the note reads "starts at about {clock} (in 2 minutes)". Then `docker restart`: no off-grid test (A5, B6).
4. **Test every server:**
   - the dialog appears (screen reader, focus); Cancel sends nothing;
   - confirm, then cancel mid-download after 3 servers;
   - the cancelled row has bytes > 0, reads "Cancelled after download" or "Cancelled", and the remaining servers read "not reached (test cancelled)" (A3, B1, B7).
5. **Share link with `--speedtest-public-budget=300MB`:**
   - one test runs; then 429 with `resets_at`;
   - queue 3 visitors with 250 MB left: only one runs;
   - `used_bytes` never goes over 300,000,000;
   - the public dialog shows the allowance and "stop partway" lines;
   - one summary request in the HAR;
   - public `api/speed?part=windows` shows only `origin: "test"` (A1, B1, B5, B8).
6. **Restart with `--speedtest-public-budget=150MB`:** fyisp starts; the WARN is logged; the link answers 403 `budget_too_small`; Details shows the off reason (A1).
7. **`ss -tn state established dst <ip>`** during 20 tests: never more than 9 (A4).
8. **On a fast line** (the RC host's own ~1 Gbit, or the netns 2 Gbit stand-in):
   - tests show `rate_method: "peak90"`;
   - far servers show "at least normal" / "not counted", and the row says "Too fast to measure from this far…";
   - the all-capped table has the "at least" headers (A2, B10).
9. **Details charts:** the legend entries; the blue dashed tick appears only on the owner's view (B5).
10. **Report card** (webdev `-backfill 72h`, with the owner's browser zone America/New_York):
    - "3 days" everywhere, with the zone on every time;
    - "Slowest servers from …" with server labels in the right order;
    - the screen-reader text;
    - the public page grep returns 0 (B2–B4).
11. **Regressions from v0.7.0:**
    - traces (§3);
    - masking (no incidents or off spans in windows);
    - no iperf3 traffic while off;
    - IPv4 only;
    - no test above 210 MB.

---

## 5. Release notes outline (v0.7.1)

**Title:** fyisp v0.7.1: speed test fixes

- **Share-link speed tests stay inside their allowance.**
  - A test from the share link starts only when a whole test (up to about 210 MB) still fits in what's left of the daily allowance, and every test is now hard-stopped at that size, even against a misbehaving server.
  - An allowance smaller than one test turns share-link tests off, with a warning, instead of being exceeded.
- **Fast lines: steadier results.**
  - **The measurement:** fyisp now measures each test from the point where the speed levels off, so tests that hit the 100 MB limit are measured the same way as the rest.
  - **Tests still speeding up when they hit the limit** are kept as "at least" values, never as slow results.
  - **Older data:** fast-line results from v0.7.0 are kept but not used for patterns, which rebuild over about 3 days.
- **More accurate data counts:** the data each test uses now includes what was still in transit.
- **The first speed test runs about 2 minutes after you turn them on** (it used to be anywhere from 2 to 9). Details says when.
- **"Test every server" asks first,** with the time and data. On the share link it also shows the owner's remaining allowance, and whether the run will stop partway.
- **Reports:**
  - times are in the owner's own time zone, shown with every time;
  - one count of days and tests;
  - the slowest servers are named ("Region · Provider, City") with their usual speed next to the result.
- **"First network past your provider" sees through internet exchanges** (for example "…where they meet at DE-CIX").
- **Charts:** a legend for speed-test windows. For you, those started from your share link are shown separately. People with your link no longer see which tests were whose.
- **Smaller fixes:**
  - `docker run … fyisp version` (and `profiles`, `report`, `export`, `paths`) work with the image;
  - the verdict wording says "looks fine" rather than "is fine";
  - the public Speed tab loads its summary once;
  - cancelled tests read "Cancelled", never as a failure;
  - fewer "≥" marks when every test on your line hit the limit;
  - the warm-up line on Home has no "slowing" arrow.
- **Upgrading from v0.7.0:**
  - **Nothing to do.** There is no database change, so you can go back to v0.7.0.
  - **Docker:** the image now sets its defaults through environment variables, and `docker run … IMG --share` and compose `command:` lines work as before.
- **Known limits:**
  - IPv4 only.
  - Fast lines are bounded by the 100 MB per-direction limit, so far servers may only give "at least" results.
  - The server list is built in.
  - Exchanges not on fyisp's built-in list, and private interconnects, can't be seen through.
  - The upload byte fix for macOS hasn't been checked on a Mac against a live server yet.
