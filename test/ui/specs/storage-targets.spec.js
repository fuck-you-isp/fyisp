// Details → Storage: the targets (api/storage). Each test runs its own
// fyisp-webdev -storage (a temporary targets file, a faked network: names
// ending .lan are on the LAN, "down" in a host times out), so what it adds
// or deletes never reaches the other specs.
// @ts-check
const { test, expect, webdev } = require('./fixtures');

// The second "Lab" is refused with 409 name_taken, on purpose.
test.use({ failOk: [/status of 409/] });

/** Starts fyisp-webdev -storage scenario on port, or skips without it. @param {number} port @param {string} scenario */
async function start(port, scenario) {
  try {
    return await webdev(port, ['-storage', scenario]);
  } catch (e) {
    test.skip(/ENOENT/.test(String(e)), 'fyisp-webdev is not installed here (the uitest image has it)');
    throw e;
  }
}

test('empty state, then add, check, edit and delete a target', async ({ page }) => {
  const port = 3201;
  const proc = await start(port, 'empty');
  try {
    await page.goto(`http://127.0.0.1:${port}/#v=storage`);
    await expect(page.getByRole('button', { name: 'Storage', pressed: true })).toBeVisible();
    const head = page.locator('.st-head');
    await expect(head.getByRole('heading', { name: 'Storage tests' })).toBeVisible();
    await expect(head).toContainText('A check sends one request now.');
    await head.getByRole('button', { name: 'Add a target' }).click();
    await expect(head.getByRole('button', { name: 'Add a target' })).toHaveCount(0);

    const form = page.locator('.st-form');
    await expect(page.getByRole('heading', { name: 'Add a storage target' })).toBeFocused();
    // The browser's own checks come first, with the focus on the field.
    await form.getByRole('button', { name: 'Save target' }).click();
    await expect(form.getByLabel('Name', { exact: true })).toBeFocused();
    await expect(form.getByLabel('Name', { exact: true })).toHaveAttribute('aria-invalid', 'true');
    await expect(form.locator('#st-f-name-err')).toHaveText('Give the target a name.');
    await form.getByLabel('Name', { exact: true }).fill('Lab');
    await form.getByLabel('Endpoint').fill('https://s3.eu-central-1.example.net/bucket');
    await form.getByRole('button', { name: 'Save target' }).click();
    await expect(form.locator('#st-f-endpoint-err')).toHaveText('Leave out the path; just https://host[:port].');
    await expect(form.getByLabel('Name', { exact: true })).not.toHaveAttribute('aria-invalid', 'true');

    // The addressing choice only with a bucket; a dotted bucket in the host name is warned about.
    await form.getByLabel('Endpoint').fill('https://s3.eu-central-1.example.net');
    await expect(form.getByRole('group', { name: 'Bucket address' })).toBeHidden();
    await form.getByLabel('Bucket (optional)').fill('my.bucket');
    await expect(form.getByRole('group', { name: 'Bucket address' })).toBeVisible();
    await expect(form.locator('.st-preview')).toHaveText('Requests go to: https://my.bucket.s3.eu-central-1.example.net/');
    await expect(form.getByText('Bucket names with dots break the certificate check')).toBeVisible();
    await form.getByRole('radio', { name: /In the path/ }).check();
    await expect(form.getByText('Bucket names with dots break the certificate check')).toBeHidden();
    await expect(form.locator('.st-preview')).toHaveText('Requests go to: https://s3.eu-central-1.example.net/my.bucket/');
    await form.getByRole('button', { name: 'Save target' }).click();

    // Saved: the list, with the new row focused and announced.
    const row = page.locator('.st-list tbody tr[data-id]');
    await expect(row).toHaveCount(1);
    await expect(row.locator('th')).toBeFocused();
    await expect(row).toContainText('s3.eu-central-1.example.net');
    await expect(row).toContainText('us-east-1');
    await expect(page.locator('#storage [role="status"]')).toHaveText('Saved Lab.');
    await expect(page.locator('.st-head')).toContainText('1 target');

    // The same name again is refused by fyisp, on the field.
    await page.getByRole('button', { name: 'Add a target' }).click();
    await form.getByLabel('Name', { exact: true }).fill('lab');
    await form.getByLabel('Endpoint').fill('http://nas.lan:9000');
    await form.getByRole('button', { name: 'Save target' }).click();
    await expect(form.locator('#st-f-name-err')).toHaveText('Another target is already called lab.');
    await expect(form.getByLabel('Name', { exact: true })).toBeFocused();
    await form.getByLabel('Name', { exact: true }).fill('NAS');
    await form.getByRole('button', { name: 'Save target' }).click();
    await expect(row).toHaveCount(2);
    await expect(page.locator('tr[data-id]', { hasText: 'NAS' })).toContainText('on your network');
    await expect(page.locator('tr[data-id]', { hasText: 'NAS' })).toContainText('http://nas.lan:9000');

    // Check: one request; the result under the row, the focus kept, while it runs too.
    await page.getByRole('button', { name: 'Check Lab' }).click();
    await expect(page.getByRole('button', { name: 'Checking Lab' })).toHaveText('Checking…');
    await expect(page.getByRole('button', { name: 'Checking Lab' })).toBeFocused();
    await expect(page.getByRole('button', { name: 'Check Lab' })).toHaveText('Check');
    await expect(page.getByRole('button', { name: 'Check Lab' })).toBeFocused();
    const result = page.locator('.st-check').first();
    await expect(result).toContainText('Connected.');
    await expect(result).toContainText('Name lookup 3.20 ms, connect 11.4 ms, encryption 23.1 ms, first byte 31.0 ms.');
    await expect(result).toContainText('It answered (403), as expected without keys.');
    await expect(page.locator('#storage [role="status"]')).toHaveText('Lab: Connected.');

    // Edit: a new region drops the last check.
    await page.getByRole('button', { name: 'Edit Lab' }).click();
    await expect(page.getByRole('heading', { name: 'Edit Lab' })).toBeFocused();
    await expect(form.getByLabel('Name', { exact: true })).toHaveValue('Lab');
    await expect(form.getByRole('radio', { name: /In the path/ })).toBeChecked();
    await form.getByLabel('Region (optional)').fill('eu-central-1');
    await form.getByRole('button', { name: 'Save changes' }).click();
    await expect(page.locator('tr[data-id]', { hasText: 'Lab' })).toContainText('eu-central-1');
    await expect(page.locator('.st-check')).toHaveCount(0);

    // Escape on an untouched form goes back to its button; a touched one asks.
    await page.getByRole('button', { name: 'Edit NAS' }).click();
    await page.keyboard.press('Escape');
    await expect(page.getByRole('button', { name: 'Edit NAS' })).toBeFocused();
    await page.getByRole('button', { name: 'Edit NAS' }).click();
    await form.getByLabel('Name', { exact: true }).fill('NAS 2');
    await page.keyboard.press('Escape');
    const discard = page.getByRole('dialog', { name: 'Discard your changes?' });
    await expect(discard.getByRole('button', { name: 'Keep editing' })).toBeFocused();
    await discard.getByRole('button', { name: 'Discard' }).click();
    await expect(page.getByRole('button', { name: 'Edit NAS' })).toBeFocused();

    // Delete asks first (Cancel is the safe choice), then the focus moves on.
    await page.getByRole('button', { name: 'Delete Lab' }).click();
    const dlg = page.getByRole('dialog', { name: 'Delete Lab?' });
    await expect(dlg.getByRole('button', { name: 'Cancel' })).toBeFocused();
    await dlg.getByRole('button', { name: 'Cancel' }).click();
    await expect(row).toHaveCount(2);
    await page.getByRole('button', { name: 'Delete Lab' }).click();
    await dlg.getByRole('button', { name: 'Delete target' }).click();
    await expect(row).toHaveCount(1);
    await expect(page.locator('tr[data-id] th', { hasText: 'NAS' })).toBeFocused();
    await expect(page.locator('#storage [role="status"]')).toHaveText('Deleted Lab.');

    // It all survives a reload (the server's file).
    await page.reload();
    await expect(row).toHaveCount(1);
  } finally {
    proc.kill();
  }
});

