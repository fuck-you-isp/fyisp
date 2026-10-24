// Details → Storage: campaigns (api/storage/run, campaign/*). Each test runs
// its own fyisp-webdev -storage targets: the network is faked, a host with
// "down" in it times out and one with "auth" refuses keys, and rounds wait
// in the queue with the fake speed tests.
// @ts-check
const { test, expect, webdev } = require('./fixtures');
const AxeBuilder = require('@axe-core/playwright').default;
const { setTheme, WIDTHS, THEMES } = require('./home');

// Refused keys answer 400 keys_refused, on purpose.
test.use({ failOk: [/status of 400/] });

/** @param {number} port */
async function start(port) {
  try {
    return await webdev(port, ['-storage', 'targets', '-no-speed']);
  } catch (e) {
    test.skip(/ENOENT/.test(String(e)), 'fyisp-webdev is not installed here (the uitest image has it)');
    throw e;
  }
}

/** @param {import('@playwright/test').Page} page */
async function axe(page) {
  const r = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa']).analyze();
  return r.violations.map((v) => `${v.id}: ${v.nodes.map((n) => n.target.join(' ')).slice(0, 4).join(' | ')}`);
}

test('one round now: the grid, a failed cell and its attempts', async ({ page }) => {
  test.setTimeout(120e3);
  const port = 3221;
  const proc = await start(port);
  try {
    await page.goto(`http://127.0.0.1:${port}/#v=storage`);
    const head = page.locator('.st-head');
    await expect(head).toContainText('Storage tests: Idle');
    await expect(page.locator('.st-nores')).toContainText('No results yet');
    await head.getByRole('button', { name: 'Run one round now…' }).click();
    const dlg = page.getByRole('dialog', { name: 'Run one round now' });
    await expect(dlg.getByRole('checkbox', { name: 'NAS' })).toBeChecked();
    await dlg.getByRole('checkbox', { name: 'Wrong keys' }).uncheck();
    await dlg.getByRole('button', { name: 'Run round' }).click();
    await expect(dlg).toHaveCount(0);
    // It ran (10 fresh connections a second apart, then 5 reused ones per
    // target): the history has it, and the grid a column per round.
    await expect(page.locator('.st-hist')).toContainText('On demand', { timeout: 60e3 });
    const grid = page.getByRole('grid', { name: 'Rounds' });
    await expect(grid.locator('tbody tr')).toHaveCount(4);
    await expect(grid.getByRole('gridcell')).toHaveCount(4);
    const flaky = grid.getByRole('gridcell', { name: /^Flaky edge, round 1 at .*: failed\. \d+ of \d+ attempts: No answer\.$/ });
    await expect(flaky).toHaveText('✕');
    await expect(grid.getByRole('gridcell', { name: /^AWS Virginia, round 1 at .*: all worked\.$/ })).toBeVisible();
    // One tab stop, reached with Tab; arrows move; Enter opens the round.
    await expect(grid.locator('td[tabindex="0"]')).toHaveCount(1);
    await page.getByRole('button', { name: 'Show as table' }).focus();
    await page.keyboard.press('Tab');
    await expect(grid.getByRole('gridcell', { name: /^AWS Virginia, round 1/ })).toBeFocused();
    await page.keyboard.press('ArrowDown');
    await page.keyboard.press('ArrowDown');
    await page.keyboard.press('ArrowDown');
    await expect(flaky).toBeFocused();
    await page.keyboard.press('Enter');
    const detail = page.getByRole('heading', { name: /^Flaky edge · round 1 · / });
    await expect(detail).toBeFocused();
    await expect(page.locator('.st-detail')).toContainText('No answer');
    await expect(page.locator('.st-log')).toContainText('No answer');
    // The evidence: a finding (too early after one round), each target's card,
    // the routes traced after the first round, and the report.
    await expect(page.locator('.st-found')).toContainText('Too early to tell');
    await expect(page.locator('.st-card', { hasText: 'Flaky edge' })).toContainText('failed');
    await expect(page.locator('.st-traces')).toContainText('first round', { timeout: 20e3 });
    await page.locator('.st-report summary').click();
    await expect(page.getByRole('link', { name: 'Download CSV' })).toHaveAttribute('href', /format=csv/);
    await page.screenshot({ path: 'out/shots/storage-round.png', fullPage: true });
  } finally {
    proc.kill();
  }
});

