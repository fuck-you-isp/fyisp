// Details > Charts: the y axis fits the mean lines, not the per-bucket max, so
// one slow probe does not flatten every line on the panel. A 5 s max is
// injected into one bucket of every series; the lines must still reach the
// top of the plot. Screenshot: out/shots/charts-yaxis-outlier.png.
// @ts-check
const { test, expect } = require('./fixtures');

/** The topmost row of the plot with saturated (line-coloured) pixels, as a
 * fraction of the plot height from the top. The min–max band and the baseline
 * band are pale fills, so only the lines count. @param {import('@playwright/test').Page} page @param {string} group */
async function topLineRow(page, group) {
  return page.evaluate((group) => {
    const panel = /** @type {HTMLElement} */ (document.getElementById('panel-' + group));
    const canvas = /** @type {HTMLCanvasElement} */ (panel.querySelector('.u-wrap canvas'));
    const over = /** @type {HTMLElement} */ (panel.querySelector('.u-over'));
    const cr = canvas.getBoundingClientRect(), or = over.getBoundingClientRect();
    const k = canvas.width / cr.width;
    const x0 = Math.round((or.left - cr.left) * k), y0 = Math.round((or.top - cr.top) * k);
    const w = Math.round(or.width * k), hgt = Math.round(or.height * k);
    const px = /** @type {CanvasRenderingContext2D} */ (canvas.getContext('2d')).getImageData(x0, y0, w, hgt).data;
    for (let y = 0; y < hgt; y++) {
      for (let x = 0; x < w; x++) {
        const i = (y * w + x) * 4;
        const mx = Math.max(px[i], px[i + 1], px[i + 2]), mn = Math.min(px[i], px[i + 1], px[i + 2]);
        if (px[i + 3] > 200 && mx > 0 && (mx - mn) / mx > 0.45) return y / hgt;
      }
    }
    return 1;
  }, group);
}

test('one slow probe does not flatten the lines', async ({ page }) => {
  // An incident's opaque top edge would pass for a line at row 0.
  await page.route('**/api/incidents?*', (route) => route.fulfill({ json: [] }));
  await page.route('**/api/panel?*', async (route) => {
    const res = await route.fetch();
    const body = await res.json();
    for (const s of body.series) {
      const i = s.max.findIndex((/** @type {number|null} */ v) => v != null);
      if (i >= 0) s.max[i] = 5000;
    }
    await route.fulfill({ response: res, json: body });
  });
  await page.goto('/#v=c');
  await expect(page.locator('#panel-work .u-over')).toBeVisible();
  await page.waitForLoadState('networkidle');
  // The highest mean is at 1/1.08 of the axis, so near the top; with the axis
  // fitted to 5000 ms the lines would sit in the bottom few percent.
  expect(await topLineRow(page, 'work')).toBeLessThan(0.25);
  await page.locator('#panel-work').screenshot({ path: 'out/shots/charts-yaxis-outlier.png' });
});
