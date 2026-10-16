// v0.10.0: Investigate needs no admin token. Opening it starts a trace only
// for the owner (status.can_control, or a pasted admin token); everyone
// else clicks Trace, and nobody is asked for a token.
// When the hourly start budget is used up, opening the view shows a calm
// state; a click shows the error.
// @ts-check
const { test, expect } = require('./fixtures');
const { patchStatus } = require('./home');

/**
 * A target without an always-on trace, so opening it wants an on-demand
 * one. Each test takes its own (i): a trace a test started on fyisp-webdev
 * keeps running, and opening its target then starts none.
 * @param {import('@playwright/test').Page} page @param {number} i
 */
async function onDemandTarget(page, i) {
  const prof = await (await page.request.get('/api/profile')).json();
  expect(prof.features.trace).toBe(true);
  const t = prof.targets.filter((/** @type {any} */ x) => !x.trace)[i];
  expect(t).toBeTruthy();
  return /** @type {string} */ (t.name);
}

/** The banner under the status never says Tracing… while nothing runs, at either width. @param {import('@playwright/test').Page} page */
async function notTracingBanner(page) {
  const sum = page.locator('#inv .inv-summary');
  for (const width of [1400, 390]) {
    await page.setViewportSize({ width, height: 900 });
    await expect(sum).toBeVisible();
    await expect(sum).not.toContainText('Tracing…');
  }
  await page.setViewportSize({ width: 1400, height: 900 });
}

/** Counts POST api/investigate, answering each with answer (or the real server). @param {import('@playwright/test').Page} page @param {any} [answer] */
async function countPosts(page, answer) {
  const posts = { n: 0 };
  await page.route('**/api/investigate?*', async (route) => {
    if (route.request().method() === 'POST') posts.n++;
    if (answer) return route.fulfill(answer);
    return route.continue();
  });
  return posts;
}

test('a viewer without the admin session clicks Trace, and is never asked for the token', async ({ page }) => {
  await patchStatus(page, { controls: 'admin', admin: false, can_control: false });
  const target = await onDemandTarget(page, 0);
  const posts = await countPosts(page);
  await page.goto('/#' + new URLSearchParams({ t: target }));
  const st = page.locator('#inv .inv-status');
  await expect(st.locator('.pill', { hasText: 'Not tracing' })).toBeVisible();
  await page.waitForLoadState('networkidle');
  expect(posts.n).toBe(0);
  await expect(page.locator('#inv .inv-summary')).toContainText('Not tracing. Click Trace');
  await notTracingBanner(page);

  await st.getByRole('button', { name: 'Trace', exact: true }).click();
  await expect(st.locator('.pill.on', { hasText: /^Tracing… \d+ of 250 rounds$/ })).toBeVisible();
  expect(posts.n).toBe(1);
  await expect(page.getByRole('dialog', { name: 'Admin token' })).toHaveCount(0);
  await expect(page.locator('dialog[open]')).toHaveCount(0);
});

test('the owner\'s view starts a trace by itself', async ({ page }) => {
  await patchStatus(page, { controls: 'admin', admin: true, can_control: true });
  const target = await onDemandTarget(page, 1);
  const posts = await countPosts(page, { json: { target, state: 'running', rounds_done: 0, rounds_max: 250, interval_ms: 2500, started_at: Date.now() } });
  await page.goto('/#' + new URLSearchParams({ t: target }));
  await expect(page.locator('#inv .inv-status .pill.on', { hasText: 'Tracing…' })).toBeVisible();
  expect(posts.n).toBe(1);
});

test('the hourly budget: calm on open, an error on a click', async ({ page, failOk }) => {
  failOk.push(/\/api\/investigate\b/);
  const target = await onDemandTarget(page, 2);
  const posts = await countPosts(page, {
    status: 429, headers: { 'Retry-After': '840' },
    json: { error: 'too many on-demand traces started in the last hour', code: 'rate_limited', retry_after: 840 },
  });
  // fyisp-webdev's controls are 'enabled' (loopback, no token): opening starts one.
  await page.goto('/#' + new URLSearchParams({ t: target }));
  const st = page.locator('#inv .inv-status');
  const calm = st.locator('.pill', { hasText: '12 traces have been started in the last hour, the most allowed. You can start another in about 14 minutes.' });
  await expect(calm).toBeVisible();
  await expect(calm).not.toHaveClass(/\berr\b/);
  expect(posts.n).toBe(1);
  await notTracingBanner(page);

  await st.getByRole('button', { name: 'Trace', exact: true }).click();
  await expect(st.locator('.pill.err', { hasText: 'Too many traces started in the last hour. Try again in about 14 minutes.' })).toBeVisible();
  await expect(st.getByRole('button', { name: 'Try again' })).toBeVisible();
  expect(posts.n).toBe(2);
});

