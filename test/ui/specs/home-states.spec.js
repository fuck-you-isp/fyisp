// Home (v0.9.0): every state renders the server's answer (card colour,
// headline, tab title, learning note, problems) and is screenshotted at
// 1400 and 390 px in both themes for review (not diffed).
// @ts-check
const { test, expect } = require('./fixtures');
const { fx, STATES, serveHome, openHome, setTheme, WIDTHS, THEMES } = require('./home');

for (const name of STATES) {
  test(`state ${name}`, async ({ page }) => {
    const d = fx(`now-${name}`);
    await serveHome(page, d);
    await openHome(page);
    const card = page.locator('.hp-answer');
    // Stale is the client's own answer (its sentence, its key); the rest is the server's.
    const tone = d.stale ? 'stale' : d.answer.tone;
    await expect(card).toHaveClass(new RegExp(`\\b${tone}\\b`));
    await expect(card.locator('.hp-headline')).toHaveText(d.stale ? 'fyisp hasn’t checked since 10:21 am.' : d.answer.headline);
    await expect(page).toHaveTitle(`${d.stale ? '? Not checking' : d.answer.tab} – fyisp`);
    if (d.answer.sub && !d.stale) await expect(card.locator('.hp-sub')).toHaveText(d.answer.sub);
    await expect(page.locator('.hp-learning')).toBeVisible({ visible: !!d.learning });
    await expect(page.locator('.hp-summary')).toHaveText(d.problems.summary);
    await expect(page.locator('.hp-problem')).toHaveCount(d.problems.items.length);
    await expect(page.locator('.hp-problem.live')).toHaveCount(d.problems.items.filter((/** @type {any} */ p) => p.ongoing).length);
    await expect(page.locator('.hp-hop')).toHaveCount(3);
    await expect(page.locator('.hp-foot-text')).toHaveText(d.footer);
    // The header is the brand and the theme button: no Home | Details switch.
    await expect(page.locator('#viewnav')).toBeHidden();
    // Each tile's accessible name is the server's short aria text.
    for (const t of d.tiles) {
      await expect(page.locator(`.hp-tile[data-id="${t.id}"]`)).toHaveAccessibleName(t.aria);
    }

    for (const w of WIDTHS) {
      await page.setViewportSize({ width: w, height: w > 500 ? 900 : 844 });
      for (const theme of THEMES) {
        await setTheme(page, theme);
        await page.screenshot({ path: `out/shots/home-${name}-${w}-${theme}.png`, fullPage: true });
      }
    }
  });
}

test('the stale card copy and dimmed tiles', async ({ page }) => {
  await serveHome(page, 'now-stale');
  await openHome(page);
  await expect(page.locator('.hp-sub')).toHaveText('Is the computer it runs on asleep or turned off? Everything below is from 10:21 am and may be out of date.');
  await expect(page.locator('.hp-checked')).toHaveText(/^Last checked 10:21 am, \d+ (minutes|hours) ago$/);
  await expect(page.locator('#home')).toHaveClass(/is-stale/);
  // Bars lose their colour, text keeps its contrast (no opacity).
  const f = await page.locator('.hp-tile .hp-bars').first().evaluate((el) => getComputedStyle(el).filter);
  expect(f).toBe('grayscale(1)');
  expect(await page.locator('.hp-tile').first().evaluate((el) => getComputedStyle(el).opacity)).toBe('1');
  // The tile descriptions say it's the last known state.
  await expect(page.locator('#hp-d-zoom')).toHaveText(/^Last known at 10:21 am: /);
});

test('checking: grey card, grey no-data bars', async ({ page }) => {
  await serveHome(page, 'now-checking');
  await openHome(page);
  await expect(page.locator('.hp-answer')).toHaveClass(/checking/);
  await expect(page.locator('.hp-checked')).toHaveText('');
  await expect(page.locator('.hp-day')).toBeHidden();
  await expect(page.locator('.hp-tile .hp-bars i.b-none')).toHaveCount(264);
});

