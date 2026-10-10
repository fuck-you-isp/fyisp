// The share-link report's "Speed through the day": in the third person, for a
// support agent. Mbit/s next to "% of normal", time-of-day blocks with the
// slower hours called out, the regions slowest then, and what the evidence is
// (how many tests, which providers, the method, and that it isn't the plan's
// advertised speed). Shown only once speed tests have run for about 3 days.
// Plain ES module, no build step.
// @ts-check

import { h, fetchJSON, tzParam } from './core.js';
import { hourRange } from './speedmap.js';

/** @typedef {import('./speed.js').SpeedSummary} SpeedSummary */
/** @typedef {import('./speed.js').SpeedRow} SpeedRow */

const BLOCKS = /** @type {[string, number, number][]} */ ([['Night', 0, 6], ['Morning', 6, 12], ['Afternoon', 12, 18], ['Evening', 18, 24]]);

function mb(/** @type {number|null} */ bps) {
  if (bps == null) return '—';
  const m = bps / 1e6;
  return m >= 1000 ? `${(m / 1000).toFixed(1)} Gbit/s` : `${m < 10 ? m.toFixed(1) : Math.round(m)} Mbit/s`;
}
function median(/** @type {number[]} */ v) {
  if (!v.length) return null;
  const s = [...v].sort((a, b) => a - b), m = s.length >> 1;
  return s.length % 2 ? s[m] : (s[m - 1] + s[m]) / 2;
}
/** "12–6 am"-style block labels from local hours. */
function blockLabel(/** @type {number} */ a, /** @type {number} */ b) { return `${hourName(a)} to ${hourName(b % 24)}`; }
function hourName(/** @type {number} */ hr) { return new Date(2000, 0, 1, hr).toLocaleTimeString([], { hour: 'numeric' }); }

export class SpeedReport {
  constructor() {
    /** @type {SpeedSummary|null} */
    this.d = null;
    this.off = false;
    this.el = h('section', { class: 'ho-time rp-speed', 'aria-labelledby': 'rp-speed-h', hidden: true });
  }

  async load() {
    if (this.off) return;
    try {
      this.d = /** @type {SpeedSummary} */ (await fetchJSON('api/speed?' + new URLSearchParams({ days: '7', tz: tzParam() })));
    } catch (e) {
      if (/** @type {any} */ (e).status === 404) this.off = true;
      return;
    }
    this.render();
  }

