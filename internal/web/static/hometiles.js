// fyisp Home (v0.9.0): the 11 app tiles, their 5-minute bars, the key, the
// rules disclosure and the inline detail panel. The tiles are built once and
// patched in place on every poll, so focus and screen-reader position survive
// a refresh. All wording comes from /api/home (Go, golden tested), except
// bar times, the bar scale and the static rules text below.
// @ts-check

import { h, smooth, hpClock } from './core.js';
import { TileChart } from './homechart.js';

/** @typedef {{from:number,state:string,note:string}} HomeBar */
/** @typedef {{id:string,name:string,kind:string,state:string,word:string,since:number|null,note:string,aria:string,reason:string,enable_hint?:string,bars_summary:string,bars:HomeBar[]}} HomeTile */
/** @typedef {{label:string,value:string,limit?:string,bad?:boolean}} HomeMetric */
/** @typedef {{from:number,to:number,kind:string,text:string}} HomeBand */
/** @typedef {{at:number,id:string,state:string,word:string,reason:string,coverage?:string,legend?:string,metrics:HomeMetric[],from:number,step:number,ms:(number|null)[],usual_ms:number|null,bands:HomeBand[],text:string}} HomeTileDetail */
/**
 * What the tiles need from Home.
 * @typedef {{loadTile: (id: string) => Promise<HomeTileDetail>, stale: () => boolean, shown: () => boolean}} TilesHost
 */

const BAR_MS = 5 * 60e3;
// The open panel's chart refreshes this often.
export const TILE_MS = 60e3;

/** A status icon: colour plus a glyph, hidden from screen readers (the word says it). */
export function ico(/** @type {string} */ state, big = false) {
  const s = /^(good|slow|down|none|off|info)$/.test(state) ? state : 'none';
  return h('span', { class: `hp-ico ${s}${big ? ' big' : ''}`, 'aria-hidden': 'true' });
}

/** Writes text only when it changed (no needless mutations for screen readers). */
export function setText(/** @type {Element} */ el, /** @type {string} */ text) {
  if (el.textContent !== text) el.textContent = text;
}
/** Sets or removes an attribute, only when it changed. */
export function setAttr(/** @type {Element} */ el, /** @type {string} */ name, /** @type {string|null} */ v) {
  if (v == null) { if (el.hasAttribute(name)) el.removeAttribute(name); } else if (el.getAttribute(name) !== v) el.setAttribute(name, v);
}
function setClass(/** @type {Element} */ el, /** @type {string} */ c) { if (el.className !== c) el.className = c; }

/** "9:40–9:45 am", "11:55 am–12:00 pm". */
function clockRange(/** @type {number} */ a, /** @type {number} */ b) {
  const pa = hpClock(a), pb = hpClock(b);
  return pa.slice(-2) === pb.slice(-2) ? `${hpClock(a, false)}–${pb}` : `${pa}–${pb}`;
}
/** A bar's word when the server gave no note. */
function barWord(/** @type {string} */ state, /** @type {string} */ kind) {
  switch (state) {
    case 'good': return 'Good';
    case 'slow': return kind === 'call' ? 'Choppy' : 'Slow';
    case 'down': return 'Down';
  }
  return 'fyisp wasn’t running';
}
function barState(/** @type {string} */ s) { return /^(good|slow|down)$/.test(s) ? s : 'none'; }

/** Mixed text and bold runs for the static rules: strings are text, [s] is bold, [[s]] italic. */
function rich(/** @type {(string|string[]|string[][])[]} */ parts) {
  return parts.map((p) => (typeof p === 'string' ? p : Array.isArray(p[0]) ? h('i', { text: p[0][0] }) : h('b', { text: /** @type {string} */ (p[0]) })));
}

