// Home (v0.9.0): the live region speaks only when the answer changes. Not on
// load, not on polls with the same answer, once when the key changes; and the
// tiles are patched in place, so focus and nodes survive every poll.
// @ts-check
const { test, expect } = require('./fixtures');
const { fx, serveHome, openHome, settle } = require('./home');

/** Records every change inside the live region with its aria-live at that moment. */
function recordLive() {
  /** @type {{live:string|null}[]} */
  const log = [];
  /** @type {any} */ (window).__live = log;
  new MutationObserver((ms) => {
    for (const m of ms) {
      const n = m.target.nodeType === 1 ? /** @type {Element} */ (m.target) : m.target.parentElement;
      const region = n && n.closest('.hp-answer [role="status"]');
      if (region) log.push({ live: region.getAttribute('aria-live') });
    }
  }).observe(document, { subtree: true, childList: true, characterData: true });
}
const spoken = (/** @type {import('@playwright/test').Page} */ page) => page.evaluate(() => /** @type {any} */ (window).__live.filter((/** @type {any} */ x) => x.live === 'polite').length);

/** One 15 s poll, handled. @param {import('@playwright/test').Page} page @param {{nowCalls:number}} ctl */
async function poll(page, ctl) {
  const n = ctl.nowCalls;
  await page.clock.runFor(15e3);
  await expect.poll(() => ctl.nowCalls).toBeGreaterThan(n);
  await settle(page);
}

test('announces once per answer change, never on load', async ({ page }) => {
  await page.clock.install();
  await page.addInitScript(recordLive);
  const ctl = await serveHome(page, 'now-good');
  await openHome(page);
  const region = page.locator('.hp-answer [role="status"]');
  await expect(region).toHaveAttribute('aria-live', 'polite');
  // The first answer was written while the region was still off.
  expect(await page.evaluate(() => /** @type {any} */ (window).__live.length)).toBeGreaterThan(0);
  expect(await spoken(page)).toBe(0);

  // Focus a tile and tag the nodes: three polls with the same answer change nothing.
  const zoom = page.locator('.hp-tile[data-id="zoom"]');
  await zoom.focus();
  await page.evaluate(() => {
    for (const el of document.querySelectorAll('.hp-tile, .hp-tile .hp-bars i, .hp-problem')) /** @type {any} */ (el).__tag = 1;
  });
  for (let i = 0; i < 3; i++) await poll(page, ctl);
  expect(await spoken(page)).toBe(0);
  await expect(zoom).toBeFocused();
  expect(await page.evaluate(() => [...document.querySelectorAll('.hp-tile, .hp-tile .hp-bars i, .hp-problem')].every((el) => /** @type {any} */ (el).__tag === 1))).toBe(true);
  expect(await page.locator('.hp-tile .hp-bars i').count()).toBe(264);

  // A new answer (another key) is spoken once; the same answer again is not.
  ctl.now = fx('now-zoom');
  await poll(page, ctl);
  await expect(region.locator('.hp-headline')).toHaveText('Your internet is fine. Zoom isn’t connecting.');
  const once = await spoken(page);
  expect(once).toBeGreaterThan(0);
  for (let i = 0; i < 2; i++) await poll(page, ctl);
  expect(await spoken(page)).toBe(once);
  // The tiles changed in place: same nodes, focus kept.
  await expect(zoom).toBeFocused();
  await expect(zoom).toHaveClass(/is-down/);
  expect(await zoom.evaluate((el) => /** @type {any} */ (el).__tag)).toBe(1);
  // The problems list is keyed: old items keep their nodes, the new one is first.
  await expect(page.locator('.hp-problem').first()).toHaveClass(/live/);
  expect(await page.locator('.hp-problem').nth(1).evaluate((el) => /** @type {any} */ (el).__tag)).toBe(1);
  await expect(page).toHaveTitle('! Zoom down – fyisp');
});

