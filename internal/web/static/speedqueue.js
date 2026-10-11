// Details → Speed, the queue: every test (Test now, a full test of every
// server, the schedule's slots) waits its turn in one queue and exactly one
// runs at a time (docs/SPEEDTEST-API.md, "Queue"). This shows where your own
// request is ("You're #2, starts in about 18 minutes", then its progress),
// what else is waiting, and the last full test's results. The owner can
// cancel any entry and clear the share link's. Plain ES module, no build step.
// @ts-check

import { h, fetchJSON, apiWrite, canWrite, dialog, MODE, zoneSuffix } from './core.js';
import { clock, when } from './home.js';
import { readingCell, lowerBound } from './speedmap.js';

/** @typedef {import('./speed.js').SpeedSummary} SpeedSummary */
/** @typedef {import('./speed.js').SpeedTest} SpeedTest */
/** @typedef {{id:number,kind:string,who:string,state:string,target:{id:string,label:string}|null,position:number,enqueued_at:number,started_at:number|null,finished_at:number|null,est_start:number,est_finish:number,step:number,steps:number,server:{id:string,label:string}|null,direction:string|null,tests:number[],origin?:string,results?:SweepResult[],wait_reason?:string|null,stopped_reason?:string|null}} QueueEntry */
/** @typedef {{server:{id:string,label:string},status:string,test?:number,down_bps?:number|null,up_bps?:number|null}} SweepResult */
/** @typedef {{at:number,max_waiting:number,full:boolean,sweeps_full:boolean,mine:number|null,running:QueueEntry|null,waiting:QueueEntry[],last_sweep:QueueEntry|null,public_enabled?:boolean,public_start?:boolean,speed_on?:boolean,budget?:{resets_at?:number|null,time_hour_s?:number,time_day_s?:number,time_hour_max_s?:number,time_day_max_s?:number,time_resets_at?:number|null,sweep_available_at?:number|null}}} QueueState */
/** @typedef {{t:SpeedTest,region:string,provider:string}} SwRow */
/** @typedef {{summary:()=>SpeedSummary|null, done:(e:QueueEntry, tag:string)=>void, say:(text:string)=>void, sweepHash:()=>boolean, started:()=>void}} QueueHost */

const POLL_MS = 3000;
const WHY = 'Running a speed test needs the admin token. Open the link fyisp printed when it started, or paste the token here.';

/** "in about 18 minutes", "in under a minute". */
function inAbout(/** @type {number} */ ms) {
  const m = Math.round(ms / 60e3);
  return m < 1 ? 'in under a minute' : `in about ${m} minute${m === 1 ? '' : 's'}`;
}
/** "Tests from this link are paused until 3:12 PM: they slow the line while they run." @param {number} at */
function pausedUntil(at) {
  const why = 'they slow the line while they run';
  return at ? `Tests from this link are paused until ${when(at)}: ${why}.` : `Tests from this link are paused for now: ${why}.`;
}
/** A full test from the link waits 30 minutes after the last one. @param {number} at */
function nextSweep(at) { return at ? `The next full test from this link can start at ${when(at)}.` : 'A full test from this link ran recently. Try again in a little while.'; }
// A full test: seconds per server, with the pause between tests. Each test runs up to 10 seconds each way.
const SWEEP_S = 26;
const TEST_S = 20;
/** "14 minutes", "1 minute". @param {number} s seconds */
function minWords(s) { const m = Math.max(1, Math.round(s / 60)); return `${m} minute${m === 1 ? '' : 's'}`; }


