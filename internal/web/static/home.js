// fyisp Home (v0.9.0): the default view. One answer ("is my internet OK,
// and which apps work"), the connection strip, 11 app tiles with their last
// 2 hours, the speed, and the problems of the last 7 days. Every sentence
// comes from /api/home2 (Go, golden tested); this file lays it out, patches
// it in place on each poll, and keeps it accessible: one live region that
// speaks only when the answer changes, and the tab title. Plain ES module,
// no build step.
// @ts-check

import { h, fetchJSON, tzParam, smooth, hpClock } from './core.js';
import { startSingleTest, queueError } from './speedqueue.js';
import { Tiles, ico, setText } from './hometiles.js';

/** @typedef {import('./hometiles.js').HomeTile} HomeTile */
/** @typedef {{key:string,tone:string,icon:string,tab:string,headline:string,sub:string,day:{text:string,link:string}|null}} HomeAnswer */
/** @typedef {{id:string,label:string,state:string,sub:string}} HomeHop */
/** @typedef {{at:number,down_mbps:number,up_mbps:number,text:string,note:string}} HomeSpeedResult */
/** @typedef {{available:boolean,latest:HomeSpeedResult|null,empty_text:string,usual:{down_mbps:number,up_mbps:number}|null,running:{entry:number,expected_end:number}|null,can_control:boolean}} HomeSpeed */
/** @typedef {{id:string,from:number,to:number,ongoing:boolean,level:string,when:string,what:string,cause:string,cause_kind:string,evidence:string}} HomeProblem */
/**
 * part=now (docs/HOME-API.md, v2).
 * @typedef {{v:number,at:number,last_check:number,stale:boolean,watching_since:number,footer:string,learning:{until:number,text:string}|null,monitored:number,
 *   answer:HomeAnswer,path:HomeHop[],path_hint:string,tiles:HomeTile[],speed:HomeSpeed|null,problems:{summary:string,items:HomeProblem[]}|null}} HomeNow
 */
/**
 * What Home needs from the page (app.js owns the hash and the status).
 * @typedef {{openDetails: (hash: string) => void, detailsHash: () => string, status: () => any, speedOn: () => boolean}} HomeHost
 */

// The v2 API; it becomes api/home when the old Home is removed.
export const HOME_API = 'api/home2';
const NOW_MS = 15000;
// In a background tab Home keeps polling, slower, so its title stays true.
const HIDDEN_MS = 60000;
// The startup probe gives up after this long (the old Home then shows).
const PROBE_MS = 4000;
// A poll that hasn't answered by then counts as failed, so the next one can start.
const POLL_TIMEOUT_MS = 10000;
// Home calls itself stale after this long without a fresh check, or this many failed polls in a row.
const STALE_MS = 120e3;
const FAILS = 2;
// A speed test this page started: the queue is polled this often.
const QUEUE_MS = 3000;
// …and given up after this many failed polls in a row.
const QUEUE_FAILS = 5;
const SPEED_NOTE = 'This usually takes under a minute.';
const SPEED_LOST = 'The speed test didn’t finish. Try again in a few minutes.';

/** The v2 answer, or null when this server only has the old Home (before v0.9.0). */
export async function probeHome() {
  try {
    const d = await fetchJSON(HOME_API + '?' + new URLSearchParams({ part: 'now', tz: tzParam() }), { signal: AbortSignal.timeout(PROBE_MS) }, 1);
    return d && d.v === 2 ? /** @type {HomeNow} */ (d) : null;
  } catch {
    return null;
  }
}

/** "a few seconds ago", "4 minutes ago", "2 hours ago". */
function ago(/** @type {number} */ ms) {
  if (ms < 60e3) return 'a few seconds ago';
  const m = Math.round(ms / 60e3);
  if (m < 60) return `${m} minute${m === 1 ? '' : 's'} ago`;
  const hh = Math.round(m / 60);
  return `${hh} hour${hh === 1 ? '' : 's'} ago`;
}
/** "in about 3 minutes", "in under a minute". */
function inAbout(/** @type {number} */ ms) {
  const m = Math.round(ms / 60e3);
  return m < 1 ? 'in under a minute' : `in about ${m} minute${m === 1 ? '' : 's'}`;
}
function tone(/** @type {string} */ t) { return /^(ok|warn|bad|stale|checking|info)$/.test(t) ? t : 'checking'; }
function hopState(/** @type {string} */ s) { return /^(good|slow|down)$/.test(s) ? s : 'none'; }

