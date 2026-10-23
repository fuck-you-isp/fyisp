// Signing in: every route but the page needs the admin token. Without the
// session cookie the page shows only the sign-in dialog; the "open this
// link" URL (#admin=<token>) signs in by itself and leaves the address bar.
// These tests drop the token playwright.config.js sends on every request.
// @ts-check
const { test, expect, DEV_ADMIN } = require('./fixtures');

test.use({ extraHTTPHeaders: {} });

test('without the token, only the sign-in dialog; a pasted token signs in', async ({ page, failOk }) => {
  failOk.push(/\/api\//, /status of 401/);
  await page.goto('/');
  const dlg = page.getByRole('dialog', { name: 'Sign in' });
  await expect(dlg).toBeVisible();
  await expect(dlg).toContainText('This dashboard needs its admin token.');
  await expect(page.locator('#home.hp')).toHaveCount(0);
  // Escape doesn't leave an empty page behind.
  await page.keyboard.press('Escape');
  await expect(dlg).toBeVisible();

  await dlg.getByLabel('Admin token').fill('not-the-token');
  await dlg.getByRole('button', { name: 'Sign in' }).click();
  await expect(dlg).toContainText('That is not this dashboard’s admin token.');
  await expect(dlg.getByLabel('Admin token')).toBeFocused();

  await dlg.getByLabel('Admin token').fill(DEV_ADMIN);
  await dlg.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.locator('#home.hp')).toBeVisible();
  await expect(page.locator('.hp-tile')).toHaveCount(11);
  await expect(page.getByRole('dialog', { name: 'Sign in' })).toHaveCount(0);
  const cookies = await page.context().cookies();
  const c = cookies.find((x) => x.name === 'fyisp_admin_3000');
  expect(c && c.httpOnly && c.sameSite === 'Strict').toBeTruthy();
});

test('the link signs in and leaves the address bar', async ({ page }) => {
  await page.goto('/#admin=' + DEV_ADMIN);
  await expect(page.locator('#home.hp')).toBeVisible();
  await expect(page).not.toHaveURL(/admin=/);
  await expect(page.locator('dialog[open]')).toHaveCount(0);
});

test('an old link says so in the sign-in dialog', async ({ page, failOk }) => {
  failOk.push(/\/api\//, /status of 40[13]/);
  await page.goto('/#admin=' + 'f'.repeat(32));
  const dlg = page.getByRole('dialog', { name: 'Sign in' });
  await expect(dlg).toContainText('This link is not valid any more');
  await expect(page).not.toHaveURL(/admin=/);
});
