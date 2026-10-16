// Details > Charts: an incident naming services (a service verdict) is marked
// only on the panels that draw one of them; a connection-wide one (ISP, no
// targets) on every panel, its tooltip line the verdict's headline. Each scenario is its
// own fyisp-webdev -verdict, which keeps the incident going since 13 min ago:
// the right of the 30 min chart is inside it. Screenshots go to
// out/shots/charts-incident-<kind>.png.
// @ts-check
const { test, expect, webdev } = require('./fixtures');

/** The tooltip's incident lines when hovering near the right of a panel's chart. @param {import('@playwright/test').Page} page @param {string} group */
async function incidentLines(page, group) {
  const over = page.locator(`#panel-${group} .u-over`);
  await over.scrollIntoViewIfNeeded();
  const box = await over.boundingBox();
  if (!box) throw new Error(`no chart in #panel-${group}`);
  await page.mouse.move(box.x + box.width * 0.9, box.y + box.height / 2);
  await expect(page.locator('#tip')).toBeVisible();
  const lines = await page.locator('#tip .row.inc').allTextContents();
  await page.mouse.move(0, 0);
  return lines;
}

/** Opens the chart wall of a fresh fyisp-webdev -verdict kind on port. @param {import('@playwright/test').Page} page @param {number} port @param {string} kind */
async function openCharts(page, port, kind) {
  let proc;
  try {
    proc = await webdev(port, ['-verdict', kind]);
  } catch (e) {
    test.skip(/ENOENT/.test(String(e)), 'fyisp-webdev is not installed here (the uitest image has it)');
    throw e;
  }
  await page.goto(`http://127.0.0.1:${port}/#v=c`);
  await expect(page.locator('#panel-call-apps .u-over')).toBeVisible();
  await expect(page.locator('#panel-work .u-over')).toBeVisible();
  await page.waitForLoadState('networkidle');
  return proc;
}

test('a service incident is marked only on the panel with the service', async ({ page }) => {
  const proc = await openCharts(page, 3301, 'service');
  try {
    // Webex and Discord are call apps; the work panel has neither.
    expect(await incidentLines(page, 'call-apps')).toEqual(['incident: 2 services look affected: Discord and Webex.']);
    expect(await incidentLines(page, 'work')).toEqual([]);
    await page.locator('#panel-call-apps').screenshot({ path: 'out/shots/charts-incident-service.png' });
    await page.locator('#panel-work').screenshot({ path: 'out/shots/charts-incident-service-other.png' });
  } finally {
    proc.kill();
  }
});

test('a connection-wide incident is marked on every panel', async ({ page }) => {
  const proc = await openCharts(page, 3302, 'isp');
  try {
    for (const group of ['call-apps', 'work']) {
      expect(await incidentLines(page, group)).toEqual(['Internet provider problem']);
    }
    await page.locator('#panel-work').screenshot({ path: 'out/shots/charts-incident-isp.png' });
  } finally {
    proc.kill();
  }
});

test.describe('a locale tag Intl rejects', () => {
  test.use({ locale: 'en-US@posix' });

  test('Home and the charts still render', async ({ page }) => {
    await page.goto('/');
    await expect(page.locator('.hp-tile')).toHaveCount(11);
    await page.goto('/#v=c');
    await expect(page.locator('#panels canvas:visible').first()).toBeVisible();
    await page.waitForLoadState('networkidle');
  });
});