test('a campaign: refused keys, then start, pause, resume and stop; a schedule', async ({ page }) => {
  test.setTimeout(180e3);
  const port = 3222;
  const proc = await start(port);
  try {
    await page.goto(`http://127.0.0.1:${port}/#v=storage`);
    await page.getByRole('button', { name: 'New campaign…' }).click();
    await expect(page.getByRole('heading', { name: 'New campaign' })).toBeFocused();
    // No target form opens beside it.
    await expect(page.getByRole('button', { name: 'Edit NAS' })).toHaveCount(0);
    await expect(page.locator('.st-targets .st-add')).toHaveCount(0);
    const form = page.locator('.st-cform');
    for (const name of ['Backups \\(R2\\)', 'NAS', 'Flaky edge']) await form.getByRole('checkbox', { name: new RegExp(`^${name} `) }).uncheck();
    await expect(form.getByRole('group', { name: 'Keys for Wrong keys' })).toBeVisible();
    await expect(form.locator('.st-est')).toContainText('Each round:');
    const keys = form.getByRole('group', { name: 'Keys for Wrong keys' });
    await keys.getByLabel('Access key ID').fill('AKIAEXAMPLE1234');
    await keys.getByLabel('Secret access key').fill('not-the-secret');
    await form.getByLabel('Rounds', { exact: true }).fill('3');
    await form.getByLabel('Every (minutes)').fill('2');
    await form.getByRole('button', { name: 'Start campaign' }).click();
    await expect(keys.locator('.st-err')).toContainText('These keys were refused');
    await expect(keys.getByLabel('Access key ID')).toBeFocused();
    await expect(keys.getByLabel('Secret access key')).toHaveValue('');
    await keys.getByLabel('Access key ID').fill('');
    await form.getByRole('button', { name: 'Start campaign' }).click();
    const head = page.locator('.st-head');
    await expect(head).toContainText('Campaign running');
    await expect(page.locator('#storage > [role="status"]')).toContainText('Campaign started');
    // A grid cell keeps the focus while the results redraw: paused from
    // elsewhere (as by another tab), the campaign's new state redraws them.
    const grid = page.getByRole('grid', { name: 'Rounds' });
    await expect(grid.getByRole('gridcell').first()).toBeVisible({ timeout: 60e3 });
    await grid.locator('td[tabindex="0"]').focus();
    await page.evaluate(async () => {
      document.querySelector('.st-res-head')?.setAttribute('data-was', '');
      const token = document.querySelector('meta[name="fyisp-token"]')?.getAttribute('content') || '';
      const s = await (await fetch('api/storage')).json();
      const r = await fetch(`api/storage/campaign/pause?id=${s.live[0].id}`, { method: 'POST', headers: { 'X-FYISP-Token': token } });
      if (!r.ok) throw new Error(`pause: ${r.status}`);
    });
    await expect(head).toContainText('Campaign paused', { timeout: 30e3 });
    await expect(page.locator('.st-res-head[data-was]')).toHaveCount(0, { timeout: 30e3 });
    await expect(grid.locator('td.st-c:focus')).toHaveCount(1);

    // Paused, so nothing redraws while axe looks.
    await expect(page.locator('.st-panel')).toBeHidden({ timeout: 60e3 });
    for (const w of WIDTHS) {
      await page.setViewportSize({ width: w, height: 900 });
      for (const theme of THEMES) {
        await setTheme(page, theme);
        expect(await axe(page), `running ${w}px ${theme}`).toEqual([]);
        const { sw, iw } = await page.evaluate(() => ({ sw: document.documentElement.scrollWidth, iw: window.innerWidth }));
        expect(sw, `running ${w}px: no sideways scrolling`).toBeLessThanOrEqual(iw);
      }
    }
    await page.setViewportSize({ width: 1400, height: 900 });
    await page.screenshot({ path: 'out/shots/storage-campaign.png', fullPage: true });

    await head.getByRole('button', { name: 'Resume' }).click();
    await expect(head).toContainText('Campaign running');
    await expect(head.getByRole('button', { name: 'Pause' })).toBeFocused();
    await head.getByRole('button', { name: 'Pause' }).click();
    await expect(head).toContainText('Campaign paused');
    await expect(head.getByRole('button', { name: 'Resume' })).toBeFocused();
    await head.getByRole('button', { name: 'Resume' }).click();
    await expect(head).toContainText('Campaign running');
    await head.getByRole('button', { name: 'Stop campaign…' }).click();
    const stop = page.getByRole('dialog', { name: 'Stop this campaign?' });
    await expect(stop.getByRole('button', { name: 'Keep it' })).toBeFocused();
    await stop.getByRole('button', { name: 'Stop campaign' }).click();
    await expect(head).toContainText('Storage tests: Idle');
    await expect(head.getByRole('button', { name: 'New campaign…' })).toBeFocused();

    // A schedule: every day, all day (so it is inside its window now).
    await head.getByRole('button', { name: 'New campaign…' }).click();
    await form.getByRole('radio', { name: /Recurring/ }).check();
    await form.getByRole('button', { name: 'Every day' }).click();
    await form.getByLabel('From', { exact: true }).fill('00:00');
    await form.getByLabel('To', { exact: true }).fill('23:59');
    await expect(form.getByRole('button', { name: 'Start schedule' })).toBeVisible();
    await expect(form.locator('.st-est')).toContainText('Each window:');
    await form.getByRole('button', { name: 'Start schedule' }).click();
    await expect(head).toContainText(/Schedule (running|on)/);
    await head.getByRole('button', { name: 'Remove schedule…' }).click();
    await page.getByRole('dialog', { name: 'Remove the schedule?' }).getByRole('button', { name: 'Remove schedule' }).click();
    await expect(head).toContainText('Storage tests: Idle');
    await expect(page.locator('.st-hist tbody tr')).toHaveCount(2);

    // Escape on an untouched form goes back to its button; a touched one
    // asks, and Discard drops what was typed in the keys.
    await head.getByRole('button', { name: 'New campaign…' }).click();
    await page.keyboard.press('Escape');
    await expect(head.getByRole('button', { name: 'New campaign…' })).toBeFocused();
    await head.getByRole('button', { name: 'New campaign…' }).click();
    // Changing a field picks its choice.
    await form.getByLabel('Hours').fill('3');
    await expect(form.getByRole('radio', { name: 'For a while', exact: true })).toBeChecked();
    await form.getByLabel('Start time').fill('2030-01-01T09:00');
    await expect(form.getByRole('radio', { name: 'Start at', exact: true })).toBeChecked();
    const k2 = form.getByRole('group', { name: 'Keys for AWS Virginia' });
    await k2.getByLabel('Access key ID').fill('AKIAEXAMPLE9999');
    await k2.getByLabel('Secret access key').fill('typed-then-cancelled');
    await page.keyboard.press('Escape');
    const discard = page.getByRole('dialog', { name: 'Discard this campaign?' });
    await expect(discard.getByRole('button', { name: 'Keep editing' })).toBeFocused();
    await discard.getByRole('button', { name: 'Keep editing' }).click();
    await expect(k2.getByLabel('Secret access key')).toBeFocused();
    await page.keyboard.press('Escape');
    await discard.getByRole('button', { name: 'Discard' }).click();
    await expect(head.getByRole('button', { name: 'New campaign…' })).toBeFocused();
    await head.getByRole('button', { name: 'New campaign…' }).click();
    await expect(form.getByRole('group', { name: 'Keys for AWS Virginia' }).getByLabel('Secret access key')).toHaveValue('');
    await expect(form.getByRole('group', { name: 'Keys for AWS Virginia' }).getByLabel('Access key ID')).toHaveValue('');
  } finally {
    proc.kill();
  }
});

