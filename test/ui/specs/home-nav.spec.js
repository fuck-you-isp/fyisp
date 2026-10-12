// Home (v0.9.0) navigation: "Technical details →" opens Details and Back
// returns; old Home links still land on Home; the problems link moves focus
// without touching the hash; a first answer that never comes holds up
// neither Details nor Home.
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

test('a first answer that never comes doesn\'t hold up Details', async ({ page, failOk }) => {
  failOk.push(/\/api\/home/);
  await page.route('**/api/home?*', () => { /* never answers */ });
  const t0 = Date.now();
  await page.goto('/#v=c');
  await expect(page.locator('#panels canvas:visible').first()).toBeVisible();
  expect(Date.now() - t0).toBeLessThan(3500);
});

test('after a first answer that never comes, Home polls on its own', async ({ page, failOk }) => {
  failOk.push(/\/api\/home\?part=now/);
  let hung = 0;
  await page.route('**/api/home?part=now*', (route) => { if (hung++ === 0) return; /* the first never answers */ route.fallback(); });
  await page.goto('/');
  await expect(page.locator('#home')).toBeVisible({ timeout: 8000 });
  await expect(page.locator('.hp-tile')).toHaveCount(11, { timeout: 8000 });
  expect(hung).toBeGreaterThan(1);
});