export class Home {
  /** @param {HomeHost} host @param {HomeNow|null} [first] the answer probeHome() already fetched */
  constructor(host, first = null) {
    this.host = host;
    /** @type {HomeNow|null} */
    this.data = null;
    this.recvAt = 0; // when the last answer arrived (client clock)
    this.nowAt = 0; // when the last poll started
    this.fails = 0;
    /** The part=now request in flight. @type {Promise<void>|null} */
    this.nowP = null;
    this.lastKey = '';
    this.rendered = false;
    this.wasStale = false;
    /** A speed test this page started: its queue entry, its state and what to say. @type {{id:number,state:string,text:string,fails:number}|null} */
    this.mine = null;
    /** @type {number|undefined} */
    this.qTimer = undefined;
    this.spErr = '';

    // Answer card: the headline and sub line are the only live region.
    this.ansIco = ico('none', true);
    this.headEl = h('h2', { class: 'hp-headline' });
    this.subEl = h('p', { class: 'hp-sub' });
    this.live = h('div', { role: 'status', 'aria-live': 'off', 'aria-atomic': 'true' }, this.headEl, this.subEl);
    this.dayEl = h('p', { class: 'hp-day' });
    this.checkedEl = h('p', { class: 'hp-checked' });
    this.card = h('section', { class: 'hp-answer checking', 'aria-label': 'Your connection now' },
      this.ansIco, h('div', { class: 'hp-answer-body' }, this.live, this.dayEl, this.checkedEl));
    this.learnEl = h('div', { class: 'hp-learning', hidden: true });

    // Connection strip: router → provider → the internet.
    this.hops = [0, 1, 2].map((i) => {
      const o = { ico: ico('none'), label: h('span'), sub: h('div', { class: 'hp-hop-sub' }), li: h('li', { class: 'hp-hop' }) };
      o.li.append(
        i ? h('span', { class: 'hp-arrow', 'aria-hidden': 'true', text: '→' }) : '',
        h('div', { class: 'hp-hopbox' }, h('div', { class: 'hp-hop-lbl' }, o.ico, o.label), o.sub));
      return o;
    });
    this.hintEl = h('p', { class: 'hp-hint' });
    this.pathSec = h('section', { class: 'hp-sec', 'aria-labelledby': 'hp-h-conn' },
      h('h2', { id: 'hp-h-conn', text: 'Your connection' }),
      h('ol', { class: 'hp-path' }, ...this.hops.map((o) => o.li)), this.hintEl);

    this.tiles = new Tiles({
      loadTile: (id) => fetchJSON(HOME_API + '?' + new URLSearchParams({ part: 'tile', id, tz: tzParam() })),
      stale: () => this.isStale(),
      shown: () => this.shown(),
    });

    // Speed.
    this.spLine = h('p', { class: 'hp-speed-line' });
    this.spNote = h('p', { class: 'hp-speed-note' });
    this.spBtn = /** @type {HTMLButtonElement} */ (h('button', { type: 'button', class: 'btn hp-speed-btn', text: 'Test speed now', onclick: () => this.testSpeed() }));
    // What this page's own test is doing, said once per step (the line above isn't live).
    this.spSay = h('p', { class: 'sr-only', role: 'status' });
    this.speedSec = h('section', { class: 'hp-sec hp-speed', 'aria-labelledby': 'hp-h-speed', hidden: true },
      h('h2', { id: 'hp-h-speed', text: 'Speed' }), this.spLine, this.spNote, this.spBtn, this.spSay);

    // Problems in the last 7 days, keyed by id.
    this.probHead = h('h2', { id: 'hp-problems', tabindex: '-1', text: 'Problems in the last 7 days' });
    this.probSum = h('p', { class: 'hp-summary' });
    this.probList = h('ul', { class: 'hp-problems' });
    /** @type {Map<string, {li:HTMLElement, ico:HTMLElement, when:HTMLElement, what:HTMLElement, cause:HTMLElement, causeB:HTMLElement, ev:HTMLElement}>} */
    this.probs = new Map();
    this.probSec = h('section', { class: 'hp-sec', 'aria-labelledby': 'hp-problems' }, this.probHead, this.probSum, this.probList);

    this.footText = h('span', { class: 'hp-foot-text' });
    this.detailsLink = h('a', {
      href: '#', text: 'Technical details →',
      onclick: (/** @type {MouseEvent} */ e) => { if (e.ctrlKey || e.metaKey || e.shiftKey || e.button) return; e.preventDefault(); this.host.openDetails(''); },
    });
    this.foot = h('footer', { class: 'hp-foot' }, this.footText, this.detailsLink);

    this.el = h('main', { id: 'home', class: 'hp', hidden: true },
      this.card, this.learnEl, this.pathSec, this.tiles.el, this.speedSec, this.probSec, this.foot);

    // Coming back to the tab fetches at once (app.js's handler skips Home); in the background polls are every 60 s.
    document.addEventListener('visibilitychange', () => { if (!document.hidden && this.shown()) this.loadNow(); });

    if (first) { this.data = first; this.recvAt = Date.now(); this.nowAt = this.recvAt; }
  }