test('keys a campaign holds: the targets table, Delete, Edit and Forget keys', async ({ page }) => {
  test.setTimeout(120e3);
  const port = 3223;
  const proc = await start(port);
  try {
    await page.goto(`http://127.0.0.1:${port}/#v=storage`);
    const head = page.locator('.st-head');
    await page.getByRole('button', { name: 'New campaign…' }).click();
    const form = page.locator('.st-cform');
    for (const name of ['Backups \\(R2\\)', 'NAS', 'Flaky edge', 'Wrong keys']) await form.getByRole('checkbox', { name: new RegExp(`^${name} `) }).uncheck();
    const keys = form.getByRole('group', { name: 'Keys for AWS Virginia' });
    await keys.getByLabel('Access key ID').fill('AKIAEXAMPLE1234');
    await keys.getByLabel('Secret access key').fill('example-secret');
    await form.getByRole('button', { name: 'Start campaign' }).click();
    await expect(head).toContainText('Campaign running');
    await head.getByRole('button', { name: 'Pause' }).click();
    await expect(head).toContainText('Campaign paused');

    // The row says a campaign holds keys and uses it.
    const row = page.locator('.st-list tr[data-id]', { hasText: 'AWS Virginia' });
    await expect(row.locator('.st-keycol')).toHaveText('In memory · key …1234');
    await expect(row).toContainText('in a live campaign');
    await expect(page.locator('.st-list tr[data-id]', { hasText: 'NAS' }).locator('.st-keycol')).toHaveText('None');

    // Delete explains instead of asking.
    await page.getByRole('button', { name: 'Delete AWS Virginia' }).click();
    await expect(page.getByRole('dialog')).toHaveCount(0);
    await expect(page.locator('.st-targets [role="alert"]')).toHaveText('A live campaign uses this target: stop it first.');
    await expect(page.getByRole('button', { name: 'Delete AWS Virginia' })).toBeFocused();

    // The edit form warns that a change to the endpoint deletes the keys.
    await page.getByRole('button', { name: 'Edit AWS Virginia' }).click();
    await expect(page.locator('.st-keys-note')).toContainText('deletes them');
    await page.keyboard.press('Escape');
    await expect(page.getByRole('button', { name: 'Edit AWS Virginia' })).toBeFocused();

    // Forget keys asks (Cancel first), then the campaign waits for keys.
    await page.getByRole('button', { name: 'Forget the keys for AWS Virginia' }).click();
    const dlg = page.getByRole('dialog', { name: 'Forget the keys for AWS Virginia?' });
    await expect(dlg.getByRole('button', { name: 'Cancel' })).toBeFocused();
    await dlg.getByRole('button', { name: 'Forget keys' }).click();
    await expect(row.locator('.st-keycol')).toHaveText('None');
    await expect(row.locator('th')).toBeFocused();
    await expect(page.getByRole('button', { name: 'Forget the keys for AWS Virginia' })).toHaveCount(0);
    await expect(page.locator('.st-wait')).toContainText('AWS Virginia');
    await expect(page.locator('#storage > [role="status"]')).toHaveText('Forgot the keys for AWS Virginia.');

    // Stopped: the in-use alert goes with it.
    await page.getByRole('button', { name: 'Delete AWS Virginia' }).click();
    await expect(page.locator('.st-targets [role="alert"]')).toBeVisible();
    await head.getByRole('button', { name: 'Stop campaign…' }).click();
    await page.getByRole('dialog', { name: 'Stop this campaign?' }).getByRole('button', { name: 'Stop campaign' }).click();
    await expect(head).toContainText('Storage tests: Idle');
    await expect(page.locator('.st-targets [role="alert"]')).toBeHidden();
    await expect(row).not.toContainText('in a live campaign');
  } finally {
    proc.kill();
  }
});
