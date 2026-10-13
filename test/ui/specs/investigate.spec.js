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

test('Speed says why its controls are off', async ({ page }) => {
  await patchStatus(page, { controls: 'disabled', admin: false, can_control: false });
  await page.goto('/#v=speed');
  await expect(page.getByText('Speed-test controls are off on this dashboard (--admin-token=off).').first()).toBeVisible();
  await expect(page.getByText('from the device running fyisp')).toHaveCount(0);
});