/** One tile: built once, patched by update(). */
class Tile {
  /** @param {string} id @param {(t: Tile) => void} onClick */
  constructor(id, onClick) {
    this.id = id;
    this.state = '';
    /** @type {HomeTile|null} */
    this.data = null;
    this.lastAt = 0; // the data's last check, for "Last known at …" while stale
    this.icoEl = ico('none');
    this.wordEl = h('span', { class: 'hp-word' });
    this.nameEl = h('span', { class: 'hp-name' });
    this.noteEl = h('span', { class: 'hp-since' });
    this.barsEl = h('span', { class: 'hp-bars', 'aria-hidden': 'true' });
    /** @type {HTMLElement[]} */
    this.bars = [];
    this.scaleFrom = h('span');
    this.scaleEl = h('span', { class: 'hp-scale', 'aria-hidden': 'true' }, this.scaleFrom, h('span', { text: 'now' }));
    this.descEl = h('span', { class: 'sr-only', id: `hp-d-${id}` });
    this.el = /** @type {HTMLButtonElement} */ (h('button', {
      type: 'button', class: 'hp-tile', 'data-id': id, 'aria-controls': 'hp-detail', 'aria-expanded': 'false', 'aria-describedby': `hp-d-${id}`,
      onclick: () => onClick(this),
    }, this.nameEl, h('span', { class: 'hp-st' }, this.icoEl, this.wordEl), this.noteEl, this.barsEl, this.scaleEl, this.descEl));
  }

  /** @param {HomeTile} t @param {boolean} stale */
  update(t, stale) {
    this.data = t;
    const off = t.state === 'off';
    const state = off ? 'off' : /^(good|slow|down)$/.test(t.state) ? t.state : 'none';
    setClass(this.el, `hp-tile is-${state}${!off && t.note && state === 'good' ? ' has-note' : ''}`);
    setAttr(this.el, 'aria-label', t.aria || `${t.name}: ${t.word}`);
    setClass(this.icoEl, `hp-ico ${state}`);
    setText(this.nameEl, t.name);
    setText(this.wordEl, t.word);
    setText(this.noteEl, off ? '' : t.note || '');
    const reason = stale && !off && this.lastAt ? `Last known at ${hpClock(this.lastAt)}: ${t.reason}` : t.reason;
    setText(this.descEl, [reason, off ? t.enable_hint : t.bars_summary].filter(Boolean).join(' '));
    // Bars: 24 built once; an off tile has a dashed baseline instead (only that area is rebuilt).
    if (off !== (this.state === 'off') || !this.state) {
      this.bars = off ? [] : Array.from({ length: 24 }, () => h('i'));
      this.barsEl.replaceChildren(...(off ? [h('i', { class: 'hp-base' })] : this.bars));
      this.barsEl.classList.toggle('off', off);
    }
    this.state = state;
    if (off) { setText(this.scaleFrom, ''); return; }
    const bars = t.bars || [];
    for (let i = 0; i < this.bars.length; i++) {
      const b = bars[i - (this.bars.length - bars.length)];
      const el = this.bars[i];
      if (!b) { setClass(el, 'b-none'); setAttr(el, 'title', null); continue; }
      const s = barState(b.state);
      setClass(el, `b-${s}`);
      const now = i === this.bars.length - 1 ? ' (now)' : '';
      setAttr(el, 'title', `${clockRange(b.from, b.from + BAR_MS)}${now}: ${b.note || barWord(s, t.kind)}`);
    }
    setText(this.scaleFrom, bars.length ? hpClock(bars[0].from, false) : '');
  }
}

