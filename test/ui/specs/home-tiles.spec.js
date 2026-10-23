// Home (v0.9.0): the tile detail panel. It opens after the clicked tile's
// row (4 columns on desktop, 2 below 840 px) with a caret under the tile,
// closes with Close or Escape and gives focus back, refreshes its chart every
// 60 s, and never fetches for a tile that isn't checked.
// @ts-check
const { test, expect } = require('./fixtures');
const { fx, serveHome, openHome, settle, setTheme, THEMES } = require('./home');

const IDS = ['zoom', 'teams', 'meet', 'slack', 'm365', 'gws', 'github', 'dropbox', 'onedrive', 'youtube', 'netflix'];

/** The panel's position: the tile it follows, and whether the caret is within the clicked tile. @param {import('@playwright/test').Page} page @param {string} id */
async function placement(page, id) {
  return page.evaluate((id) => {
    const p = /** @type {HTMLElement} */ (document.getElementById('hp-detail'));
    const prev = /** @type {HTMLElement} */ (p.previousElementSibling);
    const t = /** @type {HTMLElement} */ (document.querySelector(`.hp-tile[data-id="${id}"]`));
    const caret = p.getBoundingClientRect().left + parseFloat(getComputedStyle(p).getPropertyValue('--caret'));
    const r = t.getBoundingClientRect();
    const grid = /** @type {HTMLElement} */ (p.parentElement);
    return {
      after: prev.dataset.id,
      cols: getComputedStyle(grid).gridTemplateColumns.split(' ').length,
      lefts: new Set([...grid.querySelectorAll('.hp-tile')].map((x) => /** @type {HTMLElement} */ (x).offsetLeft)).size,
      caretIn: caret > r.left && caret < r.right,
      below: p.getBoundingClientRect().top >= r.bottom,
    };
  }, id);
}

for (const [width, cols] of [[1400, 4], [841, 4], [839, 2]]) {
  test(`panel placement at ${width} px (${cols} columns)`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 });
    await serveHome(page, 'now-good');
    await openHome(page);
    for (const id of IDS) {
      const i = IDS.indexOf(id);
      await page.locator(`.hp-tile[data-id="${id}"]`).click();
      await expect(page.locator('#hp-detail')).toBeVisible();
      const p = await placement(page, id);
      expect(p.cols).toBe(cols);
      expect(p.lefts).toBe(cols);
      // After the last tile of the clicked tile's row; the 3-tile last row (4 columns) ends at Netflix.
      expect(p.after).toBe(IDS[Math.min(IDS.length - 1, Math.floor(i / cols) * cols + cols - 1)]);
      expect(p.caretIn).toBe(true);
      expect(p.below).toBe(true);
    }
  });
}

test('the panel moves when the grid goes from 4 to 2 columns', async ({ page }) => {
  await serveHome(page, 'now-good');
  await openHome(page);
  await page.locator('.hp-tile[data-id="teams"]').click();
  expect((await placement(page, 'teams')).after).toBe('slack');
  await page.setViewportSize({ width: 600, height: 900 });
  await expect.poll(async () => (await placement(page, 'teams')).after).toBe('teams');
  expect((await placement(page, 'teams')).caretIn).toBe(true);
});

