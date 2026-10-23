// Details → Storage: a campaign's timeline (api/storage?part=campaign): one
// column per round, one row per target, each cell the worst thing in that
// round as a fill, a glyph and a sentence (never colour alone). One tab
// stop; arrows move inside, Enter opens the round's attempts
// (part=round). Show as table writes each cell's letter as text.
// Plain ES module, no build step.
// @ts-check

import { h, fetchJSON, clock, fmtMs, fmtMbps } from './core.js';
import { classShort, statusWords } from './storagewords.js';

/** @typedef {{id:number,seq:number|null,due:number,start:number|null,end:number|null,queue_wait_ms:number|null,outcome:string,skip_reason:string|null,skips:Object<string,string>,bytes_down:number,bytes_up:number}} StorageRound */
/** @typedef {{class:string,failed:number,of:number,status:string}} StorageNote */
/** @typedef {{campaign:any, rounds:StorageRound[], cells:Object<string,string>, notes:Object<string,Object<string,StorageNote>>, throughput:Object<string,{at:number[],down_bps:(number|null)[],up_bps:(number|null)[]}>}} StorageCampaignData */

/** Each cell letter: its class, glyph and words. */
export const CELL = /** @type {Object<string, {cls:string, glyph:string, word:string}>} */ ({
  o: { cls: 'ok', glyph: '', word: 'all worked' },
  s: { cls: 'slow', glyph: '!', word: 'slow: a transfer stalled' },
  f: { cls: 'failed', glyph: '✕', word: 'failed' },
  a: { cls: 'config', glyph: '⊘', word: 'refused: keys, clock or settings' },
  k: { cls: 'nokeys', glyph: '○', word: 'no keys: connection checks only' },
  b: { cls: 'skipped', glyph: '–', word: 'didn’t run' },
  g: { cls: 'gap', glyph: '', word: 'fyisp was not running' },
  c: { cls: 'cancelled', glyph: '×', word: 'cancelled' },
});

/** Why a round didn't run, in words. */
const SKIP = /** @type {Object<string,string>} */ ({
  busy: 'its last round was still waiting in the queue', stale: 'it waited too long in the queue', paused: 'the campaign was paused',
  no_keys: 'no keys', other_campaign: 'another campaign was running',
});

/** A step's name: Fresh 1, Reused 2, Download, Upload, Seed. @param {any} a */
export function stepName(a) {
  switch (a.phase) {
    case 'cold': return `Fresh ${a.n + 1}`;
    case 'warm': return `Reused ${a.n + 1}`;
    case 'down': return 'Download';
    case 'up': return a.op === 'delete' ? 'Delete upload' : 'Upload';
    case 'seed': return a.op === 'list' ? 'List' : 'Seed';
  }
  return a.phase;
}

/** "52.216.12.x" for "52.216.12.0/24". @param {string|null} p */
export function edge(p) { return p ? p.replace(/\.0\/24$/, '.x') : ''; }

export class StorageGrid {
  /** @param {{say:(t:string)=>void, name:(id:string)=>string}} host */
  constructor(host) {
    this.host = host;
    /** @type {StorageCampaignData|null} */
    this.d = null;
    this.asTable = false;
    /** The focused cell: [target row, round column]. */
    this.cur = [0, -1];
    /** The round open in the detail: [target, round id], or null. @type {[string, number]|null} */
    this.open = null;
    /** What the detail shows ("target:round id"). */
    this.shownDetail = '';
    this.detailSeq = 0;
    /** The grid was scrolled away from the newest round. */
    this.scrolledBack = false;
    this.wrap = h('div', { class: 'st-tl-wrap' });
    this.detail = h('section', { class: 'st-detail sp-body', 'aria-labelledby': 'st-detail-h', hidden: true });
    this.el = h('section', { class: 'st-grid sp-body', 'aria-labelledby': 'st-tl-h' });
  }