test('a visitor through a proxy on a token-less dashboard clicks Trace too', async ({ page }) => {
  // The server says can_control false for a proxied request; controls stays 'enabled'.
  await patchStatus(page, { controls: 'enabled', admin: false, can_control: false });
  const target = await onDemandTarget(page, 3);
  const posts = await countPosts(page);
  await page.goto('/#' + new URLSearchParams({ t: target }));
  await expect(page.locator('#inv .inv-status .pill', { hasText: 'Not tracing' })).toBeVisible();
  await page.waitForLoadState('networkidle');
  expect(posts.n).toBe(0);
});

test('a pasted admin token starts the owner\'s trace and goes with it', async ({ page }) => {
  await page.addInitScript(() => sessionStorage.setItem('fyisp-admin', 's3cret'));
  await patchStatus(page, { controls: 'admin', admin: false, can_control: false });
  const target = await onDemandTarget(page, 4);
  /** @type {Record<string,string>[]} */
  const sent = [];
  await page.route('**/api/investigate?*', (route) => {
    sent.push(route.request().headers());
    return route.fulfill({ json: { target, state: 'running', rounds_done: 0, rounds_max: 250, interval_ms: 2500, started_at: Date.now() } });
  });
  await page.goto('/#' + new URLSearchParams({ t: target }));
  await expect(page.locator('#inv .inv-status .pill.on', { hasText: 'Tracing…' })).toBeVisible();
  expect(sent).toHaveLength(1);
  expect(sent[0]['x-fyisp-admin']).toBe('s3cret');
  await expect(page.locator('dialog[open]')).toHaveCount(0);
});

/** The hops table's rows of hop n (fyisp-webdev: one address per hop). @param {import('@playwright/test').Page} page @param {number} n */
function hopRow(page, n) {
  return page.locator('#inv table.hops tbody tr').filter({ has: page.locator('td.hn', { hasText: new RegExp(`^${n}$`) }) });
}

// fyisp-webdev's steadyLoss: loss from hop 3 to the destination, hop 5
// rate-limiting its replies. The labels come only from the server's flags.
test('steady loss below the server\'s threshold gets no label, and the destination is not blamed', async ({ page }) => {
  await page.goto('/#' + new URLSearchParams({ t: 'CDN-Cloudflare', r: '6h' }));
  const sum = page.locator('#inv .inv-summary');
  await expect(sum).toContainText('too little to tell where the loss starts');
  await expect(sum).not.toContainText('no hop before it');
  await expect(sum).toContainText('hop 5 (');
  await expect(hopRow(page, 5).locator('.lnote')).toHaveText('not real — router rate-limiting');
  for (const n of [1, 2, 3, 4, 6, 7, 8, 9, 10, 11, 12]) {
    await expect(hopRow(page, n)).toHaveCount(1);
    await expect(hopRow(page, n).locator('.lnote, .lbadge')).toHaveCount(0);
  }
});

test('steady loss above the server\'s threshold: real loss starts at hop 3', async ({ page }) => {
  await page.goto('/#' + new URLSearchParams({ t: 'Google DNS 8.8.8.8', r: '6h' }));
  await expect(page.locator('#inv .inv-summary .is-head')).toHaveText(/^Real loss starts at hop 3 \(AS64500 EXAMPLE-ISP, your ISP\) and continues to the destination/);
  await expect(hopRow(page, 5).locator('.lnote')).toHaveText('not real — router rate-limiting');
  // One marker, on the first hop the server flags, not a badge on every hop with real loss.
  await expect(page.locator('#inv table.hops .lbadge')).toHaveCount(1);
  await expect(hopRow(page, 3).locator('.lbadge.real')).toHaveText('loss starts here');
  for (const n of [4, 6, 7, 8, 9, 10, 11, 12]) await expect(hopRow(page, n).locator('.lnote, .lbadge')).toHaveCount(0);
  for (const n of [1, 2]) await expect(hopRow(page, n).locator('.lnote, .lbadge')).toHaveCount(0);
});

