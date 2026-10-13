// Details → Speed, the queue: every test (Test now, a full test of every
// server, the schedule's slots) waits its turn in one queue and exactly one
// runs at a time (docs/SPEEDTEST-API.md, "Queue"). This shows where your own
// request is ("You're #2, starts in about 18 minutes", then its progress),
// what else is waiting, and the last full test's results. The owner can
// cancel any entry. Plain ES module, no build step.
// @ts-check

import { h, fetchJSON, apiWrite, canWrite, speedRefusal, dialog, clock, fmtMbps } from './core.js';
import { readingCell, lowerBound } from './speedmap.js';

/** @typedef {import('./speed.js').SpeedSummary} SpeedSummary */
/** @typedef {import('./speed.js').SpeedTest} SpeedTest */
/** @typedef {{id:number,kind:string,who:string,state:string,target:{id:string,label:string}|null,position:number,enqueued_at:number,started_at:number|null,finished_at:number|null,est_start:number,est_finish:number,step:number,steps:number,server:{id:string,label:string}|null,direction:string|null,tests:number[],origin?:string,results?:SweepResult[],stopped_reason?:string|null}} QueueEntry */
/** @typedef {{server:{id:string,label:string},status:string,test?:number,down_bps?:number|null,up_bps?:number|null}} SweepResult */
/** @typedef {{at:number,mine:number|null,running:QueueEntry|null,waiting:QueueEntry[],last_sweep:QueueEntry|null}} QueueState */
/** @typedef {{t:SpeedTest,region:string,provider:string}} SwRow */
/** @typedef {{summary:()=>SpeedSummary|null, done:(e:QueueEntry, tag:string)=>void, say:(text:string)=>void, sweepHash:()=>boolean, started:()=>void}} QueueHost */

const POLL_MS = 3000;
const WHY = 'Running a speed test needs the admin token. Open the link fyisp printed when it started, or paste the token here.';

/** "in about 18 minutes", "in under a minute". */
function inAbout(/** @type {number} */ ms) {
  const m = Math.round(ms / 60e3);
  return m < 1 ? 'in under a minute' : `in about ${m} minute${m === 1 ? '' : 's'}`;
}
// A full test: seconds per server, with the pause between tests. Each test runs up to 10 seconds each way.
const SWEEP_S = 26;
/** "2.4 GB", "850 MB". @param {number} b */
function gbData(b) { return b >= 1e9 ? `${(b / 1e9).toFixed(1).replace(/\.0$/, '')} GB` : `${Math.max(1, Math.round(b / 1e6))} MB`; }
/** Why a full test ended early, or ''. @param {QueueEntry} e */
function stoppedWhy(e) {
  switch (e.stopped_reason) {
    case 'unreachable': return 'Stopped after 3 servers in a row didn’t answer. Check your connection.';
    case 'cancelled': return 'Cancelled.';
  }
  return '';
}

/** What an entry is, in words. @param {QueueEntry} e */
function what(e) {
  if (e.kind === 'sweep') return 'Test of every server';
  if (e.kind === 'scheduled') return e.target ? `Scheduled test: ${e.target.label}` : 'Scheduled test';
  return e.target ? `Test of ${e.target.label}` : 'Test of the nearest servers';
}
/** Who asked. @param {QueueEntry} e */
function whoWord(e) {
  return e.who === 'scheduled' ? 'the schedule' : 'you';
}
/** Why a start or cancel failed, from the API's code. @param {any} e */
export function queueError(e) {
  const b = e.body || {};
  switch (b.code) {
    case 'skipped': return 'You skipped that server.';
    case 'not_found': return 'That test isn’t in the queue any more.';
    case 'forbidden': return speedRefusal();
  }
  if (e.status === 0) return ''; // the admin dialog was cancelled
  return 'Couldn’t reach fyisp. Try again.';
}