test('goes stale on its own when fyisp stops answering, and back', async ({ page, failOk }) => {
  failOk.push(/\/api\/home\?part=now/);
  await page.clock.install();
  await page.addInitScript(recordLive);
  const ctl = await serveHome(page, 'now-good');
  await openHome(page);
  await expect(page.locator('.hp-answer [role="status"]')).toHaveAttribute('aria-live', 'polite');
  await expect(page.getByRole('button', { name: 'Test speed now' })).toBeEnabled();
  ctl.down = true;
  // Two failed polls in a row: the client's own stale answer.
  await page.clock.runFor(15e3);
  await settle(page);
  await page.clock.runFor(15e3);
  await settle(page);
  await expect(page.locator('.hp-answer')).toHaveClass(/stale/);
  await expect(page.locator('.hp-headline')).toHaveText('fyisp hasn’t checked since 10:29 am.');
  await expect(page).toHaveTitle('? Not checking – fyisp');
  await expect(page.getByRole('button', { name: 'Test speed now' })).toBeDisabled();
  const n = await spoken(page);
  expect(n).toBeGreaterThan(0);
  // Back: the server's answer again, spoken once more.
  ctl.down = false;
  await poll(page, ctl);
  await expect(page.locator('.hp-answer')).toHaveClass(/\bok\b/);
  expect(await spoken(page)).toBeGreaterThan(n);
});

test('a last check older than 2 minutes is stale even when the server answers', async ({ page }) => {
  const d = fx('now-good');
  d.last_check = d.at - 125e3;
  await serveHome(page, d);
  await openHome(page);
  await expect(page.locator('.hp-answer')).toHaveClass(/stale/);
  await expect(page.locator('.hp-headline')).toHaveText('fyisp hasn’t checked since 10:27 am.');
});

test('a background tab keeps polling and stays truthful', async ({ page }) => {
  await page.clock.install();
  await page.addInitScript(recordLive);
  const ctl = await serveHome(page, 'now-good');
  await openHome(page);
  await expect(page.locator('.hp-answer [role="status"]')).toHaveAttribute('aria-live', 'polite');
  // The tab goes to the background for 3 minutes.
  await page.evaluate(() => {
    Object.defineProperty(document, 'hidden', { configurable: true, get: () => true });
    Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => 'hidden' });
    document.dispatchEvent(new Event('visibilitychange'));
  });
  const n = ctl.nowCalls;
  for (let i = 0; i < 3; i++) {
    await page.clock.runFor(60e3);
    await expect.poll(() => ctl.nowCalls).toBe(n + i + 1);
    await settle(page);
    await expect(page).toHaveTitle('✓ All good – fyisp');
  }
  // Back in front: no false alarm, nothing announced.
  await page.evaluate(() => {
    Object.defineProperty(document, 'hidden', { configurable: true, get: () => false });
    Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => 'visible' });
    document.dispatchEvent(new Event('visibilitychange'));
  });
  await settle(page);
  await expect(page.locator('.hp-answer')).toHaveClass(/\bok\b/);
  expect(await spoken(page)).toBe(0);
});

test('a poll that hangs is given up, and the next one recovers', async ({ page, failOk }) => {
  failOk.push(/\/api\/home\?part=now/);
  await page.clock.install();
  const ctl = await serveHome(page, 'now-good');
  await openHome(page);
  // The next part=now never answers; the ones after it do.
  let hung = 0;
  await page.route('**/api/home?part=now*', (route) => { if (hung++ === 0) return; /* never answers */ route.fallback(); });
  const n = ctl.nowCalls;
  await page.clock.runFor(15e3);
  await expect.poll(() => hung).toBe(1);
  // Within the 10 s timeout nothing else is asked.
  await page.clock.runFor(5e3);
  expect(hung).toBe(1);
  // AbortSignal.timeout runs on the real clock: wait for it, then the next tick polls again.
  await expect.poll(async () => { await page.clock.runFor(1000); return ctl.nowCalls; }, { timeout: 20e3 }).toBeGreaterThan(n);
  await settle(page);
  await expect(page.locator('.hp-answer')).toHaveClass(/\bok\b/);
  await expect(page).toHaveTitle('✓ All good – fyisp');
});
