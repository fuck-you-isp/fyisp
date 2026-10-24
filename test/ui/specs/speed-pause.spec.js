// Details → Speed: pausing the schedule (api/speed/config {"pause"}).
// Pause schedule… asks how long; while paused the header says so instead of
// the next test, and Resume schedule brings it back. The pause is the
// server's (it survives a reload). The spec resumes at the end, so the specs
// after it find the schedule running.
// @ts-check
const { test, expect } = require('./fixtures');

// A failure half-way must not leave the schedule paused for the specs after.
test.afterEach(async ({ page }) => {
  await page.evaluate(async () => {
    const { apiWrite } = await import('/static/core.js');
    await apiWrite('POST', 'api/speed/config', { pause: false });
  });
});

test('Pause schedule for an hour, then resume', async ({ page }) => {
  await page.goto('/#v=speed');
  const state = page.locator('.sp-state');
  await expect(state).toContainText(/next test|testing now/);
  await page.getByRole('button', { name: 'Pause schedule…' }).click();
  const dlg = page.getByRole('dialog', { name: 'Pause scheduled speed tests?' });
  await expect(dlg).toBeVisible();
  await expect(dlg.getByRole('radio', { name: 'Until I resume them' })).toBeChecked();
  await dlg.getByRole('radio', { name: 'For 1 hour' }).check();
  await dlg.getByRole('button', { name: 'Pause', exact: true }).click();
  await expect(dlg).toHaveCount(0);
  await expect(state).toContainText('scheduled tests paused until');
  await expect(state).not.toContainText('next test');
  const resume = page.getByRole('button', { name: 'Resume schedule' });
  await expect(resume).toBeFocused();
  await expect(page.locator('.sp-note')).toContainText('Scheduled tests paused until');

  // The server keeps it: a reload still shows the pause.
  await page.reload();
  await expect(state).toContainText('scheduled tests paused until');

  await page.getByRole('button', { name: 'Resume schedule' }).click();
  await expect(state).not.toContainText('paused');
  await expect(page.getByRole('button', { name: 'Pause schedule…' })).toBeFocused();
  await expect(page.locator('.sp-note')).toContainText('Scheduled tests resumed.');
});

test('Pause until a time refuses a time in the past; Cancel changes nothing', async ({ page }) => {
  await page.goto('/#v=speed');
  await page.getByRole('button', { name: 'Pause schedule…' }).click();
  const dlg = page.getByRole('dialog', { name: 'Pause scheduled speed tests?' });
  await dlg.getByLabel('End time').fill('2020-01-01T10:00');
  await expect(dlg.getByRole('radio', { name: 'Until a time' })).toBeChecked();
  await dlg.getByRole('button', { name: 'Pause', exact: true }).click();
  const again = page.getByRole('dialog', { name: 'Pause scheduled speed tests?' });
  await expect(again.getByRole('alert')).toHaveText('Pick a time between 1 minute and 30 days from now.');
  await expect(again.getByLabel('End time')).toBeFocused();
  await expect(again.getByLabel('End time')).toHaveAttribute('aria-invalid', 'true');
  await again.getByRole('button', { name: 'Cancel' }).click();
  await expect(again).toHaveCount(0);
  await expect(page.locator('.sp-state')).not.toContainText('paused');
  await expect(page.getByRole('button', { name: 'Pause schedule…' })).toBeFocused();
});