/** The tile grid, the key, the rules and the detail panel. */
export class Tiles {
  /** @param {TilesHost} host */
  constructor(host) {
    this.host = host;
    /** @type {Map<string, Tile>} */
    this.tiles = new Map();
    /** @type {Tile[]} */
    this.order = [];
    /** @type {string|null} */
    this.openId = null;
    /** @type {HomeTileDetail|null} */
    this.detail = null;
    this.detailErr = false;
    this.unmonitored = false;
    this.tileAt = 0;
    this.seq = 0;
    this.busy = false;
    this.dirty = false; // resized or re-themed while hidden

    this.grid = h('div', { class: 'hp-tiles' });

    // The panel: one element, moved after the clicked tile's row.
    this.dIco = ico('none');
    this.dTitle = h('span');
    this.dHead = h('h3', { tabindex: '-1' }, this.dIco, this.dTitle);
    this.dClose = h('button', { type: 'button', class: 'btn hp-close', text: 'Close', onclick: () => this.close(true) });
    this.dReason = h('p', { class: 'hp-reason' });
    this.dMetrics = h('div', { class: 'hp-metrics' });
    this.dCoverage = h('p', { class: 'hp-coverage' });
    this.dEnable = h('p', { class: 'hp-enable' });
    this.dMsg = h('p', { class: 'hp-chartmsg' });
    this.chartBox = h('div', { class: 'hp-chart' });
    this.chartWrap = h('div', { class: 'hp-chartwrap', role: 'img', 'aria-describedby': 'hp-chart-desc' }, this.chartBox);
    this.chartDesc = h('p', { class: 'sr-only', id: 'hp-chart-desc' });
    this.legendWho = h('span');
    this.legendUsual = h('span', {}, h('span', { class: 'hp-sw usual' }), 'Usual');
    this.legend = h('div', { class: 'hp-legend', 'aria-hidden': 'true' },
      h('span', {}, h('span', { class: 'hp-sw' }), this.legendWho),
      this.legendUsual,
      h('span', {}, h('span', { class: 'hp-sw band' }), 'Problem'),
      h('span', {}, h('span', { class: 'hp-sw off' }), 'fyisp wasn’t running'));
    this.chartPart = h('div', { class: 'hp-chartpart' }, this.dMsg, this.chartWrap, this.chartDesc, this.legend);
    this.panel = h('div', { class: 'hp-detail', id: 'hp-detail', role: 'region' },
      h('div', { class: 'hp-dhead' }, this.dHead, this.dClose),
      this.dReason, this.dMetrics, this.dCoverage, this.dEnable, this.chartPart);
    this.chart = new TileChart(this.chartBox);

    const bar = (/** @type {string} */ s) => h('span', { class: 'hp-bars' }, h('i', { class: `b-${s}` }));
    this.el = h('section', { class: 'hp-sec', 'aria-labelledby': 'hp-h-tiles' },
      h('h2', { id: 'hp-h-tiles', text: 'The things you use' }),
      this.grid,
      h('div', { class: 'hp-key', 'aria-hidden': 'true' },
        h('span', { text: 'Each bar is 5 minutes, last 2 hours:' }),
        h('span', {}, bar('good'), 'Good'),
        h('span', {}, bar('slow'), 'Slow or Choppy for 2+ min'),
        h('span', {}, bar('down'), 'Down, or dropped for 30+ s'),
        h('span', {}, bar('none'), 'fyisp wasn’t running')),
      h('details', { class: 'hp-rules' },
        h('summary', { text: 'How fyisp decides Good, Slow, Choppy and Down' }),
        h('ul', {}, ...[
          [['Calls'], ' (Zoom, Teams, Meet, Slack) are ', ['Choppy'], ' when delay is over 150 ms, jitter over 30 ms, or 2% or more of packets are lost — the points where people start to notice on a call. ', ['Down'], ' means over 400 ms, 10% loss, or not connecting.'],
          [['Everything else'], ' is ', ['Slow'], ' when it takes more than twice its usual time ', [['and']], ' at least 30 ms longer. ', ['Down'], ' means it isn’t answering.'],
          [['YouTube and Netflix'], ' buffer ahead, so slowness alone never marks them Down — only failing to load does. If they buffer, test your speed.'],
          [['Drops of 30 seconds or more always show'], ', even if everything recovers: the bar turns red and the tile notes the drop for 30 minutes. A drop only counts when most of an app’s checks fail together, so one flaky server can’t set it off.'],
          ['For slowdowns, the word on a tile changes once 3 of the last 5 minutes are bad, and goes back to Good after 5 good minutes in a row.'],
          ['“Usual” is learned from your own connection over the last day, leaving out bad minutes.'],
          [['Likely cause'], ' comes from comparing your router, your internet provider’s first stop, and all the apps at the same moment. It’s a strong hint, not proof.'],
        ].map((p) => h('li', {}, ...rich(p))))));

    document.addEventListener('keydown', (e) => {
      if (e.key !== 'Escape' || !this.openId || !this.host.shown()) return;
      // The admin-token dialog (or any other) handles its own Escape.
      if (document.querySelector('dialog[open]') || (e.target instanceof Element && e.target.closest('dialog'))) return;
      // Only from the panel or its tile: Escape elsewhere on the page isn't about the panel.
      const a = document.activeElement;
      if (!a || !(this.panel.contains(a) || a === this.tiles.get(this.openId)?.el)) return;
      this.close(true);
    });
    // 4 columns ↔ 2 columns, and the caret under the tile on any resize.
    let raf = 0;
    window.addEventListener('resize', () => {
      cancelAnimationFrame(raf);
      raf = requestAnimationFrame(() => {
        if (!this.openId) return;
        // Home hidden (Details on screen): nothing to measure; refit() catches up when it's back.
        if (!this.host.shown()) { this.dirty = true; return; }
        this.place();
        this.chart.resize();
      });
    });
  }