  shown() { return !this.el.hidden; }

  /** Home opened (or the page's refresh): render what we have, then poll. */
  refresh() {
    this.render();
    // The panel's chart may have been resized or re-themed while Details was on screen.
    this.tiles.refit();
    return this.loadNow();
  }

  /** Every second: poll when due (every 60 s in a background tab), keep "Checked …" and the stale state current. */
  tick() {
    if (!this.shown()) return;
    const now = Date.now();
    if (!this.nowP && now - this.nowAt >= (document.hidden ? HIDDEN_MS : NOW_MS)) this.loadNow();
    // Going stale between polls (an asleep host can't answer) redraws everything; else just "Checked …".
    else if (this.isStale() !== this.wasStale) this.render();
    else this.renderAnswer();
    if (!document.hidden && !this.isStale()) this.tiles.tick(now);
  }

  /**
   * Polls part=now. A call while one is in flight shares it, or with
   * fresh waits for it and then asks again (an answer started earlier may
   * predate what the caller is waiting for).
   */
  async loadNow(fresh = false) {
    if (this.nowP) {
      if (!fresh) return this.nowP;
      await this.nowP.catch(() => {});
      if (this.nowP) return this.loadNow(true);
    }
    this.nowP = this.fetchNow().finally(() => { this.nowP = null; });
    return this.nowP;
  }

  async fetchNow() {
    this.nowAt = Date.now();
    try {
      const d = await fetchJSON(HOME_API + '?' + new URLSearchParams({ part: 'now', tz: tzParam() }), { signal: AbortSignal.timeout(POLL_TIMEOUT_MS) });
      if (!d || d.v !== 2) throw new Error('not a v2 answer');
      this.data = /** @type {HomeNow} */ (d);
      this.recvAt = Date.now();
      this.fails = 0;
    } catch {
      this.fails++;
    }
    this.render();
  }

  /** How old the newest check is now (ms), or NaN before the first one. */
  age() {
    const d = this.data;
    if (!d || !d.last_check) return NaN;
    return Math.max(0, d.at - d.last_check) + (Date.now() - this.recvAt);
  }

  /**
   * Stale: the server says so, it can't be reached, or its last check is
   * too old by now. Time since the last answer only counts while a poll
   * after it is pending or failed: a throttled background tab that simply
   * hasn't asked is not evidence that fyisp stopped.
   */
  isStale() {
    const d = this.data;
    if (!d) return this.fails >= FAILS;
    if (d.stale || this.fails >= FAILS) return true;
    const asked = this.nowAt > this.recvAt;
    return Math.max(0, d.at - d.last_check) + (asked ? Date.now() - this.recvAt : 0) >= STALE_MS && !!d.last_check;
  }

  /** The answer to show: the server's, or the client's own when fyisp has gone quiet. @returns {HomeAnswer} */
  answer() {
    const d = this.data;
    if (this.isStale()) {
      const t = d && d.last_check ? hpClock(d.last_check) : '';
      return {
        key: 'stale', tone: 'stale', icon: 'none', tab: '? Not checking', day: d ? d.answer.day : null,
        headline: t ? `fyisp hasn’t checked since ${t}.` : 'fyisp can’t be reached right now.',
        sub: t ? `Is the computer it runs on asleep or turned off? Everything below is from ${t} and may be out of date.`
          : 'Is the computer it runs on asleep or turned off?',
      };
    }
    if (!d) return { key: 'checking', tone: 'checking', icon: 'none', tab: 'Checking', headline: 'Checking your connection…', sub: '', day: null };
    return d.answer;
  }

  render() {
    const d = this.data;
    const stale = this.isStale();
    this.wasStale = stale;
    this.el.classList.toggle('is-stale', stale);
    this.renderAnswer();
    if (d) {
      this.renderLearning(d);
      this.renderPath(d);
      this.tiles.update(d.tiles || [], d.last_check);
      this.renderSpeed(d);
      this.renderProblems(d);
      setText(this.footText, d.footer || '');
    }
    const href = this.host.detailsHash() || '#';
    if (this.detailsLink.getAttribute('href') !== href) this.detailsLink.setAttribute('href', href);
    if (!this.rendered) {
      this.rendered = true;
      // The first answer is on screen before the region goes live, so loading the page announces nothing.
      requestAnimationFrame(() => requestAnimationFrame(() => this.live.setAttribute('aria-live', 'polite')));
    }
  }