/** POST api/speed/run: resolves to {entry, duplicate}, throws like apiWrite (queueError says why). @param {string} kind @param {string} id */
function queueRun(kind, id) {
  const q = new URLSearchParams({ kind });
  if (id) q.set('id', id);
  return apiWrite('POST', 'api/speed/run?' + q, undefined, WHY);
}
/** Queues one test: of a server, a region, or ('') the nearest servers. @param {string} [id] */
export function startSingleTest(id = '') { return queueRun('single', id); }

export class SpeedQueue {
  /** @param {QueueHost} host */
  constructor(host) {
    this.host = host;
    /** @type {QueueState|null} */
    this.q = null;
    /** Entries this page asked for, and what to do when each ends. @type {Map<number,string>} */
    this.watch = new Map();
    this.note = '';
    this.lastStep = '';
    /** @type {number|undefined} */
    this.timer = undefined;
    this.seq = 0;
    /** The last finished sweep, and one under way with its results so far. @type {{entry:QueueEntry|null,tests:SpeedTest[],running:QueueEntry|null}|null} */
    this.sweep = null;
    this.sweepAt = 0;
    /** @type {{key:string,asc:boolean}} */
    this.sort = { key: 'region', asc: true };
    this.scrolled = false;
    /** @type {string|null} the disabled button's reason last said; null before the first render */
    this.lastWhy = null;

    this.el = h('section', { class: 'sp-queue', 'aria-labelledby': 'sp-q-h', hidden: true });
    this.sweepEl = h('section', { id: 'sp-sweep', class: 'sp-sweep', 'aria-labelledby': 'sp-sw-h' });
  }

  /** Reads the queue; polls every 3 s while anything waits or runs and the page is visible. */
  async load() {
    const seq = ++this.seq;
    try {
      const q = /** @type {QueueState} */ (await fetchJSON('api/speed?part=queue'));
      if (seq !== this.seq) return;
      this.q = q;
    } catch {
      if (seq !== this.seq) return;
    }
    await this.settle();
    // The last full test's results, when it changed.
    const ls = this.q?.last_sweep;
    // The last full test's results when it changed, and a running one's every poll.
    const sweeping = this.q?.running?.kind === 'sweep' || !!this.sweep?.running;
    if (sweeping || (ls && (ls.finished_at || 0) !== this.sweepAt) || (!ls && !this.sweep)) await this.loadSweep();
    this.render();
    clearTimeout(this.timer);
    const busy = this.q && (this.q.running || this.q.waiting.length || this.watch.size);
    if (busy) this.timer = window.setTimeout(() => { if (!this.el.isConnected || this.el.closest('[hidden]')) return; this.load(); }, POLL_MS);
  }

  /** Watched entries that left the queue: fetch how they ended and hand them on. */
  async settle() {
    const q = this.q;
    if (!q) return;
    const live = new Set([q.running?.id, ...q.waiting.map((e) => e.id)]);
    for (const [id, tag] of [...this.watch]) {
      if (live.has(id)) continue;
      this.watch.delete(id);
      try {
        const e = /** @type {QueueEntry} */ (await fetchJSON('api/speed?part=entry&id=' + id));
        this.lastStep = '';
        if (e.kind === 'sweep') {
          await this.loadSweep();
          this.host.say(e.state === 'done' ? `The test of every server is done: ${e.tests.length} tests.${stoppedWhy(e) ? ' ' + stoppedWhy(e) : ''}` : 'The test of every server was cancelled.');
        }
        this.host.done(e, tag);
      } catch { /* gone: nothing to say */ }
    }
  }

  async loadSweep() {
    try {
      const r = await fetchJSON('api/speed?part=sweep');
      this.sweep = { entry: r.entry, tests: Array.isArray(r.tests) ? r.tests : [], running: r.running || null };
      this.sweepAt = r.entry?.finished_at || 0;
    } catch { /* keep what we had */ }
  }

  /** Whether this viewer may start tests: the owner with controls. */
  canStart() { return canWrite(); }

