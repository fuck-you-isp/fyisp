// Home (v0.9.0): "Test speed now". It queues one test through the speed
// queue (apiWrite), follows it until it's done and shows the new result; it
// is disabled while stale or while another test runs.
// @ts-check
const { test, expect } = require('./fixtures');
const { fx, serveHome, openHome, settle } = require('./home');

/**
 * Fakes the speed queue: run puts entry 41 in it, then each queue poll
 * moves it along `steps` (waiting, running, gone).
 * @param {import('@playwright/test').Page} page @param {string[]} steps
 */
async function fakeQueue(page, steps) {
  const ctl = { runs: 0, /** @type {Record<string,string>} */ headers: {}, polls: 0, entry: 0 };
  const entry = (/** @type {string} */ state) => ({ id: 41, kind: 'single', who: 'you', state, position: state === 'waiting' ? 1 : 0, est_start: Date.now() + 2 * 60e3, est_finish: Date.now() + 3 * 60e3 });
  const q = (/** @type {string} */ s) => ({ at: Date.now(), mine: s === 'gone' ? null : 41, running: s === 'running' ? entry('running') : null, waiting: s === 'waiting' ? [entry('waiting')] : [], last_sweep: null });
  await page.route('**/api/speed/run?*', (route) => {
    ctl.runs++;
    ctl.headers = route.request().headers();
    route.fulfill({ json: { entry: entry(steps[0]), duplicate: false } });
  });
  await page.route('**/api/speed?part=queue', (route) => { route.fulfill({ json: q(steps[Math.min(++ctl.polls, steps.length - 1)]) }); });
  await page.route('**/api/speed?part=entry&id=41', (route) => { ctl.entry++; route.fulfill({ json: { ...entry('done'), tests: [7] } }); });
  return ctl;
}

test('Test speed now: waiting, testing, then the new result', async ({ page }) => {
  await page.clock.install();
  const home = await serveHome(page, 'now-good');
  const q = await fakeQueue(page, ['waiting', 'running', 'gone']);
  await openHome(page);
  const btn = page.getByRole('button', { name: 'Test speed now' });
  const say = page.locator('.hp-speed [role="status"]');
  await btn.click();
  await expect(page.locator('.hp-speed-line')).toHaveText('Waiting for another test, starts in about 2 minutes.');
  await expect(say).toHaveText('Waiting for another test, starts in about 2 minutes.');
  await expect(btn).toBeDisabled();
  expect(q.runs).toBe(1);
  expect(q.headers['x-fyisp-token']).toBeTruthy();
  await expect(page.locator('.hp-speed-note')).toBeHidden();
  await page.clock.runFor(3000);
  await expect(page.locator('.hp-speed-line')).toHaveText('Testing your speed…');
  await expect(page.locator('.hp-speed-note')).toHaveText('This usually takes under a minute.');
  await expect(say).toHaveText('Testing your speed…');
  home.now = fx('now-speed-done');
  await page.clock.runFor(3000);
  await expect(page.locator('.hp-speed-line')).toHaveText('Your speed: about 925 Mbps down / 938 Mbps up — about your usual speed.');
  await expect(page.locator('.hp-speed-note')).toHaveText('Today 10:29 am');
  await expect(say).toHaveText('Your speed: about 925 Mbps down / 938 Mbps up — about your usual speed.');
  await expect(btn).toBeEnabled();
  expect(q.entry).toBe(1);
});

test('a refused start says why', async ({ page, failOk }) => {
  failOk.push(/\/api\/speed\/run\b/);
  await serveHome(page, 'now-good');
  await page.route('**/api/speed/run?*', (route) => route.fulfill({ status: 403, json: { error: 'forbidden', code: 'forbidden' } }));
  await openHome(page);
  await page.getByRole('button', { name: 'Test speed now' }).click();
  await expect(page.locator('.hp-speed-note')).toHaveText('fyisp refused that. Reload the page and try again.');
  await expect(page.locator('.hp-speed [role="status"]')).toHaveText('fyisp refused that. Reload the page and try again.');
  await expect(page.getByRole('button', { name: 'Test speed now' })).toBeEnabled();
});

test('disabled while stale', async ({ page }) => {
  await serveHome(page, 'now-stale');
  await openHome(page);
  await expect(page.getByRole('button', { name: 'Test speed now' })).toBeDisabled();
});

test('a test started elsewhere shows and disables the button', async ({ page }) => {
  const ctl = await serveHome(page, 'now-speed-running');
  await openHome(page);
  await expect(page.locator('.hp-speed-line')).toHaveText('Testing your speed…');
  await expect(page.locator('.hp-speed-note')).toHaveText('This usually takes under a minute.');
  await expect(page.getByRole('button', { name: 'Test speed now' })).toBeDisabled();
  // Nobody polls the queue for someone else's test; the next answer has the result.
  ctl.now = fx('now-speed-done');
  await page.reload();
  await settle(page);
  await expect(page.getByRole('button', { name: 'Test speed now' })).toBeEnabled();
});

test('no speed section when speed tests are off', async ({ page }) => {
  const d = fx('now-good');
  d.speed = { available: false, latest: null, empty_text: '', running: null };
  await serveHome(page, d);
  await openHome(page);
  await expect(page.locator('.hp-speed')).toBeHidden();
});
