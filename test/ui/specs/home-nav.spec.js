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

test('without /api/home2 the old Home stays', async ({ page }) => {
  await page.goto('/');
  await expect(page.locator('#home')).toBeVisible();
  await expect(page.locator('#home')).toHaveClass(/\bhome\b/);
  await expect(page.locator('.hp-tile')).toHaveCount(0);
  await expect(page.locator('#viewnav button[aria-current="page"]')).toHaveText('Home');
});

test('an answer that isn\'t v2 keeps the old Home', async ({ page }) => {
  await page.route('**/api/home2?*', (route) => route.fulfill({ json: { v: 1 } }));
  await page.goto('/');
  await expect(page.locator('#home')).toHaveClass(/\bhome\b/);
});
