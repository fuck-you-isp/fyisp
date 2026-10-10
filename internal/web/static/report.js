// fyisp's public link: the internet quality report. A stranger reads it (an
// internet provider's support agent, an employer's IT), so it is in the third
// person, every time is absolute and carries its UTC offset, and it shows
// the whole week's problems, home-side ones included. Every sentence comes
// from /api/home in the public voice (the share link forces it); Details,
// with the charts, is one click away. Plain ES module, no build step.
// @ts-check

import { h, fetchJSON, tzParam } from './core.js';
import { LEVELS, NODE_ICON, badge, cls, minutes, buildTimeline } from './home.js';
import { SpeedReport } from './speedreport.js';

/** @typedef {import('./home.js').HomeLive} HomeLive */
/** @typedef {import('./home.js').HomeHistory} HomeHistory */
/** @typedef {import('./home.js').HomeProblem} HomeProblem */

const LIVE_MS = 15000;
const HISTORY_MS = 60000;
const LIVE_FAILS = 2;

/** "UTC+2", "UTC−5:30" for minutes east of UTC. */
function zone(/** @type {number} */ tz) {
  const sign = tz < 0 ? '−' : '+';
  const a = Math.abs(tz), hh = Math.floor(a / 60), mm = a % 60;
  return tz === 0 ? 'UTC' : `UTC${sign}${hh}${mm ? ':' + String(mm).padStart(2, '0') : ''}`;
}
/** An absolute date and time in the viewer's locale: "Sat 10 Oct, 14:59". */
function stamp(/** @type {number} */ ms) {
  const d = new Date(ms);
  return `${d.toLocaleDateString([], { weekday: 'short', day: 'numeric', month: 'short' })}, ${d.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' })}`;
}
/** The time only, for the end of a stretch on the same day. */
function time(/** @type {number} */ ms) { return new Date(ms).toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' }); }
/** "Sat 10 Oct, 14:59 to 15:20" (the date once when both ends are on one day). */
function span(/** @type {number} */ a, /** @type {number} */ b) {
  return new Date(a).toDateString() === new Date(b).toDateString() ? `${stamp(a)} to ${time(b)}` : `${stamp(a)} to ${stamp(b)}`;
}
/** @type {Object<string,string>} the problem table's "Side" column */
const SIDE = { home: 'Home network', provider: 'Provider', other: 'Elsewhere' };

/**
 * The report. It has Home's interface for app.js (el, refresh, tick, render, shown).
 */
export class Report {
  constructor() {
    /** @type {HomeLive|null} */
    this.live = null;
    /** @type {HomeHistory|null} */
    this.history = null;
    this.liveAt = 0;
    this.liveFails = 0;
    this.liveBusy = false;
    this.historyAt = 0;
    this.historyBusy = false;
    this.historyErr = false;
    this.focus = { idx: -1, span: '' };

    this.zoneEl = h('p', { class: 'rp-zone' });
    this.nowBadge = h('span', {});
    this.nowHead = h('h2', { class: 'ho-headline', id: 'rp-now-h' });
    this.nowCause = h('p', { class: 'ho-cause' });
    this.nowPath = h('ol', { class: 'ho-path', 'aria-label': 'Where the problem is' });
    this.nowMeta = h('p', { class: 'ho-meta' });
    this.now = h('section', { class: 'ho-card lvl-checking', 'aria-labelledby': 'rp-now-h' },
      h('p', { class: 'rp-kicker', text: 'Right now' }),
      h('div', { class: 'ho-card-top' }, this.nowBadge, this.nowHead), this.nowCause, this.nowPath, this.nowMeta);

    this.periodEl = h('p', { class: 'rp-period' });
    this.sumEl = h('p', { class: 'ho-summary' });
    this.sidesEl = h('ul', { class: 'rp-sides', 'aria-label': 'Problems by side' });
    this.coverEl = h('p', { class: 'ho-muted' });
    this.stripsEl = h('div', { class: 'ho-strips' });
    this.legendEl = h('div', { class: 'ho-legend' });
    this.errEl = h('p', { class: 'ho-err', hidden: true }, 'Couldn’t load the history. ',
      h('button', { class: 'ho-link', type: 'button', text: 'Try again', onclick: () => this.loadHistory() }));
    this.week = h('section', { class: 'ho-time', 'aria-labelledby': 'rp-week-h' },
      h('h2', { id: 'rp-week-h', text: 'The last 7 days' }),
      this.periodEl, this.sumEl, this.sidesEl, this.errEl, this.stripsEl, this.legendEl, this.coverEl);

    this.tbody = h('tbody');
    this.tableNote = h('p', { class: 'ho-muted' });
    this.table = h('section', { class: 'ho-time rp-problems', 'aria-labelledby': 'rp-prob-h' },
      h('h2', { id: 'rp-prob-h', text: 'Problems' }),
      this.tableNote,
      h('table', { class: 'rp-table' },
        h('thead', {}, h('tr', {}, ...['When', 'How long', 'Side', 'What happened', 'Worst minute', ''].map((t) => h('th', { scope: 'col', text: t })))),
        this.tbody));

    this.speed = new SpeedReport();
    this.el = h('main', { id: 'home', class: 'home report', hidden: true },
      h('header', { class: 'rp-head' },
        h('p', { class: 'ho-muted', text: 'A record of this internet connection’s quality, measured from a device in this home and shared read-only by its owner.' }),
        this.zoneEl),
      this.now, this.week, this.speed.el, this.table,
      h('p', { class: 'ho-muted rp-foot', text: 'Every few seconds, a device in this home sends small test messages to its router, its internet provider and common apps and websites. The other people on a call aren’t measured. “Details” has the charts behind this report.' }));
  }

  shown() { return !this.el.hidden; }
  refresh() { return Promise.all([this.loadLive(), this.loadHistory()]); }
  tick() {
    if (!this.shown() || document.hidden) return;
    const now = Date.now();
    if (now - this.liveAt >= LIVE_MS && !this.liveBusy) this.loadLive();
    if (now - this.historyAt >= HISTORY_MS && !this.historyBusy) this.loadHistory();
  }
  tz() { return tzParam(); }

  async loadLive() {
    if (this.liveBusy) return;
    this.liveBusy = true;
    const started = Date.now();
    try {
      this.live = await fetchJSON('api/home?' + new URLSearchParams({ part: 'live', tz: this.tz() }));
      this.liveFails = 0;
      this.liveAt = Date.now();
    } catch {
      this.liveFails++;
      this.liveAt = started;
    } finally {
      this.liveBusy = false;
    }
    this.renderNow();
  }

  async loadHistory() {
    if (this.historyBusy) return;
    this.historyBusy = true;
    this.historyAt = Date.now();
    try {
      this.history = await fetchJSON('api/home?' + new URLSearchParams({ part: 'history', span: 'w', tz: this.tz() }));
      this.historyErr = false;
    } catch {
      this.historyErr = true;
      this.historyAt = Date.now() - HISTORY_MS + 15000;
    } finally {
      this.historyBusy = false;
    }
    this.renderWeek();
    this.speed.load();
  }

  render() { this.renderNow(); this.renderWeek(); }

  /** Right now: the third-person answer and the path, as of an absolute time. */
  renderNow() {
    document.title = 'Internet quality report';
    const d = this.live;
    const lost = this.liveFails >= LIVE_FAILS;
    if (!d) {
      this.now.className = 'ho-card lvl-checking';
      this.nowBadge.replaceChildren(badge('checking', LEVELS.checking.word));
      this.nowHead.textContent = lost ? 'This report isn’t available right now' : 'Loading…';
      this.nowCause.textContent = lost ? 'The device running fyisp may be off or asleep.' : '';
      return;
    }
    const a = d.answer;
    const level = lost ? 'checking' : a.level;
    this.now.className = `ho-card lvl-${cls(level)}`;
    this.nowBadge.replaceChildren(badge(level, (LEVELS[level] || LEVELS.checking).word));
    this.nowHead.textContent = a.headline;
    this.nowCause.textContent = [a.cause, a.hint].filter(Boolean).join(' ');
    this.nowCause.hidden = !this.nowCause.textContent;
    const nodes = (d.path || []).filter((n) => (n.id !== 'device' && n.id !== 'apps') || n.state !== 'ok' || n.blamed);
    const items = [];
    nodes.forEach((n, i) => {
      if (i) items.push(h('li', { class: 'ho-arrow', 'aria-hidden': 'true', text: '→' }));
      items.push(h('li', { class: `ho-node st-${cls(n.state)}${n.blamed ? ' blamed' : ''}` },
        h('span', { class: 'ho-icon', 'aria-hidden': 'true', text: NODE_ICON[n.state] || '?' }),
        h('span', { class: 'ho-node-l', text: n.label }),
        n.state !== 'ok' ? h('span', { class: 'ho-node-w', text: n.words }) : h('span', { class: 'sr-only', text: `: ${n.words}` }),
        n.blamed && n.culprit ? h('span', { class: 'ho-node-b', text: n.culprit }) : null));
    });
    this.nowPath.hidden = a.level === 'checking' || !items.length;
    this.nowPath.replaceChildren(...items);
    const z = this.history ? zone(this.history.tz) : '';
    const bits = [];
    if (a.since && a.level !== 'good' && a.level !== 'checking') bits.push(`Since ${stamp(a.since)}${z ? ` (${z})` : ''}, ${minutes(Date.now() - a.since)}`);
    bits.push(lost ? `Not updated since ${stamp(this.liveAt)}` : `Checked ${stamp(d.at)}${z ? ` (${z})` : ''}`);
    this.nowMeta.textContent = bits.join(' · ');
  }

  /** The week: period, summary, provider vs home counts, the strips, coverage, and the problem table. */
  renderWeek() {
    this.errEl.hidden = !this.historyErr;
    const d = this.history;
    if (!d) {
      this.sumEl.textContent = this.historyErr ? '' : 'Loading…';
      return;
    }
    const z = zone(d.tz);
    this.zoneEl.textContent = `All times are in ${z}.`;
    this.periodEl.textContent = `${stamp(d.from)} to ${stamp(d.to)} (${z})`;
    this.sumEl.replaceChildren(h('span', { text: d.summary }));
    if (d.known && d.known.text) this.sumEl.append(' ', h('span', { text: `${d.known.text} ${stamp(d.known.since)} (${z}).` }));
    // Provider side vs home side, as counts in words.
    const s = d.sides || { home: 0, provider: 0, other: 0 };
    this.sidesEl.replaceChildren(...[['provider', 'In the provider’s network'], ['home', 'In the home network'], ['other', 'Elsewhere']].map(([k, l]) =>
      h('li', { class: `rp-side${s[/** @type {'home'} */ (k)] ? ' some' : ''}` },
        h('span', { class: 'rp-n', text: String(s[/** @type {'home'} */ (k)] || 0) }),
        h('span', { text: l }))));
    const t = buildTimeline(d, this.focus, (st) => this.pickBlock(st), { noKeep: true, offWord: 'Not measured (fyisp wasn’t running)', dates: true });
    this.stripsEl.replaceChildren(t.grid);
    this.legendEl.replaceChildren(...t.legend);
    // How much of the period was measured (the server's figure): honest about gaps.
    if (typeof d.coverage === 'number') this.coverEl.textContent = `Measured ${Math.round(d.coverage * 100)}% of these 7 days.`;
    this.renderTable(d);
    this.renderNow(); // its times carry the zone, which comes with the history
  }

  /** A timeline block picked: show its problem in the table. */
  pickBlock(/** @type {{run?:{incident?:number}|null}} */ st) {
    const id = st.run && st.run.incident;
    const row = id ? this.tbody.querySelector(`tr[data-id="${id}"]`) : null;
    if (row) {
      row.scrollIntoView({ block: 'center' });
      /** @type {HTMLElement} */ (row).focus();
    }
  }

  renderTable(/** @type {HomeHistory} */ d) {
    const rows = d.problems || [];
    const known = rows.filter((p) => p.level === 'info').length;
    this.tableNote.textContent = rows.length
      ? `Every problem of the last 7 days, newest first, including those in the home network.${known ? ' Known issues are listed but not counted.' : ''}`
      : 'No problems in the last 7 days.';
    this.tbody.replaceChildren(...rows.map((p) => {
      const end = p.ongoing ? Date.now() : p.to || p.from;
      const lvl = p.level || 'bad';
      return h('tr', { class: `lvl-${cls(lvl)}`, 'data-id': String(p.id), tabindex: '-1' },
        h('td', { class: 'rp-when', 'data-label': 'When' }, p.ongoing ? `Since ${stamp(p.from)}, ongoing` : span(p.from, end)),
        h('td', { 'data-label': 'How long', text: minutes(end - p.from) }),
        h('td', { 'data-label': 'Side' }, badge(lvl, lvl === 'info' ? LEVELS.info.word : SIDE[p.side || ''] || 'Elsewhere')),
        h('td', { class: 'rp-what', 'data-label': 'What happened', text: p.text || p.where }),
        h('td', { 'data-label': 'Worst minute', text: p.peak_loss >= 0.995 ? 'nothing got through' : `${Math.round(p.peak_loss * 100)}% lost` }),
        h('td', {}, h('a', {
          class: 'ho-link', href: p.details, text: 'Charts',
          'aria-label': `Charts for ${p.ongoing ? 'the problem since ' + stamp(p.from) : 'the problem ' + span(p.from, end)}`,
          onclick: (/** @type {MouseEvent} */ e) => { e.preventDefault(); this.openDetails(p.details); },
        })));
    }));
  }

  /** Set by app.js: opens Details at a hash. @type {(hash: string) => void} */
  openDetails = () => {};
}