  renderAnswer() {
    const a = this.answer();
    const cc = `hp-answer ${tone(a.tone)}`, ic = `hp-ico big ${/^(good|slow|down|info)$/.test(a.icon) ? a.icon : 'none'}`;
    if (this.card.className !== cc) this.card.className = cc;
    if (this.ansIco.className !== ic) this.ansIco.className = ic;
    // The live region changes only with the answer's key; same-key text changes are rare and logged.
    if (a.key !== this.lastKey || this.headEl.textContent !== a.headline || this.subEl.textContent !== a.sub) {
      if (a.key === this.lastKey) console.debug('fyisp home: answer text changed under key', a.key);
      this.lastKey = a.key;
      setText(this.headEl, a.headline);
      setText(this.subEl, a.sub);
      this.subEl.hidden = !a.sub;
    }
    this.renderDay(a.day);
    const age = this.age();
    setText(this.checkedEl, !isFinite(age) ? '' : this.isStale()
      ? `Last checked ${hpClock(/** @type {HomeNow} */ (this.data).last_check)}, ${ago(age)}`
      : `Checked ${ago(age)} · updates on its own`);
    if (this.shown()) {
      const title = `${a.tab} – fyisp`;
      if (document.title !== title) document.title = title;
    }
  }

  /** "Last 24 hours: 4 short problems, …" with the server's link words pointing at the problems list. @param {{text:string,link:string}|null} day */
  renderDay(day) {
    const text = day ? day.text : '';
    const sig = text + '\u0000' + (day ? day.link : '');
    if (this.dayEl.dataset.sig === sig) return;
    this.dayEl.dataset.sig = sig;
    this.dayEl.hidden = !text;
    const i = day && day.link ? text.indexOf(day.link) : -1;
    if (!day || i < 0) { this.dayEl.textContent = text; return; }
    this.dayEl.replaceChildren(text.slice(0, i), h('a', {
      href: '#hp-problems', text: day.link,
      onclick: (/** @type {MouseEvent} */ e) => {
        // A real hash change would be routed as a view change: scroll and focus instead.
        e.preventDefault();
        this.probHead.focus({ preventScroll: true });
        this.probHead.scrollIntoView({ block: 'start', behavior: smooth() });
      },
    }), text.slice(i + day.link.length));
  }

  /** @param {HomeNow} d */
  renderLearning(d) {
    this.learnEl.hidden = !d.learning;
    setText(this.learnEl, d.learning ? d.learning.text : '');
  }

  /** @param {HomeNow} d */
  renderPath(d) {
    const path = d.path || [];
    this.hops.forEach((o, i) => {
      const p = path[i] || { id: '', label: '', state: 'unknown', sub: '' };
      const s = hopState(p.state);
      if (o.li.className !== `hp-hop is-${s}`) o.li.className = `hp-hop is-${s}`;
      if (o.ico.className !== `hp-ico ${s}`) o.ico.className = `hp-ico ${s}`;
      setText(o.label, p.label);
      setText(o.sub, p.sub);
    });
    setText(this.hintEl, d.path_hint || '');
    this.hintEl.hidden = !d.path_hint;
  }

  // ---------- speed ----------

  /** @param {HomeNow} d */
  renderSpeed(d) {
    const sp = d.speed;
    this.speedSec.hidden = !sp || !sp.available || !this.host.speedOn();
    if (!sp || this.speedSec.hidden) return;
    const st = this.host.status();
    let line, note;
    if (this.mine) { line = this.mine.text; note = this.spErr || (this.mine.state === 'running' ? SPEED_NOTE : ''); }
    else if (sp.running) { line = 'Testing your speed…'; note = SPEED_NOTE; }
    else if (sp.latest) { line = sp.latest.text; note = this.spErr || sp.latest.note; }
    else { line = sp.empty_text; note = this.spErr; }
    setText(this.spLine, line);
    setText(this.spNote, note);
    this.spNote.hidden = !note;
    // Admin-token dashboards ask for the token on click (apiWrite); disabled controls hide the button.
    this.spBtn.hidden = !(sp.can_control || (st && st.controls === 'admin'));
    this.spBtn.disabled = !!this.mine || !!sp.running || this.isStale();
  }

