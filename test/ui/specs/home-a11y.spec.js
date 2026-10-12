// Home (v0.9.0) accessibility: axe (WCAG 2.1 A and AA) for every state in
// both themes at 1400 and 390 px, with a panel open too; the 5-minute bars
// at least 3:1 against their tile; no sideways scrolling on a phone.
// @ts-check
const { test, expect } = require('./fixtures');
const AxeBuilder = require('@axe-core/playwright').default;
const { STATES, serveHome, openHome, setTheme, WIDTHS, THEMES } = require('./home');

/** axe violations, as short lines for a readable failure. @param {import('@playwright/test').Page} page */
async function axe(page) {
  const r = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa']).analyze();
  return r.violations.map((v) => `${v.id}: ${v.nodes.map((n) => n.target.join(' ')).slice(0, 4).join(' | ')}`);
}

/** The lowest contrast of any bar against its tile's background (WCAG relative luminance). @param {import('@playwright/test').Page} page */
function barContrast(page) {
  return page.evaluate(() => {
    const rgb = (/** @type {string} */ s) => (s.match(/[\d.]+/g) || []).slice(0, 3).map(Number);
    const lum = (/** @type {number[]} */ c) => {
      const [r, g, b] = c.map((v) => { v /= 255; return v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4; });
      return 0.2126 * r + 0.7152 * g + 0.0722 * b;
    };
    let worst = { ratio: 99, what: '' };
    for (const i of document.querySelectorAll('.hp-tile .hp-bars i:not(.hp-base)')) {
      const tile = /** @type {HTMLElement} */ (i.closest('.hp-tile'));
      const a = lum(rgb(getComputedStyle(i).backgroundColor)), b = lum(rgb(getComputedStyle(tile).backgroundColor));
      const ratio = (Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05);
      if (ratio < worst.ratio) worst = { ratio, what: `${tile.dataset.id} ${i.className} on ${tile.className}` };
    }
    return worst;
  });
}

for (const name of STATES) {
  test(`axe and bar contrast: ${name}`, async ({ page }) => {
    await serveHome(page, `now-${name}`);
    await openHome(page);
    for (const w of WIDTHS) {
      await page.setViewportSize({ width: w, height: 900 });
      for (const theme of THEMES) {
        await setTheme(page, theme);
        expect(await axe(page), `${name} ${w}px ${theme}`).toEqual([]);
        // Stale bars are grey on purpose (their height still tells the state).
        if (name !== 'stale') {
          const c = await barContrast(page);
          expect(c.ratio, `${name} ${theme}: ${c.what}`).toBeGreaterThanOrEqual(3);
        }
      }
    }
  });
}

test('axe with a panel open, both themes, both widths', async ({ page }) => {
  await serveHome(page, 'now-zoom');
  await openHome(page);
  await page.locator('.hp-tile[data-id="zoom"]').click();
  await expect(page.locator('#hp-detail canvas')).toHaveCount(1);
  await page.locator('.hp-rules summary').click();
  for (const w of WIDTHS) {
    await page.setViewportSize({ width: w, height: 900 });
    for (const theme of THEMES) {
      await setTheme(page, theme);
      expect(await axe(page), `${w}px ${theme}`).toEqual([]);
    }
  }
});

for (const w of [320, 390]) {
  test(`no sideways scrolling at ${w} px`, async ({ page }) => {
    await page.setViewportSize({ width: w, height: 800 });
    await serveHome(page, 'now-isp');
    await openHome(page);
    await page.locator('.hp-tile[data-id="netflix"]').click();
    await expect(page.locator('#hp-detail canvas')).toHaveCount(1);
    const { sw, iw } = await page.evaluate(() => ({ sw: document.documentElement.scrollWidth, iw: window.innerWidth }));
    expect(sw).toBeLessThanOrEqual(iw);
  });
}

test('the strip goes vertical on a phone', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 800 });
  await serveHome(page, 'now-good');
  await openHome(page);
  const tops = await page.locator('.hp-hopbox').evaluateAll((els) => els.map((e) => e.getBoundingClientRect().top));
  expect(tops[1]).toBeGreaterThan(tops[0]);
  expect(tops[2]).toBeGreaterThan(tops[1]);
});
