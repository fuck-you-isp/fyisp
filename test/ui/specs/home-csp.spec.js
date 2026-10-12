// Home (v0.9.0) under the page's CSP (script-src 'self'; style-src 'self'):
// every state, a panel with its chart, the rules, a theme switch and Details.
// The shared fixture fails the test on any CSP violation, console error or
// exception.
// @ts-check
const { test, expect } = require('./fixtures');
const { STATES, serveHome, openHome } = require('./home');

test('no CSP violations or errors in any state', async ({ page }) => {
  const ctl = await serveHome(page, 'now-good');
  for (const name of STATES) {
    ctl.now = require('./home').fx(`now-${name}`);
    await openHome(page);
    await page.locator('.hp-tile:not(.is-off)').first().click();
    await expect(page.locator('#hp-detail')).toBeVisible();
    await page.locator('.hp-rules summary').click();
    await page.locator('#theme').click();
    await page.reload();
  }
  await page.getByRole('link', { name: 'Technical details →' }).click();
  await expect(page.locator('#home')).toBeHidden();
  await page.waitForLoadState('networkidle');
});