  /** Test speed now: queue one test of the nearest servers, then follow it until it's done. */
  async testSpeed() {
    if (this.mine) return;
    this.spErr = '';
    // Cleared first, so the same message twice in a row is still announced.
    setText(this.spSay, '');
    this.mine = { id: 0, state: 'starting', text: 'Testing your speed…', fails: 0 };
    this.render();
    try {
      const r = await startSingleTest();
      this.mine.id = r.entry.id;
      this.follow(r.entry);
      setText(this.spSay, this.mine.text);
      this.render();
      this.qTimer = window.setTimeout(() => this.pollQueue(), QUEUE_MS);
    } catch (e) {
      this.mine = null;
      this.spErr = queueError(e);
      setText(this.spSay, this.spErr);
      this.render();
    }
  }

  /** The line for this page's entry: running, or waiting its turn. @param {any} e */
  follow(e) {
    if (!this.mine) return;
    this.mine.state = e.state;
    this.mine.text = e.state === 'running' ? 'Testing your speed…' : `Waiting for another test, starts ${inAbout((e.est_start || Date.now()) - Date.now())}.`;
  }

  async pollQueue() {
    const mine = this.mine;
    if (!mine) return;
    // Left Home: stop following; the next part=now shows a test still running.
    if (!this.shown()) { this.mine = null; return; }
    try {
      const q = await fetchJSON('api/speed?part=queue');
      mine.fails = 0;
      const e = q.running && q.running.id === mine.id ? q.running : (q.waiting || []).find((/** @type {any} */ x) => x.id === mine.id);
      if (e) {
        const before = mine.text;
        this.follow(e);
        if (mine.text !== before) setText(this.spSay, mine.text);
        this.render();
        this.qTimer = window.setTimeout(() => this.pollQueue(), QUEUE_MS);
        return;
      }
      // It left the queue: how did it end? Then the fresh result.
      const done = await fetchJSON('api/speed?part=entry&id=' + mine.id).catch(() => null);
      this.mine = null;
      await this.loadNow(true);
      const latest = this.data && this.data.speed && this.data.speed.latest;
      this.spErr = done && done.state === 'done' ? '' : SPEED_LOST;
      setText(this.spSay, this.spErr || (latest ? latest.text : ''));
      this.render();
      if (document.activeElement === document.body) this.spBtn.focus();
    } catch {
      if (++mine.fails < QUEUE_FAILS) { this.qTimer = window.setTimeout(() => this.pollQueue(), QUEUE_MS); return; }
      this.mine = null;
      this.spErr = SPEED_LOST;
      setText(this.spSay, SPEED_LOST);
      this.render();
    }
  }

  // ---------- problems ----------

  /** @param {HomeNow} d */
  renderProblems(d) {
    const p = d.problems || { summary: '', items: [] };
    setText(this.probSum, p.summary || '');
    const items = p.items || [];
    const seen = new Set();
    /** @type {HTMLElement[]} */
    const order = [];
    for (const it of items) {
      seen.add(it.id);
      let r = this.probs.get(it.id);
      if (!r) {
        r = { li: h('li'), ico: ico('none'), when: h('div', { class: 'hp-when' }), what: h('div', { class: 'hp-what' }), cause: h('div', { class: 'hp-cause' }), causeB: h('b'), ev: h('span', { class: 'hp-ev' }) };
        r.cause.append('Likely cause: ', r.causeB, '. ', r.ev);
        r.li.append(r.ico, h('div', { class: 'hp-pbody' }, r.when, r.what, r.cause));
        this.probs.set(it.id, r);
      }
      const c = `hp-problem${it.ongoing ? ' live' : ''}`;
      if (r.li.className !== c) r.li.className = c;
      const lv = it.level === 'down' ? 'down' : 'slow';
      if (r.ico.className !== `hp-ico ${lv}`) r.ico.className = `hp-ico ${lv}`;
      setText(r.when, it.when);
      setText(r.what, it.what);
      setText(r.causeB, it.cause || '');
      setText(r.ev, it.evidence || '');
      r.cause.hidden = !it.cause;
      order.push(r.li);
    }
    for (const [id, r] of this.probs) if (!seen.has(id)) { r.li.remove(); this.probs.delete(id); }
    // Re-append only when the order changed, so unchanged items keep their nodes and place.
    if ([...this.probList.children].some((li, i) => li !== order[i]) || this.probList.children.length !== order.length) this.probList.append(...order);
    this.probList.hidden = !order.length;
  }

  /** The theme changed: only the open panel's canvas chart needs redrawing. */
  themeChanged() { this.tiles.themeChanged(); }
}
