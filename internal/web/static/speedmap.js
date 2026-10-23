// Details → Speed, the evidence: "What we found", speed by hour of day (a
// heatmap of regions or servers, each cell the row's median as a share of
// its own normal), the selected row's tests by hour and over the days, its
// providers side by side, delay under load and the tests themselves. Every
// colour has a word and a shape: five lightness bins, a hatch on the worst,
// an outline for "no tests yet" and a dot for "filling in". Plain ES module,
// no build step.
// @ts-check

import { h, fetchJSON, apiWrite, css, rgba, showTip, hideTip, tzParam, clock, fmtMbps } from './core.js';

/** @typedef {import('./speed.js').SpeedSummary} SpeedSummary */
/** @typedef {import('./speed.js').SpeedRow} SpeedRow */
/** @typedef {import('./speed.js').SpeedTest} SpeedTest */
/** @typedef {{at:number,kind:string,row:SpeedRow,tests:SpeedTest[],windows:{start:number,end:number}[]}} SpeedView */
/** @typedef {{sel:()=>string, setSel:(v:string)=>void, days:()=>number, setDays:(d:number)=>void, nofb:()=>boolean, setNofb:(b:boolean)=>void, refresh:()=>void, sweep:()=>boolean}} SpeedHost */

/** A word over the finding: how sure, or what kind of nothing. */
const KICKER = /** @type {Object<string,string>} */ ({ early_sign: 'Early sign', most_likely: 'Most likely', points_to: 'Clear pattern', mild: 'A little slower', mixed: 'Mixed results', not_enough_data: 'Still learning', too_fast: 'Too fast to compare' });

/**
 * The finding's supporting lines under its sentence: the caveat (Wi-Fi,
 * household, behind another router). The server already writes the far
 * regions into the sentence, the slower days into the evidence and the
 * far end's evening into the sentence; each is added here only when it
 * is not, so nothing says it twice.
 * @param {SpeedSummary} d @param {import('./speed.js').SpeedFinding} f
 */
function findingExtras(d, f) {
  const lines = [];
  const caveat = f.caveat;
  const said = `${f.text} ${f.evidence || ''} ${caveat || ''}`;
  const evening = f.hours && f.hours.length && f.hours.every((x) => x >= 17 || x <= 1);
  if (f.slow_days && f.day_count && !/Slower on \d/.test(said)) lines.push(`Slower at those hours on ${f.slow_days} of ${f.day_count} ${evening ? 'evenings' : 'days'} with tests.`);
  if (f.far_regions && f.far_regions.length && !/too far away/.test(said)) {
    const names = f.far_regions.map((id) => d.regions.find((r) => r.id === id)?.title || id);
    const list = names.length > 1 ? `${names.slice(0, -1).join(', ')} and ${names[names.length - 1]}` : names[0];
    lines.push(`${list} ${names.length > 1 ? 'are' : 'is'} too far away to show it.`);
  }
  if (f.far_evening && !/evening there/.test(said)) lines.push('It’s evening at the far end too, so busy time there could explain it as well.');
  if (caveat) lines.push(caveat);
  if (f.bridged && !/container|second router/.test(said)) lines.push('Tests ran from inside a container or VM (or behind a second router), so Wi-Fi and the home router can’t be ruled out.');
  return lines.length ? h('ul', { class: 'sp-found-x' }, ...lines.map((t) => h('li', { text: t }))) : '';
}

/** Bins of "% of normal": lower bound, class, the word a screen reader hears. The 75% edge is the dip rule's. */
const BINS = /** @type {[number, string, string][]} */ ([
  [0.9, 'b1', 'normal'], [0.75, 'b2', 'a little below normal'], [0.6, 'b3', 'below normal'],
  [0.4, 'b4', 'well below normal'], [-Infinity, 'b5', 'far below normal']]);
function bin(/** @type {number} */ v) { return /** @type {[number, string, string]} */ (BINS.find(([min]) => v >= min)); }

/** Shapes by provider, in order of first appearance: never colour alone. */
const SHAPES = ['circle', 'triangle', 'square', 'diamond', 'pentagon'];
const SHAPE_CHAR = { circle: '●', triangle: '▲', square: '■', diamond: '◆', pentagon: '⬟' };

const H12 = /[ap]/i.test(new Date(2000, 0, 1, 13).toLocaleTimeString([], { hour: 'numeric' }).replace(/\d/g, ''));
/** "9 PM" or "21:00", the viewer's way. */
function hourLong(/** @type {number} */ hr) { return new Date(2000, 0, 1, hr).toLocaleTimeString([], { hour: 'numeric' }); }
/** Column labels: "12a", "2", … "12p" in 12-hour locales, else "0", "2", … */
function hourShort(/** @type {number} */ hr) {
  if (!H12) return String(hr);
  const n = hr % 12 || 12;
  return hr === 0 ? '12a' : hr === 12 ? '12p' : String(n);
}
/** The slower hours to mark: the finding's when it found a dip, else the pattern's. @param {SpeedSummary} d */
function dipHoursOf(d) {
  const f = d.finding;
  // Only a finding with hours: not while still learning, and not for mixed or nothing.
  if (f) return f.hours && f.hours.length && !['nothing', 'mixed', 'not_enough_data'].includes(f.case) ? f.hours : [];
  return d.pattern ? d.pattern.hours : [];
}
/** "8 to 11 pm" for a run of hours (the end is the last hour's end). @param {number[]} hours */
function hourRange(hours) {
  hours = hours || [];
  if (!hours.length) return '';
  const a = hours[0], b = (hours[hours.length - 1] + 1) % 24;
  return `${hourLong(a)} to ${hourLong(b)}`;
}
function pct(/** @type {number} */ v) { return `${Math.round(v * 100)}%`; }
function median(/** @type {number[]} */ v) {
  if (!v.length) return null;
  const s = [...v].sort((a, b) => a - b), m = s.length >> 1;
  return s.length % 2 ? s[m] : (s[m - 1] + s[m]) / 2;
}
const isPhoneGrid = () => window.matchMedia('(max-width: 999.98px)').matches;

/** A test's result in words, never a code; busy is never a slow result. @param {SpeedTest} t */
function resultWord(t) {
  if (t.outcome === 'busy') return 'Server busy';
  if (t.outcome === 'unreachable') return 'Didn’t answer';
  if (t.outcome === 'failed') return 'Didn’t finish';
  if (t.outcome === 'cancelled') return t.down_bps != null ? `Cancelled after download (${fmtMbps(t.down_bps)} down)` : 'Cancelled';
  const bits = [t.fallback_level === 'nearby_city' ? 'Backup city ◤' : t.fallback ? 'Backup server ◤' : 'OK'];
  if (t.capped_by_server) bits.push('limited by server ⊘');
  return bits.join(' · ');
}
/**
 * A lower bound: the direction was still speeding up when it reached its
 * 10 seconds (A2, *_lower_bound), so the speed was at least this.
 * @param {SpeedTest} t @param {'down'|'up'} dir
 */