  render() {
    const d = this.d;
    // About 3 days of tests: most regions have at least half their hours filled.
    const all = d ? (d.rows && d.rows.length ? d.rows : d.per_region) : [];
    const ready = !!d && all.length > 0 && all.filter((r) => r.hours.filter((x) => x.n >= 3).length >= 12).length >= Math.ceil(all.length / 2);
    this.el.hidden = !ready;
    if (!d || !ready) return;
    const rows = all;
    // Mbit/s from the fastest region (nearest, usually): far ones always read lower.
    const ref = [...rows].sort((a, b) => (b.best_down_bps || 0) - (a.best_down_bps || 0))[0];
    const dip = d.finding?.hours?.length ? d.finding.hours : d.pattern ? d.pattern.hours : [];
    const cell = (/** @type {number[]} */ hours) => {
      const norms = [], n = [];
      for (const r of rows) for (const hr of hours) { const x = r.hours[hr]; if (x && x.score_median != null) norms.push(x.score_median); if (x) n.push(x.n); }
      const refB = median(hours.map((hr) => ref.hours[hr]?.down_median_bps).filter((x) => x != null).map(Number));
      return { pct: median(norms), bps: refB, n: n.reduce((s, x) => s + x, 0) };
    };
    const lines = BLOCKS.map(([name, a, b]) => ({ name: `${name} (${blockLabel(a, b)})`, ...cell([...Array(b - a).keys()].map((i) => a + i)), sub: false }));
    if (dip.length) lines.push({ name: hourRange(dip), ...cell(dip), sub: true });
    const others = cell([...Array(24).keys()].filter((x) => !dip.includes(x)));
    const slow = (/** @type {{pct:number|null}} */ x) => x.pct != null && others.pct != null && x.pct <= others.pct * 0.75;
    // The regions slowest in the slower hours.
    const worst = dip.length ? rows.map((r) => ({ r, v: median(dip.map((hr) => r.hours[hr]?.score_median).filter((x) => x != null).map(Number)) }))
      .filter((x) => x.v != null).sort((a, b) => /** @type {number} */ (a.v) - /** @type {number} */ (b.v)).slice(0, 3) : [];
    const ok = rows.reduce((s, r) => s + r.n, 0);
    const bad = rows.reduce((s, r) => s + r.busy + r.unreachable + r.failed, 0);
    const inChain = new Set(d.regions.flatMap((r) => r.servers));
    const providers = new Set(d.servers.filter((s) => inChain.has(s.id)).map((s) => s.provider));
    const drop = d.pattern ? Math.round(d.pattern.drop * 100) : 0;
    const text = d.finding?.text_public ? d.finding.text_public : d.pattern
      ? `Downloads were about ${drop}% slower from ${hourRange(dip)} than at other times of day, across the test servers.`
      : 'Download speed was about the same at every time of day.';
    const pct = (/** @type {number|null} */ v) => (v == null ? '—' : `${Math.round(v * 100)}%`);
    this.el.replaceChildren(
      h('h2', { id: 'rp-speed-h', text: 'Speed through the day' }),
      h('p', { class: 'ho-summary', text }),
      h('table', { class: 'rp-table rp-speed-t' },
        h('thead', {}, h('tr', {},
          h('th', { scope: 'col', text: 'Time of day' }),
          h('th', { scope: 'col', class: 'num', text: `Download (median, ${ref.label})` }),
          h('th', { scope: 'col', class: 'num', text: '% of normal (all regions)' }),
          h('th', { scope: 'col', class: 'num', text: 'Tests' }))),
        h('tbody', {}, ...lines.map((x) => h('tr', { class: x.sub ? 'sub' : '' },
          h('th', { scope: 'row', text: x.sub ? `└ ${x.name}` : x.name }),
          h('td', { class: 'num', text: mb(x.bps) + (slow(x) ? ' ▼' : '') }),
          h('td', { class: 'num', text: pct(x.pct) + (slow(x) ? ' ▼ slower' : '') }),
          h('td', { class: 'num', text: x.n.toLocaleString() }))))),
      worst.length ? h('div', {}, h('h3', { text: `Slowest from ${hourRange(dip)}` }),
        h('ul', { class: 'rp-speed-worst' }, ...worst.map((x) => h('li', { text: `${x.r.label}: ${pct(x.v)} of normal` })))) : '',
      h('h3', { text: 'About this evidence' }),
      h('ul', { class: 'rp-speed-ev' },
        d.finding?.evidence ? h('li', { text: d.finding.evidence.replace(/^Based on/, 'Based on') }) : '',
        h('li', { text: d.connection && d.connection.type === 'wifi' ? 'The measuring device used Wi-Fi, which can itself slow down in the evening. A test over a cable would be stronger evidence.'
          : d.connection && d.connection.type === 'wired' ? `The measuring device is connected by cable${d.connection.source === 'user' ? ' (as the owner said)' : ''}.` : 'How the measuring device is connected (cable or Wi-Fi) isn’t known.' }),
        h('li', { text: `${ok.toLocaleString()} tests over ${d.days} days to ${rows.length} regions, with ${providers.size} hosting providers.${bad ? ` ${bad.toLocaleString()} couldn’t run (usually the server was busy).` : ''}` }),
        h('li', { text: 'Speeds are what this device got from public test servers. They are not a measure of the plan’s advertised speed.' }),
        h('li', { text: 'Each test downloads, then uploads, for a few seconds with 8 connections. “% of normal” compares each test with the same server’s usual top speed.' }),
        h('li', { text: 'Seconds when tests ran are left out of the connection-quality record above.' })));
  }
}
