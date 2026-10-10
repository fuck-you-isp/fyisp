// fyisp Home: the default view. One answer ("is my internet OK for calls
// and work right now, and if not, whose fault is it"), a tile per kind of
// thing the user relies on, a Today/Week timeline, "Mark a bad call",
// "What happened at…?" and "Make a report". Every sentence of the answer,
// the tiles and the timeline summary comes from /api/home (Go, golden
// tested); this file lays it out, formats times in the viewer's locale and
// keeps it accessible. Plain ES module, no build step.
// @ts-check

import { h, MODE, fetchJSON, apiWrite, canWrite, toLocalInput, fromLocalInput, copyText } from './core.js';

/** @typedef {{level:string,who:string,confidence?:string,kind:string,calls?:string,app?:string,since?:number,headline:string,cause?:string,hint?:string,try:string[],fact?:string,fact_at?:number,announce:string,recovered?:number}} HomeAnswer */
/** @typedef {{id:string,label:string,state:string,words:string,blamed?:boolean,culprit?:string,tooltip?:string}} HomeNode */
/** @typedef {{id:string,title:string,level:string,state:string,label:string,note?:string,names:string[],more?:number,affected?:string[],ok:number,total:number,details:string}} HomeTile */
/** @typedef {{state:string,delay?:string,dropouts?:string,smoothness?:string,words?:string,worst_at?:number}} HomeCalls */
/** @typedef {{app:string,title:string,level:string,state:string,label:string,note?:string}} HomeApp */
/** @typedef {{id:string,label:string,done:boolean}} HomeStep */
/** @typedef {{part:string,voice:string,at:number,answer:HomeAnswer,path:HomeNode[],tiles:HomeTile[],other?:{affected:number,total:number,line:string}|null,calls?:HomeCalls,apps:HomeApp[],apps_note?:string,steps?:HomeStep[],speed?:{text:string,level:string}}} HomeLive */
/** @typedef {{from:number,to:number,level:string,who:string,kind?:string,label:string,text?:string,source:string,incident?:number,ongoing?:boolean}} HomeRun */
/** @typedef {{from:number,to:number}} HomeSpan */
/** @typedef {{date:string,from:number,to:number,work_from?:number,work_to?:number,runs:HomeRun[],off:HomeSpan[],no_record?:boolean}} HomeDay */
/** @typedef {{id:number,from:number,to?:number,ongoing?:boolean,kind:string,who:string,where:string,side?:string,level?:string,confidence?:string,text?:string,peak_loss:number,details:string}} HomeProblem */
/** @typedef {{part:string,voice:string,at:number,tz:number,span:string,from:number,to:number,first_data?:number,days:HomeDay[],summary:string,counts:Object<string,number>,problems:HomeProblem[],work?:{measured:number,bad:number,good:number},known?:{since:number,text:string}|null,sides?:{home:number,provider:number,other:number},coverage?:number}} HomeHistory */
/** @typedef {{part:string,voice:string,at:number,time:number,from:number,to:number,measured:boolean,calls?:string,bad_from?:number,bad_to?:number,level:string,who:string,kind?:string,headline:string,cause?:string,fact?:string,fact_at?:number}} HomeAt */
/** @typedef {{id:number,at:string,text:string,public:boolean}} HomeNote */
/**
 * What Home needs from the page (app.js owns the hash, the status and sharing).
 * @typedef {{
 *   span: () => string, setSpan: (s: string) => void,
 *   openDetails: (hash: string) => void,
 *   status: () => any, startShare: () => Promise<void>,
 *   notesOn: () => boolean, notesChanged: () => void, reportsOn: () => boolean,
 * }} HomeHost
 */

const LIVE_MS = 15000;
const HISTORY_MS = 60000;
// The live part fails this many polls in a row before Home says it can't reach fyisp.
const LIVE_FAILS = 2;
// Undo for "Mark a bad call".
const UNDO_MS = 10000;
// Keep-it-running guide (README).
const KEEP_RUNNING = 'https://github.com/fuck-you-isp/fyisp#keep-it-running';

/** Icon and word per level: never colour alone. */
/** @type {Object<string,{icon:string,word:string}>} */
export const LEVELS = {
  good: { icon: '✓', word: 'OK' },
  warn: { icon: '!', word: 'Minor problem' },
  bad: { icon: '✕', word: 'Problem' },
  info: { icon: 'i', word: 'Known issue' },
  checking: { icon: '…', word: 'Checking' },
  affected: { icon: '–', word: 'Affected' },
};
/** @type {Object<string,string>} path node states */
export const NODE_ICON = { ok: '✓', warn: '!', bad: '✕', unknown: '?', finding: '…', affected: '–' };
// The tab title: the state first, so a background tab says it.
/** @type {Object<string,string>} */
const TITLE_WORD = { good: 'Fine', info: 'Fine', warn: 'Trouble', bad: 'Trouble', checking: 'Checking' };

