# fyisp v0.7.1: plan (FINAL)

**Status:** FINAL, 2026-10-11 (A2 scoring settled: net-isp's share-based hybrid). The execution team (cq-core, ux-eng, cq-ui) starts from this file.

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
   - **Ramp-capped results** are kept as **lower bounds** ("not slow"); see 12 for the final scoring.
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
11. **New B10:** "≥" only on ramp-capped results, with the column-header form when all are (ux-content M8, net-isp, go-review).
12. **Addenda** (the lead's rulings after the reviews):
    - **A2 scoring (FINAL, amended 2026-10-11 after net-isp's implementation review):** a coverage guard, per server over 14 days.
      - The server is "too fast to measure from here" (out of the finding and the by-hour stats) when its baseline rows (those without `*_lower_bound`) cover **fewer than 12 distinct local hours**, or **more than 50% of them fall within any 6-hour window**.
      - Otherwise the baseline comes from those rows, and lower-bound rows count as "at least normal" (`max(score, 1.0)`).
      - This replaces the 20% share: the share rule also threw away a correctly detected 42%-capped case, and net-isp's probe showed the coverage guard passes all four fixture cases (0/17/42% → `isp`, 83% → `too_fast`).
      - All servers out: case `too_fast`.
      - `max(p90, capped lower bounds)` is dropped (it depends on RTT).
    - **The ramp-capped flag** is decoded once in the store. v0.7.0 rows with `down_short && ramp_ms == 0` count as ramp-capped, rather than being skipped as in the draft.
13. **A6:** IX ASNs are skipped before the `isp == 0` branch; the scan continues past unanswered hops; networks are grouped by (peer ASN, via); the list is verified once against a routing table (LINX's two ASNs).
14. **A7 now runs the traceroute *before* the test window,** synchronously and bounded, instead of async after the save (go-review).

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

#### A2. [M] Fast lines: one steady-rate estimator, lower bounds counted as "at least normal" (cq-core) — *after A1*
**Estimator.** It applies to every capped direction, and is optional for uncapped ones.
- **Ticks:** the 100 ms ticks, smoothed by a 3-tick moving average. Download ticks come from received bytes; **upload ticks from per-tick `tcpi_bytes_acked`** (Linux), never written bytes.
- **Peak:** the highest smoothed tick.
- **Window:** from the first smoothed tick ≥ 90% of the peak, to the **actual timestamp of the last byte** (not a tick boundary).
- **Rate:** bytes in the window ÷ the window's duration.
- **Stored** in `Extra`: `rate_method` (`"peak90"`, or `"whole"` as the fallback) and `win_ms`.

**Ramp-capped** (a lower bound).
- **Which results:**
  - new results whose smoothed peak is in the last 2 ticks (still rising, no plateau);
  - new results with fewer than 3 ticks in the window;
  - **v0.7.0 rows** with `down_short` (`up_short`) and `ramp_ms == 0` in that direction.
- **Where it's decided:** once, when the store decodes a test (`model.SpeedTest.RampCapped{Down,Up}`). No other code re-derives it.
- **Other cases:** no qualifying tick gives `rate_method: "whole"`, flagged; non-Linux uploads use the v0.7.0 method, with `rate_method: "whole"`.
- **Capped after the ramp** (a plateau was seen before the cap): a **valid rate**, scored normally (see B10 for its wording).

**Scoring: net-isp's share-based hybrid** (superseded: the "share decides" test below is replaced by the coverage guard in the Addenda above; the rest still applies. API field names are `down_lower_bound` / `up_lower_bound`, not "ramp_capped").
- **The share decides.** For each server, take the **share** of its scheduled rows in the last 14 days that are ramp-capped (per direction).
- **Under 20% ramp-capped:**
  - `Baseline()` (the p90) is built from the **non-ramp-capped** rows, with the existing minimum-n rules;
  - ramp-capped rows count as "**at least normal**" (`score = max(score, 1.0)`) in by-hour stats, patterns and the finding, never slow and never dropped.
  - (Only the fastest ≤ 20% are missing from the baseline, so it reads a few percent low at most.)
- **20% or more ramp-capped:** the uncapped rows are no longer a sample of normal; they are the slow tail. The server is "**too fast to measure from here**":
  - out of the finding, the by-hour stats and the patterns;
  - its rows are still shown and exported;
  - it's never shown as "learning".
- **Every server out:** when every scheduled server is "too fast to measure from here", the finding's case is `too_fast`. Its text is "Your line is too fast for these tests to compare times of day", never "nothing" or "within 10%".
- **Dropped:** `max(p90 uncapped, median of capped lower bounds)`. net-isp showed it depends on RTT and fails on mid and far servers.
- **Not a gate any more:** the "fewer than 3 uncapped rows" rule is replaced by the share.
- **Nothing stored is rewritten.** Patterns recompute on read.
- **v0.8 note** (net-isp): the ramp-capped share by hour is itself a speed signal (90% capped by day, 10% in the evening means a dip). Use it once the estimator has proved itself.

**Fixture pass criterion** (net-isp; must pass before PR 3 merges).
- **Fixture:** daytime rows are ramp-capped (fast); evening rows are uncapped, at 60% of the true rate.
- **Criterion:** the result is never "nothing", and the baseline is never built from evening-only rows. Either the dip is found, or the server is marked "too fast to measure from here". With more than 20% capped, as in this fixture, expect "too fast".
- A second fixture with 10% of rows capped (a slightly faster day) must **find** the evening dip.

**Acceptance.**
1. **Pure-function unit tests** of the estimator on synthetic tick series:
   - slow start then plateau: the rate is the plateau within 3%;
   - a single high spike tick: smoothed away;
   - still rising at the end: ramp-capped;
   - 1-tick and 2-tick runs: ramp-capped;
   - no qualifying tick: `"whole"`;
   - a deep upload send buffer (written ≫ acked in the first ticks): uses acked.
2. **One coarse timing test** with the fake server. No other timing-sensitive asserts (`-race`).
3. **Under the cap** (100 Mbit/s): by-hour, finding and baseline outputs are byte-identical to v0.7.0 (goldens).
4. **net-isp's fixtures** (above):
   - over 20% capped: "too fast to measure from here", never "nothing", and the baseline never comes from evening-only rows;
   - 10% capped: the evening dip is found.
5. **A 2 Gbit/s line with a 30% evening cut** (fixture plus a netns or fake-server case):
   - near servers that plateau (under 20% capped) find the evening pattern;
   - far servers (mostly capped) are "too fast to measure from here", never slow, never "nothing".
6. **`finding_test`** on a 1 Gbit/s line where near regions dip and far regions are ramp-capped: the case is `isp`.
7. **Share thresholds:**
   - a server at 19% capped is scored;
   - one at 20% is "too fast to measure from here";
   - all servers too fast gives case `too_fast` with its sentence.
8. **v0.7.0 rows** with `down_short && ramp_ms == 0` are decoded as ramp-capped, and old rows capped after the ramp score normally.

**UI and copy** (cq-ui, ux-content S10 adapted to the ruling):
- **"% of normal" cell** of a ramp-capped result: "**at least normal**", muted and never an error colour. The ▸ More sentence is its `title` and screen-reader text.
- **▸ More:** "**Still speeding up when it reached its 100 MB limit, so the speed was at least this. It counts as at least normal in the time-of-day pattern, but isn't used to set this server's normal.**"
- **"Too fast to measure from here"** strings (added to ux-content's string table; the public voice is the same, with "the line" for "your line"):

  | Where | String |
  |---|---|
  | Server row (heatmap Servers view, table group header) | "**Too fast to measure from here**" (muted), with the `title` / screen-reader text below |
  | Server row explanation (▸ More, `title`) | "**Most tests to {server} reached their 100 MB limit while still speeding up, so they can't show when this line slows down. It's left out of the time-of-day pattern.**" |
  | Region row, when every server in it is too fast | "**Too fast to measure from here**", with "Every server in {region} is too fast to measure from here within the 100 MB limit." |
  | Finding, case `too_fast` (Details, Home is silent, report) | "**Your line is too fast for these tests to compare times of day: most tests reach their 100 MB limit while still speeding up.**" |
  | Finding evidence line, when some servers are out | "{k} of {n} servers were too fast to measure from here and are left out." |
  | Report method bullet | "Servers that this line reaches faster than a 100 MB test can measure are left out of the time-of-day comparison." |
- **"Problems only"** never includes ramp-capped rows.

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
- A constant list of about 15 IX route-server and LAN ASNs: DE-CIX 6695; AMS-IX 1200; LINX 5459 **and** 8714; France-IX 51706; NL-ix 34307; Netnod 8674; SIX 33108; JPNAP 7527; HKIX 4635; IX.br 26162; …
- Each entry is **verified once** against PeeringDB and a current routing table before committing, with the source noted in a comment. (LINX has two ASNs: check that both are listed.)
- No runtime fetch, no rDNS.

**Lookup** (`handoffOf`).
- IX ASNs are skipped **before** the `isp == 0` branch, so an IX hop can never become the ISP either.
- They're treated like ASN 0, and the scan **keeps going past unanswered hops**. The peer is the next hop with a non-IX ASN.
- The IX's name is returned as `via` in the API **only**, inside the existing `if !n.public` block (rev-security). It isn't stored.
- No schema change. The handoff is re-traced per server daily, so `via` fills within a day.

**Finding.**
- **Grouping:** networks are grouped by **(peer ASN, via)**.
- **Local evidence:** keeps `via`.
- **Wording** (ux-content S11, local):
  - **Peer known:** "the link between your internet provider and **Leaseweb**, where they meet at **DE-CIX (an internet exchange)**"; for a transit peer, "…and **Lumen (AS3356)**, meeting at DE-CIX (an internet exchange)".
  - **Peer unknown:** "…the route through **DE-CIX (an internet exchange)** to these servers".
  - It **never blames the exchange**, which is a meeting point. It uses the tier's verb ("may be" when early).
- **Format:** everywhere in the UI, "{Name} (AS{n})", never "AS{n} {NAME}" in upper case.
- **Public voice:** no `via`, no IX name and no AS numbers. **The finding never names the IX**, only the peer.

**Acceptance.**
1. A handoff test with the path ISP → DE-CIX LAN hop → Leaseweb gives ASN 60781 with `via: "DE-CIX"`.
2. The path ISP → IX hop → unanswered hop → Leaseweb still gives the peer as Leaseweb with `via: "DE-CIX"`.
3. A path whose last answer is the IX hop gives the peer null with `via: "DE-CIX"`, and the finding uses the "route through" wording.
4. An IX hop first on the path (where `isp` would be 0) never becomes the ISP.
5. `SchemaVersion()` is unchanged.
6. The public golden grep for `"via"`, `DE-CIX` and `AS[0-9]` in `text_public` and the public report returns 0.

#### A7. [S] The daily handoff traceroute runs before the test, on an idle line (cq-core)
**Fix** (go-review update; it replaces "after the save").
- When a server's handoff is due (at most daily), run its traceroute **before `Windows.Begin`**, synchronously and with a time bound (the existing `handoffBudget`, at most 15 s).
- It runs on the idle line, so it never overlaps a test's load, and it can't write after the store closes.
- The test itself is then saved straight after it ends, with no traceroute between.

**Conditions** (rev-security, still apply):
- The traceroute targets the **same IP the test will dial** (resolved once and checked by `Server.check`), never a re-resolved name.
- It's at most one at a time (it runs synchronously inside the one-at-a-time queue).
- It's cancelled on shutdown (via the run's context).
- There are no late writes.

**Cost.** A test whose handoff is due starts up to 15 s later, at most once a day per server. The slot's 45 s cap doesn't include the traceroute.

**Acceptance.**
1. With a 5 s fake traceroute, the test's window starts after it ends, and the row is saved within 1 s of the test's end.
2. The traceroute's ICMP is not inside any test window.
3. A traceroute that runs past the bound is abandoned, and the test still runs.
4. A shutdown during a traceroute exits cleanly, with no write after close.

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

#### B10. [S] "≥" only where it's true, without the noise (cq-ui; net-isp, ux-content M8, go-review)
**"≥" marks only ramp-capped results** (A2): the results that were still speeding up at the limit.

**Capped after the ramp** (a plateau was seen, then the cap ended the test): a **valid rate**.
- No "≥".
- ▸ More: "**Short test ({s} s): it reached its 100 MB limit after the speed had levelled off.**"

**Column rule.**
- When **every** shown row in a direction is ramp-capped: no per-cell "≥", and the column header becomes "**Down (Mbit/s, at least)**" / "**Up (Mbit/s, at least)**".
- Mixed tables keep "≥" on the ramp-capped cells.

**One note above the table**, replacing today's line under it:
- all ramp-capped: "**Your line is faster than these tests can fully measure: each one stops after 100 MB in each direction. Treat the numbers as 'at least'.**"
- mixed: "**≥ means the test reached its 100 MB limit while still speeding up, so the speed was at least this.**"

**Screen readers.** The visible "≥" is `aria-hidden`, with sr-only "at least ".

**Report and share link.** No per-row marks. When every test was ramp-capped, one sentence in the method bullet: "On this line every test reached its 100 MB limit while still speeding up, so speeds are lower limits."

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
| 3 | `fix/v071-estimator` | A2: the estimator, the ramp-capped flag in the store, "at least normal" scoring, the baseline from non-ramp-capped rows, "too fast to measure", net-isp's fixture and the 2 Gbit fixtures; its UI cells and ▸ More copy | cq-core (+ cq-ui for cells) | 2 d | 2 |
| 4 | `fix/v071-schedule` | A5 (first test), A7 (traceroute before the window), A6 (IX list, grouping and wording) | cq-core | 1.5 d | 3 |
| 5 | `fix/v071-report` | B2 (zone, days, evidence), B3, B4; settings for the owner's zone | cq-core + cq-ui | 1.5 d | 3 |
| 6 | `fix/v071-speed-ui` | B1, B5 (legend + public API neutralising), B6, B7, B8, B9, B10, C6 | cq-ui (+ cq-core for the B5 API) | 2 d | 2, 4 |
| 7 | `fix/v071-cli-copy` | C1, C2, C3, C4 | cq-core | 0.5 d | — |
| 8 | `docs/v071` | README, SECURITY.md and SPEEDTEST-API.md for A1, A2, A5, A6, B2 (zone), B5, C1; the release notes; C5 | ux-product (+ owners' review) | 0.5 d | 1–7 |

**Total:** about 8 dev-days (go-review's estimate), roughly 3–4 calendar days with three owners in parallel. The draft's measurement PR is split three ways: PR 1 (A3, A4), PR 3 (A2) and PR 4 (A5, A6, A7).

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
  - `finding_test` with near dips and far ramp-capped results (`isp`);
  - **net-isp's fixtures**: over 20% capped gives "too fast to measure from here" (never "nothing", never a baseline from evening-only rows); 10% capped finds the dip;
  - the coverage-guard thresholds (11 vs 12 hours, 50% vs 51% within 6 hours; all too fast gives case `too_fast`);
  - v0.7.0 rows decoded as ramp-capped from `down_short && ramp_ms == 0`.
- **A4:** peak accepted-not-closed ≤ 9.
- **A5:** turn-on, mode change, restart, the grid hand-over (≥ `spacing/2`), and idle samples < 20 giving missing.
- **A6:** the DE-CIX paths (peer found; past an unanswered hop; peer null; IX first); grouping by (peer, via); the schema is unchanged; the public grep.
- **A7:** the traceroute runs before `Windows.Begin`, never inside a window; the time bound; shutdown with no write after close.
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
   - ramp-capped far servers show "at least normal" and "≥" (or the "at least" header);
   - a server whose uncapped rows fail the coverage guard (under 12 local hours, or over 50% within 6 hours) says "Too fast to measure from here" and is out of the finding;
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
  - **Older data:** fast-line results from v0.7.0 that hit the limit while speeding up now count as "at least normal" too. Nothing stored is changed.
  - **Very fast lines:** servers that every test reaches while still speeding up are marked "too fast to measure from here" and left out of the findings.
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

---

## 6. A2 confirmation (closed)

- **2026-10-11:** net-isp confirmed that the lead's first A2 rule **fails** their fixture. The baseline built from uncapped rows is the evening tail, so the dip scored about 1.0. They proposed the share-based hybrid.
- rev-network's outside view agreed with the diagnosis.
- The lead ruled the **hybrid FINAL** (A2, "Scoring").
- Fixture pass criterion: never "nothing", never a baseline from evening-only rows.