export function lowerBound(t, dir) { return /** @type {any} */ (t)[`${dir}_lower_bound`] === true; }
/** Every test runs up to 10 seconds each way. */
const LB_MORE = 'Still speeding up when its 10 seconds ran out, so the speed was at least this. It counts as at least normal in the time-of-day pattern, but isn’t used to set this server’s normal.';
const LB_OUT = 'This test was still speeding up when its 10 seconds ran out, so it shows only a lower limit and isn’t used in the time-of-day pattern.';
/** A server or region left out because most tests to it end while still speeding up (A2). */
const TOO_FAST = 'Too fast to measure from here';
/** @param {SpeedRow} r @param {boolean} region */
function tooFastWhy(r, region) {
  // The server decides (too_fast); the words never name its threshold.
  return region ? `None of the servers in ${r.label} has enough full-speed tests across the day to know its normal, so the region is left out of the time-of-day pattern.`
    : `There aren’t enough full-speed tests to ${r.label} across the day to know its normal, so it’s left out of the time-of-day pattern.`;
}
/**
 * A reading's cell. "≥" only on lower bounds and only when the column mixes
 * them with full readings (a column of only lower bounds says "at least" in
 * its header); the mark is hidden from screen readers, which hear "at least".
 * @param {string} k @param {number|null} b @param {boolean} lb @param {boolean} mark
 */
/** Whether a lower bound counts as at least normal: the server's *_lb_counted (omitted when false). @param {SpeedTest} t @param {'down'|'up'} dir */
function lbCounted(t, dir) { return /** @type {any} */ (t)[`${dir}_lb_counted`] === true; }
/** The "% of normal" cell of a lower bound: "at least normal", or "not counted", muted either way. @param {boolean} atLeast */
function lbCell(atLeast) {
  const why = atLeast ? LB_MORE : LB_OUT;
  return h('td', { class: 'num sp-nc', 'data-k': '% of normal', title: why }, atLeast ? 'at least normal' : 'not counted', h('span', { class: 'sr-only', text: `: ${why}` }));
}
export function readingCell(k, b, lb, mark) {
  if (b == null) return h('td', { class: 'num', 'data-k': k, text: '—' });
  const v = fmtMbps(b, true);
  return h('td', { class: 'num', 'data-k': k },
    lb && mark ? h('span', { 'aria-hidden': 'true', text: '≥ ' }) : null,
    lb ? h('span', { class: 'sr-only', text: 'at least ' }) : null, v);
}
/** "8 → 21 ms (+13)" for one layer of a test's latency. @param {import('./speed.js').SpeedLat|undefined} idle @param {import('./speed.js').SpeedLat|undefined} load */
function lat(idle, load) {
  if (!load) return 'not known';
  const lost = load.loss ? `, ${Math.round(load.loss * 100)}% lost` : '';
  // Fewer than 20 idle probes (or fyisp had just started): no "before" to compare with.
  return idle ? `${Math.round(idle.p50_ms)} → ${Math.round(load.p50_ms)} ms${lost}` : `${Math.round(load.p50_ms)} ms (not measured before the test)${lost}`;
}
/** "Lumen (AS3356), where they meet at DE-CIX (an internet exchange)"; a path that ends at the exchange names only the exchange. @param {any} ho */
function handoffWords(ho) {
  const via = ho.via ? `${ho.via} (an internet exchange)` : '';
  if (!ho.asn) return via ? `through ${via}` : 'not known';
  return `${ho.name} (AS${ho.asn})${via ? `, where they meet at ${via}` : ''}`;
}

export class SpeedMap {
  /** @param {SpeedHost} host */
  constructor(host) {
    this.host = host;
    /** @type {SpeedSummary|null} */
    this.d = null;
    /** @type {'regions'|'servers'} */
    this.rows = 'regions';
    /** @type {'pct'|'mbps'} */
    this.unit = 'pct';
    /** @type {'down'|'up'} */
    this.dir = 'down';
    this.numbers = false;
    /** @type {SpeedView|null} the selected row's tests */
    this.view = null;
    this.viewKey = '';
    this.viewSeq = 0;
    this.problems = false;
    this.shown = 15;
    /** @type {{key:string,asc:boolean}} */
    this.sort = { key: 'label', asc: true };
    this.found = '';
    this.skipNote = '';
    /** The heatmap cell that holds the tab stop: [row, column]. */
    this.cur = [0, 0];

    this.say = h('p', { class: 'sr-only', role: 'status' });
    this.foundEl = h('section', { class: 'sp-found', 'aria-labelledby': 'sp-found-h' });
    this.mapEl = h('section', { class: 'sp-map', 'aria-labelledby': 'sp-map-h' });
    this.sumEl = h('section', { class: 'sp-sum', 'aria-labelledby': 'sp-sum-h' });
    this.rowEl = h('section', { class: 'sp-row', 'aria-labelledby': 'sp-row-h' });
    this.el = h('div', { class: 'sp-evidence' }, this.say, this.foundEl, this.mapEl, this.rowEl, this.sumEl);
    window.matchMedia('(max-width: 999.98px)').addEventListener('change', () => this.renderMap());
  }

  /** @param {SpeedSummary} d */
  render(d) {
    this.d = d;
    this.renderFound();
    this.renderMap();
    this.renderSummary();
    this.loadRow();
  }

  // ---------- what we found ----------

  renderFound() {
    const d = /** @type {SpeedSummary} */ (this.d);
    const rows = d.rows && d.rows.length ? d.rows : d.per_region;
    const ok = rows.reduce((s, r) => s + r.n, 0);
    const bad = rows.reduce((s, r) => s + r.busy + r.unreachable + r.failed, 0);
    const filled = rows.reduce((s, r) => s + r.hours.filter((x) => x.n >= 3).length, 0);
    // The server's finding (tiers, localisation, caveats); without one, plain facts about how far along the data is.
    const f = d.finding;
    const text = f && f.text ? f.text
      : d.pattern ? `${d.pattern.text} Around ${hourRange(d.pattern.hours)}.`
      : !ok ? 'No tests yet. The first results show here a few minutes after a test.'
        : filled < rows.length * 12 ? 'We need about 3 days of tests to compare times of day. The grid below fills in as tests run.'
          : 'No time of day stands out.';
    // Mild and mixed name themselves; otherwise how sure the finding is.
    const kick = !f ? '' : f.case === 'mild' || f.case === 'mixed' || f.case === 'too_fast' ? KICKER[f.case] : KICKER[f.tier || ''] || KICKER[f.case] || '';
    const notes = rows.filter((r) => r.note).map((r) => h('li', { text: /** @type {string} */ (r.note) }));
    const failing = ok + bad >= 20 && bad / (ok + bad) >= 0.3;
    const unreachable = rows.reduce((s, r) => s + r.unreachable, 0);
    this.foundEl.replaceChildren(
      h('h2', { id: 'sp-found-h', text: 'What we found' }),
      kick ? h('p', { class: 'sp-kicker', text: kick }) : '',
      h('p', { class: 'sp-found-text', text }),
      f ? findingExtras(d, f) : '',
      f && f.evidence ? h('p', { class: 'muted small', text: f.evidence })
        : ok ? h('p', { class: 'muted small', text: `Based on ${ok.toLocaleString()} test${ok === 1 ? '' : 's'} over ${d.days} days${d.exclude_fallback ? ', without backup servers' : ''}.` }) : '',
      failing ? h('p', { class: 'notice warn', text: unreachable > bad * 0.8
        ? 'Tests can’t reach most servers. Something on this network may be blocking them (a firewall or VPN).'
        : `Many tests couldn’t run (${bad.toLocaleString()} of ${(ok + bad).toLocaleString()}). Usually the public servers were busy. Patterns may take longer to show.` }) : '',
      notes.length ? h('ul', { class: 'sp-notes' }, ...notes) : '');
    if (this.found && this.found !== text) this.say.textContent = text;
    this.found = text;
  }

  // ---------- the heatmap ----------