  /** Patches the tiles from part=now. @param {HomeTile[]} list @param {number} lastAt the data's last check (ms), for stale reasons */
  update(list, lastAt) {
    const stale = this.host.stale();
    this.grid.classList.toggle('is-stale', stale);
    const ids = list.map((t) => t.id).join(',');
    if (ids !== this.order.map((t) => t.id).join(',')) {
      // The set is fixed (11 tiles); this only runs on the first poll.
      this.order = list.map((t) => this.tiles.get(t.id) || new Tile(t.id, (x) => this.toggle(x)));
      this.tiles = new Map(this.order.map((t) => [t.id, t]));
      if (this.openId && !this.tiles.has(this.openId)) this.close(false);
      this.grid.replaceChildren(...this.order.map((t) => t.el));
      if (this.openId) this.place();
    }
    for (const t of list) {
      const tile = /** @type {Tile} */ (this.tiles.get(t.id));
      // The open tile changed state: a 404 from before may not hold any more.
      if (t.id === this.openId && tile.data && tile.data.state !== t.state) this.unmonitored = false;
      tile.lastAt = lastAt;
      tile.update(t, stale);
    }
    if (this.openId) this.renderPanel();
  }

  /** Every second while Home is on screen: the open panel's chart every 60 s. */
  tick(/** @type {number} */ now) {
    const t = this.openId && this.tiles.get(this.openId);
    if (!t || t.state === 'off' || this.unmonitored || this.busy) return;
    if (now - this.tileAt >= TILE_MS) this.loadDetail();
  }

  /** @param {Tile} t */
  toggle(t) {
    if (this.openId === t.id) { this.close(false); return; }
    this.openId = t.id;
    this.detail = null;
    this.detailErr = false;
    this.unmonitored = false;
    this.tileAt = 0;
    this.syncExpanded();
    this.chart.clear();
    this.renderPanel();
    this.place();
    this.dHead.focus({ preventScroll: true });
    this.panel.scrollIntoView({ block: 'nearest', behavior: smooth() });
    if (t.state !== 'off') this.loadDetail();
  }

  /** Closes the panel; focus goes back to its tile when asked (Close, Escape). */
  close(/** @type {boolean} */ refocus) {
    const id = this.openId;
    if (!id) return;
    this.openId = null;
    this.seq++;
    this.busy = false;
    this.syncExpanded();
    this.chart.clear();
    this.panel.remove();
    if (refocus) this.tiles.get(id)?.el.focus();
  }

  syncExpanded() {
    for (const t of this.order) setAttr(t.el, 'aria-expanded', String(t.id === this.openId));
  }