test('Speed says why its controls are off', async ({ page }) => {
  await patchStatus(page, { controls: 'disabled', admin: false, can_control: false });
  await page.goto('/#v=speed');
  await expect(page.getByText('Speed-test controls are off on this dashboard (--admin-token=off).').first()).toBeVisible();
  await expect(page.getByText('from the device running fyisp')).toHaveCount(0);
});

// ---------- mtr's view: columns, session range, reports ----------

const MTR_COLS = ['#', 'Host', 'ASN', 'Loss%', 'Snt', 'Last', 'Avg', 'Best', 'Wrst', 'StDev'];
const IPV4 = /\b\d{1,3}(\.\d{1,3}){3}\b/;

/** The hops table's header texts. @param {import('@playwright/test').Page} page */
async function headers(page) {
  return (await page.locator('#inv table.hops thead th').allTextContents()).slice(0, -1); // the last is the latency bar
}

test('the hops table has mtr\'s columns, and More columns is remembered', async ({ page }) => {
  // Start without the choice, once: the reload below must keep it.
  await page.addInitScript(() => {
    try { if (!sessionStorage.getItem('t-once')) { localStorage.removeItem('fyisp-inv-more'); sessionStorage.setItem('t-once', '1'); } } catch { /* none */ }
  });
  await page.goto('/#' + new URLSearchParams({ t: 'Google DNS 8.8.8.8', r: '1h' }));
  await expect(hopRow(page, 1)).toHaveCount(1);
  expect(await headers(page)).toEqual(MTR_COLS);
  // Snt is answered plus lost; Last and StDev are numbers in the raw tier.
  const row = hopRow(page, 12);
  await expect(row.locator('td[data-label="Snt"]')).toHaveText(/^\d+$/);
  await expect(row.locator('td[data-label="Last"]')).toHaveText(/^\d+(\.\d+)?$/);
  await expect(row.locator('td[data-label="StDev"]')).toHaveText(/^\d+(\.\d+)?$/);

  const more = page.getByRole('button', { name: 'More columns' });
  await expect(more).toHaveAttribute('aria-pressed', 'false');
  await more.click();
  await expect(more).toHaveAttribute('aria-pressed', 'true');
  expect(await headers(page)).toEqual([...MTR_COLS, 'P95', 'Javg', 'Jmax']);
  await expect(row.locator('td[data-label="Jmax"]')).toHaveText(/^\d+(\.\d+)?$/);

  await page.reload();
  await expect(hopRow(page, 1)).toHaveCount(1);
  expect(await headers(page)).toEqual([...MTR_COLS, 'P95', 'Javg', 'Jmax']);
  await page.getByRole('button', { name: 'More columns' }).click();
  expect(await headers(page)).toEqual(MTR_COLS);
});

/**
 * Adds an on-demand session to every api/trace answer and records the
 * ranges asked for.
 * @param {import('@playwright/test').Page} page @param {any} inv
 */
async function withSession(page, inv) {
  /** @type {{from:string,to:string}[]} */
  const asked = [];
  await page.route('**/api/trace?*', async (route) => {
    const u = new URL(route.request().url());
    asked.push({ from: u.searchParams.get('from') || '', to: u.searchParams.get('to') || '' });
    const res = await route.fetch();
    const body = await res.json();
    body.investigation = { target: body.target, interval_ms: 2500, rounds_max: 250, ...inv };
    return route.fulfill({ response: res, json: body });
  });
  return asked;
}