  /** Rows on screen: regions, or servers under their region. @returns {{group:string, row:SpeedRow, key:string}[]} */
  mapRows() {
    const d = /** @type {SpeedSummary} */ (this.d);
    // The schedule's own rows (regions, or the nearby servers), else every region.
    if (this.rows === 'regions') {
      const rows = d.rows && d.rows.length ? d.rows : d.per_region;
      return rows.map((r) => ({ group: '', row: r, key: (r.kind === 'server' ? 's:' : 'r:') + r.id }));
    }
    const srv = new Map(d.servers.map((x) => [x.id, x]));
    const title = new Map(d.regions.map((r) => [r.id, r.title]));
    const where = (/** @type {string} */ id) => { const x = srv.get(id); return (x && x.region && title.get(x.region)) || (x && x.area) || 'Other'; };
    const order = [...d.regions.map((r) => r.title)];
    const rank = (/** @type {string} */ g) => { const i = order.indexOf(g); return i < 0 ? order.length + (g === 'Other' ? 1 : 0) : i; };
    return [...d.per_server]
      .map((r) => ({ group: where(r.id), row: r, key: 's:' + r.id }))
      .sort((a, b) => rank(a.group) - rank(b.group) || a.group.localeCompare(b.group));
  }

  /**
   * One cell's value: the share of the row's normal (colour), what to print, and n.
   * Upload has no server "best", so its normal is the row's best hour.
   * @param {SpeedRow} r @param {number[]} hrs the hours this cell covers
   */
  cell(r, hrs) {
    const up = this.dir === 'up';
    // Mbit/s colours against the row's best hour, so a far row isn't dark for being far.
    const best = Math.max(0, ...r.hours.filter((x) => x.n >= 3).map((x) => /** @type {number} */ ((up ? x.up_median_bps : x.down_median_bps) || 0)));
    let worst = null, bps = null, n = 0, fb = 0, failed = 0;
    for (const hr of hrs) {
      const x = r.hours[hr];
      if (!x) continue;
      n += x.n;
      fb += x.n_fallback || 0;
      failed += x.n_failed || 0;
      const b = up ? x.up_median_bps : x.down_median_bps;
      const v = this.unit === 'mbps' ? (b != null && best ? b / best : null) : up ? x.up_score_median : x.score_median;
      // The worst hour of a block speaks for it.
      if (v != null && (worst == null || v < worst)) { worst = v; bps = b; }
    }
    return { v: worst, bps, n, fb, failed };
  }

  renderMap() {
    const d = this.d;
    if (!d) return;
    const phone = isPhoneGrid();
    const cols = phone ? [0, 3, 6, 9, 12, 15, 18, 21].map((a) => [a, a + 1, a + 2]) : [...Array(24).keys()].map((x) => [x]);
    const dip = new Set(dipHoursOf(d));
    const rows = this.mapRows();
    const sel = this.host.sel() || (rows[0] ? rows[0].key : '');
    const seg = (/** @type {string} */ label, /** @type {[string,string][]} */ opts, /** @type {string} */ cur, /** @type {(v:string)=>void} */ set) =>
      h('div', { class: 'seg sm', role: 'group', 'aria-label': label }, ...opts.map(([v, l]) => h('button', {
        type: 'button', text: l, 'aria-pressed': String(v === cur), onclick: () => { if (v !== cur) set(v); },
      })));
    const controls = h('div', { class: 'sp-ctl' },
      seg('Rows', [['regions', d.mode === 'nearby' ? 'Nearby' : d.mode === 'custom' ? 'Your servers' : 'Regions'], ['servers', 'Every server']], this.rows, (v) => { this.rows = /** @type {'regions'} */ (v); this.cur = [0, 0]; this.renderMap(); this.renderSummary(); }),
      seg('Span', [['7', '7 days'], ['30', '30 days']], String(this.host.days()), (v) => this.host.setDays(Number(v))),
      seg('Show', [['pct', '% of normal'], ['mbps', 'Mbit/s']], this.unit, (v) => { this.unit = /** @type {'pct'} */ (v); this.renderMap(); this.renderRow(); }),
      seg('Direction', [['down', 'Download'], ['up', 'Upload']], this.dir, (v) => { this.dir = /** @type {'down'} */ (v); this.renderMap(); this.renderRow(); }),
      h('label', { class: 'sp-check' }, h('input', { type: 'checkbox', checked: this.host.nofb(), onchange: (/** @type {Event} */ e) => this.host.setNofb(/** @type {HTMLInputElement} */ (e.target).checked) }), 'Leave out backup servers'));

    const what = this.unit === 'pct' ? '% of normal' : 'Mbit/s (darker: slower than this row’s best)';
    const head = h('tr', {}, h('td', { class: 'sp-corner' }), ...cols.map((c) => {
      const isDip = c.some((x) => dip.has(x));
      return h('th', { scope: 'col', class: isDip ? 'dip' : '', 'aria-label': phone ? `${hourLong(c[0])} to ${hourLong((c[2] + 1) % 24)}` : hourLong(c[0]) },
        phone || c[0] % 2 === 0 ? hourShort(c[0]) : '');
    }));
    const body = [];
    let group = '';
    const [ci, cj] = this.cur;
    rows.forEach((x, i) => {
      if (x.group && x.group !== group) {
        group = x.group;
        body.push(h('tr', { class: 'sp-grp' }, h('th', { scope: 'rowgroup', colspan: String(cols.length + 1), text: group })));
      }
      const tooFast = /** @type {any} */ (x.row).too_fast === true;
      const label = x.row.label + (tooFast ? '' : x.row.learning ? ' (learning)' : '');
      const tr = h('tr', { class: x.key === sel ? 'sel' : '', 'aria-selected': String(x.key === sel) },
        h('th', { scope: 'row', class: 'sp-rl' }, h('button', { type: 'button', class: 'sp-rb', tabindex: '-1', text: label, title: tooFast ? tooFastWhy(x.row, x.row.kind !== 'server') : `Show ${label}`, onclick: () => this.select(x.key) }),
          tooFast ? h('span', { class: 'sp-tf', text: TOO_FAST, title: tooFastWhy(x.row, x.row.kind !== 'server') }, h('span', { class: 'sr-only', text: `: ${tooFastWhy(x.row, x.row.kind !== 'server')}` })) : null));
      cols.forEach((c, j) => {
        const { v, bps, n, fb, failed } = this.cell(x.row, c);
        const at = phone ? `${hourLong(c[0])} to ${hourLong((c[2] + 1) % 24)}` : hourLong(c[0]);
        let cls = 'sp-c', name;
        const val = this.unit === 'pct' ? (v != null ? pct(v) : '') : (bps != null ? String(Math.round(bps / 1e6)) : '');
        if (!n && failed) { cls += ' none fail'; name = `${label}, ${at}: no result, the server was busy or didn’t answer`; } else if (!n) { cls += ' none'; name = `${label}, ${at}: no tests yet`; } else if (v == null) {
          cls += ' few';
          name = `${label}, ${at}: filling in, ${n} test${n === 1 ? '' : 's'} so far`;
        } else {
          const [, c2, word] = bin(v);
          cls += ' ' + c2;
          name = `${label}, ${at}: ${this.unit === 'pct' ? pct(v) + ' of normal' : fmtMbps(bps)}, ${n} tests, ${word}`;
        }
        if (fb && n) { cls += ' fb'; name += `, ${fb} on a backup server`; }
        if (dip.has(c[0])) cls += ' dip';
        const td = h('td', { class: cls, 'aria-label': name, tabindex: i === ci && j === cj ? '0' : '-1', 'data-i': String(i), 'data-j': String(j) },
          this.numbers && val ? h('span', { 'aria-hidden': 'true', text: val }) : null,
          !this.numbers && n && v == null ? h('span', { class: 'sp-dot', 'aria-hidden': 'true' }) : null,
          !this.numbers && !n && failed ? h('span', { class: 'sp-x', 'aria-hidden': 'true', text: '×' }) : null);
        tr.append(td);
      });
      body.push(tr);
    });
    const table = h('table', { class: 'sp-hm' + (this.numbers ? ' nums' : '') + (phone ? ' blocks' : ''), role: 'grid', 'aria-labelledby': 'sp-map-h', 'aria-describedby': 'sp-map-what' },
      h('thead', {}, head), h('tbody', {}, ...body));
    table.addEventListener('keydown', (e) => this.gridKey(/** @type {KeyboardEvent} */ (e), rows, cols.length));
    table.addEventListener('focusin', (e) => this.cellTip(/** @type {HTMLElement} */ (e.target)));
    table.addEventListener('focusout', () => hideTip());
    table.addEventListener('mouseover', (e) => this.cellTip(/** @type {HTMLElement} */ (e.target)));
    table.addEventListener('mouseleave', () => hideTip());
    table.addEventListener('click', (e) => {
      const td = /** @type {HTMLElement|null} */ (/** @type {HTMLElement} */ (e.target).closest('td.sp-c'));
      if (td) { this.cur = [Number(td.dataset.i), Number(td.dataset.j)]; this.select(rows[this.cur[0]].key); }
    });
    const legend = h('div', { class: 'sp-legend', 'aria-hidden': this.numbers ? 'true' : null },
      ...(this.numbers ? [] : [
        ...[['b1', '90% or more'], ['b2', '75–89%'], ['b3', '60–74%'], ['b4', '40–59%'], ['b5', 'under 40%']].map(([c, l]) => h('span', {}, h('i', { class: `sp-sw sp-c ${c}` }), l)),
        h('span', {}, h('i', { class: 'sp-sw sp-c few' }, h('span', { class: 'sp-dot' })), 'filling in (under 3 tests)'),
        h('span', {}, h('i', { class: 'sp-sw sp-c none' }), 'no tests yet'),
        h('span', {}, h('i', { class: 'sp-sw sp-c none fail' }, h('span', { class: 'sp-x', text: '×' })), 'no result (server busy)'),
        h('span', {}, h('i', { class: 'sp-sw sp-c b1 fb' }), 'includes backup servers')]),
      dip.size ? h('span', {}, h('i', { class: 'sp-sw sp-dipmark' }), `slower hours: ${hourRange(dipHoursOf(/** @type {SpeedSummary} */ (d)))}`) : null);
    this.mapEl.replaceChildren(
      h('div', { class: 'sp-sec-head' }, h('h2', { id: 'sp-map-h', text: phone ? 'Speed by time of day' : 'Speed by hour of day' }),
        h('button', { class: 'btn sm', type: 'button', text: this.numbers ? 'Show as colours' : 'Show as table', 'aria-pressed': String(this.numbers), onclick: () => { this.numbers = !this.numbers; this.renderMap(); } })),
      controls,
      h('p', { id: 'sp-map-what', class: 'muted small', text: `${this.dir === 'down' ? 'Download' : 'Upload'}, ${what}, by local time. Each cell is the median of that hour’s tests over ${d.days} days.${phone ? ' Each column is 3 hours; the slowest hour speaks for it. Tap a row for every hour.' : ''}` }),
      h('div', { class: 'sp-hm-wrap' }, table),
      legend);
  }