  /**
   * Adds a test to the queue. kind 'single' (id: a server, a region, or '' the
   * nearest) or 'sweep'; tag says what to do with the result.
   * @param {string} kind @param {string} id @param {string} tag
   */
  async start(kind, id, tag) {
    this.note = '';
    try {
      const r = await queueRun(kind, id);
      /** @type {QueueEntry} */
      const e = r.entry;
      this.watch.set(e.id, tag);
      this.host.started();
      const where = e.state === 'running' ? 'It’s starting now.' : `You’re #${e.position}, it starts ${inAbout(e.est_start - Date.now())}.`;
      this.host.say(r.duplicate ? `You already asked for this. ${where}` : `${what(e)} added. ${where}`);
    } catch (e) {
      this.note = queueError(e);
      if (this.note) this.host.say(this.note);
    }
    await this.load();
    // A full test lands on the queue's heading; a single test on its own status line.
    /** @type {HTMLElement|null} */ (kind === 'sweep' ? document.getElementById('sp-q-h') : this.el.querySelector('.sp-mine-t'))?.focus();
  }

  /**
   * "Test every server": what it costs, then [Cancel] [Test N servers]. Cancel
   * is focused first and sends nothing; the server's checks still decide.
   */
  async confirmSweep() {
    const d = this.host.summary();
    const servers = d ? d.servers : [];
    const sw = /** @type {any} */ (d || {});
    // The server's count (skipped servers left out) and its estimate of the whole run's data.
    const n = sw.sweep_servers || servers.filter((x) => !x.skipped).length || 45;
    const skipped = servers.filter((x) => x.skipped).length;
    const mins = Math.max(1, Math.round(n * SWEEP_S / 60));
    const data = sw.sweep_bytes > 0 ? ` It uses about ${gbData(sw.sweep_bytes)} of data.` : '';
    const p = (/** @type {string} */ t) => h('p', { text: t });
    /** @type {Node[]} */
    const body = [p(`One test on each of the ${n} servers, one after another.${skipped ? ` ${skipped === 1 ? '1 server you skipped won’t be tested.' : `${skipped} servers you skipped won’t be tested.`}` : ''}`)];
    body.push(p(`Takes about ${mins} minute${mins === 1 ? '' : 's'}: each test runs up to 10 seconds each way.${data}`));
    body.push(p('Your line is busy most of that time, so calls may stutter.'));
    body.push(p('The results are a snapshot. They don’t change the hourly pattern.'));
    if (d && !d.enabled) body.push(p('Speed tests are off. This runs once and doesn’t turn them on.'));
    const v = await dialog({
      title: 'Test every server now?',
      body,
      buttons: [{ label: 'Cancel', value: 'cancel' }, { label: n === 1 ? 'Test 1 server' : `Test ${n} servers`, value: 'ok', primary: true }],
      safe: true,
    });
    if (v === 'ok') { await this.start('sweep', '', 'sweep'); return; }
    /** @type {HTMLElement|null} */ (this.sweepEl.querySelector('.sp-go'))?.focus();
  }

  /** @param {number} id */
  async cancel(id) {
    try {
      await apiWrite('POST', 'api/speed/cancel?id=' + id, undefined, 'Cancelling a speed test needs the admin token. Open the link fyisp printed when it started, or paste the token here.');
      this.host.say('Cancelled.');
    } catch (e) { this.note = queueError(e); if (this.note) this.host.say(this.note); }
    await this.load();
    /** @type {HTMLElement|null} */ (this.el.querySelector('h2'))?.focus();
  }

  /** This viewer's own entry, waiting or running. */
  mine() {
    const q = this.q;
    if (!q || q.mine == null) return null;
    if (q.running && q.running.id === q.mine) return q.running;
    return q.waiting.find((e) => e.id === q.mine) || null;
  }

  render() {
    this.renderQueue();
    this.renderSweep();
    if (this.host.sweepHash() && !this.scrolled && this.sweepEl.isConnected && !this.sweepEl.closest('[hidden]')) {
      this.scrolled = true;
      requestAnimationFrame(() => this.sweepEl.scrollIntoView({ block: 'start' }));
    }
  }