/** Why a full test ended early, or ''. @param {QueueEntry} e */
function stoppedWhy(e) {
  switch (e.stopped_reason) {
    case 'unreachable': return 'Stopped after 3 servers in a row didn’t answer. Check your connection.';
    case 'limit': return MODE === 'public' ? 'Stopped: today’s allowance for tests from this link ran out.' : 'Stopped: today’s share-link allowance ran out.';
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
/** Who asked, relative to this viewer; never an address. Locally, origin says where it came from. @param {QueueEntry} e */
function whoWord(e) {
  if (e.origin === 'public') return 'someone on your share link';
  if (e.who === 'you') return 'you';
  if (e.who === 'scheduled') return 'the schedule';
  return MODE === 'public' ? 'someone else' : 'someone on your share link';
}
/** Why a start or cancel failed, from the API's code. @param {any} e */
function queueError(e) {
  const b = e.body || {};
  const s = Number(b.retry_after) || Number(e.retryAfter) || 0;
  switch (b.code) {
    case 'queue_full': return 'The test queue is full right now. Try again later.';
    case 'speed_off': return 'Speed tests are off on this connection right now.';
    case 'skipped': return 'The owner skipped that server.';
    case 'rate_limited': return `You’ve asked for several tests recently. Try again ${s > 90 ? `in about ${Math.round(s / 60)} minutes` : s > 0 ? `in ${s} seconds` : 'in a little while'}.`;
    case 'time_cap': return pausedUntil(Number(b.resets_at) || (s ? Date.now() + s * 1000 : 0));
    case 'cooldown': case 'sweep_cooldown': return nextSweep(Number(b.resets_at || b.available_at) || (s ? Date.now() + s * 1000 : 0));
    case 'disabled': return 'The owner of this connection hasn’t turned on tests from this link.';
    case 'not_found': return 'That test isn’t in the queue any more.';
    case 'forbidden': return MODE === 'public' ? 'Tests can’t be started from this link.' : 'Speed tests can only be changed from the device running fyisp.';
  }
  if (e.status === 0) return ''; // the admin dialog was cancelled
  return 'Couldn’t reach fyisp. Try again.';
}

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

  /** Whether this viewer may start tests: the owner with controls, or the share link when the owner allows it. */
  canStart() {
    if (MODE === 'public') return !!this.q?.public_start;
    return canWrite();
  }

  /**
   * Adds a test to the queue. kind 'single' (id: a server, a region, or '' the
   * nearest) or 'sweep'; tag says what to do with the result.
   * @param {string} kind @param {string} id @param {string} tag
   */
  async start(kind, id, tag) {
    this.note = '';
    const q = new URLSearchParams({ kind });
    if (id) q.set('id', id);
    try {
      const r = MODE === 'public'
        ? await fetchJSON('api/speed/run?' + q, { method: 'POST' }, 1)
        : await apiWrite('POST', 'api/speed/run?' + q, undefined, WHY);
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
    const q = this.q;
    const servers = d ? d.servers : [];
    const n = servers.filter((x) => !x.skipped).length || 45;
    const skipped = servers.filter((x) => x.skipped).length;
    const mins = Math.max(1, Math.round(n * SWEEP_S / 60));
    const p = (/** @type {string} */ t) => h('p', { text: t });
    /** @type {Node[]} */
    const body = [p(`One test on each of the ${n} servers, one after another.${MODE === 'local' && skipped ? ` ${skipped === 1 ? '1 server you skipped won’t be tested.' : `${skipped} servers you skipped won’t be tested.`}` : ''}`)];
    body.push(p(`Takes about ${mins} minute${mins === 1 ? '' : 's'}: each test runs up to 10 seconds each way.`));
    if (MODE === 'public') {
      body.push(p('The owner’s line is busy most of that time, so their calls may stutter.'));
      // The share link's test time this hour (the server's caps; it stops the run when they're reached).
      const b = q && q.budget;
      if (b && b.time_hour_max_s) {
        const left = Math.max(0, Math.min(b.time_hour_max_s - (b.time_hour_s || 0), (b.time_day_max_s || Infinity) - (b.time_day_s || 0)));
        body.push(p(`Tests from this link can run for up to ${minWords(b.time_hour_max_s)} an hour; ${minWords(left)} ${left < 90 ? 'is' : 'are'} left now.`));
        const fit = Math.floor(left / TEST_S);
        if (fit < n) body.push(h('p', {}, h('b', { text: `So the run will stop partway (after about ${fit} of ${n} servers).` })));
      }
      body.push(p('If the owner needs the line, your sweep pauses between servers and carries on afterwards.'));
    } else {
      body.push(p('Your line is busy most of that time, so calls may stutter.'));
    }
    body.push(p('The results are a snapshot. They don’t change the hourly pattern.'));
    if (MODE === 'local' && d && !d.enabled) body.push(p('Speed tests are off. This runs once and doesn’t turn them on.'));
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

  async clearPublic() {
    try {
      const r = await apiWrite('POST', 'api/speed/cancel?public=1', undefined, 'Clearing the queue needs the admin token. Open the link fyisp printed when it started, or paste the token here.');
      this.host.say(`Cleared ${r.cancelled} request${r.cancelled === 1 ? '' : 's'} from your share link.`);
    } catch (e) { this.note = queueError(e); if (this.note) this.host.say(this.note); }
    await this.load();
  }

  /** Why the share link can't start a test now, or ''. */
  publicWhy() {
    const q = this.q;
    if (!q || q.public_start) {
      if (q?.budget?.sweep_available_at && q.budget.sweep_available_at > Date.now()) return nextSweep(q.budget.sweep_available_at);
      return q?.sweeps_full ? 'Three full tests are already waiting. Try again later.' : '';
    }
    if (q.speed_on === false) return 'Speed tests are off on this connection right now.';
    if (!q.public_enabled) return 'Only the owner of this connection can start tests here.';
    const b = q.budget;
    const timeCap = !!b && ((b.time_hour_s || 0) >= (b.time_hour_max_s || Infinity) || (b.time_day_s || 0) >= (b.time_day_max_s || Infinity));
    if (timeCap && b) return pausedUntil(b.time_resets_at || b.resets_at || 0);
    return 'The test queue is full right now. Try again later.';
  }

  /** The share link's test time: for the owner always, for visitors while tests are allowed. */
  budgetLine() {
    const q = this.q;
    if (!q || !q.budget || !q.public_enabled || !q.budget.time_hour_max_s) return null;
    const b = q.budget;
    // "8 s", "4 min": a single test is seconds, so minutes alone would read 0.
    const dur = (/** @type {number|undefined} */ x) => ((x || 0) < 60 ? `${Math.round(x || 0)} s` : `${Math.round((x || 0) / 60)} min`);
    return h('p', { class: 'muted small', text: `Share-link tests: ${dur(b.time_hour_s)} of ${dur(b.time_hour_max_s)} of testing this hour, ${dur(b.time_day_s)} of ${dur(b.time_day_max_s)} today.${MODE === 'local' ? ' Your own tests aren’t limited.' : ''}` });
  }

  /**
   * The owner's switch: let people with the share link queue tests (off by default).
   * Turning it off drops their waiting requests.
   * @param {boolean} on
   */
  async setPublic(on) {
    try {
      const r = await apiWrite('POST', 'api/speed/config', { public: on }, 'Changing who can start tests needs the admin token. Open the link fyisp printed when it started, or paste the token here.');
      const l = r && r.limits;
      const limits = l ? ` They share at most ${minWords(l.time_hour_max_s)} of testing an hour and ${minWords(l.time_day_max_s)} a day.` : '';
      this.note = on ? `People with your share link can now start tests.${limits}` : 'Tests from the share link are off. Their waiting requests were removed.';
      this.host.say(this.note);
    } catch (e) { this.note = queueError(e); if (this.note) this.host.say(this.note); }
    await this.load();
    /** @type {HTMLElement|null} */ (this.sweepEl.querySelector('#sp-pub'))?.focus();
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
    const owner = MODE === 'local' && canWrite();
    const all = q ? [q.running, ...q.waiting].filter((e) => !!e) : [];
    const me = this.mine();
    this.el.hidden = !all.length && !this.note;
    if (this.el.hidden) return;
    const rows = all.map((e) => {
      const e2 = /** @type {QueueEntry} */ (e);
      const when = e2.state === 'running'
        ? (e2.kind === 'sweep' ? `now, server ${e2.step} of ${e2.steps}` : 'now')
        : e2.kind === 'sweep' && e2.step > 0 ? `paused at server ${e2.step} of ${e2.steps}, carries on about ${clock(e2.est_start)}${zoneSuffix()}`
          : e2.wait_reason === 'cooldown' ? `waits for the 30-minute gap after the last full test, starts about ${clock(e2.est_start)}${zoneSuffix()}`
          : `#${e2.position}, starts about ${clock(e2.est_start)}${zoneSuffix()}`;
      return h('li', { class: (e2.state === 'running' ? 'run' : '') + (e2.who === 'you' ? ' you' : '') },
        h('span', { class: 'sp-q-what', text: what(e2) }),
        h('span', { class: 'sp-q-who', text: `asked by ${whoWord(e2)}` }),
        h('span', { class: 'sp-q-when', text: when }),
        owner ? h('button', { class: 'btn sm ghost', type: 'button', text: e2.state === 'running' ? 'Stop' : 'Cancel', 'aria-label': `${e2.state === 'running' ? 'Stop' : 'Cancel'}: ${what(e2)}`, onclick: () => this.cancel(e2.id) }) : '');
    });
    const someone = q ? q.waiting.some((e) => e.who === 'someone') : false;
    this.el.replaceChildren(...[
      me ? this.mineNodes(me) : null,
      h('div', { class: 'sp-sec-head' },
        h('h2', { id: 'sp-q-h', tabindex: '-1', text: 'Test queue' }),
        owner && someone ? h('button', { class: 'btn sm', type: 'button', text: 'Clear share-link requests', onclick: () => this.clearPublic() }) : ''),
      h('p', { class: 'muted small', text: 'One test runs at a time; the rest wait their turn.' }),
      rows.length ? h('ol', { class: 'sp-q' }, ...rows) : h('p', { class: 'muted', text: 'Nothing is waiting.' }),
      this.note ? h('p', { class: 'sp-note', text: this.note }) : null,
    ].filter((x) => !!x));
  }

  /** Your request: its place in the queue, then its steps. @param {QueueEntry} e */
  mineNodes(e) {
    if (e.state === 'waiting' && e.kind === 'sweep' && e.step > 0) {
      return h('div', { class: 'sp-mine' },
        h('p', { class: 'sp-mine-t', tabindex: '-1', text: `Testing every server: paused after server ${e.step} of ${e.steps} while another test runs. It carries on next.` }));
    }
    if (e.state === 'waiting' && e.wait_reason === 'cooldown') {
      return h('div', { class: 'sp-mine' },
        h('p', { class: 'sp-mine-t', tabindex: '-1', text: `${what(e)}: full tests from this link are 30 minutes apart, so yours starts ${inAbout(e.est_start - Date.now())}. Other tests can go first.` }));
    }
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
      ? h('button', { class: 'btn primary sp-go', type: 'button', text: 'Test every server', disabled: !!mineSweep || (MODE === 'public' && (!!q?.sweeps_full || (q?.budget?.sweep_available_at || 0) > Date.now())),
        'aria-describedby': 'sp-sw-why', onclick: () => this.confirmSweep() })
      : null;
    const why = mineSweep ? 'Your full test is in the queue above.' : MODE === 'public' ? this.publicWhy() : !can ? 'Speed tests can only be started from the device running fyisp.' : '';
    // A new reason is said once; the visible text next to the button stays.
    if (why && why !== this.lastWhy && this.lastWhy != null) this.host.say(why);
    this.lastWhy = why;
    const n = d ? d.servers.filter((x) => !x.skipped).length || d.servers.length : 45;
    const mins = Math.max(1, Math.round(n * SWEEP_S / 60));
    const s = this.sweep;
    this.sweepEl.replaceChildren(...[
      h('h2', { id: 'sp-sw-h', text: 'Test every server now' }),
      MODE === 'local' && q && q.public_enabled != null ? h('label', { class: 'sp-check sp-pub' },
        h('input', { type: 'checkbox', id: 'sp-pub', checked: !!q.public_enabled, disabled: !canWrite(),
          onchange: (/** @type {Event} */ ev) => this.setPublic(/** @type {HTMLInputElement} */ (ev.target).checked) }),
        h('span', {}, 'Let people with your share link start full tests',
          h('span', { class: 'sp-hint', text: `${q.budget && q.budget.time_hour_max_s ? `They share up to ${minWords(q.budget.time_hour_max_s)} of testing an hour. ` : ''}Off by default.` }))) : null,
      h('p', { class: 'sp-lead', text: `One test on each of the ${n} servers, one after another, up to 10 seconds each way: about ${mins} minute${mins === 1 ? '' : 's'}. The results are a snapshot, kept apart from the hourly pattern.` }),
      h('div', { class: 'sp-go-row' }, start, why ? h('span', { id: 'sp-sw-why', class: 'muted small', text: why }) : null),
      this.budgetLine(),
      MODE === 'local' && this.note && this.el.hidden ? h('p', { class: 'sp-note', text: this.note }) : null,
      s && s.running && s.running.results ? this.liveNodes(s.running) : null,
      s && s.entry && s.tests.length ? this.resultNodes(s.entry, s.tests) : null,
      // A run that stopped early keeps its per-server list for the owner (cancelled, not reached), even with no results at all.
      MODE === 'local' && s && !s.running && s.entry && s.entry.stopped_reason && s.entry.results && s.entry.results.length ? this.liveNodes(s.entry, true) : null,
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
        case 'ok': return `${mbit(r.down_bps)} down, ${mbit(r.up_bps)} up`;
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
        return h('li', {}, h('span', { class: 'sp-sw-k', text: x.k }), h('span', { class: 'sp-sw-track' }, bar), h('span', { class: 'sp-sw-v', text: `${mbit(x.m)} · ${x.n} server${x.n === 1 ? '' : 's'}` }));
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
      h('p', { class: 'sp-flag' }, h('b', { text: 'On-demand run' }), ` · finished ${new Date(e.finished_at || e.est_finish).toLocaleDateString([], { weekday: 'short', month: 'short', day: 'numeric' })} ${clock(e.finished_at || e.est_finish)}${zoneSuffix()} · ${gave ? `${gave} of ${rows.length} servers gave a result${noUp ? `; ${noUp} upload${noUp === 1 ? '' : 's'} didn’t finish` : ''}` : rows.length === 2 ? 'Neither server answered, so this run has no results' : `None of the ${rows.length} servers answered, so this run has no results`}.${stoppedWhy(e) ? ' ' + stoppedWhy(e) : ''} Not part of the hourly pattern.`),
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

/** "248 Mbit/s", or "—" when not measured (busy is never 0). @param {number|null|undefined} b */
function mbit(b) {
  if (b == null) return '—';
  const m = b / 1e6;
  return m >= 1000 ? `${(m / 1000).toFixed(1)} Gbit/s` : `${m < 10 ? m.toFixed(1) : Math.round(m)} Mbit/s`;
}
/** @param {SpeedTest} t */
function resultWord(t) {
  if (t.outcome === 'busy') return 'Server busy';
  if (t.outcome === 'unreachable') return 'Didn’t answer';
  if (t.outcome === 'failed') return 'Didn’t finish';
  if (t.outcome === 'cancelled') return t.down_bps != null ? `Cancelled after download (${mbit(t.down_bps)} down)` : 'Cancelled';
  return 'OK';
}