/** Clock time in the viewer's locale (12 or 24 h). */
export function clock(/** @type {number} */ ms) { return new Date(ms).toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' }); }
/** Weekday and clock, when not today. */
export function when(/** @type {number} */ ms) {
  const d = new Date(ms);
  if (d.toDateString() === new Date().toDateString()) return clock(ms);
  return d.toLocaleDateString([], { weekday: 'short' }) + ' ' + clock(ms);
}
/** "2:10 to 2:16 pm"-ish: both ends in the viewer's clock. */
function range(/** @type {number} */ a, /** @type {number} */ b) { return b - a <= 60e3 ? when(a) : `${when(a)} to ${clock(b)}`; }
/** A duration in words: "6 minutes", "1 hour 5 minutes", "26 hours", "3 days". */
export function minutes(/** @type {number} */ ms) {
  const m = Math.max(1, Math.round(ms / 60e3));
  if (m < 60) return `${m} minute${m === 1 ? '' : 's'}`;
  const hh = Math.floor(m / 60), mm = m % 60;
  // Past 2 hours the minutes are noise; past 2 days, so are the hours.
  if (hh >= 48) { const dd = Math.round(hh / 24); return `${dd} days`; }
  if (hh >= 2) return `${hh} hours`;
  return `${hh} hour${hh === 1 ? '' : 's'}${mm ? ` ${mm} minute${mm === 1 ? '' : 's'}` : ''}`;
}
/** "a few seconds ago", "2 minutes ago". */
function ago(/** @type {number} */ ms) {
  if (ms < 60e3) return 'a few seconds ago';
  const m = Math.round(ms / 60e3);
  if (m < 60) return `${m} minute${m === 1 ? '' : 's'} ago`;
  return `at ${clock(Date.now() - ms)}`;
}
export function cls(/** @type {string} */ s) { return /^[a-z_]+$/.test(s) ? s : 'other'; }
/** An icon (hidden from screen readers) and its word. */
export function badge(/** @type {string} */ level, /** @type {string} */ word) {
  const L = LEVELS[level] || LEVELS.checking;
  return h('span', { class: `ho-badge lvl-${cls(level)}` }, h('span', { class: 'ho-icon', 'aria-hidden': 'true', text: L.icon }), h('span', { text: word }));
}

export class Home {
  /** @param {HomeHost} host */
  constructor(host) {
    this.host = host;
    /** @type {HomeLive|null} */
    this.live = null;
    /** @type {HomeHistory|null} */
    this.history = null;
    this.liveAt = 0; // when the last live answer arrived (client clock)
    this.liveFails = 0;
    this.liveBusy = false;
    this.historyAt = 0;
    this.historyBusy = false;
    this.historyErr = false;
    this.lastAnnounce = '';
    /** @type {{note:HomeNote|null,at:number,undoUntil:number,answer:HomeAt|null,err:string}|null} */
    this.marked = null;
    this.markOpen = false;
    this.markBusy = false;
    /** @type {number|null} "What happened at…?": the picked time (ms); null before the first pick */
    this.askAt = null;
    this.askSeq = 0;
    /** the timeline's roving focus (idx -1: not placed yet) */
    this.focus = { idx: -1, span: '' };

    // Live regions: one polite announcer (state changes only) and one
    // assertive one for offline / calls breaking up.
    this.announcer = h('div', { class: 'sr-only', role: 'status', 'aria-live': 'polite', 'aria-atomic': 'true' });
    this.alerter = h('div', { class: 'sr-only', role: 'alert', 'aria-live': 'assertive', 'aria-atomic': 'true' });

    // Answer card.
    this.ansBadge = h('span', { class: 'ho-ans-badge' });
    this.headEl = h('h2', { class: 'ho-headline', id: 'ho-headline' });
    this.causeEl = h('p', { class: 'ho-cause' });
    this.hintEl = h('p', { class: 'ho-hint' });
    this.pathEl = h('ol', { class: 'ho-path', 'aria-label': 'Where the problem is' });
    this.stepsEl = h('ol', { class: 'ho-steps', 'aria-label': 'What we have checked so far' });
    this.metaEl = h('p', { class: 'ho-meta' });
    this.moreBtn = h('button', { class: 'ho-link', type: 'button', 'aria-expanded': 'false', 'aria-controls': 'ho-more', text: 'What does this mean?', onclick: () => this.toggleMore() });
    this.moreEl = h('div', { class: 'ho-more', id: 'ho-more', hidden: true });
    this.tryEl = h('details', { class: 'ho-try' }, h('summary', { text: 'What you can try' }), h('ul'));
    this.card = h('section', { class: 'ho-card lvl-checking', 'aria-labelledby': 'ho-headline' },
      h('div', { class: 'ho-card-top' }, this.ansBadge, this.headEl),
      this.causeEl, this.hintEl, this.stepsEl, this.pathEl,
      h('div', { class: 'ho-card-foot' }, this.metaEl, this.moreBtn),
      this.moreEl, this.tryEl);

    // Mark a bad call.
    this.markBtn = /** @type {HTMLButtonElement} */ (h('button', { class: 'btn primary ho-markbtn', type: 'button', text: 'Mark a bad call', onclick: () => this.mark() }));
    this.earlierBtn = h('button', { class: 'ho-link', type: 'button', 'aria-expanded': 'false', 'aria-controls': 'ho-earlier', text: 'Was it earlier?', onclick: () => { this.markOpen = !this.markOpen; this.renderMark(); } });
    this.earlierEl = h('div', { class: 'ho-presets', id: 'ho-earlier', role: 'group', 'aria-label': 'When was the bad call?', hidden: true });
    this.markOut = h('div', { class: 'ho-mark-out', role: 'status' });
    this.markEl = h('section', { class: 'ho-mark', 'aria-label': 'Mark a bad call' },
      h('div', { class: 'ho-mark-row' }, this.markBtn, this.earlierBtn), this.earlierEl, this.markOut);
    // On a phone the button floats at the bottom of the screen once it has
    // been scrolled past (never over the answer); the wrapper keeps its place.
    this.markWrap = h('div', { class: 'ho-markwrap' }, this.markEl);
    this.floating = false;
    new IntersectionObserver((es) => {
      for (const e of es) this.setFloat(!e.isIntersecting && e.boundingClientRect.top < 0);
    }).observe(this.markWrap);

    // Tiles.
    this.tilesEl = h('ul', { class: 'ho-tiles', 'aria-label': 'What you use' });
    this.otherEl = h('p', { class: 'ho-other' });

    // Timeline.
    this.spanSeg = h('div', { class: 'seg sm ho-span', role: 'group', 'aria-label': 'Timeline span' });
    this.sumEl = h('p', { class: 'ho-summary' });
    this.stripsEl = h('div', { class: 'ho-strips' });
    this.timeErr = h('p', { class: 'ho-err', hidden: true }, 'Couldn’t load the history. ',
      h('button', { class: 'ho-link', type: 'button', text: 'Try again', onclick: () => this.loadHistory() }));
    this.legendEl = h('div', { class: 'ho-legend' });
    // Speed tests' time-of-day pattern: one plain line, only when there is one.
    this.speedEl = h('p', { class: 'ho-speed', hidden: true });
    this.timeEl = h('section', { class: 'ho-time', 'aria-labelledby': 'ho-time-h' },
      h('div', { class: 'ho-sec-head' }, h('h2', { id: 'ho-time-h', text: 'When was it bad?' }), this.spanSeg),
      this.sumEl, this.timeErr, this.stripsEl, this.legendEl, this.speedEl);

    // What happened at…?
    this.askIn = /** @type {HTMLInputElement} */ (h('input', { type: 'datetime-local', id: 'ho-ask-in' }));
    this.askIn.addEventListener('change', () => { const t = fromLocalInput(this.askIn.value); if (isFinite(t)) this.ask(t); });
    this.askPick = h('label', { class: 'fld ho-pick', for: 'ho-ask-in', hidden: true }, h('span', { text: 'Time' }), this.askIn);
    this.askOut = h('p', { class: 'ho-answer' });
    this.askLinks = h('div', { class: 'ho-links' });
    this.askEl = h('section', { class: 'ho-ask', 'aria-labelledby': 'ho-ask-h' },
      h('h2', { id: 'ho-ask-h', text: 'What happened at…?' }),
      h('div', { class: 'ho-presets', role: 'group', 'aria-label': 'Pick a time' }, ...this.presetButtons((t) => this.ask(t), () => this.showPicker())),
      this.askPick, this.askOut, this.askLinks);

    // Make a report.
    this.reportOut = h('div', { class: 'ho-report-out' });
    this.reportEl = h('section', { class: 'ho-report', 'aria-labelledby': 'ho-report-h' },
      h('h2', { id: 'ho-report-h', text: 'Make a report' }),
      h('p', { class: 'ho-muted', text: 'A link you can send to your internet provider or your IT team: the last 7 days, with no addresses or notes.' }),
      h('button', { class: 'btn primary ho-big', type: 'button', text: 'Make a report', onclick: () => this.report() }),
      this.reportOut);

    const pub = MODE === 'public';
    if (pub) { this.markEl.hidden = true; this.reportEl.hidden = true; }
    this.el = h('main', { id: 'home', class: 'home', hidden: true },
      this.announcer, this.alerter,
      this.card, this.markWrap, this.tilesEl, this.otherEl, this.timeEl,
      h('div', { class: 'ho-actions' }, this.askEl, this.reportEl));
  }

  /** The presets shared by "Was it earlier?" and "What happened at…?". */
  presetButtons(/** @type {(t:number)=>void} */ pick, /** @type {()=>void} */ picker) {
    return [
      h('button', { class: 'btn', type: 'button', text: 'A few minutes ago', onclick: () => pick(Date.now() - 5 * 60e3) }),
      h('button', { class: 'btn', type: 'button', text: 'An hour ago', onclick: () => pick(Date.now() - 60 * 60e3) }),
      h('button', { class: 'btn', type: 'button', text: 'Pick a time', onclick: picker }),
    ];
  }

  shown() { return !this.el.hidden; }

  /** Floats the Mark button (phones only, while Home is on screen). */
  setFloat(/** @type {boolean} */ on) {
    on = on && this.shown() && !this.markEl.hidden && window.matchMedia('(max-width: 767.98px)').matches;
    if (on === this.floating) return;
    this.markWrap.style.minHeight = on ? `${this.markEl.offsetHeight}px` : '';
    this.floating = on;
    this.markEl.classList.toggle('float', on);
    document.body.classList.toggle('ho-floating', on);
  }

  /** Loads both parts now (Home opened, or a refresh). */
  refresh() { return Promise.all([this.loadLive(), this.loadHistory()]); }

  /** Every second: reload what is due while Home is on screen. */
  tick() {
    if (!this.shown() || document.hidden) return;
    const now = Date.now();
    if (now - this.liveAt >= LIVE_MS && !this.liveBusy) this.loadLive();
    if (now - this.historyAt >= HISTORY_MS && !this.historyBusy) this.loadHistory();
    if (now % 15000 < 1000) this.renderMeta();
    if (this.marked && this.marked.undoUntil && now > this.marked.undoUntil) { this.marked.undoUntil = 0; this.renderMark(); }
  }

  tz() { return String(-new Date().getTimezoneOffset()); }

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
      this.liveAt = started; // retry on the next poll
    } finally {
      this.liveBusy = false;
    }
    this.renderLive();
  }

  async loadHistory() {
    if (this.historyBusy) return;
    this.historyBusy = true;
    this.historyAt = Date.now();
    try {
      this.history = await fetchJSON('api/home?' + new URLSearchParams({ part: 'history', span: this.host.span(), tz: this.tz() }));
      this.historyErr = false;
    } catch {
      this.historyErr = true;
      this.historyAt = Date.now() - HISTORY_MS + 15000; // retry in 15 s
    } finally {
      this.historyBusy = false;
    }
    this.renderHistory();
  }

  render() { this.renderLive(); this.renderHistory(); this.renderMark(); }

  // ---------- answer card ----------

  renderLive() {
    const d = this.live;
    const lost = this.liveFails >= LIVE_FAILS;
    this.card.classList.toggle('stale', lost);
    if (!d) {
      this.card.className = 'ho-card lvl-checking';
      this.ansBadge.replaceChildren(badge('checking', LEVELS.checking.word));
      this.headEl.textContent = lost ? 'Can’t reach fyisp on this computer' : 'Checking your connection';
      this.causeEl.textContent = lost ? 'Is it still running? We’ll keep trying.' : '';
      this.hintEl.textContent = '';
      this.pathEl.replaceChildren();
      this.stepsEl.replaceChildren();
      this.tilesEl.replaceChildren();
      this.renderMeta();
      return;
    }
    this.speedEl.hidden = !d.speed;
    this.speedEl.textContent = d.speed ? d.speed.text : '';
    const a = d.answer;
    // Can't reach fyisp: grey, never red, and the last answer "as of".
    const level = lost ? 'checking' : a.level;
    this.card.className = `ho-card lvl-${cls(level)}${lost ? ' stale' : ''}`;
    this.ansBadge.replaceChildren(badge(level, lost ? 'Not connected' : (LEVELS[level] || LEVELS.checking).word));
    if (lost) {
      this.headEl.textContent = 'Can’t reach fyisp on this computer';
      this.causeEl.textContent = `Is it still running? This is the last answer, as of ${clock(this.liveAt)}: ${a.headline}`;
      this.hintEl.textContent = '';
    } else {
      this.headEl.textContent = a.headline;
      this.causeEl.textContent = a.cause || '';
      this.hintEl.textContent = a.hint || '';
    }
    this.causeEl.hidden = !this.causeEl.textContent;
    this.hintEl.hidden = !this.hintEl.textContent;
    document.title = `${TITLE_WORD[level] || 'Checking'} · fyisp`;

    // Warm-up: the steps that fill, no numbers.
    const steps = a.level === 'checking' ? d.steps || [] : [];
    this.stepsEl.hidden = !steps.length;
    this.stepsEl.replaceChildren(...steps.map((s) => h('li', { class: s.done ? 'done' : '' },
      h('span', { class: 'ho-icon', 'aria-hidden': 'true', text: s.done ? '✓' : '…' }), s.label,
      h('span', { class: 'sr-only', text: s.done ? ', checked' : ', not checked yet' }))));

    // The path chain: router → provider → internet (→ apps when at fault).
    // This device and Apps & websites appear only when they are part of the story (offline; an app at fault).
    const nodes = (d.path || []).filter((n) => (n.id !== 'device' && n.id !== 'apps') || n.state !== 'ok' || n.blamed);
    this.pathEl.hidden = a.level === 'checking' || !nodes.length;
    const items = [];
    nodes.forEach((n, i) => {
      if (i) items.push(h('li', { class: 'ho-arrow', 'aria-hidden': 'true', text: '→' }));
      items.push(h('li', { class: `ho-node st-${cls(n.state)}${n.blamed ? ' blamed' : ''}` },
        h('span', { class: 'ho-icon', 'aria-hidden': 'true', text: NODE_ICON[n.state] || '?' }),
        h('span', { class: 'ho-node-l', text: n.label }),
        n.state !== 'ok' ? h('span', { class: 'ho-node-w', text: n.words }) : h('span', { class: 'sr-only', text: `: ${n.words}` }),
        n.blamed ? h('span', { class: 'ho-node-b', text: n.culprit || 'Problem is probably here' }) : null));
    });
    this.pathEl.replaceChildren(...items);

    // "What you can try" only when not OK.
    const tries = lost ? [] : a.try || [];
    this.tryEl.hidden = !tries.length;
    /** @type {HTMLElement} */ (this.tryEl.lastElementChild).replaceChildren(...tries.map((t) => h('li', { text: t })));
    this.renderMore();
    this.renderMeta();
    this.renderTiles();
    this.announce(lost);
  }

  /** "Checked a few seconds ago" (and "since 2:41 pm, 6 minutes"); plain text, never a live region. */
  renderMeta() {
    const d = this.live;
    const bits = [];
    if (d && d.answer.since && d.answer.level !== 'good' && d.answer.level !== 'checking') {
      bits.push(`Since ${when(d.answer.since)} (${minutes(Date.now() - d.answer.since)})`);
    }
    // Just after an outage ended: a calm "back to normal" line.
    if (d && d.answer.recovered && d.answer.level === 'good') bits.push(`Back to normal since ${when(d.answer.recovered)}`);
    if (d && d.answer.level === 'checking') bits.push('First results in about 2 minutes');
    else if (this.liveAt && d) bits.push(`Checked ${ago(Date.now() - this.liveAt)}`);
    this.metaEl.textContent = bits.join(' · ');
  }

  toggleMore() {
    const open = this.moreEl.hidden;
    this.moreEl.hidden = !open;
    this.moreBtn.setAttribute('aria-expanded', String(open));
  }

  /** The "What does this mean?" disclosure: the path explained, the call facts in words. */
  renderMore() {
    const d = this.live;
    if (!d) return;
    const kids = [];
    const a = d.answer;
    if (a.fact) kids.push(h('p', { text: a.fact_at ? `${a.fact.replace(/^In the worst minute/, `In the worst minute (${clock(a.fact_at)})`)}` : a.fact }));
    if (d.calls && d.calls.words) kids.push(h('p', {}, h('b', { text: 'Calls right now: ' }), d.calls.words));
    const dl = h('dl', { class: 'ho-glossary' });
    for (const n of d.path || []) {
      if (!n.tooltip) continue;
      dl.append(h('dt', { text: n.label }), h('dd', { text: n.tooltip }));
    }
    if (dl.childElementCount) kids.push(dl);
    const apps = d.apps || [];
    if (apps.length) {
      kids.push(h('p', { class: 'ho-apps-h', text: d.voice === 'public' ? 'Call apps' : 'Your call apps' }),
        h('ul', { class: 'ho-apps' }, ...apps.map((x) => h('li', {}, badge(x.level, x.label), ' ', h('b', { text: x.title }), x.note ? h('span', { class: 'ho-muted', text: ` ${x.note}` }) : null))));
    }
    if (d.apps_note) kids.push(h('p', { class: 'ho-muted', text: d.apps_note }));
    kids.push(h('p', { class: 'ho-muted', text: d.voice === 'public'
      ? 'Every few seconds, a device in this home sends small test messages to its router, its internet provider and common apps and websites. The other people on a call aren’t measured.'
      : 'Every few seconds we send small test messages to your router, your provider and the apps and websites you use, from this device. We can’t see the other people on a call.' }));
    this.moreEl.replaceChildren(...kids);
  }

  /** Speaks the answer only when it changes (level or who): never on a refresh. */
  announce(/** @type {boolean} */ lost) {
    const a = this.live && this.live.answer;
    const key = lost ? 'lost' : a ? `${a.level}|${a.who}|${a.calls || ''}` : '';
    if (!key || key === this.lastAnnounce) return;
    const first = this.lastAnnounce === '';
    this.lastAnnounce = key;
    if (first) return; // the page itself says it on load
    const text = lost ? 'Can’t reach fyisp on this computer.' : /** @type {HomeAnswer} */ (a).announce;
    const urgent = lost || (a && (a.who === 'offline' || a.calls === 'unusable'));
    (urgent ? this.alerter : this.announcer).textContent = text;
  }

  // ---------- tiles ----------

  renderTiles() {
    const d = this.live;
    if (!d) return;
    const lost = this.liveFails >= LIVE_FAILS;
    this.tilesEl.replaceChildren(...(d.tiles || []).map((t) => {
      const lvl = lost ? 'checking' : t.level;
      const names = t.names.join(' · ') + (t.more ? ` +${t.more}` : '');
      const label = lost ? `${t.label} (as of ${clock(this.liveAt)})` : t.label;
      const a = h('a', {
        class: `ho-tile lvl-${cls(lvl)}`, href: tileHash(t),
        'aria-label': `${t.title}: ${label}.${t.note ? ' ' + t.note : ''} See details`,
        onclick: (/** @type {MouseEvent} */ e) => { if (e.ctrlKey || e.metaKey || e.shiftKey || e.button) return; e.preventDefault(); this.host.openDetails(tileHash(t)); },
      },
      h('span', { class: 'ho-tile-t' }, h('span', { class: 'ho-icon', 'aria-hidden': 'true', text: (LEVELS[lvl] || LEVELS.checking).icon }), t.title),
      h('span', { class: 'ho-tile-l', text: label }),
      t.note ? h('span', { class: 'ho-tile-n', text: t.note }) : null,
      names ? h('span', { class: 'ho-tile-x', 'aria-hidden': 'true', text: names }) : null);
      return h('li', {}, a);
    }));
    this.tilesEl.dataset.n = String((d.tiles || []).length);
    const o = d.other;
    this.otherEl.hidden = !o || !o.line || (d.tiles || []).some((t) => t.id === 'other');
    this.otherEl.textContent = o && o.line ? o.line : '';
  }

  // ---------- timeline ----------

  renderHistory() {
    const span = this.host.span();
    this.spanSeg.replaceChildren(...[['d', 'Today'], ['w', 'Week']].map(([v, l]) => h('button', {
      type: 'button', text: l, 'aria-pressed': String(span === v),
      onclick: () => { if (this.host.span() !== v) { this.host.setSpan(v); this.history = null; this.renderHistory(); this.loadHistory(); } },
    })));
    this.timeErr.hidden = !this.historyErr;
    const d = this.history;
    if (!d || d.span !== span) {
      this.sumEl.textContent = this.historyErr ? '' : 'Loading…';
      this.stripsEl.replaceChildren();
      this.legendEl.replaceChildren();
      return;
    }
    // The summary sentence, with the times of the first few problems.
    // The server's sentence; a known issue with its start; then the problems' times (known issues aren't problems).
    const probs = d.problems.filter((p) => p.level !== 'info');
    const times = probs.slice(0, 3).map((p) => (p.ongoing ? `${when(p.from)} until now` : range(p.from, /** @type {number} */ (p.to))));
    this.sumEl.replaceChildren(h('span', { text: d.summary }));
    if (d.known && d.known.text) this.sumEl.append(' ', h('span', { text: `${d.known.text} ${when(d.known.since)}.` }));
    if (times.length) this.sumEl.append(h('span', { class: 'ho-times', text: `When: ${times.join('; ')}${probs.length > 3 ? '; …' : ''}.` }));
    const t = buildTimeline(d, this.focus, (st, a, b) => this.pickBlock(st, a, b), {});
    this.stripsEl.replaceChildren(t.grid);
    this.legendEl.replaceChildren(...t.legend);
    if (d.work && d.span === 'd' && d.work.measured) {
      this.legendEl.append(h('span', { class: 'ho-muted', text: `Working hours so far: good for calls ${Math.round(d.work.good * 100)}% of the time.` }));
    }
  }

  /** A block picked on the timeline fills "What happened at…?". */
  pickBlock(/** @type {{kind:string,run?:HomeRun|null}} */ st, /** @type {number} */ a, /** @type {number} */ b) {
    const t = st.run ? Math.max(a, st.run.from) : Math.min(Date.now(), (a + b) / 2);
    this.ask(t);
    this.askEl.scrollIntoView({ block: 'nearest' });
  }

  // ---------- what happened at…? ----------

  showPicker() {
    this.askPick.hidden = false;
    const now = Date.now();
    this.askIn.max = toLocalInput(now);
    const first = this.history && this.history.first_data;
    if (first) this.askIn.min = toLocalInput(first);
    if (!this.askIn.value) this.askIn.value = toLocalInput(now - 30 * 60e3);
    this.askIn.focus();
  }

  /** "What happened at…?" for around t (null: a few minutes ago), in the server's words. */
  async ask(/** @type {number|null} */ t) {
    this.askAt = t;
    const seq = ++this.askSeq;
    const r = await this.at(t == null ? Date.now() - 5 * 60e3 : t);
    if (seq !== this.askSeq) return;
    this.askOut.className = `ho-answer lvl-${cls(r ? r.level : 'checking')}`;
    this.askOut.replaceChildren(...this.atNodes(r));
    const links = [];
    if (r && r.measured) {
      const a = r.bad_from || r.from, b = r.bad_to || r.to;
      const hash = `#v=calls&from=${Math.round(a - 10 * 60e3)}&to=${Math.round(Math.min(Date.now(), b + 10 * 60e3))}`;
      links.push(h('a', { class: 'ho-link', href: hash, onclick: (/** @type {MouseEvent} */ e) => { e.preventDefault(); this.host.openDetails(hash); }, text: 'See details' }));
    }
    this.askLinks.replaceChildren(...links);
  }

  /**
   * The server's answer for around t (api/home?part=at), or null.
   * @param {number} t
   * @returns {Promise<HomeAt|null>}
   */
  async at(t) {
    try {
      return await fetchJSON('api/home?' + new URLSearchParams({ part: 'at', at: String(Math.round(Math.min(t, Date.now()))), tz: this.tz() }));
    } catch {
      return null;
    }
  }

  /** An answer for a time: the time (bold), the headline, then the cause and the fact. */
  atNodes(/** @type {HomeAt|null} */ r) {
    if (!r) return [h('span', { text: 'Couldn’t check that time. Try again.' })];
    const span = r.bad_from && r.bad_to ? `${when(r.bad_from)} to ${clock(r.bad_to)}` : `Around ${when(r.time)}`;
    const L = LEVELS[r.level] || LEVELS.checking;
    const kids = [badge(r.level, L.word), ' ', h('b', { text: `${span}: ` }), `${r.headline}.`];
    if (r.cause) kids.push(' ', r.cause);
    if (r.fact) kids.push(' ', r.fact_at ? r.fact.replace(/^In the worst minute/, `In the worst minute (${clock(r.fact_at)})`) : r.fact);
    return kids;
  }

  // ---------- mark a bad call ----------

  renderMark() {
    if (MODE === 'public') return;
    this.markBtn.disabled = this.markBusy;
    this.earlierBtn.setAttribute('aria-expanded', String(this.markOpen));
    this.earlierEl.hidden = !this.markOpen;
    if (this.markOpen && !this.earlierEl.childElementCount) {
      this.earlierEl.append(...this.presetButtons((t) => this.mark(t), () => this.markPicker()));
    }
    const m = this.marked;
    const kids = [];
    if (m) {
      kids.push(h('p', { class: 'ho-mark-a' }, ...(m.answer ? this.atNodes(m.answer) : [this.markBusy ? 'Checking that time…' : ''])));
      if (m.err) kids.push(h('p', { class: 'ho-muted', text: m.err }));
      else if (m.note) {
        kids.push(h('p', { class: 'ho-muted' }, `Saved a private note at ${clock(m.at)}. `,
          m.undoUntil ? h('button', { class: 'ho-link', type: 'button', text: 'Undo', onclick: () => this.undo() }) : null));
      }
    }
    this.markOut.replaceChildren(...kids);
  }

  markPicker() {
    const input = /** @type {HTMLInputElement} */ (h('input', { type: 'datetime-local', id: 'ho-mark-in', max: toLocalInput(Date.now()), value: toLocalInput(Date.now() - 15 * 60e3) }));
    input.addEventListener('change', () => { const t = fromLocalInput(input.value); if (isFinite(t)) this.mark(t); });
    const lab = h('label', { class: 'fld ho-pick', for: 'ho-mark-in' }, h('span', { text: 'Time of the call' }), input);
    this.earlierEl.querySelector('.ho-pick')?.remove();
    this.earlierEl.append(lab);
    input.focus();
  }

  /**
   * Why writes (notes, sharing) can't happen from this page, in plain
   * words, or '' when they can. Never mentions flags.
   */
  writeBlock() {
    const st = this.host.status();
    if (MODE !== 'local' || !st || !this.host.notesOn() || st.controls === 'disabled') return 'Not saved: this dashboard is read-only on this device.';
    if (st.controls === 'admin' && !st.admin) return 'Not saved yet: to save notes or share from this browser, open the link fyisp showed when it started (in its log or terminal window).';
    return canWrite() ? '' : 'Not saved: this dashboard is read-only on this device.';
  }

  /** One click: a private note at t (now by default) and, beside it, what happened then. */
  async mark(/** @type {number|undefined} */ t) {
    if (this.markBusy) return;
    const at = t == null ? Date.now() : t;
    // "Was it earlier?" moves the note just saved instead of adding a second one.
    const prev = t != null && this.marked ? this.marked.note : null;
    this.markBusy = true;
    this.marked = { note: prev, at, undoUntil: 0, answer: null, err: '' };
    this.renderMark();
    const answer = await this.at(at);
    this.marked.answer = answer;
    const block = this.writeBlock();
    if (block) {
      this.marked.note = null;
      this.marked.err = block;
    } else {
      const body = { at: new Date(at).toISOString(), text: answer ? `Bad call: ${answer.headline}. ${answer.cause || ''}`.trim() : 'Bad call', public: false };
      try {
        const note = prev
          ? await apiWrite('PUT', `api/annotations/${prev.id}`, body)
          : await apiWrite('POST', 'api/annotations', body);
        this.marked.note = note;
        this.marked.undoUntil = Date.now() + UNDO_MS;
        this.host.notesChanged();
      } catch {
        this.marked.err = 'Couldn’t save the note. Try again.';
      }
    }
    this.markBusy = false;
    this.renderMark();
  }

  async undo() {
    const m = this.marked;
    if (!m || !m.note) return;
    try {
      await apiWrite('DELETE', `api/annotations/${m.note.id}`);
      this.marked = null;
      this.markOut.replaceChildren(h('p', { class: 'ho-muted', text: 'Removed.' }));
      this.host.notesChanged();
    } catch {
      m.err = 'Couldn’t remove the note. Try again.';
      this.renderMark();
    }
  }

  // ---------- make a report (the public link) ----------

  /** The report is the public link: a redacted 7-day summary. */
  async report() {
    const st = this.host.status();
    const out = this.reportOut;
    // The home-network warning needs the week's counts, whatever span is on screen.
    let counts = this.history && this.history.span === 'w' ? this.history.counts : null;
    if (!counts) {
      try { counts = (await fetchJSON('api/home?' + new URLSearchParams({ part: 'history', span: 'w', tz: this.tz() }))).counts; } catch { /* no warning then */ }
    }
    const kids = [];
    // Honest when the evidence points home.
    if (counts && counts.home > (counts.provider || 0) + (counts.beyond || 0)) {
      kids.push(h('p', { class: 'ho-warn', text: 'Most problems this week were in your home network (Wi-Fi or router). Your provider will likely say the same.' }));
    }
    if (st && st.controls === 'admin' && !st.admin) {
      kids.push(h('p', { class: 'ho-muted', text: 'To share from this browser, first open the link fyisp showed when it started (in its log or terminal window). Then try again.' }));
      out.replaceChildren(...kids);
      return;
    }
    if (!canWrite() || (st && st.controls === 'disabled')) {
      kids.push(h('p', { class: 'ho-muted', text: 'Sharing is turned off on this setup, so we can’t make a link here. Details has a report you can download.' }));
      out.replaceChildren(...kids);
      return;
    }
    const url = st && st.share && st.share.phase === 'connected' ? String(st.share.url || '') : '';
    if (!url) {
      kids.push(h('p', { class: 'ho-muted', role: 'status', text: 'Making your link. This takes about 10 seconds…' }));
      out.replaceChildren(...kids);
      try { await this.host.startShare(); } catch { /* the status below says */ }
      for (let i = 0; i < 20; i++) {
        const s = this.host.status();
        if (s && s.share && (s.share.phase === 'connected' || s.share.phase === 'error')) break;
        await new Promise((r) => setTimeout(r, 1000));
      }
      return this.reportLink(kids);
    }
    this.reportLink(kids);
  }

  reportLink(/** @type {Node[]} */ kids) {
    const st = this.host.status();
    const sh = st && st.share;
    if (!sh || sh.phase !== 'connected' || !sh.url) {
      kids.push(h('p', { class: 'ho-warn', role: 'alert', text: 'We couldn’t make the link. Check your internet and try again.' }));
      this.reportOut.replaceChildren(...kids);
      return;
    }
    const input = /** @type {HTMLInputElement} */ (h('input', { type: 'text', readonly: true, value: sh.url, id: 'ho-report-url' }));
    input.addEventListener('focus', () => input.select());
    const copy = h('button', { class: 'btn', type: 'button', text: 'Copy link', onclick: () => copyText(sh.url, copy) });
    kids.push(h('label', { class: 'fld', for: 'ho-report-url' }, h('span', { text: 'Your report link' }), input),
      h('div', { class: 'ho-links' }, copy),
      h('p', { class: 'ho-muted', text: 'Anyone with the link can see the last 7 days of this connection’s quality. It stops working when fyisp stops; you can stop sharing in Details.' }));
    this.reportOut.replaceChildren(...kids);
  }
}