  /** Arrow keys move between cells, Home/End to the row's ends, Enter picks the row. */
  gridKey(/** @type {KeyboardEvent} */ e, /** @type {{key:string}[]} */ rows, /** @type {number} */ ncol) {
    const td = /** @type {HTMLElement|null} */ (/** @type {HTMLElement} */ (e.target).closest('td.sp-c'));
    if (!td) return;
    let [i, j] = [Number(td.dataset.i), Number(td.dataset.j)];
    switch (e.key) {
      case 'ArrowRight': j = Math.min(ncol - 1, j + 1); break;
      case 'ArrowLeft': j = Math.max(0, j - 1); break;
      case 'ArrowDown': i = Math.min(rows.length - 1, i + 1); break;
      case 'ArrowUp': i = Math.max(0, i - 1); break;
      case 'Home': j = 0; break;
      case 'End': j = ncol - 1; break;
      case 'Enter': case ' ': e.preventDefault(); this.select(rows[i].key); return;
      default: return;
    }
    e.preventDefault();
    const next = /** @type {HTMLElement|null} */ (this.mapEl.querySelector(`td[data-i="${i}"][data-j="${j}"]`));
    if (!next) return;
    td.tabIndex = -1;
    next.tabIndex = 0;
    next.focus();
    this.cur = [i, j];
  }

  cellTip(/** @type {HTMLElement} */ t) {
    const td = t.closest && t.closest('td.sp-c');
    if (!td) return;
    const r = td.getBoundingClientRect();
    showTip([h('div', { text: td.getAttribute('aria-label') || '' })], r.left + r.width / 2, r.bottom + 4);
  }

  /** @param {string} key 'r:<region>' or 's:<server>' */
  select(key) {
    this.host.setSel(key);
    for (const tr of this.mapEl.querySelectorAll('tbody tr')) tr.classList.remove('sel');
    this.renderMap();
    this.loadRow();
    requestAnimationFrame(() => this.rowEl.scrollIntoView({ block: 'nearest', behavior: window.matchMedia('(prefers-reduced-motion: reduce)').matches ? 'auto' : 'smooth' }));
  }

  // ---------- the selected row ----------

  async loadRow() {
    const d = this.d;
    if (!d) return;
    const rows = this.mapRows();
    const key = this.host.sel() || (rows[0] ? rows[0].key : '');
    if (!key) { this.rowEl.replaceChildren(); return; }
    const [k, id] = [key.slice(0, 1), key.slice(2)];
    const q = new URLSearchParams({ [k === 'r' ? 'region' : 'server']: id, days: String(d.days), tz: tzParam() });
    if (k === 'r' && d.exclude_fallback) q.set('exclude_fallback', '1');
    const want = q.toString();
    const seq = ++this.viewSeq;
    try {
      const v = /** @type {SpeedView} */ (await fetchJSON('api/speed?' + want));
      if (seq !== this.viewSeq) return;
      if (want !== this.viewKey) { this.shown = 15; }
      this.view = v;
      this.viewKey = want;
    } catch (e) {
      if (seq !== this.viewSeq) return;
      this.view = null;
      this.rowEl.replaceChildren(h('p', { class: 'muted', text: /** @type {any} */ (e).status === 404 ? 'That region or server isn’t in the list any more.' : 'Couldn’t load these tests. They’ll show on the next refresh.' }));
      return;
    }
    this.renderRow();
  }

