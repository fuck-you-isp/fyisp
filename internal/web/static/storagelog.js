// Details → Storage: a campaign's failures, newest first, 15 at a time
// (api/storage?part=attempts&failed=1), filtered by target; and the past
// campaigns, each one a button that shows it.
// Plain ES module, no build step.
// @ts-check

import { h, fetchJSON, fmtTime } from './core.js';
import { classShort, statusWords } from './storagewords.js';
import { edge, stepName } from './storagegrid.js';
import { size } from './storageform.js';

const PAGE = 15;

export class FailureLog {
  /** @param {{name:(id:string)=>string}} host */
  constructor(host) {
    this.host = host;
    this.campaign = 0;
    /** @type {{id:string,name:string}[]} */
    this.targets = [];
    this.target = '';
    /** @type {any[]} */
    this.items = [];
    this.total = 0;
    /** @type {number|null} */
    this.next = null;
    this.seq = 0;
    this.spanMs = 0;
    this.err = '';
    this.el = h('section', { class: 'st-log sp-body', 'aria-labelledby': 'st-log-h' });
  }

  /**
   * Shows campaign id's failures from the newest, as many as were shown
   * (Show more survives a reload). spanMs: how long the campaign has run
   * (times get dates over a day).
   * @param {number} id @param {{id:string,name:string}[]} targets @param {number} [spanMs]
   */
  async load(id, targets, spanMs = 0) {
    const keep = id === this.campaign ? Math.max(this.items.length, PAGE) : PAGE;
    if (id !== this.campaign) this.target = '';
    this.campaign = id;
    this.targets = targets;
    this.spanMs = spanMs;
    this.items = [];
    this.next = null;
    await this.more(keep);
  }

  async more(n = PAGE) {
    const seq = ++this.seq;
    const q = new URLSearchParams({ part: 'attempts', campaign: String(this.campaign), failed: '1', limit: String(Math.min(n, 500)) });
    if (this.target) q.set('target', this.target);
    if (this.next) q.set('before', String(this.next));
    try {
      const r = await fetchJSON('api/storage?' + q);
      if (seq !== this.seq) return;
      this.items.push(...r.items);
      this.total = r.total;
      this.next = r.next_before;
      this.err = '';
    } catch {
      if (seq !== this.seq) return;
      this.err = 'Couldn’t load the failures. They’ll show again when fyisp answers.';
    }
    this.render();
  }

  /** Redraws, keeping the focus on a filter button or Show more (by data-key). */
  render() {
    const a = /** @type {HTMLElement|null} */ (document.activeElement);
    const had = a && this.el.contains(a) ? a.dataset.key : '';
    const span = this.items.length ? this.items[0].at - this.items[this.items.length - 1].at : 0;
    const filter = this.targets.length > 1 ? h('div', { class: 'seg', role: 'group', 'aria-label': 'Target' },
      ...[{ id: '', name: 'All targets' }, ...this.targets].map((t) => h('button', { type: 'button', text: t.name, 'aria-pressed': String(this.target === t.id), 'data-key': `t:${t.id}`,
        onclick: () => { this.target = t.id; this.items = []; this.load(this.campaign, this.targets, this.spanMs).then(() => /** @type {HTMLElement|null} */ (this.el.querySelector('.seg button[aria-pressed="true"]'))?.focus()); } }))) : null;
    this.el.replaceChildren(...[
      h('div', { class: 'sp-sec-head' }, h('h3', { id: 'st-log-h', tabindex: '-1', text: 'Failures' }), filter),
      this.err ? h('p', { class: 'notice danger', text: this.err }) : null,
      !this.items.length && !this.err ? h('p', { class: 'muted', text: 'No failures in this campaign.' }) : null,
      this.items.length ? h('div', { class: 'st-scroll' }, h('table', { class: 'sp-st' },
        h('caption', { class: 'sr-only', text: 'Failed attempts, newest first' }),
        h('thead', {}, h('tr', {}, ...['When', 'Target', 'Step', 'What happened', 'Server', 'Request ID'].map((c) => h('th', { scope: 'col', text: c })))),
        h('tbody', {}, ...this.items.map((a) => h('tr', { class: a.group === 'config' ? 'bad' : null },
          h('th', { scope: 'row', class: 'when', text: fmtTime(a.at, Math.max(span, this.spanMs)) }),
          h('td', { 'data-k': 'Target', text: this.host.name(a.target) }),
          h('td', { 'data-k': 'Step', text: stepName(a) }),
          h('td', { 'data-k': 'Result', text: `${classShort(a.class)}${statusWords(a.http_status, a.s3_code)}${a.group === 'config' ? ' (configuration)' : ''}` }),
          h('td', { text: edge(a.edge_prefix) }),
          h('td', { class: 'st-rid', title: [a.request_id, a.host_id].filter(Boolean).join('\n') || null, text: a.request_id || '' })))))) : null,
      this.items.length ? h('p', { class: 'muted small', text: `${this.items.length} of ${this.total} shown.` }) : null,
      this.next ? h('button', { type: 'button', class: 'btn st-more', 'data-key': 'more', text: `Show ${PAGE} more`, onclick: () => this.more().then(() => /** @type {HTMLElement|null} */ (this.el.querySelector('.st-more'))?.focus()) }) : null,
    ].filter((n) => n != null));
    if (had) /** @type {HTMLElement|null} */ (this.el.querySelector(`[data-key="${CSS.escape(had)}"]`) || this.el.querySelector('h3'))?.focus();
  }
}

/** "Thu, Oct 23, 7:00 pm". @param {number} ms */
export function day(ms) {
  return new Date(ms).toLocaleString([], { weekday: 'short', month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' });
}

/**
 * The past campaigns table. show(id) displays one.
 * @param {any[]} hist @param {number} current @param {(id:number)=>void} show
 */
export function historyTable(hist, current, show) {
  if (!hist.length) return null;
  return h('section', { class: 'st-hist sp-body', 'aria-labelledby': 'st-hist-h' },
    h('h2', { id: 'st-hist-h', text: 'Past campaigns' }),
    h('div', { class: 'st-scroll' }, h('table', { class: 'sp-st' },
      h('caption', { class: 'sr-only', text: 'Past campaigns, newest first' }),
      h('thead', {}, h('tr', {}, ...['When', 'Kind', 'Targets', 'Rounds', 'Data', ''].map((c) => h('th', { scope: 'col', text: c })))),
      h('tbody', {}, ...hist.map((c) => h('tr', { class: c.id === current ? 'sel' : null },
        h('th', { scope: 'row', text: day(c.start_at || c.created) }),
        h('td', { 'data-k': 'Kind', text: c.shape === 'recurring' ? 'Schedule' : 'On demand' }),
        h('td', { 'data-k': 'Targets', text: c.targets.map((/** @type {any} */ t) => t.name).join(', ') }),
        h('td', { class: 'num', 'data-k': 'Rounds', text: String(c.rounds_done) }),
        h('td', { class: 'num', 'data-k': 'Data', text: `${size(c.bytes_down)} down, ${size(c.bytes_up)} up` }),
        h('td', {}, h('button', { type: 'button', class: 'btn sm', text: c.id === current ? 'Shown' : 'Show', disabled: c.id === current, 'data-key': `show:${c.id}`,
          'aria-label': `Show the campaign of ${day(c.start_at || c.created)}`, onclick: () => show(c.id) }))))))));
}