/** A tile's Details link, as the server picks it (#v=calls, or the Overview filtered with ot=). */
function tileHash(/** @type {HomeTile} */ t) { return t.details; }

/**
 * The Today/Week timeline: one row of blocks per day (an hour each, 3 hours
 * on a phone) whose height and pattern carry the state, a "now" marker, the
 * hour axis and the legend. One tab stop; arrow keys move across blocks.
 * focus keeps the roving position across refreshes. Home and the public
 * report share it.
 * @param {HomeHistory} d
 * @param {{idx:number,span:string}} focus
 * @param {(st:{kind:string,level?:string,run?:HomeRun|null}, a:number, b:number) => void} onPick
 * @param {{noKeep?:boolean,offWord?:string,dates?:boolean}} opts (dates: label rows "Wed 7 Oct", for the report read later)
 */
export function buildTimeline(d, focus, onPick, opts) {
  const phone = window.matchMedia('(max-width: 767.98px)').matches;
  const per = phone ? 3 : 1; // hours per block: blocks stay ≥ 24 px wide on a phone
  const rows = [];
  /** @type {HTMLElement[]} */
  const blocks = [];
  for (const day of d.days) {
    const cells = [];
    const name = new Date(day.from + 12 * 3600e3).toLocaleDateString([], { weekday: 'short' });
    for (let hr = 0; hr < 24; hr += per) {
      const a = day.from + hr * 3600e3, b = Math.min(day.from + (hr + per) * 3600e3, day.from + 86400e3);
      const st = blockState(day, d, a, b);
      const label = blockLabel(name, a, b, st);
      const cell = h('button', {
        type: 'button', class: `ho-blk b-${st.kind}${st.level ? ' lvl-' + st.level : ''}`, 'aria-label': label, title: label, tabindex: '-1',
        disabled: st.kind === 'later' || null,
        onclick: () => onPick(st, a, b),
      });
      if (st.kind !== 'later') blocks.push(cell);
      cells.push(cell);
    }
    const strip = h('div', { class: 'ho-cells' }, ...cells);
    if (day.to < day.from + 86400e3) {
      // Today: a "now" marker; the hours after it are faded.
      // The word "now" only on the one-day strip: on the week it would sit on the row above.
      const now = h('span', { class: 'ho-now', 'aria-hidden': 'true' }, d.span === 'd' ? h('span', { text: 'now' }) : null);
      now.style.left = `${((day.to - day.from) / 86400e3) * 100}%`;
      strip.append(now);
    }
    rows.push(h('div', { class: 'ho-row' },
      h('span', { class: 'ho-day', 'aria-hidden': 'true', text: opts.dates ? new Date(day.from + 12 * 3600e3).toLocaleDateString([], { weekday: 'short', day: 'numeric', month: 'short' }) : d.span === 'd' ? 'Today' : name }), strip));
  }
  // Hour marks under the strips, each at hour/24 of the strip.
  const axis = h('div', { class: 'ho-axis' });
  for (const x of [0, 6, 12, 18]) {
    const m = h('span', { text: clock(new Date(d.days[0].from).setHours(x, 0, 0, 0)).replace(':00', '') });
    m.style.left = `${(x / 24) * 100}%`;
    axis.append(m);
  }
  const marks = h('div', { class: 'ho-row ho-marks', 'aria-hidden': 'true' }, h('span', { class: 'ho-day' }), axis);
  const grid = h('div', { class: 'ho-grid', role: 'group', 'aria-label': `${d.span === 'd' ? 'Today' : 'The last 7 days'}, ${per === 1 ? 'one block per hour' : `one block per ${per} hours`}. Use the arrow keys to move between blocks.` }, ...rows, marks);
  // Roving focus: one tab stop, arrows move across blocks (and rows).
  // It starts at the latest problem block, else the current hour (the last block that has happened).
  if (focus.idx < 0 || focus.idx >= blocks.length || focus.span !== d.span) {
    const lastRun = blocks.map((b) => b.classList.contains('b-run')).lastIndexOf(true);
    focus.idx = lastRun >= 0 ? lastRun : blocks.length - 1;
    focus.span = d.span;
  }
  if (blocks[focus.idx]) blocks[focus.idx].tabIndex = 0;
  grid.addEventListener('keydown', (e) => {
    const i = blocks.indexOf(/** @type {HTMLElement} */ (e.target));
    if (i < 0) return;
    const cols = 24 / per;
    const step = { ArrowRight: 1, ArrowLeft: -1, ArrowDown: cols, ArrowUp: -cols, Home: -i, End: blocks.length - 1 - i }[e.key];
    if (step == null) return;
    e.preventDefault();
    const j = Math.max(0, Math.min(blocks.length - 1, i + step));
    blocks[i].tabIndex = -1;
    blocks[j].tabIndex = 0;
    blocks[j].focus();
    focus.idx = j;
  });
  const anyOff = d.days.some((x) => x.off.length);
  const legend = [
    h('span', {}, h('span', { class: 'ho-key b-fine', 'aria-hidden': 'true' }), 'No problems'),
    h('span', {}, h('span', { class: 'ho-key b-run lvl-warn', 'aria-hidden': 'true' }), LEVELS.warn.word),
    h('span', {}, h('span', { class: 'ho-key b-run lvl-bad', 'aria-hidden': 'true' }), LEVELS.bad.word),
    d.days.some((x) => x.runs.some((r) => r.level === 'info')) ? h('span', {}, h('span', { class: 'ho-key b-run lvl-info', 'aria-hidden': 'true' }), LEVELS.info.word) : null,
    h('span', {}, h('span', { class: 'ho-key b-off', 'aria-hidden': 'true' }), opts.offWord || 'fyisp wasn’t running'),
    d.days.some((x) => x.no_record) || (d.first_data && d.first_data > d.from) ? h('span', {}, h('span', { class: 'ho-key b-none', 'aria-hidden': 'true' }), 'No record yet') : null,
    anyOff && !opts.noKeep ? h('a', { class: 'ho-keep', href: KEEP_RUNNING, target: '_blank', rel: 'noopener', text: 'How to keep it running' }) : null,
  ].filter((x) => x != null);
  return { grid, legend: /** @type {HTMLElement[]} */ (legend) };
}