  renderRow() {
    const d = this.d, v = this.view;
    if (!d || !v) return;
    const r = v.row;
    const isRegion = v.kind === 'region';
    const servers = new Map(d.servers.map((s) => [s.id, s]));
    // Shapes and colours by server, in the region's chain order, then as they appear.
    const chain = isRegion ? (d.regions.find((x) => x.id === r.id)?.servers || r.servers || []) : [r.id];
    const ids = [...new Set([...chain, ...v.tests.map((t) => t.server)])];
    const look = new Map(ids.map((id, i) => [id, { shape: SHAPES[i % SHAPES.length], color: css(`--s${(i % 8) + 1}`) || '#3987e5' }]));
    const best = r.best_down_bps || 0;
    const ups = v.tests.filter((t) => t.outcome === 'ok' && t.up_bps != null).map((t) => /** @type {number} */ (t.up_bps));
    const bestUp = ups.length ? [...ups].sort((a, b) => a - b)[Math.floor(ups.length * 0.9)] || Math.max(...ups) : 0;
    const val = (/** @type {SpeedTest} */ t) => {
      const b = this.dir === 'up' ? t.up_bps : t.down_bps;
      if (b == null) return null;
      if (this.unit === 'mbps') return b / 1e6;
      const sc = this.dir === 'up' ? t.up_score : t.score;
      if (sc != null) return sc;
      const n = this.dir === 'up' ? bestUp : best;
      return n ? b / n : null;
    };
    const fmtY = (/** @type {number} */ y) => (this.unit === 'mbps' ? `${Math.round(y)} Mbit/s` : pct(y));
    const pts = v.tests.filter((t) => val(t) != null).map((t) => {
      const dt = new Date(t.start);
      const lk = look.get(t.server) || { shape: 'circle', color: '#888' };
      return { t, hx: dt.getHours() + dt.getMinutes() / 60, y: /** @type {number} */ (val(t)), ...lk };
    });
    const line = this.unit === 'pct'
      ? r.hours.map((x) => { const v = this.dir === 'up' ? x.up_score_median : x.score_median; return v != null ? { x: x.hour + 0.5, y: v } : null; })
      : r.hours.map((x) => {
        const b = this.dir === 'up' ? x.up_median_bps : x.down_median_bps;
        if (b == null) return null;
        return { x: x.hour + 0.5, y: this.unit === 'mbps' ? b / 1e6 : b / ((this.dir === 'up' ? bestUp : best) || 1) };
      });
    const tipFor = (/** @type {{t:SpeedTest,y:number}} */ p) => [
      h('div', { class: 't', text: `${new Date(p.t.start).toLocaleDateString([], { weekday: 'short', month: 'short', day: 'numeric' })} ${clock(p.t.start)}` }),
      h('div', { text: `${p.t.label}${p.t.fallback ? ' (backup server)' : ''}${p.t.trigger !== 'schedule' ? ' · on demand' : ''}` }),
      h('div', { text: `${fmtMbps(p.t.down_bps)} down · ${fmtMbps(p.t.up_bps)} up${this.unit === 'pct' ? ` · ${fmtY(p.y)} of normal` : ''}` })];
    const dipHours = r.dip?.hours || dipHoursOf(d);
    const byHour = scatter({
      x: [0, 24], ticks: [0, 3, 6, 9, 12, 15, 18, 21, 24].map((x) => ({ v: x, label: x === 24 ? '' : hourShort(x) })),
      shade: dipHours.map((x) => [x, x + 1]), points: pts.map((p) => ({ ...p, x: p.hx })), line, fmtY, tip: tipFor, pctAxis: this.unit === 'pct',
    });
    const t0 = d.at - d.days * 86400e3;
    const dayTicks = [];
    for (let i = 0; i <= d.days; i += d.days > 7 ? 5 : 1) { const at = new Date(t0 + i * 86400e3); at.setHours(0, 0, 0, 0); dayTicks.push({ v: at.getTime(), label: at.toLocaleDateString([], d.days > 7 ? { month: 'short', day: 'numeric' } : { weekday: 'short' }) }); }
    const overDays = scatter({
      x: [t0, d.at], ticks: dayTicks, shade: [], points: pts.map((p) => ({ ...p, x: p.t.start })), line: [], fmtY, tip: tipFor, pctAxis: this.unit === 'pct', short: true,
    });
    const legend = h('ul', { class: 'sp-shapes' }, ...ids.filter((id) => v.tests.some((t) => t.server === id)).map((id) => {
      const lk = /** @type {{shape:string,color:string}} */ (look.get(id));
      const sw = h('span', { class: 'sp-shape', 'aria-hidden': 'true', text: SHAPE_CHAR[/** @type {'circle'} */ (lk.shape)] });
      sw.style.color = lk.color;
      return h('li', {}, sw, servers.get(id)?.label || id, chain[0] === id && isRegion ? h('span', { class: 'muted', text: ' (first choice)' }) : '');
    }), v.tests.some((t) => t.fallback) ? h('li', { class: 'muted' }, h('span', { class: 'sp-shape', 'aria-hidden': 'true', text: '○' }), 'hollow: a backup server ran it') : '');
    const okN = v.tests.filter((t) => t.outcome === 'ok').length;
    const summary = this.chartSummary(r, dipHours);
    byHour.setAttribute('aria-label', summary);
    overDays.setAttribute('aria-label', `${r.label}, ${okN} tests over ${d.days} days, one point per test.`);
    this.rowEl.replaceChildren(
      h('div', { class: 'sp-sec-head' },
        h('h2', { id: 'sp-row-h', text: r.label }),
        h('span', { class: 'muted small', text: isRegion ? chain.map((id) => servers.get(id)?.label || id).join(' · ') : (servers.get(r.id)?.display || '') })),
      r.why ? h('p', { class: 'muted small', text: `Tested because: ${r.why}` }) : '',
      /** @type {any} */ (r).too_fast ? h('p', { class: 'sp-note' }, h('b', { text: `${TOO_FAST}. ` }), tooFastWhy(r, isRegion)) : '',
      r.note ? h('p', { class: 'notice warn', text: r.note }) : '',
      h('h3', { text: `${this.dir === 'down' ? 'Download' : 'Upload'} by hour of day, ${this.unit === 'pct' ? '% of normal' : 'Mbit/s'}${r.learning && !/** @type {any} */ (r).too_fast && this.unit === 'pct' ? ' (learning: its normal isn’t known yet)' : ''}` }),
      h('div', { class: 'sp-chart' }, byHour),
      legend,
      h('p', { class: 'muted small', text: 'One mark per test; the line is each hour’s median.' + (dipHours.length ? ` Shaded: ${hourRange(dipHours)}.` : '') }),
      h('h3', { text: `Over the last ${d.days} days` }),
      h('div', { class: 'sp-chart short' }, overDays),
      isRegion ? this.compareNodes(v, servers, look, dipHours) : '',
      this.loadNodes(),
      this.testNodes(v, servers));
  }

  /** The by-hour chart in words. @param {SpeedRow} r @param {number[]} dipHours */
  chartSummary(r, dipHours) {
    const vals = r.hours.filter((x) => x.score_median != null);
    if (!vals.length) return `${r.label}: not enough tests yet to show speed by hour.`;
    const dipV = vals.filter((x) => dipHours.includes(x.hour)).map((x) => /** @type {number} */ (x.score_median));
    const rest = vals.filter((x) => !dipHours.includes(x.hour)).map((x) => /** @type {number} */ (x.score_median));
    const a = median(rest), b = median(dipV);
    if (dipV.length && a != null && b != null) return `${r.label}, download as % of normal by hour: about ${pct(a)} most of the day, ${pct(b)} from ${hourRange(dipHours)}.`;
    return `${r.label}, download as % of normal by hour: about ${pct(/** @type {number} */ (median(vals.map((x) => /** @type {number} */ (x.score_median)))))} through the day.`;
  }