test('open, close and focus', async ({ page }) => {
  const ctl = await serveHome(page, 'now-zoom');
  await openHome(page);
  const zoom = page.locator('.hp-tile[data-id="zoom"]');
  const panel = page.locator('#hp-detail');
  await expect(zoom).toHaveAttribute('aria-expanded', 'false');
  await expect(zoom).toHaveAttribute('aria-controls', 'hp-detail');
  await zoom.click();
  await expect(panel).toBeVisible();
  await expect(panel).toHaveAccessibleName('Zoom details');
  await expect(zoom).toHaveAttribute('aria-expanded', 'true');
  await expect(panel.locator('h3')).toBeFocused();
  await expect(panel.locator('h3')).toHaveText('Zoom — last 24 hours');
  await expect(panel.locator('.hp-reason')).toHaveText('Not connecting since 10:05 am. Likely cause: Zoom’s side — the general call path and everything else are normal.');
  // Zoom's own servers aren't tested: its call figures are the general call path's (fine), and say so.
  await expect(panel.locator('.hp-metrics > span')).toHaveText(['General call path:', 'Delay 21 ms (choppy over 150 ms)', 'Jitter 2 ms (over 30 ms)', 'Packet loss 0% (2% or more)']);
  await expect(panel.locator('.hp-legend')).toContainText('How quickly the general call path answers (lower is better)');
  await expect(panel.locator('.hp-coverage')).toHaveText('Call quality is judged from the general call path; fyisp can’t test Zoom’s own servers before a meeting.');
  await expect(panel.locator('.hp-chartwrap')).toHaveAccessibleName('Zoom, how quickly it answered over the last 24 hours');
  await expect(panel.locator('.hp-chartwrap')).toHaveAccessibleDescription(/^Usually about 18 ms\./);
  await expect(panel.locator('.hp-chartwrap canvas')).toHaveCount(1);
  expect(ctl.tileCalls).toEqual(['zoom']);

  // Close gives focus back to the tile.
  await panel.getByRole('button', { name: 'Close' }).click();
  await expect(panel).toHaveCount(0);
  await expect(zoom).toBeFocused();
  await expect(zoom).toHaveAttribute('aria-expanded', 'false');

  // Escape too; clicking the open tile again just closes it.
  await zoom.click();
  await expect(panel).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(panel).toHaveCount(0);
  await expect(zoom).toBeFocused();
  await zoom.click();
  await zoom.click();
  await expect(panel).toHaveCount(0);

  // Another tile replaces the open one.
  await zoom.click();
  await page.locator('.hp-tile[data-id="m365"]').click();
  await expect(panel).toHaveAccessibleName('Microsoft 365 details');
  await expect(zoom).toHaveAttribute('aria-expanded', 'false');
  await expect(panel.locator('.hp-metrics > span')).toHaveText(['Usual 12 ms', 'Slow above 42 ms']);
  await expect(panel.locator('.hp-legend > span:has(.hp-sw.usual)')).toBeVisible();
});

test('a stream tile and a learning tile', async ({ page }) => {
  const ctl = await serveHome(page, 'now-learning');
  ctl.tile = (id) => fx(id === 'gws' ? 'tile-gws-learning' : 'tile-youtube');
  await openHome(page);
  await page.locator('.hp-tile[data-id="youtube"]').click();
  await expect(page.locator('#hp-detail .hp-metrics > span')).toHaveText(['Usual 7 ms', 'Slow above 37 ms', 'Only marked Down if it stops loading']);
  await page.locator('.hp-tile[data-id="gws"]').click();
  // Still learning: no metrics, no usual line in the legend.
  await expect(page.locator('#hp-detail .hp-metrics')).toBeHidden();
  await expect(page.locator('#hp-detail .hp-legend > span:has(.hp-sw.usual)')).toBeHidden();
  await expect(page.locator('#hp-detail .hp-chartwrap')).toHaveAccessibleDescription('Still learning what’s usual. Not watched before 9:50 am.');
});

test('an off tile: the reason and how to turn it on, no chart, no fetch', async ({ page }) => {
  const ctl = await serveHome(page, 'now-custom-config');
  await openHome(page);
  const gh = page.locator('.hp-tile[data-id="github"]');
  await gh.click();
  const panel = page.locator('#hp-detail');
  await expect(panel).toBeVisible();
  await expect(panel.locator('.hp-reason')).toHaveText('fyisp isn’t checking GitHub on this setup.');
  await expect(panel.locator('.hp-enable')).toHaveText('To check GitHub, add the tag tile:github to a target in your --config file, or base the file on extends: [default].');
  await expect(panel.locator('canvas')).toHaveCount(0);
  await expect(panel.locator('.hp-chartpart')).toBeHidden();
  await expect(panel.locator('.hp-metrics')).toBeHidden();
  expect(ctl.tileCalls).toEqual([]);
});

test('a 404 for the tile shows the off panel, not an error', async ({ page, failOk }) => {
  failOk.push(/\/api\/home\?part=tile/);
  const ctl = await serveHome(page, 'now-good');
  ctl.tile = () => null;
  await openHome(page);
  await page.locator('.hp-tile[data-id="dropbox"]').click();
  await expect.poll(() => ctl.tileCalls).toEqual(['dropbox']);
  await expect(page.locator('#hp-detail .hp-chartpart')).toBeHidden();
  await expect(page.locator('#hp-detail canvas')).toHaveCount(0);
  await expect(page.locator('#hp-detail .hp-reason')).toHaveText('Working normally.');
});

