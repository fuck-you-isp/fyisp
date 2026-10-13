// v0.10.0 removed notes, reports and "Mark a bad call". Technical details
// has none of them left, old bookmarks that opened their lists still work,
// a right-click on a chart is the browser's own again, and the Speed panel
// (which shares the owner-controls check with them) still works.
// @ts-check
const { test, expect } = require('./fixtures');

test('no notes, reports or Mark a bad call in Technical details', async ({ page }) => {
  await page.goto('/#v=calls');
  await expect(page.getByRole('heading', { name: 'What happened at …?' })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Show on the charts' })).toBeVisible();
  await expect(page.getByText('Mark a bad call')).toHaveCount(0);
  await expect(page.getByText('Create evidence report')).toHaveCount(0);

  await page.getByRole('button', { name: 'Charts', exact: true }).click();
  await expect(page.locator('#panels canvas:visible').first()).toBeVisible();
  await page.waitForLoadState('networkidle');
  await expect(page.locator('#notes, #reports, #note-pop')).toHaveCount(0);
  for (const name of ['+ note', 'Create report', 'Report']) {
    await expect(page.getByRole('button', { name, exact: true })).toHaveCount(0);
  }
  await expect(page.locator('summary', { hasText: /^Notes$|^Reports$/ })).toHaveCount(0);
  await expect(page.locator('.foot-help').first()).not.toContainText('Flags mark notes');

  // A right-click on a chart opens no note editor (and the page doesn't
  // take the browser's menu away).
  const over = page.locator('#panels .u-over:visible').first();
  const prevented = await over.evaluate((el) => {
    const r = el.getBoundingClientRect();
    const e = new MouseEvent('contextmenu', { bubbles: true, cancelable: true, clientX: r.left + r.width / 2, clientY: r.top + r.height / 2 });
    el.dispatchEvent(e);
    return e.defaultPrevented;
  });
  expect(prevented).toBe(false);
  await expect(page.locator('dialog[open]')).toHaveCount(0);

  // The API agrees: no features, plain 404s.
  const prof = await (await page.request.get('/api/profile')).json();
  expect(prof.features).not.toHaveProperty('notes');
  expect(prof.features).not.toHaveProperty('reports');
  for (const p of ['/api/annotations', '/api/reports']) {
    expect((await page.request.get(p)).status()).toBe(404);
  }
});

test('old #n=1 and #rp=1 bookmarks open Details', async ({ page }) => {
  await page.goto('/#r=30m&k=tcp&o=1&n=1&rp=1&v=c');
  await expect(page.locator('#panels canvas:visible').first()).toBeVisible();
  await expect(page.locator('#outages')).toHaveAttribute('open', '');
  await page.waitForLoadState('networkidle');
  // The hash is rewritten without the old keys.
  await expect(page).not.toHaveURL(/[#&](n|rp)=1/);
});

test('Speed: Test now and the queue still work', async ({ page }) => {
  await page.goto('/#v=speed');
  const now = page.getByRole('button', { name: 'Test now', exact: true });
  await expect(now).toBeEnabled();
  await now.click();
  await expect(page.getByRole('heading', { name: 'Test queue' })).toBeVisible();
  // Ours has Stop (or Cancel): the owner's controls render. Stop it, so the
  // specs after this one find the queue as it was.
  const mine = page.locator('.sp-queue li.you');
  await expect(mine).toHaveCount(1);
  await mine.getByRole('button', { name: /^(Stop|Cancel):/ }).click();
  await expect(page.locator('.sp-result', { hasText: 'The test was cancelled.' })).toBeVisible();
  await expect(mine).toBeHidden();
});
