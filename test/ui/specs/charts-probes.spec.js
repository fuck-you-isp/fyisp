// Details > Charts: with one target shown, "Probes over X ms" counts and marks
// that target's probes slower than X (api/probes). fyisp-webdev -spikes puts
// fixed spikes on Microsoft-Teams (about 50 ms normally): +1 s at 5, 12 and
// 120 min ago, +10-50 ms at 3-25 and 90 min ago, a timeout at 10 min ago.
// Screenshots: out/shots/charts-probes*.png.
// @ts-check
const { test, expect, webdev } = require('./fixtures');

const TARGET = 'Microsoft-Teams';

/** Opens the chart wall of a fresh fyisp-webdev -spikes on port, range r, with TARGET isolated. @param {import('@playwright/test').Page} page @param {number} port @param {string} r */
async function openIsolated(page, port, r) {
  let proc;
  try {
    proc = await webdev(port, ['-verdict', 'ok', '-spikes']);
  } catch (e) {
    test.skip(/ENOENT/.test(String(e)), 'fyisp-webdev is not installed here (the uitest image has it)');
    throw e;
  }
  await page.goto(`http://127.0.0.1:${port}/#v=c&r=${r}&k=tcp`);
  const panel = page.locator('#panel-call-apps');
  await expect(panel.locator('.u-over')).toBeVisible();
  await expect(panel.locator('.probes')).toBeHidden();
  await panel.locator('.legend li', { hasText: TARGET }).click();
  await expect(panel.locator('.probes')).toBeVisible();
  return { proc, panel };
}

test('one target shown: count, mark and remember its probes over a threshold', async ({ page }) => {
  const { proc, panel } = await openIsolated(page, 3311, '30m');
  try {
    const input = panel.locator('.probes input');
    await expect(input).toHaveAttribute('placeholder', /\d/);
    await input.fill('200');
    await input.press('Enter');
    // Only the two +1 s spikes of the last 30 minutes are over 200 ms; both
    // are likely drops, and the timeout is lost.
    const count = panel.locator('.probes .pcount');
    const want = /^2 of [\d,]+ probes over 200 ms · in this range: 2 likely dropped packets \(1–1.4 s\), \d+ lost$/;
    await expect(count).toHaveText(want);
    await expect(panel.locator('.probes [role=status]')).toHaveText(want);
    await panel.screenshot({ path: 'out/shots/charts-probes.png' });

    // Raising it filters what is loaded; a value out of range is refused.
    await input.fill('2000');
    await expect(count).toHaveText(/^None of [\d,]+ probes over 2000 ms/);
    await input.fill('-1');
    await expect(input).toHaveAttribute('aria-invalid', 'true');
    await expect(count).toHaveText('Enter a number of milliseconds from 0 to 60000');

    // The threshold is kept per target: a reload shows it again.
    await input.fill('200');
    await input.press('Enter');
    await expect(count).toHaveText(/^2 of /);
    await page.reload();
    await expect(panel.locator('.u-over')).toBeVisible();
    await panel.locator('.legend li', { hasText: TARGET }).click();
    await expect(input).toHaveValue('200');

    // Showing every target again hides the control.
    await panel.locator('.legend li', { hasText: TARGET }).click();
    await expect(panel.locator('.probes')).toBeHidden();
  } finally {
    proc.kill();
  }
});

test('a longer range counts the older spike too', async ({ page }) => {
  const { proc, panel } = await openIsolated(page, 3312, '6h');
  try {
    const input = panel.locator('.probes input');
    await input.fill('200');
    await input.press('Enter');
    await expect(panel.locator('.probes .pcount')).toHaveText(/^3 of [\d,]+ probes over 200 ms · in this range: 3 likely dropped packets/);
    await panel.screenshot({ path: 'out/shots/charts-probes-6h.png' });
    // At 1-minute buckets the +1 s probes are above the axis: a caret at the
    // top of the plot, whose tooltip names the probe.
    const over = panel.locator('.u-over');
    const box = /** @type {{x:number,y:number,width:number,height:number}} */ (await over.boundingBox());
    let tip = '';
    for (let x = box.width - 2; x > 0 && !tip.includes('Likely a dropped packet'); x -= 3) {
      await page.mouse.move(box.x + x, box.y + 3);
      tip = (await page.locator('#tip').isVisible()) ? await page.locator('#tip').innerText() : '';
    }
    expect(tip).toContain(TARGET);
    expect(tip).toMatch(/1\.\d\d s/);
    expect(tip).toContain('Likely a dropped packet, sent again after 1 s');
    await page.mouse.move(0, 0);
  } finally {
    proc.kill();
  }
});

test('the network path has no probes control', async ({ page }) => {
  const { proc } = await openIsolated(page, 3313, '30m');
  try {
    const path = page.locator('#panel-path');
    await expect(path.locator('.u-over')).toBeVisible();
    await path.locator('.legend li').first().click();
    await expect(path.locator('.probes')).toBeHidden();
  } finally {
    proc.kill();
  }
});

test('a capped answer says it shows the slowest probes only', async ({ page }) => {
  await page.route('**/api/probes?*', async (route) => {
    const q = new URL(route.request().url()).searchParams;
    const to = Number(q.get('to'));
    await route.fulfill({ json: {
      target: TARGET, kind: 'tcp', from: Number(q.get('from')), to, over_ms: Number(q.get('over_ms')),
      n: 120, over: 40, complete_above_ms: 300, probes: [{ ts: to - 60e3, ms: 310 }, { ts: to - 120e3, ms: 320 }],
      likely_drop: 0, lost_total: 0, lost_by: {}, lost: [], lost_capped: false,
    } });
  });
  const { proc, panel } = await openIsolated(page, 3315, '30m');
  try {
    const input = panel.locator('.probes input');
    await input.fill('100');
    await input.press('Enter');
    await expect(panel.locator('.probes .pcount')).toHaveText('40 of 120 probes over 100 ms · showing the slowest 2');
  } finally {
    proc.kill();
  }
});

test.describe('when api/probes fails', () => {
  test.use({ failOk: [/api\/probes/] });
  test('the chart stays and the count says why', async ({ page }) => {
    await page.route('**/api/probes?*', (route) => route.fulfill({ status: 500, json: { error: 'query failed: disk on fire', code: 'error' } }));
    const { proc, panel } = await openIsolated(page, 3314, '30m');
    try {
      await expect(panel.locator('.probes .pcount')).toHaveText(/^Couldn’t count probes: /);
      await expect(panel.locator('.u-over')).toBeVisible();
    } finally {
      proc.kill();
    }
  });
});