test('a running trace session is the default range; a picked range wins', async ({ page }) => {
  const started = Date.now() - 3 * 60e3;
  const asked = await withSession(page, { state: 'running', rounds_done: 72, started_at: started });
  await page.goto('/#' + new URLSearchParams({ t: 'Microsoft-Teams', r: '6h' }));
  await expect.poll(() => asked.some((a) => a.from === String(started) && a.to === 'now')).toBe(true);
  const ranges = page.locator('#ranges');
  await expect(ranges.getByRole('button', { name: 'This trace' })).toHaveAttribute('aria-pressed', 'true');
  await expect(ranges.getByRole('button', { name: '6h', exact: true })).toHaveAttribute('aria-pressed', 'false');

  const n = asked.length;
  await ranges.getByRole('button', { name: '1h', exact: true }).click();
  await expect.poll(() => asked.slice(n).some((a) => a.from === 'now-1h')).toBe(true);
  await expect(ranges.getByRole('button', { name: '1h', exact: true })).toHaveAttribute('aria-pressed', 'true');
  await expect(ranges.getByRole('button', { name: 'This trace' })).toHaveAttribute('aria-pressed', 'false');

  const m = asked.length;
  await ranges.getByRole('button', { name: 'This trace' }).click();
  await expect.poll(() => asked.slice(m).some((a) => a.from === String(started))).toBe(true);
});

test('a finished trace session ends where it finished', async ({ page }) => {
  const started = Date.now() - 20 * 60e3, finished = started + 625e3;
  const asked = await withSession(page, { state: 'finished', rounds_done: 250, started_at: started, finished_at: finished });
  await page.goto('/#' + new URLSearchParams({ t: 'Microsoft-Teams' }));
  await expect.poll(() => asked.some((a) => a.from === String(started) && a.to === String(finished))).toBe(true);
});

/** Opens Copy report in format fmt and returns the report. @param {import('@playwright/test').Page} page @param {string} fmt */
async function report(page, fmt) {
  const box = page.locator('#inv .inv-report');
  if (!(await box.evaluate((/** @type {HTMLDetailsElement} */ d) => d.open))) await box.locator('summary').click();
  await box.getByRole('button', { name: fmt }).click();
  const ta = box.locator('textarea');
  await expect(ta).not.toHaveValue('');
  return ta.inputValue();
}

test('Copy report: mtr\'s text, JSON and CSV, from fyisp, never an address', async ({ page }) => {
  await page.goto('/#' + new URLSearchParams({ t: 'Google DNS 8.8.8.8', r: '1h' }));
  await expect(hopRow(page, 12)).toHaveCount(1);

  const text = (await report(page, 'Text (mtr -rwz)')).split('\n');
  expect(text[0]).toMatch(/^Start: \d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\+0000$/);
  expect(text[1]).toMatch(/^HOST: fyisp +Loss% {3}Snt {3}Last {3}Avg {2}Best {2}Wrst StDev$/);
  expect(text[2]).toMatch(/^ {2}1\.\|-- AS\?\?\? +_gateway \(192\.168\.1\.1\) +\d+\.\d\d% +\d+ +\d+\.\d/);
  expect(text.find((l) => /^ {2}3\.\|-- /.test(l))).toMatch(/^ {2}3\.\|-- AS64500 +edge1\.city\.isp\.example \(198\.51\.100\.1\) /);
  // Columns line up with the header.
  const end = text[1].length;
  for (const l of text.slice(2, 14)) expect(l.length).toBe(end);
  expect(text.join('\n')).toMatch(/fyisp: ICMP echo probes, one round every 10 s, at most 1 probe a second per router/);

  const j = JSON.parse(await report(page, 'JSON (mtr -j)'));
  expect(Object.keys(j)).toEqual(['report']);
  expect(Object.keys(j.report)).toEqual(['mtr', 'hubs']);
  expect(j.report.mtr).toEqual({ src: 'fyisp', dst: '8.8.8.8', tos: 0, tests: expect.any(Number), psize: '44', bitpattern: 'n/a' });
  expect(j.report.mtr.src).not.toMatch(IPV4);
  expect(j.report.hubs).toHaveLength(12);
  expect(Object.keys(j.report.hubs[0])).toEqual(['count', 'host', 'ASN', 'Loss%', 'Snt', 'Last', 'Avg', 'Best', 'Wrst', 'StDev']);
  expect(j.report.hubs[0]).toMatchObject({ count: 1, host: '_gateway (192.168.1.1)', ASN: 'AS???' });
  expect(j.report.hubs[11]).toMatchObject({ count: 12, ASN: 'AS15169' });
  for (const hub of j.report.hubs) {
    expect(typeof hub['Loss%']).toBe('number');
    expect(hub.Snt).toBeGreaterThan(0);
  }

  const csv = (await report(page, 'CSV (mtr -C)')).trim().split('\n');
  expect(csv[0]).toBe('Mtr_Version,Start_Time,Status,Host,Hop,Ip,Asn,Loss%,Snt,,Last,Avg,Best,Wrst,StDev');
  expect(csv).toHaveLength(13);
  expect(csv[1]).toMatch(/^fyisp,\d{10},OK,8\.8\.8\.8,1,_gateway \(192\.168\.1\.1\),AS\?\?\?,\d+\.\d\d,\d+,,\d+\.\d\d,/);
  for (const l of csv.slice(1)) expect(l.split(',')).toHaveLength(15);
});