  renderQueue() {
    const q = this.q;
    const owner = canWrite();
    const all = q ? [q.running, ...q.waiting].filter((e) => !!e) : [];
    const me = this.mine();
    this.el.hidden = !all.length && !this.note;
    if (this.el.hidden) return;
    const rows = all.map((e) => {
      const e2 = /** @type {QueueEntry} */ (e);
      const when = e2.state === 'running'
        ? (e2.kind === 'sweep' ? `now, server ${e2.step} of ${e2.steps}` : 'now')
        : `#${e2.position}, starts about ${clock(e2.est_start)}`;
      return h('li', { class: (e2.state === 'running' ? 'run' : '') + (e2.who === 'you' ? ' you' : '') },
        h('span', { class: 'sp-q-what', text: what(e2) }),
        h('span', { class: 'sp-q-who', text: `asked by ${whoWord(e2)}` }),
        h('span', { class: 'sp-q-when', text: when }),
        owner ? h('button', { class: 'btn sm ghost', type: 'button', text: e2.state === 'running' ? 'Stop' : 'Cancel', 'aria-label': `${e2.state === 'running' ? 'Stop' : 'Cancel'}: ${what(e2)}`, onclick: () => this.cancel(e2.id) }) : '');
    });
    this.el.replaceChildren(...[
      me ? this.mineNodes(me) : null,
      h('div', { class: 'sp-sec-head' },
        h('h2', { id: 'sp-q-h', tabindex: '-1', text: 'Test queue' })),
      h('p', { class: 'muted small', text: 'One test runs at a time; the rest wait their turn.' }),
      rows.length ? h('ol', { class: 'sp-q' }, ...rows) : h('p', { class: 'muted', text: 'Nothing is waiting.' }),
      this.note ? h('p', { class: 'sp-note', text: this.note }) : null,
    ].filter((x) => !!x));
  }

  /** Your request: its place in the queue, then its steps. @param {QueueEntry} e */
  mineNodes(e) {
    if (e.state === 'waiting') {
      return h('div', { class: 'sp-mine' },
        h('p', { class: 'sp-mine-t', tabindex: '-1', text: `${what(e)}: you’re #${e.position}, it starts ${inAbout(e.est_start - Date.now())}.` }));
    }
    const sweep = e.kind === 'sweep';
    const step = e.direction === 'picking' || !e.server ? 'find' : e.direction === 'download' ? 'down' : 'up';
    const names = { find: 'Finding a free server', down: 'Download', up: 'Upload' };
    const order = ['find', 'down', 'up'];
    const at = order.indexOf(step);
    const left = Math.max(5, Math.round((e.est_finish - Date.now()) / 1000));
    const pct = sweep ? Math.round(((e.step - 1) + (at + 1) / 3) / Math.max(1, e.steps) * 100)
      : Math.min(99, Math.max(2, Math.round((Date.now() - (e.started_at || Date.now())) / Math.max(1, e.est_finish - (e.started_at || Date.now())) * 100)));
    const title = sweep ? `Testing every server: server ${e.step} of ${e.steps}${e.server ? `, ${e.server.label}` : ''}…`
      : e.server ? `Testing with ${e.server.label}…` : 'Testing…';
    // Announce starting, not every step: a sweep has 45 servers.
    if (this.lastStep !== `${e.id}`) { this.lastStep = `${e.id}`; this.host.say(sweep ? 'The test of every server has started.' : title); }
    return h('div', { class: 'sp-mine' },
      h('p', { class: 'sp-mine-t sp-prog-title', tabindex: '-1', text: title }),
      h('ol', { class: 'sp-steps' }, ...order.map((s, i) => h('li', { class: i < at ? 'done' : i === at ? 'now' : '' },
        h('span', { class: 'sp-step-mark', 'aria-hidden': 'true', text: i < at ? '✓' : i === at ? '●' : '○' }),
        names[/** @type {'find'} */ (s)],
        h('span', { class: 'sr-only', text: i < at ? ' (done)' : i === at ? ' (now)' : '' })))),
      h('progress', { class: 'sp-bar', max: '100', value: String(Math.min(99, Math.max(1, pct))), 'aria-label': sweep ? 'Progress through every server' : 'Test progress' }),
      h('p', { class: 'muted small', text: `${left > 120 ? `About ${Math.round(left / 60)} minutes left.` : `About ${left} seconds left.`} Calls may stutter briefly.` }));
  }