  /** Moves the panel after the last tile of the open tile's row, with the caret under the tile. */
  place() {
    const t = this.openId && this.tiles.get(this.openId);
    if (!t) return;
    const idx = this.order.indexOf(t);
    const cols = Math.max(1, getComputedStyle(this.grid).gridTemplateColumns.split(' ').filter(Boolean).length);
    const last = this.order[Math.min(this.order.length - 1, Math.floor(idx / cols) * cols + cols - 1)];
    if (last.el.nextElementSibling !== this.panel) last.el.after(this.panel);
    const g = this.grid.getBoundingClientRect(), r = t.el.getBoundingClientRect();
    this.panel.style.setProperty('--caret', `${Math.round(r.left - g.left + r.width / 2)}px`);
  }

  async loadDetail() {
    const id = /** @type {string} */ (this.openId);
    const seq = ++this.seq;
    this.busy = true;
    this.tileAt = Date.now();
    try {
      const d = await this.host.loadTile(id);
      if (seq !== this.seq) return;
      this.detail = d;
      this.detailErr = false;
    } catch (e) {
      if (seq !== this.seq) return;
      // 404: the tile isn't monitored after all; the panel shows it as off.
      if (/** @type {any} */ (e).status === 404) this.unmonitored = true;
      else this.detailErr = true;
    } finally {
      if (seq === this.seq) this.busy = false;
    }
    if (this.openId === id) this.renderPanel();
  }

  renderPanel() {
    const t = this.openId ? this.tiles.get(this.openId) : null;
    if (!t || !t.data) return;
    const td = t.data;
    const d = this.detail && this.detail.id === td.id ? this.detail : null;
    const off = t.state === 'off' || this.unmonitored;
    setAttr(this.panel, 'aria-label', `${td.name} details`);
    setClass(this.dIco, `hp-ico ${t.state}`);
    setText(this.dTitle, `${td.name} — last 24 hours`);
    setText(this.dReason, (d && !off ? d.reason : td.reason) || '');
    // Metrics: label, value, (limit); a bad one is marked in words too.
    const ms = d && !off ? d.metrics || [] : [];
    const sig = JSON.stringify(ms);
    if (this.dMetrics.dataset.sig !== sig) {
      this.dMetrics.dataset.sig = sig;
      this.dMetrics.replaceChildren(...ms.map((m) => h('span', { class: m.bad ? 'bad' : null },
        m.value ? `${m.label} ` : m.label, m.value ? h('b', { text: m.value }) : '', m.limit ? ` (${m.limit})` : '', m.bad ? h('span', { class: 'sr-only', text: ', over the limit' }) : null)));
    }
    this.dMetrics.hidden = !ms.length;
    setText(this.dCoverage, (d && !off && d.coverage) || '');
    this.dCoverage.hidden = !this.dCoverage.textContent;
    setText(this.dEnable, off ? td.enable_hint || '' : '');
    this.dEnable.hidden = !off || !td.enable_hint;
    this.chartPart.hidden = off;
    if (off) return;
    setText(this.dMsg, d ? '' : this.detailErr ? 'Couldn’t load the last 24 hours. fyisp will try again in a minute.' : 'Loading the last 24 hours…');
    this.dMsg.hidden = !!d;
    this.chartWrap.hidden = !d;
    this.legend.hidden = !d;
    setAttr(this.chartWrap, 'aria-label', `${td.name}, how quickly it answered over the last 24 hours`);
    // The server names the line when it isn't the app's own (the general call path).
    setText(this.legendWho, (d && d.legend) || `How quickly ${td.name} answers (lower is better)`);
    if (!d) return;
    setText(this.chartDesc, d.text || '');
    this.legendUsual.hidden = d.usual_ms == null;
    if (this.chart.d !== d) this.chart.set(d);
  }

  themeChanged() {
    if (!this.host.shown()) { this.dirty = true; return; }
    this.chart.themeChanged();
  }

  /** Home is back on screen: re-place the panel and redraw its chart if the window or theme changed meanwhile. */
  refit() {
    if (!this.dirty) return;
    this.dirty = false;
    if (!this.openId) return;
    this.place();
    this.chart.themeChanged();
  }
}