/** @type {Object<string,number>} run levels: a known issue (info) ranks below every problem */
const RUN_RANK = { bad: 3, warn: 2, info: 1 };
/** The worst run overlapping [a, b): bad over warn over info, then the longest. */
function worstRun(/** @type {HomeRun[]} */ runs, /** @type {number} */ a, /** @type {number} */ b) {
  let best = null;
  for (const r of runs) {
    if (!(r.from < b && r.to > a)) continue;
    const rank = RUN_RANK[r.level] || 1;
    const bestRank = best ? RUN_RANK[best.level] || 1 : 0;
    if (!best || rank > bestRank || (rank === bestRank && r.to - r.from > best.to - best.from)) best = r;
  }
  return best;
}

/**
 * The state of one timeline block [a, b): the worst run, else not running,
 * no record, later today, or fine.
 * @param {HomeDay} day @param {HomeHistory} d @param {number} a @param {number} b
 * @returns {{kind:string,level?:string,run?:HomeRun|null}}
 */
function blockState(day, d, a, b) {
  if (a >= day.to) return { kind: 'later' };
  if (day.no_record || (d.first_data && b <= d.first_data)) return { kind: 'none' };
  const run = worstRun(day.runs, a, b);
  if (run) return { kind: 'run', level: run.level, run };
  const end = Math.min(b, day.to);
  const off = day.off.filter((s) => s.from < end && s.to > a).reduce((n, s) => n + Math.min(end, s.to) - Math.max(a, s.from), 0);
  if (off >= (end - a) / 2) return { kind: 'off' };
  return { kind: 'fine' };
}

/** "Tue 14:00 to 15:00, problem: The problem was most likely with your internet provider. …" (the run's text is the server's). */
function blockLabel(/** @type {string} */ day, /** @type {number} */ a, /** @type {number} */ b, /** @type {{kind:string,level?:string,run?:HomeRun|null}} */ st) {
  const t = `${day} ${clock(a)} to ${clock(b)}`;
  switch (st.kind) {
    case 'later': return `${t}, later today`;
    case 'none': return `${t}, no record yet`;
    case 'off': return `${t}, fyisp wasn’t running`;
    case 'fine': return `${t}, no problems`;
  }
  const r = /** @type {HomeRun} */ (st.run);
  return `${t}, ${(LEVELS[st.level || 'warn'] || LEVELS.warn).word.toLowerCase()}: ${r.text || r.label}`;
}