  // ---------- the full test of every server ----------

  renderSweep() {
    const d = this.host.summary();
    const q = this.q;
    const mineSweep = q && [q.running, ...q.waiting].some((e) => e && e.kind === 'sweep' && e.who === 'you');
    const can = this.canStart();
    const start = can
      ? h('button', { class: 'btn primary sp-go', type: 'button', text: 'Test every server', disabled: !!mineSweep,
        'aria-describedby': 'sp-sw-why', onclick: () => this.confirmSweep() })
      : null;
    const why = mineSweep ? 'Your full test is in the queue above.' : !can ? speedRefusal() : '';
    // A new reason is said once; the visible text next to the button stays.
    if (why && why !== this.lastWhy && this.lastWhy != null) this.host.say(why);
    this.lastWhy = why;
    const n = d ? /** @type {any} */ (d).sweep_servers || d.servers.filter((x) => !x.skipped).length || d.servers.length : 45;
    const mins = Math.max(1, Math.round(n * SWEEP_S / 60));
    const s = this.sweep;
    this.sweepEl.replaceChildren(...[
      h('h2', { id: 'sp-sw-h', text: 'Test every server now' }),
      h('p', { class: 'sp-lead', text: `One test on each of the ${n} servers, one after another, up to 10 seconds each way: about ${mins} minute${mins === 1 ? '' : 's'}. The results are a snapshot, kept apart from the hourly pattern.` }),
      h('div', { class: 'sp-go-row' }, start, why ? h('span', { id: 'sp-sw-why', class: 'muted small', text: why }) : null),
      this.note && this.el.hidden ? h('p', { class: 'sp-note', text: this.note }) : null,
      s && s.running && s.running.results ? this.liveNodes(s.running) : null,
      s && s.entry && s.tests.length ? this.resultNodes(s.entry, s.tests) : null,
      // A run that stopped early keeps its per-server list for the owner (cancelled, not reached), even with no results at all.
      s && !s.running && s.entry && s.entry.stopped_reason && s.entry.results && s.entry.results.length ? this.liveNodes(s.entry, true) : null,
    ].filter((x) => !!x));
  }

  /**
   * A full test under way (or, finished, one that stopped early): every
   * server with its state, in run order. Status is words and a mark, never
   * colour alone; nothing here is announced.
   * @param {QueueEntry} e @param {boolean} [finished]
   */
  liveNodes(e, finished = false) {
    const res = e.results || [];
    const done = res.filter((r) => r.status !== 'waiting' && r.status !== 'testing').length;
    const word = (/** @type {SweepResult} */ r) => {
      switch (r.status) {
        case 'ok': return `${fmtMbps(r.down_bps)} down, ${fmtMbps(r.up_bps)} up`;
        case 'testing': return 'testing now…';
        case 'busy': return 'server busy';
        case 'unreachable': return 'didn’t answer';
        case 'failed': return 'didn’t finish';
        case 'cancelled': return 'cancelled'; // the server whose test the cancel stopped mid-run
        case 'skipped': return e.stopped_reason === 'cancelled' ? 'not reached (test cancelled)' : 'not reached';
        default: return 'waiting';
      }
    };
    // A cancelled test is neutral, like one never reached: never ×.
    const mark = { ok: '✓', testing: '●', busy: '×', unreachable: '×', failed: '×', cancelled: '–', skipped: '–', waiting: '○' };
    return h('div', { class: 'sp-live' },
      // A run stopped before any result: say why here, since there are no results above.
      finished && !(e.tests || []).length && stoppedWhy(e) ? h('p', { class: 'sp-flag', text: stoppedWhy(e) }) : null,
      h('h3', { text: finished ? `Every server in this run (${res.filter((r) => r.status !== 'skipped' && r.status !== 'waiting').length} of ${res.length} reached)` : `${e.who === 'you' ? 'Your' : 'A'} full test: ${done} of ${res.length} servers done` }),
      h('ol', { class: 'sp-live-list' }, ...res.map((r) => h('li', { class: 'st-' + r.status },
        h('span', { class: 'sp-live-m', 'aria-hidden': 'true', text: mark[/** @type {'ok'} */ (r.status)] || '○' }),
        h('span', { class: 'sp-live-n', text: r.server.label }),
        h('span', { class: 'sp-live-v', text: word(r) })))));
  }