  /** @param {StorageCampaignData} d */
  set(d) {
    const same = this.d && this.d.campaign.id === d.campaign.id;
    const atEnd = !this.d || this.cur[1] >= this.d.rounds.length - 1;
    this.d = d;
    if (!same) { this.cur = [0, d.rounds.length - 1]; this.open = null; this.shownDetail = ''; this.detail.hidden = true; this.scrolledBack = false; }
    else if (atEnd) this.cur[1] = d.rounds.length - 1; // the cursor follows new rounds when it was on the newest
    this.cur[1] = Math.min(Math.max(this.cur[1], 0), d.rounds.length - 1);
    this.render();
  }

  /** The words for one cell. @param {string} tid @param {number} i */
  cellWords(tid, i) {
    const d = /** @type {StorageCampaignData} */ (this.d);
    const r = d.rounds[i];
    const ch = (d.cells[tid] || '')[i] || 'b';
    const st = CELL[ch] || CELL.b;
    const at = clock(r.start || r.due);
    const name = this.host.name(tid);
    if (ch === 'g') return `fyisp was not running ${clock(r.due)}–${clock(r.end || r.due)}`;
    let w = `${name}, round ${r.seq != null ? r.seq + 1 : ''} at ${at}: ${st.word}`;
    if (ch === 'b' && r.skip_reason) w = `${name}, round at ${at}: didn’t run: ${SKIP[r.skip_reason] || r.skip_reason}`;
    const n = d.notes[tid]?.[String(i)];
    if (n) w += `. ${n.failed} of ${n.of} attempts: ${classShort(n.class)}${n.status ? ` (${n.status})` : ''}`;
    return w + '.';
  }

  render() {
    const d = this.d;
    if (!d) { this.el.replaceChildren(); return; }
    const a = document.activeElement;
    const hadCell = !!a && this.wrap.contains(a);
    const hadToggle = !!a && a.classList.contains('st-astable');
    const scroll = this.wrap.scrollLeft;
    const targets = d.campaign.targets;
    const many = d.rounds.length > 24;
    const head = h('tr', {}, h('th', { scope: 'col', class: 'st-tl-name', text: 'Target' }),
      ...d.rounds.map((r, i) => {
        const t = r.outcome === 'gap' ? `${clock(r.due)}–${clock(r.end || r.due)}` : clock(r.start || r.due);
        const label = r.outcome === 'gap' ? `fyisp not running ${t}` : `Round ${r.seq != null ? r.seq + 1 : ''}, ${t}`;
        return h('th', { scope: 'col', 'aria-label': label, text: !many || i % 3 === 0 || i === d.rounds.length - 1 ? t : '' });
      }));
    const rows = targets.map((/** @type {any} */ t, ri) => h('tr', {},
      h('th', { scope: 'row', class: 'st-tl-name', text: t.name }),
      ...d.rounds.map((_, i) => {
        const ch = (d.cells[t.id] || '')[i] || 'b';
        const st = CELL[ch] || CELL.b;
        const focus = ri === this.cur[0] && i === this.cur[1];
        return h('td', { class: `st-c st-${st.cls}`, role: 'gridcell', tabindex: focus ? '0' : '-1', 'data-r': String(ri), 'data-i': String(i),
          'aria-label': this.cellWords(t.id, i), title: this.cellWords(t.id, i) },
        h('span', { 'aria-hidden': 'true', text: this.asTable ? ch.toUpperCase() : st.glyph }));
      })));
    const table = h('table', { class: 'st-tl' + (this.asTable ? ' st-tl-text' : ''), role: 'grid', 'aria-labelledby': 'st-tl-h', 'aria-describedby': 'st-tl-what',
      onkeydown: (/** @type {KeyboardEvent} */ e) => this.key(e), onclick: (/** @type {MouseEvent} */ e) => this.click(e) },
    h('thead', {}, head), h('tbody', {}, ...rows));
    this.wrap.replaceChildren(table);
    this.el.replaceChildren(...[
      h('div', { class: 'sp-sec-head' }, h('h3', { id: 'st-tl-h', text: 'Rounds' }),
        h('button', { type: 'button', class: 'btn sm st-astable', 'aria-pressed': String(this.asTable), text: 'Show as table', onclick: () => { this.asTable = !this.asTable; this.render(); } })),
      h('p', { class: 'muted small', id: 'st-tl-what', text: 'One column per round, one row per target. Each cell shows the worst thing in that round. Arrow keys move; Enter shows its attempts.' }),
      d.rounds.length ? null : h('p', { class: 'muted', text: 'No rounds yet.' }),
      many ? h('p', { class: 'muted small', text: 'Scroll sideways for earlier rounds.' }) : null,
      this.wrap,
      h('ul', { class: 'st-legend', 'aria-label': 'Legend' }, ...'ofsakbgc'.split('').map((ch) => h('li', {},
        // As a table, each word with the letter its cells show.
        h('span', { class: `st-c st-${CELL[ch].cls}`, 'aria-hidden': this.asTable ? null : 'true', text: this.asTable ? ch.toUpperCase() : CELL[ch].glyph }), ' ' + CELL[ch].word))),
    ].filter((n) => n != null));
    this.wrap.onscroll = () => { this.scrolledBack = this.wrap.scrollLeft + this.wrap.clientWidth < this.wrap.scrollWidth - 4; };
    // The newest round in view, unless the viewer scrolled back.
    requestAnimationFrame(() => { this.wrap.scrollLeft = this.scrolledBack ? scroll : this.wrap.scrollWidth; });
    if (hadCell) this.move(this.cur[0], this.cur[1]);
    if (hadToggle) /** @type {HTMLElement|null} */ (this.el.querySelector('.st-astable'))?.focus();
    if (this.open) this.loadDetail();
  }