test('the open panel refreshes every 60 s', async ({ page }) => {
  await page.clock.install();
  const ctl = await serveHome(page, 'now-good');
  await openHome(page);
  await page.locator('.hp-tile[data-id="teams"]').click();
  await expect.poll(() => ctl.tileCalls.length).toBe(1);
  await settle(page);
  await page.clock.runFor(30e3);
  expect(ctl.tileCalls.length).toBe(1);
  await page.clock.runFor(31e3);
  await expect.poll(() => ctl.tileCalls.length).toBe(2);
  await settle(page);
  await page.clock.runFor(61e3);
  await expect.poll(() => ctl.tileCalls.length).toBe(3);
  expect(ctl.tileCalls).toEqual(['teams', 'teams', 'teams']);
  // Closed: no more fetches.
  await page.keyboard.press('Escape');
  await page.clock.runFor(120e3);
  expect(ctl.tileCalls.length).toBe(3);
});

test('Escape is left to an open dialog', async ({ page, failOk }) => {
  failOk.push(/\/api\/speed\/run\b/);
  await serveHome(page, 'now-good');
  await openHome(page);
  await page.locator('.hp-tile[data-id="meet"]').click();
  await expect(page.locator('#hp-detail')).toBeVisible();
  // A 401 (fyisp restarted with a new admin token) opens the sign-in dialog.
  await page.route('**/api/speed/run?*', (route) => route.fulfill({ status: 401, json: { error: 'admin token required', code: 'unauthorized' } }));
  await page.getByRole('button', { name: 'Test speed now' }).click();
  const dlg = page.getByRole('dialog', { name: 'Sign in' });
  await expect(dlg).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(dlg).toHaveCount(0);
  await expect(page.locator('#hp-detail')).toBeVisible();
  // Focus is back on the speed button: Escape there isn't about the panel.
  await page.keyboard.press('Escape');
  await expect(page.locator('#hp-detail')).toBeVisible();
  await page.locator('#hp-detail h3').focus();
  await page.keyboard.press('Escape');
  await expect(page.locator('#hp-detail')).toHaveCount(0);
  await expect(page.locator('.hp-tile[data-id="meet"]')).toBeFocused();
});

test('the chart fits again after a resize and a theme switch on Details', async ({ page }) => {
  await serveHome(page, 'now-zoom');
  await openHome(page);
  await page.locator('.hp-tile[data-id="zoom"]').click();
  await expect(page.locator('#hp-detail canvas')).toHaveCount(1);
  await page.getByRole('link', { name: 'Technical details →' }).click();
  await expect(page.locator('#home')).toBeHidden();
  await page.setViewportSize({ width: 1000, height: 900 });
  await page.locator('#theme').click();
  await page.goBack();
  await expect(page.locator('#home')).toBeVisible();
  await expect(page.locator('#hp-detail canvas')).toHaveCount(1);
  await expect.poll(() => page.evaluate(() => {
    const box = /** @type {HTMLElement} */ (document.querySelector('#hp-detail .hp-chart'));
    const c = /** @type {HTMLCanvasElement} */ (box.querySelector('canvas'));
    return Math.abs(c.getBoundingClientRect().width - box.clientWidth);
  })).toBeLessThan(2);
  expect(await page.locator('#hp-detail .hp-chart').evaluate((el) => el.clientWidth)).toBeGreaterThan(600);
});

for (const theme of THEMES) {
  for (const width of [1400, 390]) {
    test(`panel screenshot ${width} px ${theme}`, async ({ page }) => {
      await page.setViewportSize({ width, height: 900 });
      await setTheme(page, theme);
      await serveHome(page, 'now-zoom');
      await openHome(page);
      await page.locator('.hp-tile[data-id="zoom"]').click();
      await expect(page.locator('#hp-detail canvas')).toHaveCount(1);
      if (width < 560) {
        const box = await page.locator('#hp-detail .hp-close').boundingBox();
        expect(box && box.height).toBeGreaterThanOrEqual(40);
        expect(box && box.width).toBeGreaterThanOrEqual(64);
      }
      await page.locator('.hp-sec').nth(1).screenshot({ path: `out/shots/home-panel-zoom-${width}-${theme}.png` });
      // The theme switch redraws the chart with the other palette, without errors.
      await page.locator('#theme').click();
      await expect(page.locator('#hp-detail canvas')).toHaveCount(1);
    });
  }
}