  /** The last full test: by region and provider, then every server, sortable. @param {QueueEntry} e @param {SpeedTest[]} tests */
  resultNodes(e, tests) {
    const d = this.host.summary();
    const servers = new Map((d ? d.servers : []).map((x) => [x.id, x]));
    // Each server's own test region; the default regions first, the rest under "Other places".
    const regions = d ? d.regions : [];
    const title = new Map(regions.map((rg) => [rg.id, rg.title]));
    const isDefault = new Set(regions.filter((rg) => rg.default).map((rg) => rg.title));
    const order = regions.map((rg) => rg.title);
    const regionOf = (/** @type {SpeedTest} */ t) => { const id = servers.get(t.server)?.region || t.region; return (id && title.get(id)) || 'Other places'; };
    const rows = tests.map((t) => ({ t, region: regionOf(t), provider: servers.get(t.server)?.provider || '' }));
    const med = (/** @type {number[]} */ v) => { const s = [...v].sort((a, b) => a - b); return s.length ? (s.length % 2 ? s[s.length >> 1] : (s[s.length / 2 - 1] + s[s.length / 2]) / 2) : null; };
    const groupBars = (/** @type {'region'|'provider'} */ key, /** @type {string} */ title) => {
      const by = new Map();
      for (const r of rows) if (r.t.outcome === 'ok' && r.t.down_bps != null) (by.get(r[key]) || by.set(r[key], []).get(r[key])).push(r.t.down_bps);
      const list = [...by.entries()].map(([k, v]) => ({ k, m: /** @type {number} */ (med(v)), n: v.length })).sort((a, b) => b.m - a.m);
      if (!list.length) return null;
      const max = Math.max(1, ...list.map((x) => x.m));
      const item = (/** @type {{k:string,m:number,n:number}} */ x) => {
        const bar = h('span', { class: 'sp-sw-bar', 'aria-hidden': 'true' });
        bar.style.width = `${Math.max(2, Math.round(x.m / max * 100))}%`;
        return h('li', {}, h('span', { class: 'sp-sw-k', text: x.k }), h('span', { class: 'sp-sw-track' }, bar), h('span', { class: 'sp-sw-v', text: `${fmtMbps(x.m)} · ${x.n} server${x.n === 1 ? '' : 's'}` }));
      };
      if (key === 'provider') return h('div', { class: 'sp-sw-bars' }, h('h3', { text: title }), h('ul', {}, ...list.map(item)));
      // Regions: the default ones in catalog order, then the other places.
      const main = list.filter((x) => isDefault.has(x.k)).sort((a, b) => order.indexOf(a.k) - order.indexOf(b.k));
      const rest = list.filter((x) => !isDefault.has(x.k));
      return h('div', { class: 'sp-sw-bars' }, h('h3', { text: title }), h('ul', {}, ...main.map(item)),
        // "Other places" as a heading only when there are default regions above it; on its own it's just a bar.
        rest.length && main.length ? h('h4', { text: 'Other places' }) : null, rest.length ? h('ul', {}, ...rest.map(item)) : null);
    };
    // A server with a download counts as a result, even when its upload didn't finish.
    const gave = rows.filter((r) => r.t.down_bps != null).length;
    const noUp = rows.filter((r) => r.t.down_bps != null && r.t.up_bps == null).length;
    const bars = [groupBars('region', 'Download by region (median)'), groupBars('provider', 'Download by provider (median)')].filter((x) => !!x);
    const cols = /** @type {[string,string,(r:SwRow)=>string|number|null][]} */ ([
      ['region', 'Region', (r) => r.region], ['server', 'Server', (r) => r.t.label],
      ['down', 'Down', (r) => r.t.down_bps], ['up', 'Up', (r) => r.t.up_bps], ['result', 'Result', (r) => resultWord(r.t)]]);
    const col = cols.find((c) => c[0] === this.sort.key) || cols[0];
    const sorted = [...rows].sort((a, b) => {
      const x = col[2](a), y = col[2](b);
      if (x == null && y == null) return 0;
      if (x == null) return 1;
      if (y == null) return -1;
      const c = typeof x === 'string' ? x.localeCompare(String(y)) || a.t.label.localeCompare(b.t.label) : x - /** @type {number} */ (y);
      return this.sort.asc ? c : -c;
    });
    return h('div', { class: 'sp-sw-res' },
      h('p', { class: 'sp-flag' }, h('b', { text: 'On-demand run' }), ` · finished ${new Date(e.finished_at || e.est_finish).toLocaleDateString([], { weekday: 'short', month: 'short', day: 'numeric' })} ${clock(e.finished_at || e.est_finish)} · ${gave ? `${gave} of ${rows.length} servers gave a result${noUp ? `; ${noUp} upload${noUp === 1 ? '' : 's'} didn’t finish` : ''}` : rows.length === 2 ? 'Neither server answered, so this run has no results' : `None of the ${rows.length} servers answered, so this run has no results`}.${stoppedWhy(e) ? ' ' + stoppedWhy(e) : ''} Not part of the hourly pattern.`),
      bars.length ? h('div', { class: 'sp-sw-grid' }, ...bars) : null,
      h('div', { class: 'sp-sum-wrap' }, h('table', { class: 'sp-st sp-sw-t' },
        h('caption', { class: 'sr-only', text: 'Every server in the last full test' }),
        h('thead', {}, h('tr', {}, ...cols.map(([k, l]) => h('th', { scope: 'col', class: k === 'down' || k === 'up' ? 'num' : '', 'aria-sort': this.sort.key === k ? (this.sort.asc ? 'ascending' : 'descending') : null },
          h('button', { type: 'button', class: 'sp-sort', text: l + (this.sort.key === k ? (this.sort.asc ? ' ▲' : ' ▼') : ''), onclick: () => {
            this.sort = { key: k, asc: this.sort.key === k ? !this.sort.asc : k !== 'down' && k !== 'up' };
            this.renderSweep();
            /** @type {HTMLElement|null} */ (this.sweepEl.querySelector('th[aria-sort] .sp-sort'))?.focus();
          } }))))),
        h('tbody', {}, ...sorted.map((r) => h('tr', { class: r.t.outcome === 'ok' ? '' : 'bad' },
          h('td', { 'data-k': 'Region', text: r.region }),
          h('th', { scope: 'row', text: r.t.label }),
          readingCell('Down', r.t.down_bps, lowerBound(r.t, 'down'), true),
          readingCell('Up', r.t.up_bps, lowerBound(r.t, 'up'), true),
          h('td', { 'data-k': 'Result', text: resultWord(r.t) })))))));
  }
}

/** @param {SpeedTest} t */
function resultWord(t) {
  if (t.outcome === 'busy') return 'Server busy';
  if (t.outcome === 'unreachable') return 'Didn’t answer';
  if (t.outcome === 'failed') return 'Didn’t finish';
  if (t.outcome === 'cancelled') return t.down_bps != null ? `Cancelled after download (${fmtMbps(t.down_bps)} down)` : 'Cancelled';
  return 'OK';
}