  /** @param {KeyboardEvent} e */
  key(e) {
    const d = this.d;
    if (!d || !d.rounds.length) return;
    const nr = d.campaign.targets.length, ni = d.rounds.length;
    let [r, i] = this.cur;
    switch (e.key) {
      case 'ArrowLeft': i = Math.max(0, i - 1); break;
      case 'ArrowRight': i = Math.min(ni - 1, i + 1); break;
      case 'ArrowUp': r = Math.max(0, r - 1); break;
      case 'ArrowDown': r = Math.min(nr - 1, r + 1); break;
      case 'Home': i = e.ctrlKey ? 0 : 0; if (e.ctrlKey) r = 0; break;
      case 'End': i = ni - 1; if (e.ctrlKey) r = nr - 1; break;
      case 'Enter': case ' ': e.preventDefault(); this.show(r, i); return;
      default: return;
    }
    e.preventDefault();
    this.move(r, i);
  }

  /** @param {number} r @param {number} i */
  move(r, i) {
    this.cur = [r, i];
    for (const td of /** @type {NodeListOf<HTMLElement>} */ (this.wrap.querySelectorAll('td.st-c'))) {
      const on = td.dataset.r === String(r) && td.dataset.i === String(i);
      td.tabIndex = on ? 0 : -1;
      if (on) { td.focus(); td.scrollIntoView({ block: 'nearest', inline: 'nearest' }); }
    }
  }

  /** @param {MouseEvent} e */
  click(e) {
    const td = /** @type {HTMLElement|null} */ (/** @type {HTMLElement} */ (e.target).closest('td.st-c'));
    if (!td) return;
    const r = Number(td.dataset.r), i = Number(td.dataset.i);
    this.move(r, i);
    this.show(r, i);
  }

  /** Opens round i of target row r. @param {number} r @param {number} i */
  show(r, i) {
    const d = /** @type {StorageCampaignData} */ (this.d);
    this.open = [d.campaign.targets[r].id, d.rounds[i].id];
    this.loadDetail(true);
  }

  /** Opens the round next to the open one (dir -1 or 1). @param {number} dir */
  step(dir) {
    const d = this.d;
    if (!d || !this.open) return;
    const i = d.rounds.findIndex((x) => x.id === /** @type {[string, number]} */ (this.open)[1]) + dir;
    if (i < 0 || i >= d.rounds.length) return;
    this.open = [this.open[0], d.rounds[i].id];
    this.cur[1] = i;
    this.loadDetail(true);
  }

