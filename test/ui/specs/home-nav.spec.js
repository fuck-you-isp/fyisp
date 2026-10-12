// Home (v0.9.0) navigation: "Technical details →" opens Details and Back
// returns; old Home links still land on Home; the problems link moves focus
// without touching the hash; without /api/home2 the old Home stays.
// @ts-check
const { test, expect } = require('./fixtures');
const { serveHome, openHome } = require('./home');

test('Technical details → Details, Back → Home', async ({ page }) => {
  await serveHome(page, 'now-good');
  await openHome(page);
  const link = page.getByRole('link', { name: 'Technical details →' });
  await expect(link).toHaveAttribute('href', /^#.*v=/);
  await link.click();
  await expect(page.locator('#home')).toBeHidden();
  await expect(page).toHaveURL(/#.*v=/);
  await expect(page).toHaveTitle('Details · fyisp');
  await expect(page.locator('#viewnav button[aria-current="page"]')).toHaveText('Details');
  await page.goBack();
  await expect(page.locator('#home')).toBeVisible();
  await expect(page).toHaveTitle('✓ All good – fyisp');
  await expect(page.locator('#viewnav')).toBeHidden();
});

for (const hash of ['#v=h', '#hw=w']) {
  test(`${hash} opens Home`, async ({ page }) => {
    await serveHome(page, 'now-good');
    await openHome(page, hash);
    await expect(page).toHaveTitle('✓ All good – fyisp');
  });
}

test('the problems link focuses the list without changing the hash', async ({ page }) => {
  await serveHome(page, 'now-good');
  await openHome(page);
  const before = page.url();
  await page.locator('.hp-day').getByRole('link', { name: '4 short problems' }).click();
  await expect(page.locator('#hp-problems')).toBeFocused();
  expect(page.url()).toBe(before);
  await expect(page.locator('#home')).toBeVisible();
});

test('without /api/home2 the old Home stays', async ({ page, failOk }) => {
  // A server before v0.9.0 has no /api/home2.
  failOk.push(/\/api\/home2/);
  await page.route('**/api/home2?*', (route) => route.fulfill({ status: 404, json: { error: 'not found', code: 'not_found' } }));
  await page.goto('/');
  await expect(page.locator('#home')).toBeVisible();
  await expect(page.locator('#home')).toHaveClass(/\bhome\b/);
  await expect(page.locator('.hp-tile')).toHaveCount(0);
  await expect(page.locator('#viewnav button[aria-current="page"]')).toHaveText('Home');
});

test('a probe that never answers falls back to the old Home and doesn\'t hold up Details', async ({ page, failOk }) => {
  failOk.push(/\/api\/home2/);
  await page.route('**/api/home2?*', () => { /* never answers */ });
  const t0 = Date.now();
  await page.goto('/#v=c');
  await expect(page.locator('#panels canvas:visible').first()).toBeVisible();
  expect(Date.now() - t0).toBeLessThan(3500);
  await page.goto('about:blank');
  await page.goto('/');
  await expect(page.locator('#home')).toHaveClass(/\bhome\b/, { timeout: 8000 });
  await expect(page.locator('#home')).toBeVisible();
});

test('an answer that isn\'t v2 keeps the old Home', async ({ page }) => {
  await page.route('**/api/home2?*', (route) => route.fulfill({ json: { v: 1 } }));
  await page.goto('/');
  await expect(page.locator('#home')).toHaveClass(/\bhome\b/);
});
