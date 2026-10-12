// Helpers for the Home (v0.9.0) specs: /api/home2 served from the JSON
// fixtures (fixtures/gen.js) through page.route, so the client can be tested
// before and independently of the server's engine. Everything else (status,
// profile, Details) is the real fyisp-webdev API.
// @ts-check
const fs = require('fs');
const path = require('path');
const { expect } = require('./fixtures');

const FX = path.join(__dirname, '..', 'fixtures');
/** A fixture by name ("now-good", "tile-zoom"), as a fresh object. @param {string} name @returns {any} */
function fx(name) { return JSON.parse(fs.readFileSync(path.join(FX, `${name}.json`), 'utf8')); }
function has(/** @type {string} */ name) { return fs.existsSync(path.join(FX, `${name}.json`)); }

/** Every part=now state, for the per-state specs. */
const STATES = fs.readdirSync(FX).filter((f) => /^now-.*\.json$/.test(f)).map((f) => f.slice(4, -5)).sort();

/**
 * part=tile for a tile of the current part=now: its own fixture when there
 * is one, else the template of its kind with the tile's words; null (404)
 * for a tile that isn't monitored.
 * @param {string} id @param {any} now
 */
function tileFor(id, now) {
  const t = (now.tiles || []).find((/** @type {any} */ x) => x.id === id);
  if (!t || t.state === 'off') return null;
  const own = t.state === 'down' && has(`tile-${id}-down`) ? `tile-${id}-down` : has(`tile-${id}`) ? `tile-${id}` : '';
  if (own) return fx(own);
  const tpl = fx(t.kind === 'call' ? 'tile-teams' : t.kind === 'stream' ? 'tile-youtube' : 'tile-m365');
  return { ...tpl, id, state: t.state, word: t.word, reason: t.reason };
}

/**
 * Serves api/home2 from fixtures. The returned control changes what the
 * next poll gets (ctl.now), counts requests, and can make fyisp unreachable.
 * @param {import('@playwright/test').Page} page @param {string|object} now a fixture name or an answer
 */
async function serveHome(page, now) {
  const ctl = {
    /** @type {any} */ now: typeof now === 'string' ? fx(now) : now,
    nowCalls: 0,
    /** @type {string[]} */ tileCalls: [],
    down: false,
    /** @type {(id: string, now: any) => any} */ tile: tileFor,
  };
  await page.route('**/api/home2?*', async (route) => {
    if (ctl.down) return route.abort('connectionrefused');
    const q = new URL(route.request().url()).searchParams;
    if (q.get('part') === 'now') {
      ctl.nowCalls++;
      return route.fulfill({ json: ctl.now, headers: { 'Cache-Control': 'no-store' } });
    }
    if (q.get('part') === 'tile') {
      const id = q.get('id') || '';
      ctl.tileCalls.push(id);
      const t = ctl.tile(id, ctl.now);
      return t ? route.fulfill({ json: t }) : route.fulfill({ status: 404, json: { error: 'tile not monitored' } });
    }
    return route.fulfill({ status: 400, json: { error: 'unknown part' } });
  });
  return ctl;
}

/** Overrides fields of the real /api/status (controls, admin). @param {import('@playwright/test').Page} page @param {object} patch */
async function patchStatus(page, patch) {
  await page.route('**/api/status', async (route) => {
    const res = await route.fetch();
    route.fulfill({ response: res, json: { ...(await res.json()), ...patch } });
  });
}

/** Opens Home and waits for the new Home's first answer. @param {import('@playwright/test').Page} page @param {string} [hash] */
async function openHome(page, hash = '') {
  await page.goto('/' + hash);
  const home = page.locator('#home.hp');
  await expect(home).toBeVisible();
  await expect(page.locator('.hp-tile')).toHaveCount(11);
  return home;
}

/** Waits until the page has handled every response so far (two animation frames after). @param {import('@playwright/test').Page} page */
async function settle(page) {
  await page.evaluate(() => new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(() => r(null)))));
}

/** Light or dark, through prefers-color-scheme (the default theme follows it). @param {import('@playwright/test').Page} page @param {'light'|'dark'} theme */
async function setTheme(page, theme) { await page.emulateMedia({ colorScheme: theme }); }

const WIDTHS = [1400, 390];
const THEMES = /** @type {const} */ (['light', 'dark']);

module.exports = { fx, STATES, serveHome, patchStatus, openHome, settle, setTheme, WIDTHS, THEMES };