test('off tiles: not checked, no bars, the counts come from the server', async ({ page }) => {
  const d = fx('now-calls-profile');
  await serveHome(page, d);
  await openHome(page);
  await expect(page.locator('.hp-sub')).toContainText('all 4 apps');
  const off = page.locator('.hp-tile.is-off');
  await expect(off).toHaveCount(7);
  await expect(page.locator('.hp-tile[data-id="github"]')).toHaveAccessibleName('GitHub: not checked on this setup');
  await expect(page.locator('.hp-tile[data-id="github"]')).toHaveAccessibleDescription(/^fyisp isn’t checking GitHub on this setup\. To check GitHub, add services-dev/);
  await expect(off.locator('.hp-bars i:not(.hp-base)')).toHaveCount(0);
  await expect(off.locator('.hp-bars [title]')).toHaveCount(0);
  await expect(off.locator('.hp-word').first()).toHaveText('Not checked');
  // Never tinted: the plain surface.
  const bg = await off.first().evaluate((el) => getComputedStyle(el).backgroundColor);
  const surface = await page.locator('.hp-tile[data-id="zoom"]').evaluate((el) => getComputedStyle(el).backgroundColor);
  expect(bg).toBe(surface);
  await expect(page.locator('.hp-tile:not(.is-off) .hp-bars i')).toHaveCount(4 * 24);
});

test('tiles: the bars, their titles and the scale', async ({ page }) => {
  await serveHome(page, 'now-zoom');
  await openHome(page);
  await expect(page.locator('.hp-tile .hp-bars i')).toHaveCount(264);
  const zoom = page.locator('.hp-tile[data-id="zoom"]');
  await expect(zoom).toHaveClass(/is-down/);
  await expect(zoom.locator('.hp-since')).toHaveText('since 10:05 am');
  // The bars run from 8:35 to the current one, 10:30–10:35 (the server's 5-minute buckets).
  await expect(zoom.locator('.hp-bars i').nth(18)).toHaveAttribute('title', '10:05–10:10 am: Down since 10:05 am');
  await expect(zoom.locator('.hp-bars i').nth(23)).toHaveAttribute('title', '10:30–10:35 am (now): Down since 10:05 am');
  await expect(zoom.locator('.hp-bars i').nth(0)).toHaveAttribute('title', '8:35–8:40 am: Good');
  await expect(zoom.locator('.hp-scale')).toHaveText('8:35now');
  await expect(zoom).toHaveAccessibleDescription(/^Not connecting since 10:05 am\. .* Last 2 hours: down or dropped in 6 of 24 five-minute periods\.$/);
  // Slack's 9:41 problem: a slow bar with the server's note.
  await expect(page.locator('.hp-tile[data-id="slack"] .hp-bars i').nth(13)).toHaveAttribute('title', '9:40–9:45 am: Choppy 9:41–9:45 am — likely Slack’s side');
  // Good tiles with a drop note (blip) are covered in the blip state: the note shows, the word stays Good.
});

test('a drop note on Good tiles (working again)', async ({ page }) => {
  await serveHome(page, 'now-blip');
  await openHome(page);
  const teams = page.locator('.hp-tile[data-id="teams"]');
  await expect(teams).toHaveClass(/is-good has-note/);
  await expect(teams.locator('.hp-word')).toHaveText('Good');
  await expect(teams.locator('.hp-since')).toHaveText('Dropped 40 s at 10:14 am');
  await expect(teams.locator('.hp-bars i').nth(19)).toHaveClass('b-down');
  await expect(teams.locator('.hp-bars i').nth(19)).toHaveAttribute('title', '10:10–10:15 am: Dropped 40 s');
  await expect(page.locator('.hp-hop').nth(1)).toContainText('Dropped for 40 s at 10:14 am, fine now');
});

test('app-only problem with "working again" appended', async ({ page }) => {
  await serveHome(page, 'now-zoom-working-again');
  await openHome(page);
  await expect(page.locator('.hp-answer')).toHaveClass(/\bok\b/);
  await expect(page.locator('.hp-sub')).toHaveText(/Teams and Google Meet are working if you need to switch\. Working again\. Your internet dropped for about 40 seconds at 10:14 am\.$/);
  await expect(page).toHaveTitle('! Zoom down – fyisp');
});

test('the speed line: usual, no usual, running elsewhere', async ({ page }) => {
  const ctl = await serveHome(page, 'now-good');
  await openHome(page);
  await expect(page.locator('.hp-speed-line')).toHaveText('Your speed: about 930 Mbps down / 940 Mbps up — about your usual speed.');
  await expect(page.locator('.hp-speed-note')).toHaveText('Today 8:00 am');
  ctl.now = fx('now-speed-no-usual');
  await page.reload();
  await expect(page.locator('.hp-speed-line')).toHaveText('Your speed: about 930 Mbps down / 940 Mbps up.');
  ctl.now = fx('now-learning');
  await page.reload();
  await expect(page.locator('.hp-speed-line')).toHaveText('No speed test yet. The first one runs within the hour.');
  // No plan copy anywhere on Home.
  await expect(page.locator('#home')).not.toContainText(/\bplan\b|fiber/i);
});
