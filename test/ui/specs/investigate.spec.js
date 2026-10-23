// Investigate: opening a target without an always-on trace starts an
// on-demand one; the mtr view, its columns and reports.
// @ts-check
const { test, expect } = require('./fixtures');

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

test('opening a target starts a trace by itself', async ({ page }) => {
  const target = await onDemandTarget(page, 1);
  const posts = await countPosts(page, { json: { target, state: 'running', rounds_done: 0, rounds_max: 250, interval_ms: 2500, started_at: Date.now() } });
  await page.goto('/#' + new URLSearchParams({ t: target }));
  await expect(page.locator('#inv .inv-status .pill.on', { hasText: 'Tracing…' })).toBeVisible();
  expect(posts.n).toBe(1);
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

test('the text report keeps its columns apart for a 6-digit ASN and RTTs of a second or more', async ({ page }) => {
  await page.goto('/');
  const text = await page.evaluate(async () => {
    const { mtrText } = await import('/static/invreport.js');
    const hop = { ip: '1.2.3.4', asn: 396982, n: 10, lost: 0, loss: 0, min: 2000, mean: 9999.9, max: 9999.9, last: 9999.9, stdev: 1000 };
    return mtrText({ dst: '1.2.3.4', start: 0, end: 1000, intervalMs: 2500, hops: [{ hop: 1, ...hop }] });
  });
  const row = text.split('\n')[2];
  expect(row).toMatch(/^ {2}1\.\|-- AS396982 1\.2\.3\.4 /);
  expect(row.trim().split(/\s+/)).toEqual(['1.|--', 'AS396982', '1.2.3.4', '0.00%', '10', '9999.9', '9999.9', '2000.0', '9999.9', '1000.0']);
});

/** fyisp-webdev's hops, rewritten: no loss anywhere, sent probes at each hop. @param {import('@playwright/test').Page} page @param {number} sent */
async function lossFreeHops(page, sent) {
  await page.route('**/api/trace?*', async (route) => {
    const res = await route.fetch();
    const d = await res.json();
    d.hops = d.hops.map((/** @type {any} */ x) => ({ ...x, n: sent, lost: 0, loss: 0, rate_limited: false, loss_continues: false }));
    return route.fulfill({ response: res, json: d });
  });
}

test('one probe is too few to say there is no loss; ten are enough', async ({ page }) => {
  await lossFreeHops(page, 1);
  await page.goto('/#' + new URLSearchParams({ t: 'Google DNS 8.8.8.8', r: '1h' }));
  const head = page.locator('#inv .inv-summary .is-head');
  await expect(head).toHaveText('Too few probes yet to judge loss.');
  await page.unrouteAll({ behavior: 'wait' });
  await lossFreeHops(page, 10);
  await page.reload();
  await expect(head).toHaveText('No real packet loss on this path in this range.');
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

/** Records the mode of every api/trace request, optionally replacing "modes". @param {import('@playwright/test').Page} page @param {any} [modes] */
async function traceModes(page, modes) {
  /** @type {string[]} */
  const asked = [];
  await page.route('**/api/trace?*', async (route) => {
    asked.push(new URL(route.request().url()).searchParams.get('mode') || '');
    if (!modes) return route.continue();
    const res = await route.fetch();
    return route.fulfill({ response: res, json: { ...(await res.json()), modes } });
  });
  return asked;
}

test('the probe picker: TCP 443 and UDP traces, with a matching report and mtr command', async ({ page }) => {
  const posts = { modes: /** @type {string[]} */ ([]) };
  await page.route('**/api/investigate?*', (route) => {
    const mode = new URL(route.request().url()).searchParams.get('mode') || '';
    posts.modes.push(mode);
    return route.fulfill({ json: { target: 'Google DNS 8.8.8.8', mode, state: 'running', rounds_done: 0, rounds_max: 250, interval_ms: 2500, started_at: Date.now() } });
  });
  const asked = await traceModes(page);
  await page.goto('/#' + new URLSearchParams({ t: 'Google DNS 8.8.8.8', r: '1h' }));
  const modes = page.getByRole('group', { name: 'Probe type' });
  await expect(modes.getByRole('button', { name: 'ICMP' })).toHaveAttribute('aria-pressed', 'true');
  await expect(page.locator('#inv .inv-status .pill.on', { hasText: 'Always-on trace' })).toBeVisible();
  expect(posts.modes).toEqual([]); // the always-on trace is ICMP

  await modes.getByRole('button', { name: 'TCP 443' }).click();
  await expect(modes.getByRole('button', { name: 'TCP 443' })).toHaveAttribute('aria-pressed', 'true');
  await expect.poll(() => posts.modes).toEqual(['tcp']); // the owner's view starts the TCP trace
  expect(asked).toContain('tcp');
  await expect(page.locator('#inv .inv-status .pill.on', { hasText: 'Tracing…' })).toBeVisible();
  await expect(page.locator('#inv .inv-mode-note')).toContainText('TCP SYNs to port 443');
  await expect(page.locator('#inv .inv-run .inv-cmd')).toHaveText('mtr -T -P 443 -rwzbc 100 8.8.8.8');
  await expect(page.locator('#inv .inv-changes')).toContainText('Route changes are recorded for ICMP traces only.');
  expect(await report(page, 'Text (mtr -rwz)')).toMatch(/fyisp: TCP SYN probes to port 443, one round every/);
  expect(JSON.parse(await report(page, 'JSON (mtr -j)')).report.mtr).toMatchObject({ type: 'tcp', port: 443, psize: '60' });
  expect(new URLSearchParams(new URL(page.url()).hash.slice(1)).get('im')).toBe('tcp');

  // The mode is part of the link.
  await page.reload();
  await expect(modes.getByRole('button', { name: 'TCP 443' })).toHaveAttribute('aria-pressed', 'true');

  await modes.getByRole('button', { name: 'UDP' }).click();
  await expect(page.locator('#inv .inv-run .inv-cmd')).toHaveText('mtr -u -P 33434 -rwzbc 100 8.8.8.8');
  expect(await report(page, 'Text (mtr -rwz)')).toMatch(/fyisp: UDP probes to port 33434, /);
  await modes.getByRole('button', { name: 'ICMP' }).click();
  await expect(page.locator('#inv .inv-run .inv-cmd')).toHaveText('mtr -rwzbc 100 8.8.8.8');
  expect(new URL(page.url()).hash).not.toContain('im=');
});

test('Windows: TCP finds the routers with ICMP, and UDP is off with the reason', async ({ page }) => {
  await traceModes(page, [{ mode: 'icmp', available: true }, { mode: 'tcp', port: 443, available: true, hops_via: 'icmp' },
    { mode: 'udp', port: 33434, available: false, reason: 'windows_firewall' }]);
  await page.goto('/#' + new URLSearchParams({ t: 'Google DNS 8.8.8.8', r: '1h', im: 'tcp' }));
  const modes = page.getByRole('group', { name: 'Probe type' });
  await expect(modes.getByRole('button', { name: 'UDP' })).toBeDisabled();
  const note = page.locator('#inv .inv-mode-note');
  await expect(note).toContainText('UDP traces aren’t available on Windows');
  await expect(note).toContainText('the routers are found with ICMP');
  expect(await report(page, 'Text (mtr -rwz)')).toMatch(/fyisp: ICMP echo and TCP SYN \(port 443\) probes at each TTL, the routers named from the ICMP replies, /);
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
