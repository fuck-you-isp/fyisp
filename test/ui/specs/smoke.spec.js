// Smoke test: the current app loads, Home and Details render, and nothing
// logs an error or trips the CSP on the way.
// @ts-check
const { test, expect } = require('./fixtures');

test('Home, then Details', async ({ page }) => {
  await page.goto('/');
  const home = page.locator('#home');
  await expect(home).toBeVisible();
  await expect(page.locator('#viewnav button[aria-current="page"]')).toHaveText('Home');
  await page.waitForLoadState('networkidle');
  await page.screenshot({ path: 'out/shots/home.png', fullPage: true });

  await page.locator('#viewnav button', { hasText: 'Details' }).click();
  await expect(home).toBeHidden();
  await expect(page.locator('#viewnav button[aria-current="page"]')).toHaveText('Details');
  await expect(page).toHaveURL(/#.*v=/);
  await page.waitForLoadState('networkidle');
  await page.screenshot({ path: 'out/shots/details.png', fullPage: true });

  // The charts (hatch fills) and Speed, redrawn after a theme switch.
  await page.getByRole('button', { name: 'Charts', exact: true }).click();
  await expect(page.locator('#panels canvas:visible').first()).toBeVisible();
  await page.locator('#theme').click();
  await page.waitForLoadState('networkidle');
  await page.screenshot({ path: 'out/shots/charts-theme.png', fullPage: true });
  await page.getByRole('button', { name: 'Speed', exact: true }).click();
  await expect(page).toHaveURL(/v=speed/);
  await page.waitForLoadState('networkidle');
  await page.screenshot({ path: 'out/shots/speed.png', fullPage: true });

  await page.locator('#viewnav button', { hasText: 'Home' }).click();
  await expect(home).toBeVisible();
});
