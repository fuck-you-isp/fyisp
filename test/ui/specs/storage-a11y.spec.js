// Details → Storage accessibility: axe (WCAG 2.1 A and AA, 2.2 AA) for the
// empty state, the targets with check results, the open form and the
// delete dialog, in both themes at 1400 and 390 px; no sideways scrolling
// on a phone.
// @ts-check
const { test, expect, webdev } = require('./fixtures');
const AxeBuilder = require('@axe-core/playwright').default;
const { setTheme, WIDTHS, THEMES } = require('./home');

/** @param {import('@playwright/test').Page} page */
async function axe(page) {
  const r = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa']).analyze();
  return r.violations.map((v) => `${v.id}: ${v.nodes.map((n) => n.target.join(' ')).slice(0, 4).join(' | ')}`);
}

/** @param {import('@playwright/test').Page} page @param {string} what */
async function everyWay(page, what) {
  for (const w of WIDTHS) {
    await page.setViewportSize({ width: w, height: 900 });
    for (const theme of THEMES) {
      await setTheme(page, theme);
      expect(await axe(page), `${what} ${w}px ${theme}`).toEqual([]);
      const { sw, iw } = await page.evaluate(() => ({ sw: document.documentElement.scrollWidth, iw: window.innerWidth }));
      expect(sw, `${what} ${w}px: no sideways scrolling`).toBeLessThanOrEqual(iw);
    }
  }
  await page.setViewportSize({ width: 1400, height: 900 });
}

for (const [i, scenario] of ['empty', 'targets'].entries()) {
  test(`axe: storage ${scenario}`, async ({ page }) => {
    const port = 3211 + i;
    let proc;
    try {
      proc = await webdev(port, ['-storage', scenario]);
    } catch (e) {
      test.skip(/ENOENT/.test(String(e)), 'fyisp-webdev is not installed here (the uitest image has it)');
      throw e;
    }
    try {
      await page.goto(`http://127.0.0.1:${port}/#v=storage`);
      await expect(page.locator('.st-head h2, .st-head .sp-state').first()).toBeVisible();
      if (scenario === 'targets') {
        await expect(page.locator('.st-list tbody tr[data-id]')).toHaveCount(5);
        for (const name of ['AWS Virginia', 'Flaky edge']) {
          await page.getByRole('button', { name: `Check ${name}` }).click();
          await expect(page.getByRole('button', { name: `Check ${name}` })).toHaveText('Check');
        }
        await expect(page.locator('.st-check')).toHaveCount(2);
      }
      await everyWay(page, scenario);
      await page.getByRole('button', { name: 'Add a target' }).first().click();
      // A field error (the region) and the dotted-bucket notice, shown.
      await page.getByLabel('Name', { exact: true }).fill('New');
      await page.getByLabel('Endpoint').fill('https://s3.example.net');
      await page.getByLabel('Bucket (optional)').fill('a.b');
      await page.getByLabel('Region (optional)').fill('bad region');
      await page.getByRole('button', { name: 'Save target' }).click();
      await expect(page.locator('#st-f-region-err')).toBeVisible();
      await everyWay(page, `${scenario} form`);
      await page.screenshot({ path: `out/shots/storage-${scenario}-form.png`, fullPage: true });
      if (scenario === 'targets') {
        await page.getByRole('button', { name: 'Cancel' }).click();
        await page.getByRole('dialog').getByRole('button', { name: 'Discard' }).click();
        await page.screenshot({ path: 'out/shots/storage-targets.png', fullPage: true });
        await page.getByRole('button', { name: 'Delete NAS' }).click();
        await expect(page.getByRole('dialog', { name: 'Delete NAS?' })).toBeVisible();
        await everyWay(page, 'delete dialog');
      }
    } finally {
      proc?.kill();
    }
  });
}
