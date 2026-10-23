// Investigate traces any public host typed in its host box, with the
// picked probe type. The trace is named "host:<host>": the URL reopens it,
// and the mtr view, the report and "Run it yourself" work as for a target.
// A refusal is shown under the box with the server's reason. A pasted
// host: link never starts a trace by itself: Trace does.
// @ts-check
const { test, expect } = require('./fixtures');

test('the host box traces a public host, and the URL reopens it', async ({ page }) => {
  /** @type {string[]} */
  const posts = [];
  // The answer is fixed: other specs may hold every slot of fyisp-webdev.
  await page.route('**/api/investigate?*', (route) => {
    posts.push(new URL(route.request().url()).search);
    return route.fulfill({ json: { target: 'host:example.com', mode: 'tcp', state: 'running', rounds_done: 0, rounds_max: 250, interval_ms: 2500, started_at: Date.now() } });
  });
  await page.goto('/#' + new URLSearchParams({ t: 'Google DNS 8.8.8.8', r: '1h' }));
  const inv = page.locator('#inv');
  await inv.locator('.inv-modes').getByRole('button', { name: 'TCP 443' }).click();
  const box = inv.getByRole('textbox', { name: 'Host to trace' });
  await box.fill('Example.com');
  await box.press('Enter');
  await expect(inv.locator('.inv-status .pill.on', { hasText: /^Tracing… \d+ of 250 rounds$/ })).toBeVisible();
  // Switching the target to TCP 443 starts its own trace first.
  expect(posts).toEqual(['?target=Google+DNS+8.8.8.8&mode=tcp', '?host=Example.com&mode=tcp']);
  await expect(page).toHaveURL(/#.*t=host%3Aexample\.com/);
  await expect(inv.locator('.inv-pick')).toHaveValue('host:example.com');
  await expect(inv.locator('.inv-pick option:checked')).toHaveText('example.com');
  await expect(inv.locator('.inv-run .inv-cmd')).toHaveText('mtr -T -P 443 -rwzbc 100 example.com');
  await expect(inv.locator('.inv-host-err')).toBeEmpty();

  // Reopened from the URL: not started by itself.
  await page.goto('/#' + new URLSearchParams({ t: 'host:example.com' }));
  await expect(inv.locator('.inv-status .pill', { hasText: 'Not tracing' })).toBeVisible();
  await expect(box).toHaveValue('example.com');
  await expect(inv.locator('.inv-run .inv-cmd')).toHaveText('mtr -rwzbc 100 example.com');
  expect(posts).toHaveLength(2);
});

test('a pasted host: link never starts a trace by itself; Trace does', async ({ page }) => {
  /** @type {string[]} */
  const posts = [];
  await page.route('**/api/investigate?*', (route) => {
    if (route.request().method() === 'POST') posts.push(new URL(route.request().url()).search);
    return route.fulfill({ json: { target: 'host:example.com', mode: 'icmp', state: 'running', rounds_done: 0, rounds_max: 250, interval_ms: 2500, started_at: Date.now() } });
  });
  await page.goto('/#' + new URLSearchParams({ t: 'host:example.com' }));
  const st = page.locator('#inv .inv-status');
  await expect(st.locator('.pill', { hasText: 'Not tracing' })).toBeVisible();
  await expect(page.locator('#inv .inv-summary')).toContainText('Not tracing. Click Trace');
  await page.waitForLoadState('networkidle');
  expect(posts).toEqual([]);

  await st.getByRole('button', { name: 'Trace', exact: true }).click();
  await expect(st.locator('.pill.on', { hasText: /^Tracing… \d+ of 250 rounds$/ })).toBeVisible();
  expect(posts).toEqual(['?target=host%3Aexample.com&mode=icmp']);
});

test('a refused host shows the server\'s reason under the box', async ({ page, failOk }) => {
  failOk.push(/\/api\/investigate\b/);
  await page.goto('/#' + new URLSearchParams({ t: 'Google DNS 8.8.8.8', r: '1h' }));
  const inv = page.locator('#inv');
  const box = inv.getByRole('textbox', { name: 'Host to trace' });
  const err = inv.locator('.inv-host-err');

  // Checked by fyisp-webdev itself (syntax comes before the limits).
  await box.fill('not a host');
  await inv.getByRole('button', { name: 'Trace host' }).click();
  await expect(err).toHaveText('Can’t trace this host: not a host name or IPv4 address.');
  await expect(box).toHaveAttribute('aria-invalid', 'true');
  await expect(page).not.toHaveURL(/host%3A/);

  // A name on the LAN (answered here: the slots may be taken by other specs).
  await page.route('**/api/investigate?*', (route) => route.fulfill({ status: 403,
    json: { code: 'not_public', error: 'nas.lan resolves to a private, local or otherwise non-public address: only public hosts can be traced' } }));
  await box.fill('nas.lan');
  await box.press('Enter');
  await expect(err).toHaveText('Can’t trace this host: nas.lan resolves to a private, local or otherwise non-public address: only public hosts can be traced.');
  await expect(inv.locator('.inv-pick')).toHaveValue('Google DNS 8.8.8.8');
});
