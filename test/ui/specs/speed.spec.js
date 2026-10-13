// Details → Speed: Test now and the queue against the real server, and the
// speed formatter on both sides of 1 Gbit/s.
// @ts-check
const { test, expect } = require('./fixtures');

test('Speed: Test now and the queue work', async ({ page }) => {
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

test('speeds read in Mbit/s under 1000 and Gbit/s from it; a Mbit/s column stays in Mbit/s', async ({ page }) => {
  await page.goto('/');
  const got = await page.evaluate(async () => {
    const { fmtMbps } = await import('/static/core.js');
    const { readingCell } = await import('/static/speedmap.js');
    const bps = [null, 5.2e6, 248e6, 999.4e6, 999.6e6, 1234e6, 2.5e9];
    return {
      full: bps.map((b) => fmtMbps(b)),
      bare: bps.map((b) => fmtMbps(b, true)),
      cell: bps.map((b) => readingCell('Down', b, false, false).textContent),
    };
  });
  expect(got.full).toEqual(['—', '5.2 Mbit/s', '248 Mbit/s', '999 Mbit/s', '1.0 Gbit/s', '1.2 Gbit/s', '2.5 Gbit/s']);
  expect(got.bare).toEqual(['—', '5.2', '248', '999', '1000', '1234', '2500']);
  expect(got.cell).toEqual(got.bare);
});