  /**
   * Compare providers: within the region, each server's usual share of normal by
   * day and in the slower hours, with the word, never colour alone.
   * @param {SpeedView} v @param {Map<string,any>} servers @param {Map<string,{shape:string,color:string}>} look @param {number[]} dipHours
   */
  compareNodes(v, servers, look, dipHours) {
    const best = v.row.best_down_bps || 0;
    if (!best) return '';
    const by = new Map();
    for (const t of v.tests) {
      if (t.outcome !== 'ok' || t.down_bps == null) continue;
      const hr = new Date(t.start).getHours();
      const e = by.get(t.server) || { day: [], dip: [] };
      (dipHours.includes(hr) ? e.dip : e.day).push(t.score != null ? t.score : t.down_bps / best);
      by.set(t.server, e);
    }
    const rows = [...by.entries()].filter(([, e]) => e.day.length >= 3);
    if (rows.length < 2) return '';
    const dipWord = dipHours.length ? hourRange(dipHours) : '';
    const items = rows.map(([id, e]) => {
      const a = /** @type {number} */ (median(e.day)), b = e.dip.length >= 3 ? median(e.dip) : null;
      const dips = b != null && b <= a * 0.75;
      const name = servers.get(id)?.label || id;
      const text = b != null ? `${name}: ${pct(a)} by day, ${pct(b)} from ${dipWord}, ${dips ? 'dips' : 'no dip'}` : `${name}: usually ${pct(a)} of normal`;
      // The bar sits in a track beside its label, so its width is a share of the track, never more.
      const bar = (/** @type {number} */ x, /** @type {string} */ c) => { const s = h('span', { class: 'sp-bar-' + c }); s.style.width = `${Math.min(100, Math.round(x * 100))}%`; return h('span', { class: 'sp-cmp-track' }, s); };
      const lk = look.get(id);
      const sw = h('span', { class: 'sp-shape', 'aria-hidden': 'true', text: lk ? SHAPE_CHAR[/** @type {'circle'} */ (lk.shape)] : '' });
      if (lk) sw.style.color = lk.color;
      return h('li', { class: dips ? 'dips' : '' },
        h('span', { class: 'sp-cmp-name' }, sw, name),
        h('span', { class: 'sp-cmp-bars', 'aria-hidden': 'true' },
          h('span', { class: 'sp-cmp-row' }, h('span', { class: 'sp-cmp-l', text: 'by day' }), bar(a, 'day')),
          b != null ? h('span', { class: 'sp-cmp-row' }, h('span', { class: 'sp-cmp-l', text: dipWord }), bar(b, 'dip')) : ''),
        h('span', { class: 'sp-cmp-val', text: b != null ? `${pct(a)} → ${pct(b)}` : pct(a) }),
        h('span', { class: 'sp-cmp-word', text: b != null ? (dips ? 'dips' : 'no dip') : '' }),
        h('span', { class: 'sr-only', text: text }));
    });
    return h('div', { class: 'sp-cmp' },
      h('h3', { text: dipWord ? `Providers compared: by day vs ${dipWord}` : 'Providers compared' }),
      h('ul', {}, ...items));
  }

  /** Delay under load: the server's sentences over every test in the span. */
  loadNodes() {
    const u = /** @type {SpeedSummary} */ (this.d).under_load;
    if (!u || !u.text || !u.text.length) return '';
    return h('div', { class: 'sp-load' }, h('h3', { text: 'Under load (while a test runs)' }), h('ul', {}, ...u.text.map((t) => h('li', { text: t }))));
  }

  /** The tests, newest first: a table on desktop, cards on a phone. @param {SpeedView} v @param {Map<string,any>} servers */
  testNodes(v, servers) {
    const d = /** @type {SpeedSummary} */ (this.d);
    const best = v.row.best_down_bps || 0;
    let list = [...v.tests].reverse();
    // A test the owner stopped isn't a problem with the line.
    if (this.problems) list = list.filter((t) => (t.outcome !== 'ok' && t.outcome !== 'cancelled') || t.fallback);
    const shown = list.slice(0, this.shown);
    const regionTitle = (/** @type {string|null} */ id) => (id ? d.regions.find((r) => r.id === id)?.title || id : '');
    // "≥" per cell only where a column mixes lower bounds with full readings.
    const col = (/** @type {'down'|'up'} */ dir) => {
      const vals = shown.filter((t) => t[`${dir}_bps`] != null);
      const lb = vals.filter((t) => lowerBound(t, dir)).length;
      return { all: vals.length > 0 && lb === vals.length, some: lb > 0 };
    };
    const cd = col('down'), cu = col('up');
    const rows = shown.map((t) => {
      const bad = t.outcome !== 'ok';
      const lbDown = lowerBound(t, 'down'), lbUp = lowerBound(t, 'up');
      const x = /** @type {any} */ (t);
      const cancelled = t.outcome === 'cancelled'
        ? (x.cancelled_by === 'owner' ? `Stopped by you at ${clock(t.end)}.` : `Cancelled at ${clock(t.end)}.`) : '';
      const more = h('details', { class: 'sp-more' }, h('summary', { text: 'More', 'aria-label': `More about the ${clock(t.start)} test (${t.label})` }),
        h('dl', {},
          ...[cancelled ? ['Cancelled', cancelled] : null,
            lbDown || lbUp ? ['Speed', lbDown && !lbCounted(t, 'down') ? LB_OUT : LB_MORE] : null,
            ['Attempts', String(t.attempts)],
            /** @type {any} */ (t).port ? ['Port', String(/** @type {any} */ (t).port)] : null,
            ['Retransmits', t.down_retrans != null || t.up_retrans != null ? `${t.down_retrans ?? '—'} down, ${t.up_retrans ?? '—'} up` : 'not known'],
            ['Test length', t.down_s != null ? `${t.down_s} s down, ${t.up_s ?? '—'} s up` : `${Math.round((t.end - t.start) / 1000)} s`],
            t.bytes ? ['Data', `${Math.round(t.bytes / 1e6)} MB`] : null,
            ['Delay through your provider', t.latency?.edge ? `${lat(t.latency.edge.idle, t.latency.edge.down)} downloading; ${lat(t.latency.edge.idle, t.latency.edge.up)} uploading` : 'not known'],
            ['Router delay', t.latency?.gateway ? `${lat(t.latency.gateway.idle, t.latency.gateway.down)} downloading` : 'not known'],
            t.conn && t.conn !== 'unknown' ? ['Connection', t.conn === 'wifi' ? 'Wi-Fi' : 'cable'] : null,
            t.handoff ? ['Handed off to', handoffWords(t.handoff)] : null,
            t.fallback && t.region ? ['Why a backup', fallbackWhy(d, t)] : null]
            .filter((x) => !!x).flatMap((x) => [h('dt', { text: /** @type {string[]} */ (x)[0] }), h('dd', { text: /** @type {string[]} */ (x)[1] })])));
      return h('tr', { class: bad ? 'bad' : '' },
        h('td', { class: 'when', 'data-k': 'When', text: `${new Date(t.start).toLocaleDateString([], { weekday: 'short' })} ${clock(t.start)}` }),
        h('td', { 'data-k': 'Region', text: regionTitle(t.region) }),
        h('td', { 'data-k': 'Server', text: t.label }),
        // The phone cards label each value: they say "at least" where the column header would.
        readingCell(cd.all ? 'Down (at least)' : 'Down', t.down_bps, lbDown, !cd.all),
        readingCell(cu.all ? 'Up (at least)' : 'Up', t.up_bps, lbUp, !cu.all),
        // A lower bound counts as at least normal only when the server says so (down_lb_counted); otherwise it's left out: never slow.
        lbDown && !bad ? lbCell(lbCounted(t, 'down'))
          : h('td', { class: 'num', 'data-k': '% of normal', text: bad ? '—' : t.score != null ? pct(t.score) : best && t.down_bps != null ? pct(t.down_bps / best) : '—' }),
        h('td', { 'data-k': 'Result', text: resultWord(t) + (t.trigger !== 'schedule' ? ' · on demand' : '') }),
        h('td', { class: 'mo' }, more));
    });
    return h('div', { class: 'sp-tests' },
      h('div', { class: 'sp-sec-head' }, h('h3', { text: 'Tests' }),
        h('div', { class: 'seg sm', role: 'group', 'aria-label': 'Which tests' }, ...[[false, 'All'], [true, 'Problems only']].map(([p, l]) => h('button', {
          type: 'button', text: /** @type {string} */ (l), 'aria-pressed': String(this.problems === p), onclick: () => { this.problems = /** @type {boolean} */ (p); this.shown = 15; this.renderRow(); },
        })))),
      // One note above the table: all lower bounds, or a mix.
      // Every test runs up to 10 seconds each way.
      cd.all || cu.all ? h('p', { class: 'muted small', text: `Your line is faster than these tests can fully measure in 10 seconds each way. Treat the numbers as ‘at least’.` })
        : cd.some || cu.some ? h('p', { class: 'muted small', text: '≥ means the test was still speeding up when it stopped, so the speed was at least this.' }) : '',
      rows.length ? h('table', { class: 'sp-tt' },
        h('thead', {}, h('tr', {}, ...['When', 'Region', 'Server', cd.all ? 'Down (Mbit/s, at least)' : 'Down (Mbit/s)', cu.all ? 'Up (Mbit/s, at least)' : 'Up (Mbit/s)', '% of normal', 'Result', ''].map((x, i) => h('th', { scope: 'col', class: i >= 3 && i <= 5 ? 'num' : '', text: x })))),
        h('tbody', {}, ...rows)) : h('p', { class: 'muted', text: this.problems ? 'No problems in this span.' : 'No tests in this span yet.' }),
      list.length > this.shown ? h('button', { class: 'btn sm', type: 'button', text: `Show older (${list.length - this.shown} more)`, onclick: () => { this.shown += 30; this.renderRow(); } }) : '');
  }

