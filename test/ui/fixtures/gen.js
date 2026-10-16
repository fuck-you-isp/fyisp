// Writes the /api/home fixtures the Home specs serve through page.route,
// from the server's golden files (internal/web/testdata/home, written by
// go test ./internal/web -run TestHomeGolden -update): every scenario's
// part=now as now-<scenario>.json, and the panels the specs open as
// tile-<name>.json. So the client is tested against what the server really
// says. The clock is 2026-10-09 10:30 am in America/New_York (the specs run
// in that zone), like the goldens.
//   node fixtures/gen.js [<goldens dir> [<out dir>]]
// The Dockerfile's uitest-run stage runs it before the specs; the output is
// not committed.
// @ts-check
'use strict';
const fs = require('fs');
const path = require('path');

const gold = process.argv[2] || path.join(__dirname, '..', '..', '..', 'internal', 'web', 'testdata', 'home');
const dir = process.argv[3] || __dirname;

// tile-<name>.json: the golden it comes from.
const TILES = {
  zoom: 'good.tile-zoom', 'zoom-down': 'zoom.tile-zoom', teams: 'good.tile-teams', meet: 'good.tile-meet',
  m365: 'good.tile-m365', youtube: 'good.tile-youtube', 'gws-learning': 'learning.tile-gws',
};

/** Pretty JSON with each bars/ms array on one line, so a fixture reads top to bottom. */
function stringify(/** @type {any} */ o) {
  /** @type {string[]} */
  const keep = [];
  const s = JSON.stringify(o, (k, v) => ((k === 'bars' || k === 'ms') && Array.isArray(v) ? `@@${keep.push(JSON.stringify(v)) - 1}@@` : v), 2);
  return s.replace(/"@@(\d+)@@"/g, (_, i) => keep[Number(i)]) + '\n';
}
const read = (/** @type {string} */ name) => JSON.parse(fs.readFileSync(path.join(gold, `${name}.json`), 'utf8'));

fs.mkdirSync(dir, { recursive: true });
for (const f of fs.readdirSync(dir)) if (/^(now|tile)-.*\.json$/.test(f)) fs.unlinkSync(path.join(dir, f));
const now = fs.readdirSync(gold).filter((f) => f.endsWith('.now.json')).map((f) => f.slice(0, -'.now.json'.length)).sort();
for (const name of now) fs.writeFileSync(path.join(dir, `now-${name}.json`), stringify(read(`${name}.now`)));
for (const [name, from] of Object.entries(TILES)) fs.writeFileSync(path.join(dir, `tile-${name}.json`), stringify(read(from)));
console.log(`${now.length} part=now and ${Object.keys(TILES).length} part=tile fixtures in ${dir}, from ${gold}`);
