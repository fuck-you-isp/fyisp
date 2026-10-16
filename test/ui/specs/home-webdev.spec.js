// Home (v0.9.0) against the real /api/home: one fyisp-webdev -home per
// scenario (internal/web/homescen, the same ones the goldens and the
// fixtures come from), on its own port. The page must take the server's
// answer as is: the card's tone, every tile's state, the problems, and no
// console error. Times differ from the goldens (webdev runs now), so the
// words are left to the fixture specs. Screenshots go to
// out/shots/webdev-<scenario>.png.
// @ts-check
const { test, expect, webdev } = require('./fixtures');
const { fx, STATES } = require('./home');

STATES.forEach((name, i) => {
  test(`webdev -home ${name}`, async ({ page }) => {
    const port = 3101 + i;
    let proc;
    try {
      proc = await webdev(port, ['-home', name]);
    } catch (e) {
      test.skip(/ENOENT/.test(String(e)), 'fyisp-webdev is not installed here (the uitest image has it)');
      throw e;
    }
    try {
      const want = fx(`now-${name}`);
      const api = `http://127.0.0.1:${port}/api/home?part=now&tz=America/New_York`;
      await page.goto(`http://127.0.0.1:${port}/`);
      await expect(page.locator('#home.hp')).toBeVisible();
      await expect(page.locator('.hp-tile')).toHaveCount(11);
      await expect(page.locator('.hp-answer')).toHaveClass(new RegExp(`\\b${want.stale ? 'stale' : want.answer.tone}\\b`));
      // The page shows the server's headline (the scenario is rebuilt each minute: poll across a rollover).
      await expect.poll(async () => {
        const got = await (await fetch(api)).json();
        const shown = await page.locator('.hp-headline').textContent();
        return !!got.answer && (got.stale ? /^fyisp hasn’t checked since /.test(shown || '') : shown === got.answer.headline);
      }, { timeout: 20e3 }).toBe(true);
      for (const t of want.tiles) {
        const st = /^(good|slow|down|off)$/.test(t.state) ? t.state : 'none';
        await expect(page.locator(`.hp-tile[data-id="${t.id}"]`)).toHaveClass(new RegExp(`\\bis-${st}\\b`));
      }
      if (name !== 'dst') await expect(page.locator('.hp-problem')).toHaveCount(want.problems.items.length);
      await expect(page.locator('.hp-hop')).toHaveCount(3);
      await page.screenshot({ path: `out/shots/webdev-${name}.png`, fullPage: true });
    } finally {
      proc.kill();
    }
  });
});