  // ---------- every row, sortable ----------

  /** Skips a server, or tests it again: the whole list goes to config, minus the start-up setting's. @param {string} id @param {boolean} skip */
  async setSkip(id, skip) {
    const d = /** @type {SpeedSummary} */ (this.d);
    const list = new Set(d.servers.filter((x) => x.skipped && !x.skip_locked).map((x) => x.id));
    if (skip) list.add(id); else list.delete(id);
    const label = d.servers.find((x) => x.id === id)?.label || id;
    try {
      await apiWrite('POST', 'api/speed/config', { skip: [...list] });
      this.skipNote = skip ? `${label} is skipped. It won’t be tested.` : `${label} will be tested again.`;
    } catch (e) {
      this.skipNote = /** @type {any} */ (e).status === 401 ? '' : 'Couldn’t change that. Try again.';
    }
    this.say.textContent = this.skipNote;
    await this.host.refresh();
    /** @type {HTMLElement|null} */ (this.sumEl.querySelector(`input[aria-label="Test ${label}"]`))?.focus();
  }

  renderSummary() {
    const d = /** @type {SpeedSummary} */ (this.d);
    const regions = this.rows === 'regions';
    // Every catalog server in server mode, the skipped ones too (they have no tests), so the owner can turn them back on.
    const have = new Map(d.per_server.map((r) => [r.id, r]));
    const empty = (/** @type {import('./speed.js').SpeedServer} */ x) => /** @type {SpeedRow} */ ({ id: x.id, kind: 'server', label: x.label, latest_at: null, n: 0, busy: 0, unreachable: 0, failed: 0, fallback: 0,
      down_median_bps: null, down_p10_bps: null, up_median_bps: null, up_p10_bps: null, best_down_bps: null, dip: null, hours: [] });
    const list = regions ? d.per_region : d.servers.map((x) => have.get(x.id) || empty(x));
    const srv = new Map(d.servers.map((x) => [x.id, x]));
    const owner = !regions;
    /** @type {[string,string,(r:SpeedRow)=>number|string|null,(r:SpeedRow)=>string][]} */
    const cols = [
      ['label', regions ? 'Region' : 'Server', (r) => r.label, (r) => r.label],
      ['down', 'Download (median)', (r) => r.down_median_bps, (r) => fmtMbps(r.down_median_bps)],
      ['p10', 'Slow end (p10)', (r) => r.down_p10_bps, (r) => fmtMbps(r.down_p10_bps)],
      ['up', 'Upload (median)', (r) => r.up_median_bps, (r) => fmtMbps(r.up_median_bps)],
      ['dip', 'Slower hours', (r) => (r.dip ? -r.dip.drop : 0), (r) => (r.dip ? `${hourRange(r.dip.hours)}, −${Math.round(r.dip.drop * 100)}%` : '—')],
      ...(regions ? /** @type {typeof cols} */ ([['fb', 'Backup server', (r) => (r.n ? r.fallback / r.n : null), (r) => (r.n ? pct(r.fallback / r.n) : '—')]]) : []),
      ['busy', 'Busy', (r) => r.busy, (r) => String(r.busy)],
      ['failed', 'Failed', (r) => r.unreachable + r.failed, (r) => String(r.unreachable + r.failed)],
      ['last', 'Last test', (r) => r.latest_at, (r) => (r.latest_at ? clock(r.latest_at) : '—')],
      ...(!regions ? /** @type {typeof cols} */ ([['state', 'State', (r) => serverState(srv.get(r.id)), (r) => serverState(srv.get(r.id))]]) : []),
    ];
    const col = cols.find((c) => c[0] === this.sort.key) || cols[0];
    const sorted = [...list].sort((a, b) => {
      const x = col[2](a), y = col[2](b);
      if (x == null && y == null) return 0;
      if (x == null) return 1;
      if (y == null) return -1;
      const c = typeof x === 'string' ? x.localeCompare(String(y)) : x - /** @type {number} */ (y);
      return this.sort.asc ? c : -c;
    });
    const sel = this.host.sel();
    this.sumEl.replaceChildren(
      h('h2', { id: 'sp-sum-h', text: regions ? 'Every region' : 'Every server' }),
      owner ? h('p', { class: 'muted small', text: 'Untick a server to skip it: it is never tested, by the schedule or a full test.' }) : '',
      this.skipNote ? h('p', { class: 'sp-note', text: this.skipNote }) : '',
      h('div', { class: 'sp-sum-wrap' }, h('table', { class: 'sp-st' },
        h('thead', {}, h('tr', {}, ...cols.map(([k, l]) => h('th', {
          scope: 'col', class: k === 'label' ? '' : 'num', 'aria-sort': this.sort.key === k ? (this.sort.asc ? 'ascending' : 'descending') : null,
        }, h('button', { type: 'button', class: 'sp-sort', text: l + (this.sort.key === k ? (this.sort.asc ? ' ▲' : ' ▼') : ''), onclick: () => {
          this.sort = { key: k, asc: this.sort.key === k ? !this.sort.asc : k === 'label' };
          this.renderSummary();
          /** @type {HTMLElement|null} */ (this.sumEl.querySelector(`th[aria-sort] .sp-sort`))?.focus();
        } }))), owner ? h('th', { scope: 'col', text: 'Schedule' }) : null)),
        h('tbody', {}, ...sorted.map((r) => {
          const key = (regions ? 'r:' : 's:') + r.id;
          const x = srv.get(r.id);
          return h('tr', { class: (key === sel ? 'sel' : '') + (x && x.skipped ? ' skipped' : '') }, ...cols.map(([k, , , f], i) => (i === 0
            ? h('th', { scope: 'row' }, r.n ? h('button', { type: 'button', class: 'sp-rb', text: f(r), onclick: () => this.select(key) }) : h('span', { text: f(r) }),
              /** @type {any} */ (r).too_fast ? h('span', { class: 'muted small', title: tooFastWhy(r, regions), text: ` ${TOO_FAST}` })
                : r.learning ? h('span', { class: 'muted small', text: ' (learning)' }) : '')
            : h('td', { class: k === 'state' ? '' : 'num', 'data-k': cols[i][1], text: f(r) }))),
          owner && x ? h('td', { class: 'sp-skip' }, h('label', { class: 'sp-check', title: x.skip_locked ? 'Skipped by a start-up setting (--speedtest-skip)' : '' },
            h('input', { type: 'checkbox', checked: !x.skipped, disabled: !!x.skip_locked, 'aria-label': `Test ${x.label}`, onchange: (/** @type {Event} */ e) => this.setSkip(x.id, !/** @type {HTMLInputElement} */ (e.target).checked) }),
            'Test')) : null);
        })))));
  }
}

