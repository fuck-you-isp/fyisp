// The share-link report's "Speed through the day": in the third person, for a
// support agent. Mbit/s next to "% of normal", time-of-day blocks with the
// slower hours called out, the regions slowest then, and what the evidence is
// (how many tests, which providers, the method, and that it isn't the plan's
// advertised speed). Shown only once speed tests have run for about 3 days.
// Plain ES module, no build step.
// @ts-check

import { h, fetchShared, tzParam } from './core.js';
import { hourRange, findingExtras, dipHoursOf } from './speedmap.js';

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
/** "1 day", "3 days". @param {number} n @param {string} word */
function plural(n, word) { return `${n.toLocaleString()} ${word}${n === 1 ? '' : 's'}`; }
/** "UTC+2" from minutes east, when the server sends no zone name. @param {number} tz */
function zoneOf(tz) {
  const a = Math.abs(tz || 0), hh = Math.floor(a / 60), mm = a % 60;
  return !tz ? 'UTC' : `UTC${tz < 0 ? '−' : '+'}${hh}${mm ? ':' + String(mm).padStart(2, '0') : ''}`;
}
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
      this.d = /** @type {SpeedSummary} */ (await fetchShared('api/speed?' + new URLSearchParams({ days: '7', tz: tzParam() })));
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
    const refRow = [...rows].sort((a, b) => (b.best_down_bps || 0) - (a.best_down_bps || 0))[0];
    const dip = dipHoursOf(d);
    const cell = (/** @type {number[]} */ hours) => {
      const norms = [], n = [];
      for (const r of rows) for (const hr of hours) { const x = r.hours[hr]; if (x && x.score_median != null) norms.push(x.score_median); if (x) n.push(x.n); }
      const refB = median(hours.map((hr) => refRow.hours[hr]?.down_median_bps).filter((x) => x != null).map(Number));
      return { pct: median(norms), bps: refB, n: n.reduce((s, x) => s + x, 0) };
    };
    const lines = BLOCKS.map(([name, a, b]) => ({ name: `${name} (${blockLabel(a, b)})`, short: name, a, b, ...cell([...Array(b - a).keys()].map((i) => a + i)), sub: false }));
    // The slower hours, under the block they start in.
    if (dip.length) {
      const at = lines.findIndex((l) => dip[0] >= l.a && dip[0] < l.b);
      lines.splice(at + 1, 0, { name: hourRange(dip), short: '', a: 0, b: 0, ...cell(dip), sub: true });
    }
    const others = cell([...Array(24).keys()].filter((x) => !dip.includes(x)));
    const slow = (/** @type {{pct:number|null}} */ x) => x.pct != null && others.pct != null && x.pct <= others.pct * 0.75;
    const bad = rows.reduce((s, r) => s + r.busy + r.unreachable + r.failed, 0);
    // The regions on the schedule (the default ones), not every region a server could belong to.
    const inChain = new Set(d.regions.filter((r) => r.default !== false).flatMap((r) => r.servers));
    const providers = new Set(d.servers.filter((s) => inChain.has(s.id)).map((s) => s.provider));
    const f = d.finding || null;
    const x = /** @type {any} */ (d);
    // The owner's zone (B2): the server names it; every time of day carries it.
    const zone = x.zone_label || zoneOf(d.tz);
    const hrs = (/** @type {number[]} */ h2) => `${hourRange(h2)} ${zone}`;
    const drop = d.pattern ? Math.round(d.pattern.drop * 100) : 0;
    const text = f && f.text_public ? f.text_public : d.pattern
      ? `Downloads were about ${drop}% slower from ${hrs(dip)} than at other times of day, across the test servers.`
      : 'Download speed was about the same at every time of day.';
    const pct = (/** @type {number|null} */ v) => (v == null ? '—' : `${Math.round(v * 100)}%`);
    const days = f ? f.days : d.days;
    const fx = /** @type {any} */ (f || {});
    // Fewer than 7 days: the server's sentence (it knows when tests started), else the gap form.
    const daysNote = fx.days_note || (f && days > 0 && days < 7 ? `Tests ran on ${plural(days, 'day')} of the last 7.` : '');
    const earlyNote = f && f.tier === 'early_sign' ? (fx.early_note || `Early: ${plural(days, 'day')} of tests. A week gives a clearer picture.${f.slow_days && f.day_count && !/Slower on \d/.test(f.evidence || '') ? ` Slower on ${f.slow_days} of ${plural(f.day_count, 'evening')}.` : ''}`) : '';
    // B3: up to 3 servers by name, in the server's order; hidden when none qualify.
    /** @type {{display:string,pct:number,median_bps:number,usual_bps:number}[]} */
    const slowest = Array.isArray(fx.slowest) ? fx.slowest.slice(0, 3) : [];
    const allLB = rows.length > 0 && rows.every((r) => r.n > 0 && (/** @type {any} */ (r).n_lower_bound || 0) >= r.n);
    const anyTooFast = rows.some((r) => /** @type {any} */ (r).too_fast);
    const blockName = lines.find((l) => !l.sub && dip.length && dip[0] >= l.a && dip[0] < l.b)?.short || '';
    this.el.replaceChildren(
      h('h2', { id: 'rp-speed-h', text: 'Speed through the day' }),
      h('p', { class: 'ho-summary', text }),
      f ? findingExtras(d, f, true) : '',
      earlyNote ? h('p', { class: 'ho-muted', text: earlyNote }) : '',
      daysNote ? h('p', { class: 'ho-muted', text: daysNote }) : '',
      h('table', { class: 'rp-table rp-speed-t' },
        h('caption', { class: 'ho-muted rp-speed-cap', text: `Times in ${zone}.` }),
        h('thead', {}, h('tr', {},
          h('th', { scope: 'col', text: 'Time of day' }),
          h('th', { scope: 'col', class: 'num', text: `Download from ${refRow.label} (median)` }),
          h('th', { scope: 'col', class: 'num', text: `% of normal (all ${plural(rows.length, 'region')})` }),
          h('th', { scope: 'col', class: 'num', text: 'Tests' }))),
        h('tbody', {}, ...lines.map((l) => h('tr', { class: l.sub ? 'sub' : '' },
          // The sub-row's "└" is drawing, not words; its header says what it is.
          h('th', { scope: 'row' }, l.sub ? h('span', { 'aria-hidden': 'true', text: '└ ' }) : null,
            l.sub ? h('span', { class: 'sr-only', text: `${blockName}, ` }) : null, l.sub ? `slower hours: ${l.name}` : l.name),
          h('td', { class: 'num', text: mb(l.bps) }),
          h('td', { class: 'num' }, pct(l.pct), slow(l) ? h('span', { 'aria-hidden': 'true', text: ' ▼' }) : null, slow(l) ? ' slower' : ''),
          h('td', { class: 'num', text: l.n.toLocaleString() }))))),
      h('p', { class: 'ho-muted small', text: 'Download speeds are from the nearest region. Farther regions are usually slower because of distance, not the provider, so every region is compared by % of normal.' }),
      slowest.length ? h('div', {}, h('h3', { text: `Slowest servers from ${hrs(dip)}` }),
        h('ul', { class: 'rp-speed-worst' }, ...slowest.map((w) => {
          const name = String(w.display).replace(/ · /g, ', ');
          return h('li', { 'aria-label': `${name}: ${Math.round(w.pct * 100)}% of its usual result, median ${Math.round(w.median_bps / 1e6)} versus usual ${Math.round(w.usual_bps / 1e6)} megabits per second` },
            `${w.display}: ${Math.round(w.pct * 100)}% of its usual result (median ${Math.round(w.median_bps / 1e6)} vs usual ${Math.round(w.usual_bps / 1e6)} Mbit/s)`);
        }))) : '',
      h('h3', { text: 'About this evidence' }),
      h('ul', { class: 'rp-speed-ev' },
        f && f.evidence ? h('li', { text: f.evidence }) : '',
        h('li', { text: d.connection && d.connection.type === 'wifi' ? 'The measuring device used Wi-Fi, which can itself slow down in the evening. A test over a cable would be stronger evidence.'
          : d.connection && d.connection.type === 'wired' ? `The measuring device is connected by cable${d.connection.source === 'user' ? ' (as the owner said)' : ''}.` : 'How the measuring device is connected (cable or Wi-Fi) isn’t known.' }),
        h('li', { text: `Tests went to ${plural(rows.length, 'region')} and ${plural(providers.size, 'hosting provider')}.${bad ? ` ${bad.toLocaleString()} couldn’t run, usually because the server was busy.` : ''}` }),
        h('li', { text: 'Speeds are what this device got from public test servers. They are not a measure of the plan’s advertised speed.' }),
        h('li', { text: `Each test downloads, then uploads, for a few seconds with 8 connections. “% of normal” compares each test with the same server’s usual top speed.${allLB ? ' On this line every test reached its 100 MB limit, so speeds are lower limits.' : ''}` }),
        anyTooFast ? h('li', { text: 'Servers that this line reaches faster than a 100 MB test can measure are left out of the time-of-day comparison.' }) : '',
        h('li', { text: 'Seconds when tests ran are left out of the connection-quality record above.' })));
  }
}