test('a failed check says why; the hash and the view bar', async ({ page }) => {
  const port = 3202;
  const proc = await start(port, 'targets');
  try {
    await page.goto(`http://127.0.0.1:${port}/#v=c`);
    await page.getByRole('button', { name: 'Storage' }).click();
    await expect(page).toHaveURL(/v=storage/);
    await expect(page.locator('#ranges')).toBeHidden();
    await expect(page.locator('.st-list tbody tr[data-id]')).toHaveCount(5);
    await expect(page.locator('#storage')).not.toContainText('null');
    await page.getByRole('button', { name: 'Check Flaky edge' }).click();
    const bad = page.locator('.st-check.bad');
    await expect(bad).toContainText('Couldn’t connect: no answer in time.');
    await expect(bad.locator('.notice.danger')).toBeVisible();
  } finally {
    proc.kill();
  }
});

test('without storage tests there is no Storage button', async ({ page }) => {
  const port = 3203;
  const proc = await start(port, 'none');
  try {
    await page.goto(`http://127.0.0.1:${port}/#v=storage`);
    await expect(page.locator('.viewbar')).toBeVisible();
    await expect(page.getByRole('button', { name: 'Storage' })).toHaveCount(0);
    await expect(page.locator('#storage')).toHaveCount(0);
  } finally {
    proc.kill();
  }
});