test('Copy report copies to the clipboard, or selects the text without one', async ({ page, context }) => {
  await context.grantPermissions(['clipboard-read', 'clipboard-write']);
  await page.goto('/#' + new URLSearchParams({ t: 'Google DNS 8.8.8.8', r: '1h' }));
  await expect(hopRow(page, 12)).toHaveCount(1);
  const text = await report(page, 'CSV (mtr -C)');
  const box = page.locator('#inv .inv-report');
  await box.getByRole('button', { name: 'Copy', exact: true }).click();
  await expect(box.getByRole('status')).toHaveText('Copied.');
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(text);

  // No clipboard (an insecure origin): the text is selected instead.
  await page.evaluate(() => Object.defineProperty(navigator, 'clipboard', { value: undefined }));
  await box.getByRole('button', { name: 'Copy', exact: true }).click();
  await expect(box.getByRole('status')).toHaveText(/^Selected: press Ctrl\+C/);
  expect(await page.evaluate(() => {
    const ta = /** @type {HTMLTextAreaElement} */ (document.activeElement);
    return ta.tagName === 'TEXTAREA' ? ta.value.slice(ta.selectionStart, ta.selectionEnd) : '';
  })).toBe(text);
});

test('Run it yourself: mtr against the target\'s host, pathping on Windows', async ({ page }) => {
  await page.goto('/#' + new URLSearchParams({ t: 'Google DNS 8.8.8.8', r: '1h' }));
  const run = page.locator('#inv .inv-run');
  await expect(run.locator('.inv-cmd')).toHaveText('mtr -rwzbc 100 8.8.8.8');
  await expect(run).toContainText('pathping -n 8.8.8.8');
  await expect(run).toContainText('sudo');
  // A target with a name: the name, not its address.
  const tr = await (await page.request.get('/api/trace?' + new URLSearchParams({ target: 'Microsoft-Teams' }))).json();
  expect(tr.host).toBeTruthy();
  expect(tr.host).not.toMatch(IPV4);
  await page.goto('/#' + new URLSearchParams({ t: 'Microsoft-Teams', r: '1h' }));
  await expect(run.locator('.inv-cmd')).toHaveText(`mtr -rwzbc 100 ${tr.host}`);
});

for (const width of [1400, 390]) {
  test(`investigate screenshot ${width} px`, async ({ page }) => {
    await page.addInitScript(() => { try { localStorage.setItem('fyisp-inv-more', '1'); } catch { /* none */ } });
    await page.setViewportSize({ width, height: 900 });
    await page.goto('/#' + new URLSearchParams({ t: 'Google DNS 8.8.8.8', r: '1h' }));
    await expect(hopRow(page, 12)).toHaveCount(1);
    await page.locator('#inv .inv-report summary').click();
    await expect(page.locator('#inv .inv-report textarea')).not.toHaveValue('');
    // No page-wide horizontal scroll.
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
    await page.screenshot({ path: `out/shots/investigate-${width}.png`, fullPage: true });
    const hops = page.locator('#inv section[aria-label="Hops"]');
    await hops.scrollIntoViewIfNeeded();
    await hops.screenshot({ path: `out/shots/investigate-hops-${width}.png` });
  });
}