  async loadDetail(focus = false) {
    const d = this.d;
    if (!d || !this.open) return;
    const [tid, rid] = this.open;
    const i = d.rounds.findIndex((x) => x.id === rid);
    const round = d.rounds[i];
    if (!round) { this.open = null; this.detail.hidden = true; return; } // aged out
    if (!focus && this.shownDetail === `${tid}:${rid}`) return; // already shown: no fetch
    this.shownDetail = `${tid}:${rid}`;
    const seq = ++this.detailSeq;
    const name = this.host.name(tid);
    const title = `${name} · ${round.seq != null ? `round ${round.seq + 1}` : 'gap'} · ${clock(round.start || round.due)}`;
    const nav = h('div', { class: 'sp-acts' },
      h('button', { type: 'button', class: 'btn sm', text: 'Previous round', disabled: i === 0, onclick: () => this.step(-1) }),
      h('button', { type: 'button', class: 'btn sm', text: 'Next round', disabled: i >= d.rounds.length - 1, onclick: () => this.step(1) }),
      h('button', { type: 'button', class: 'btn sm ghost', text: 'Close', onclick: () => { this.open = null; this.shownDetail = ''; this.detail.hidden = true; this.move(this.cur[0], this.cur[1]); } }));
    const head = h('h3', { id: 'st-detail-h', tabindex: '-1', text: title });
    this.detail.hidden = false;
    /** @type {Node[]} */
    let body;
    if (round.outcome === 'gap') body = [h('p', { text: `fyisp was not running from ${clock(round.due)} to ${clock(round.end || round.due)}. Missed rounds are not made up.` })];
    else if (round.outcome === 'skipped') body = [h('p', { text: `This round didn’t run: ${SKIP[round.skip_reason || ''] || round.skip_reason}.` })];
    else {
      let rd;
      try { rd = await fetchJSON('api/storage?' + new URLSearchParams({ part: 'round', id: String(round.id) })); } catch { rd = null; }
      if (seq !== this.detailSeq) return;
      const as = rd?.attempts?.[tid] || [];
      const skip = round.skips?.[tid];
      body = [
        skip ? h('p', { class: 'muted', text: skip === 'no_keys' ? 'No keys this round: connection checks only.' : skip === 'round_cap' ? 'The round ran out of time before this target finished.' : 'The keys, the clock or the settings were refused: the target left the round.' }) : null,
        !rd ? h('p', { class: 'notice danger', text: 'Couldn’t load this round. Try again.' }) : !as.length ? h('p', { class: 'muted', text: 'No attempts for this target in this round.' }) :
          h('div', { class: 'st-scroll' }, h('table', { class: 'sp-st st-att' },
            h('caption', { class: 'sr-only', text: `Attempts of ${name} in this round` }),
            h('thead', {}, h('tr', {}, ...['Step', 'Result', 'Name lookup (ms)', 'Connect (ms)', 'Encryption (ms)', 'First byte (ms)', 'Total (ms)', 'Speed', 'Server', 'Request ID']
              .map((c) => h('th', { scope: 'col', text: c })))),
            h('tbody', {}, ...as.map((/** @type {any} */ a) => h('tr', { class: a.class === 'ok' ? null : 'bad' },
              h('th', { scope: 'row', text: stepName(a) }),
              h('td', { 'data-k': 'Result', text: a.class === 'ok' ? 'OK' : `✕ ${classShort(a.class)}${statusWords(a.http_status, a.s3_code)}` }),
              ...[a.dns_ms, a.connect_ms, a.tls_ms, a.ttfb_ms, a.total_ms].map((v) => h('td', { class: 'num', text: v == null ? '' : fmtMs(v) })),
              h('td', { class: 'num', text: a.goodput_bps != null ? fmtMbps(a.goodput_bps) : '' }),
              h('td', { text: edge(a.edge_prefix) }),
              h('td', { class: 'st-rid', text: a.request_id || '' })))))),
      ].filter((n) => n != null);
    }
    this.detail.replaceChildren(head, nav, ...body);
    if (focus) head.focus();
  }
}