/** A server's schedule state in words. @param {import('./speed.js').SpeedServer|undefined} x */
function serverState(x) {
  if (!x) return '';
  if (x.skip_locked) return 'Skipped (start-up setting)';
  if (x.skipped) return 'Skipped';
  if (x.rest_until && x.rest_until > Date.now()) return `Resting until ${clock(x.rest_until)}`;
  return 'Included';
}

/** "Leaseweb London was busy, so Clouvider London ran it." @param {SpeedSummary} d @param {SpeedTest} t */
function fallbackWhy(d, t) {
  const rg = d.regions.find((r) => r.id === t.region);
  const first = rg && d.servers.find((s) => s.id === rg.servers[0]);
  return first ? `${first.label} was busy or didn’t answer, so ${t.label} ran it.` : 'The first choice was busy.';
}

/**
 * A scatter chart on a canvas: x in hours of day or ms, y as a share (%) or
 * Mbit/s, marks shaped by server (hollow: a backup server), an optional median
 * line and shaded hours. Hover shows the nearest test. role=img; the caller sets
 * its aria-label.
 * @param {{x:[number,number], ticks:{v:number,label:string}[], shade:number[][], points:{x:number,y:number,shape:string,color:string,t:SpeedTest}[], line:({x:number,y:number}|null)[], fmtY:(y:number)=>string, tip:(p:any)=>Node[], pctAxis:boolean, short?:boolean}} o
 */
function scatter(o) {
  const cv = /** @type {HTMLCanvasElement} */ (h('canvas', { role: 'img' }));
  const draw = () => {
    const w = cv.parentElement ? cv.parentElement.clientWidth : 600;
    const ht = o.short ? 150 : 220;
    const dpr = window.devicePixelRatio || 1;
    cv.width = Math.round(w * dpr); cv.height = Math.round(ht * dpr);
    cv.style.width = `${w}px`; cv.style.height = `${ht}px`;
    const g = /** @type {CanvasRenderingContext2D} */ (cv.getContext('2d'));
    g.scale(dpr, dpr);
    const L = 44, R = 8, T = 8, B = 22;
    const ymax = Math.max(o.pctAxis ? 1.2 : 1, ...o.points.map((p) => p.y), ...o.line.filter((p) => !!p).map((p) => /** @type {{y:number}} */ (p).y)) * (o.pctAxis ? 1 : 1.08);
    const X = (/** @type {number} */ v) => L + (v - o.x[0]) / (o.x[1] - o.x[0]) * (w - L - R);
    const Y = (/** @type {number} */ v) => T + (1 - v / ymax) * (ht - T - B);
    const ink2 = css('--ink-2'), grid = css('--grid'), font = '11px system-ui, -apple-system, "Segoe UI", sans-serif';
    g.font = font;
    for (const [a, b] of o.shade) { g.fillStyle = rgba(css('--muted') || '#888', 0.14); g.fillRect(X(a), T, X(b) - X(a), ht - T - B); }
    g.strokeStyle = grid; g.lineWidth = 1; g.fillStyle = ink2;
    const ys = o.pctAxis ? [0, 0.5, 1] : niceTicks(ymax);
    g.textAlign = 'right'; g.textBaseline = 'middle';
    for (const y of ys) { g.beginPath(); g.moveTo(L, Math.round(Y(y)) + 0.5); g.lineTo(w - R, Math.round(Y(y)) + 0.5); g.stroke(); g.fillText(o.pctAxis ? pct(y) : String(Math.round(y)), L - 6, Y(y)); }
    g.textAlign = 'center'; g.textBaseline = 'top';
    for (const t of o.ticks) if (t.label) g.fillText(t.label, X(t.v), ht - B + 6);
    // The median line, broken where an hour has too few tests.
    g.strokeStyle = css('--ink') || '#000'; g.lineWidth = 1.5; g.beginPath();
    let pen = false;
    for (const p of o.line) { if (!p) { pen = false; continue; } if (pen) g.lineTo(X(p.x), Y(p.y)); else g.moveTo(X(p.x), Y(p.y)); pen = true; }
    g.stroke();
    for (const p of o.points) mark(g, p.shape, X(p.x), Y(p.y), p.color, p.t.fallback);
  };
  const pick = (/** @type {MouseEvent} */ e) => {
    const r = cv.getBoundingClientRect();
    const w = r.width, ht = r.height, L = 44, R = 8, T = 8, B = 22;
    const ymax = Math.max(o.pctAxis ? 1.2 : 1, ...o.points.map((p) => p.y), ...o.line.filter((p) => !!p).map((p) => /** @type {{y:number}} */ (p).y)) * (o.pctAxis ? 1 : 1.08);
    let best = null, bd = 100;
    for (const p of o.points) {
      const px = L + (p.x - o.x[0]) / (o.x[1] - o.x[0]) * (w - L - R), py = T + (1 - p.y / ymax) * (ht - T - B);
      const dd = Math.hypot(px - (e.clientX - r.left), py - (e.clientY - r.top));
      if (dd < bd) { bd = dd; best = p; }
    }
    if (best) showTip(o.tip(best), e.clientX, e.clientY); else hideTip();
  };
  cv.addEventListener('mousemove', pick);
  cv.addEventListener('mouseleave', () => hideTip());
  requestAnimationFrame(() => {
    draw();
    if (cv.parentElement) new ResizeObserver(() => { if (cv.isConnected) draw(); }).observe(cv.parentElement);
  });
  return cv;
}
function niceTicks(/** @type {number} */ max) {
  const step = [1, 2, 5, 10, 20, 50, 100, 200, 500, 1000].find((s) => max / s <= 5) || 1000;
  const out = [];
  for (let v = 0; v <= max; v += step) out.push(v);
  return out;
}
/** @param {CanvasRenderingContext2D} g @param {string} shape @param {number} x @param {number} y @param {string} c @param {boolean} hollow */
function mark(g, shape, x, y, c, hollow) {
  const r = 3.5;
  g.beginPath();
  if (shape === 'circle') g.arc(x, y, r, 0, Math.PI * 2);
  else if (shape === 'square') g.rect(x - r, y - r, r * 2, r * 2);
  else {
    const n = shape === 'triangle' ? 3 : shape === 'diamond' ? 4 : 5;
    const rr = shape === 'triangle' ? r * 1.3 : r * 1.2;
    for (let i = 0; i < n; i++) {
      const a = -Math.PI / 2 + i * 2 * Math.PI / n;
      if (i) g.lineTo(x + rr * Math.cos(a), y + rr * Math.sin(a)); else g.moveTo(x + rr * Math.cos(a), y + rr * Math.sin(a));
    }
    g.closePath();
  }
  if (hollow) { g.strokeStyle = c; g.lineWidth = 1.5; g.stroke(); } else { g.fillStyle = rgba(c, 0.8); g.fill(); }
}
