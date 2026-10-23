// Details → Storage: storage tests against S3-compatible endpoints the
// owner adds (api/storage, docs/STORAGE-API.md). The header says what runs
// and holds the controls (a new campaign, one round now, pause, stop);
// then the new-campaign form or the live status, a campaign's results
// (the timeline grid, a round's attempts, the failures), the targets
// (storagetargets.js) and the past campaigns. Loaded the first time
// Storage is opened.
// Plain ES module, no build step.
// @ts-check

import { h, fetchJSON, apiWrite, dialog, clock } from './core.js';
import { StorageTargets } from './storagetargets.js';
import { CampaignForm, runOnce, enterKeys, campaignError, size } from './storageform.js';
import { StorageGrid } from './storagegrid.js';
import { FailureLog, historyTable, day } from './storagelog.js';
import { askPause, activePause, pauseWords } from './speedpause.js';
import { findingSection, cardsSection, tracesSection, reportSection } from './storageevidence.js';

/** @typedef {{sel:()=>number, setSel:(id:number)=>void, speedOn:()=>boolean}} StorageHost */

/** How often the live part is polled while a round runs, and otherwise while a campaign is live. */
const LIVE_RUNNING_MS = 3e3, LIVE_IDLE_MS = 10e3;

const DAYS = ['Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat', 'Sun'];

/** "in about 3 minutes". @param {number} ms */
function inAbout(ms) {
  const m = Math.round((ms - Date.now()) / 60e3);
  if (m <= 0) return 'now';
  if (m < 60) return `in about ${m} minute${m === 1 ? '' : 's'}`;
  const hrs = Math.round(m / 60);
  return `in about ${hrs} hour${hrs === 1 ? '' : 's'}`;
}

/** "weekdays 19:00–23:00". @param {any} sc */
function windowWords(sc) {
  const d = sc.days;
  const days = d.length === 7 ? 'every day' : d.join(',') === '1,2,3,4,5' ? 'weekdays' : d.join(',') === '6,7' ? 'weekends' : d.map((/** @type {number} */ x) => DAYS[x - 1]).join(', ');
  return `${days} ${sc.from}–${sc.to}`;
}

/**
 * Rebuilds el with fn, keeping the focus on the "same" element (a redraw
 * makes new ones): by its data-key, else by its class and label.
 * @param {HTMLElement} el @param {()=>void} fn
 */
function keepFocus(el, fn) {
  const a = /** @type {HTMLElement|null} */ (document.activeElement);
  const key = a && a !== el && el.contains(a) ? [a.tagName, a.className, a.textContent, a.dataset.key] : null;
  fn();
  if (!key || document.activeElement === a) return;
  for (const c of /** @type {NodeListOf<HTMLElement>} */ (el.querySelectorAll(key[0]))) {
    if (key[3] ? c.dataset.key === key[3] : c.className === key[1] && c.textContent === key[2]) { c.focus(); return; }
  }
}

/**
 * Puts kids in el in this order, leaving the ones already there where they
 * are: moving a node drops its focus.
 * @param {HTMLElement} el @param {(Node|null|undefined|false|'')[]} kids
 */
function place(el, kids) {
  const want = /** @type {Node[]} */ (kids.filter(Boolean));
  for (const c of [...el.childNodes]) if (!want.includes(c)) c.remove();
  want.forEach((k, i) => { if (el.childNodes[i] !== k) el.insertBefore(k, el.childNodes[i] || null); });
}

/** "every 5 min", or nothing for a single round. @param {any} c a campaign */
function everyWords(c) { return c.count === 1 ? '' : ` every ${c.interval_s / 60} min`; }

/** A dialog is open: nothing redraws under it. */
function dialogOpen() { return !!document.querySelector('dialog[open]'); }

/** Phase in words. */
const PHASE = /** @type {Object<string,string>} */ ({ seed: 'preparing test objects', cold: 'fresh connections', warm: 'a reused connection', down: 'downloading', up: 'uploading' });

export class Storage {
  /** @param {StorageHost} host */
  constructor(host) {
    this.host = host;
    /** @type {any} */
    this.d = null;
    this.err = '';
    this.seq = 0;
    this.rev = 0;
    this.liveAt = 0;
    /** @type {'idle'|'form'} */
    this.panel = 'idle';
    /** @type {CampaignForm|null} */
    this.form = null;
    /** @type {any} the speed summary's schedule state, while a campaign is live */
    this.speed = null;
    /** Notices dismissed (code:campaign). @type {Set<string>} */
    this.dismissed = new Set();
    /** The campaign shown in the results, and its data. */
    this.shown = 0;
    /** @type {any} */
    this.camp = null;
    this.campSeq = 0;
    /** What the results were drawn from (redrawn only when it changes). */
    this.drawn = '';
    /** What the failure log was loaded for. */
    this.logged = '';
    /** The rev and selection the campaign and the speed schedule were loaded for. */
    this.loadedFor = '';
    this.say = h('p', { class: 'sr-only', role: 'status' });
    this.headEl = h('div', { class: 'sp-head st-head' });
    this.errEl = h('p', { class: 'notice danger', role: 'alert' });
    this.notesEl = h('div', { class: 'st-notices' });
    this.panelEl = h('section', { class: 'st-panel sp-body', 'aria-labelledby': 'st-panel-h', hidden: true });
    this.grid = new StorageGrid({ say: (t) => { this.say.textContent = t; }, name: (id) => this.name(id) });
    this.log = new FailureLog({ name: (id) => this.name(id) });
    this.resEl = h('div', { class: 'st-res' });
    this.histEl = h('div');
    this.targets = new StorageTargets({
      summary: () => this.d,
      reload: () => this.load(),
      say: (t) => { this.say.textContent = t; },
      render: () => this.render(),
    });
    this.el = h('section', { id: 'storage', class: 'storage', 'aria-label': 'Storage tests', hidden: true },
      this.say, this.headEl, this.notesEl, this.panelEl, this.resEl, this.targets.el, this.histEl);
  }

  /** A target's name, from the summary or the campaign shown. @param {string} id */
  name(id) {
    return this.d?.targets.find((/** @type {any} */ t) => t.id === id)?.name
      || this.camp?.campaign.targets.find((/** @type {any} */ t) => t.id === id)?.name || id;
  }

  /** The live campaigns. @returns {any[]} */
  live() { return this.d?.live || []; }

  /**
   * Reads the summary, and (full, or when rev or the selection changed) the
   * campaign shown and, while a campaign is live, the speed schedule.
   */
  async load(full = true) {
    const seq = ++this.seq;
    try {
      const d = await fetchJSON('api/storage');
      if (seq !== this.seq) return;
      this.d = d;
      this.rev = d.rev;
      this.err = '';
    } catch (e) {
      if (seq !== this.seq) return;
      this.loadedFor = '';
      this.err = /** @type {any} */ (e).body?.code === 'storage_file'
        ? 'Storage tests are off: fyisp can’t use its storage-targets.json file (see fyisp’s log). Fix or move it and restart fyisp; fyisp never overwrites it.'
        : 'Couldn’t load the storage tests. They’ll show again when fyisp answers.';
      return;
    }
    const key = `${this.rev}:${this.host.sel()}`;
    if (!full && key === this.loadedFor) return;
    this.loadedFor = key;
    this.speed = null;
    if (this.live().length && this.host.speedOn()) {
      try { this.speed = await fetchJSON('api/speed'); } catch { /* shown again on the next refresh */ }
    }
    await this.loadCampaign();
  }

  /** The campaign the results show: the hash's, else the live one, else the latest. */
  async loadCampaign() {
    const seq = ++this.campSeq;
    const d = this.d;
    const latest = this.live()[0]?.id || d?.history?.[0]?.id || 0;
    let want = this.host.sel() || latest;
    if (!want) { this.shown = 0; this.camp = null; return; }
    try {
      const c = await fetchJSON('api/storage?' + new URLSearchParams({ part: 'campaign', id: String(want) }));
      if (seq !== this.campSeq) return;
      this.shown = want;
      this.camp = c;
    } catch (e) {
      if (seq !== this.campSeq) return;
      if (/** @type {any} */ (e).status === 404 && this.host.sel()) { // aged out: show the latest
        this.host.setSel(0);
        want = latest;
        if (want) return this.loadCampaign();
      }
      if (/** @type {any} */ (e).status === 404) { this.shown = 0; this.camp = null; }
      // else: keep what is shown until fyisp answers
    }
  }

  /** Details' refresh (on opening, after writes; every 15 s on screen, full: false, reads the campaign only when rev changed). */
  async refresh(full = true) {
    if (this.targets.editing != null || this.panel === 'form' || dialogOpen()) return; // don't redraw under a form or a dialog
    await this.load(full);
    this.render();
    this.reloadLog();
  }

  /** The failure log, again when its campaign or its round count changed (keeping how many it shows). */
  reloadLog() {
    const c = this.camp?.campaign;
    if (!c) return;
    const key = `${c.id}:${c.rounds_done}`;
    if (key === this.logged) return;
    this.logged = key;
    this.log.load(c.id, c.targets, (c.ends_at || Date.now()) - (c.start_at || c.created));
  }

  /** Every second (app.js): while a campaign is live and Storage is on screen, polls part=live and reloads when anything changed. */
  async tick() {
    if (this.el.hidden && this.panel === 'form') { this.closeForm(null, ''); return; } // left with a form open: its keys go
    if (this.el.hidden || document.hidden || !this.live().length || this.panel === 'form' || this.targets.editing != null || dialogOpen()) return;
    const running = this.live().some((l) => l.running || l.queue_position != null);
    if (Date.now() - this.liveAt < (running ? LIVE_RUNNING_MS : LIVE_IDLE_MS)) return;
    this.liveAt = Date.now();
    let l;
    try { l = await fetchJSON('api/storage?part=live'); } catch { return; }
    if (l.rev === this.rev) return;
    const before = this.live().map((x) => `${x.id}:${x.rounds_done}`).join(',');
    if (this.d) this.d.live = l.live;
    this.rev = l.rev;
    const after = l.live.map((/** @type {any} */ x) => `${x.id}:${x.rounds_done}`).join(',');
    if (before !== after) {
      await this.refresh();
      const lv = this.live()[0];
      if (lv) this.say.textContent = `Round ${lv.rounds_done} done.`;
    } else this.renderLive();
  }

  /** Only what a live poll changes: the header, the notices and the status. */
  renderLive() {
    if (!this.d || this.err) return;
    keepFocus(this.headEl, () => this.renderHead());
    keepFocus(this.notesEl, () => this.renderNotices());
    if (this.panel !== 'form') keepFocus(this.panelEl, () => this.renderStatus());
  }

  render() {
    if (this.errEl.textContent !== this.err) this.errEl.textContent = this.err;
    if (this.err) {
      if (!this.headEl.contains(this.errEl)) this.headEl.replaceChildren(h('h2', { text: 'Storage tests' }), this.errEl);
      this.targets.render();
      return;
    }
    const d = this.d;
    if (!d) { this.headEl.replaceChildren(h('h2', { text: 'Storage tests' }), h('p', { class: 'muted', text: 'Loading…' })); return; }
    this.renderLive();
    this.renderResults();
    this.targets.render();
    keepFocus(this.histEl, () => this.histEl.replaceChildren(historyTable(d.history || [], this.shown, (id) => this.showCampaign(id)) || ''));
  }

  renderHead() {
    const d = this.d;
    const n = d.targets.length;
    const editing = this.targets.editing != null || this.panel === 'form';
    if (!n) {
      this.headEl.replaceChildren(
        h('h2', { text: 'Storage tests' }),
        h('p', { class: 'sp-lead', text: 'Find out why uploads and downloads to your cloud storage fail sometimes: your line, the path to one endpoint, or the provider.' }),
        h('p', { class: 'sp-lead', text: 'Add the S3-compatible endpoints you use. A check sends one request now. A campaign tests them over time: fresh connections every few minutes, a download and an upload for targets you give keys, and every attempt stored.' }),
        h('ul', { class: 'sp-notes' },
          h('li', { text: 'Any S3-compatible endpoint: a cloud provider’s, or a server on your network.' }),
          h('li', {}, h('b', { text: 'Without a bucket or keys:' }), ' connection checks only. Almost no data.'),
          h('li', { text: 'Endpoints see your internet address, as with any request.' })),
        editing ? null : h('div', { class: 'sp-acts' },
          h('button', { type: 'button', class: 'btn primary sp-go st-add', text: 'Add a target', onclick: () => this.targets.open('') }),
          h('span', { class: 'muted small', text: 'Nothing is tested until you ask.' })),
        h('details', { class: 'sp-before' }, h('summary', { text: 'Where do I find the endpoint?' }),
          h('p', { text: 'Your storage provider lists it as the “S3 endpoint”, “S3 API URL” or “S3-compatible endpoint”, usually with a region. It looks like https://s3.<region>.<provider-domain>. For a MinIO or NAS on your network, use its address, e.g. http://192.168.1.20:9000.' })));
      return;
    }
    const od = this.live().find((l) => l.shape === 'on_demand');
    const rec = this.live().find((l) => l.shape === 'recurring');
    /** @type {(Node|string)[]} */
    const lines = [];
    /** @type {Node[]} */
    const acts = [];
    const btn = (/** @type {string} */ text, /** @type {()=>void} */ f, primary = false, cls = '') => h('button', { type: 'button', class: `btn${primary ? ' primary' : ''} ${cls}`.trim(), text, onclick: f });
    if (od) {
      const planned = od.count ? ` of ${od.count}` : '';
      if (od.state === 'scheduled') {
        lines.push(h('b', { text: 'Campaign scheduled' }), ` · starts ${day(od.start_at)} (${inAbout(od.start_at)}) · ${od.count ? `${od.count} round${od.count === 1 ? '' : 's'}` : 'until ' + day(od.ends_at)}${everyWords(od)}`);
        acts.push(btn('Cancel campaign…', () => this.stop(od, 'Cancel the campaign?', 'It doesn’t start, and its keys are forgotten now.', 'Cancel campaign')));
      } else {
        lines.push(h('b', { text: od.state === 'paused' ? 'Campaign paused' : 'Campaign running' }), ` · round ${od.rounds_done}${planned}`);
        if (od.state !== 'paused' && od.next_at) lines.push(` · next round at ${clock(od.next_at)} (${inAbout(od.next_at)})`);
        if (od.ends_at) lines.push(` · ends ${day(od.ends_at)}`);
        lines.push(` · used ${size(od.bytes_down + od.bytes_up)} so far`);
        acts.push(od.state === 'paused' ? btn('Resume', () => this.control(od, 'resume'), true, 'st-toggle') : btn('Pause', () => this.control(od, 'pause'), false, 'st-toggle'),
          btn('Stop campaign…', () => this.stop(od, 'Stop this campaign?', 'The round in progress is stopped and its keys are forgotten. Results so far are kept.', 'Stop campaign')));
      }
    }
    if (rec) {
      if (lines.length) lines.push(h('br'));
      const sc = rec.schedule;
      const until = sc.until ? `until ${day(sc.until)}` : 'until you remove it';
      lines.push(h('b', { text: rec.state === 'paused' ? 'Schedule paused' : rec.in_window ? 'Schedule running' : 'Schedule on' }), ` · ${windowWords(sc)} · every ${rec.interval_s / 60} min · ${until}`);
      if (rec.state !== 'paused' && rec.next_at) lines.push(` · next round ${day(rec.next_at)}`);
      acts.push(rec.state === 'paused' ? btn('Resume schedule', () => this.control(rec, 'resume'), false, 'st-toggle-rec') : btn('Pause schedule', () => this.control(rec, 'pause'), false, 'st-toggle-rec'),
        btn('Remove schedule…', () => this.stop(rec, 'Remove the schedule?', 'Its saved keys are deleted now. Results stay in the history.', 'Remove schedule')));
    }
    if (!od && !rec) {
      const last = d.history?.[0];
      lines.push(h('b', { text: 'Storage tests: Idle' }), ` · ${n} target${n === 1 ? '' : 's'}${last ? ` · last campaign ended ${day(last.ends_at || last.created)}` : ''}`);
    }
    if (!od && !editing) {
      acts.unshift(btn('New campaign…', () => this.openForm(), true, 'st-new'), btn('Run one round now…', () => this.runNow(), false, 'st-run'));
    }
    this.headEl.replaceChildren(h('div', { class: 'sp-on' }, h('p', { class: 'sp-state' }, ...lines), h('div', { class: 'sp-acts' }, ...acts)));
  }

  renderNotices() {
    /** @type {Node[]} */
    const out = [];
    for (const l of this.live()) {
      if (!l.waiting_keys.length) continue;
      const names = l.waiting_keys.map((/** @type {string} */ id) => this.name(id)).join(', ');
      const why = l.shape === 'recurring' ? 'a change to a target’s settings, or Forget keys, deleted them'
        : 'fyisp doesn’t keep an on-demand campaign’s keys across a restart, and a change to a target’s settings, or Forget keys, deletes them';
      out.push(h('div', { class: 'notice danger st-wait' }, h('b', { text: 'Waiting for keys. ' }),
        `${names} ${l.waiting_keys.length === 1 ? 'has' : 'have'} no keys (${why}), so ${l.waiting_keys.length === 1 ? 'it gets' : 'they get'} connection checks only. `,
        h('button', { type: 'button', class: 'btn primary sm', text: 'Enter keys…', onclick: () => this.keysFor(l) })));
    }
    for (const n of this.d.notices || []) {
      const k = `${n.code}:${n.campaign}`;
      if (n.code !== 'schedule_ended' || this.dismissed.has(k)) continue;
      out.push(h('div', { class: 'notice st-ended' }, `The schedule ended on ${day(n.at)}. Its keys were deleted. `,
        h('button', { type: 'button', class: 'btn sm ghost', text: 'Dismiss', onclick: () => { this.dismissed.add(k); this.renderNotices(); } })));
    }
    const sp = this.speed;
    if (this.live().length && sp && sp.enabled) {
      const p = activePause(sp.pause);
      if (p) out.push(h('p', { class: 'muted small st-speed', tabindex: '-1', text: `Scheduled speed tests are ${pauseWords(p)}.` }));
      else out.push(h('div', { class: 'notice st-speed' }, 'Scheduled speed tests share the line with this campaign: they fill it for up to 25 s at a time, which can slow or fail a round. ',
        h('button', { type: 'button', class: 'btn sm', text: 'Pause scheduled speed tests…', onclick: () => this.pauseSpeed() }), ' ',
        h('a', { href: '#v=speed', text: 'Speed page' })));
    }
    this.notesEl.replaceChildren(...out);
  }

  renderStatus() {
    const l = this.live().find((x) => x.running || x.queue_position != null || (x.next_at && x.state !== 'paused'));
    if (!l) { this.panelEl.hidden = true; this.panelEl.replaceChildren(); return; }
    this.panelEl.hidden = false;
    let line;
    if (l.running) line = `Round ${l.rounds_done + 1} is running: ${PHASE[l.phase] || 'starting'}${l.target ? ` · ${this.name(l.target)}` : ''}.`;
    else if (l.queue_position != null) line = `The next round waits its turn in the test queue (position ${l.queue_position}).`;
    else line = `Next round at ${clock(l.next_at)} (${inAbout(l.next_at)}).`;
    this.panelEl.replaceChildren(h('h2', { id: 'st-panel-h', class: 'sp-mine-t', tabindex: '-1', text: l.shape === 'recurring' ? 'Schedule' : 'Campaign' }),
      h('p', { text: line }),
      h('p', { class: 'muted small', text: `${l.targets.map((/** @type {any} */ t) => `${t.name} (${t.mode === 'bucket' ? 'with keys' : 'connection checks'})`).join(', ')} · ${l.cold} fresh, ${l.warm} reused${(l.down_bytes || l.up_bytes) && l.targets.some((/** @type {any} */ t) => t.mode === 'bucket') ? `, ${size(l.down_bytes)} down and ${size(l.up_bytes)} up` : ''} per round.` }));
  }

  renderResults() {
    const c = this.camp;
    if (!c) {
      this.resEl.replaceChildren(this.d.targets.length ? h('p', { class: 'muted st-nores', text: 'No results yet. Start a campaign to see failures and speeds over time, or run one round now to check that everything works.' }) : '');
      return;
    }
    const k = c.campaign;
    const when = k.start_at || k.created;
    const title = `${k.shape === 'recurring' ? `Schedule · ${windowWords(k.schedule)}` : 'On demand'} · ${day(when)} · ${k.rounds_done} round${k.rounds_done === 1 ? '' : 's'}${everyWords(k)} · ${k.targets.length} target${k.targets.length === 1 ? '' : 's'} · ${size(k.bytes_down)} down, ${size(k.bytes_up)} up`;
    const latest = this.live()[0]?.id || this.d.history?.[0]?.id;
    // Redrawn only when the campaign's data changed (the grid keeps its focus and scroll otherwise).
    const key = `${k.id}:${c.rounds.length}:${k.rounds_done}:${k.state}:${latest}:${(c.traces || []).length}`;
    if (key === this.drawn && this.resEl.firstChild) return;
    this.drawn = key;
    this.grid.set(c);
    place(this.resEl, [
      h('div', { class: 'st-res-head' }, h('h2', { id: 'st-res-h', tabindex: '-1', text: 'Results' }), h('p', { class: 'muted', text: title }),
        latest && latest !== this.shown ? h('button', { type: 'button', class: 'btn sm', text: 'Show the latest campaign', onclick: () => this.showCampaign(0) }) : null),
      c.finding ? findingSection(c.finding) : null,
      c.cards ? cardsSection(c.cards, (id) => this.name(id), k.targets) : null,
      this.grid.el, this.grid.detail, this.log.el,
      tracesSection(c.traces || [], (id) => this.name(id)),
      this.report(k.id)]);
  }

  /** The report panel of campaign id, kept across redraws (open, ticked). @param {number} id */
  report(id) {
    if (this.reportFor !== id) { this.reportFor = id; this.reportEl = reportSection(id); }
    return /** @type {HTMLElement} */ (this.reportEl);
  }

  /** Shows a past campaign (0: the latest). @param {number} id */
  async showCampaign(id) {
    this.host.setSel(id);
    await this.loadCampaign();
    this.render();
    this.reloadLog();
    /** @type {HTMLElement|null} */ (this.el.querySelector('#st-res-h'))?.focus();
    if (this.camp) this.say.textContent = `Showing the campaign of ${day(this.camp.campaign.start_at || this.camp.campaign.created)}.`;
  }

  openForm() {
    this.panel = 'form';
    this.form = new CampaignForm(this.d.targets, {
      scheduleExists: this.live().some((l) => l.shape === 'recurring'),
      done: (c, words) => this.closeForm(c, words),
    });
    this.panelEl.hidden = false;
    // Escape anywhere in it, the heading included, cancels (asking first when something was typed).
    this.panelEl.replaceChildren(h('div', { onkeydown: (/** @type {KeyboardEvent} */ e) => { if (e.key === 'Escape' && !e.defaultPrevented) { e.preventDefault(); this.form?.cancel(); } } },
      h('h2', { id: 'st-panel-h', tabindex: '-1', text: 'New campaign' }), this.form.el));
    this.renderHead();
    /** @type {HTMLElement} */ (this.panelEl.querySelector('h2')).focus();
  }

  /** @param {any} c the campaign started, or null @param {string} words */
  async closeForm(c, words) {
    this.form?.keys.clear();
    this.form = null;
    this.panel = 'idle';
    this.panelEl.replaceChildren();
    if (c) { this.host.setSel(0); this.say.textContent = words; }
    await this.refresh();
    if (c) /** @type {HTMLElement|null} */ (this.panelEl.querySelector('h2') || this.headEl.querySelector('.st-toggle, .st-toggle-rec'))?.focus();
    else /** @type {HTMLElement|null} */ (this.headEl.querySelector('.st-new'))?.focus();
  }

  async runNow() {
    const c = await runOnce(this.d.targets);
    this.drawn = '';
    if (c) {
      this.host.setSel(0);
      this.say.textContent = 'One round queued: it runs when the test queue is free.';
      await this.refresh();
      /** @type {HTMLElement|null} */ (this.panelEl.querySelector('h2'))?.focus();
    } else /** @type {HTMLElement|null} */ (this.headEl.querySelector('.st-run'))?.focus();
  }

  /** Pause or resume. @param {any} l @param {'pause'|'resume'} what */
  async control(l, what) {
    try {
      await apiWrite('POST', `api/storage/campaign/${what}?` + new URLSearchParams({ id: String(l.id) }));
      this.say.textContent = what === 'pause'
        ? (l.shape === 'recurring' ? 'Paused. No rounds until you resume, even inside the window.' : l.count ? 'Paused. Rounds due meanwhile are skipped.' : 'Paused. The campaign still ends at its time.')
        : 'Resumed.';
    } catch (e) { this.say.textContent = campaignError(e); }
    await this.refresh();
    /** @type {HTMLElement|null} */ (this.headEl.querySelector(l.shape === 'recurring' ? '.st-toggle-rec' : '.st-toggle'))?.focus();
  }

  /** Stops (or cancels, or removes) a campaign after asking. @param {any} l @param {string} title @param {string} body @param {string} ok */
  async stop(l, title, body, ok) {
    const v = await dialog({ title, body: [h('p', { text: body })], buttons: [{ label: 'Keep it', value: 'cancel' }, { label: ok, value: 'ok', primary: true }], safe: true });
    if (v !== 'ok') { this.refocus(title.startsWith('Remove') ? 'Remove schedule…' : title.startsWith('Cancel') ? 'Cancel campaign…' : 'Stop campaign…'); return; }
    try {
      await apiWrite('POST', 'api/storage/campaign/stop?' + new URLSearchParams({ id: String(l.id) }));
      this.say.textContent = l.shape === 'recurring' ? 'Schedule removed. Its keys were deleted.' : `Campaign stopped after ${l.rounds_done} round${l.rounds_done === 1 ? '' : 's'}. Keys forgotten.`;
    } catch (e) { this.say.textContent = campaignError(e); }
    await this.refresh();
    // New campaign…, else (another campaign is still live) the first control left.
    /** @type {HTMLElement|null} */ (this.headEl.querySelector('.st-new') || this.headEl.querySelector('button') || this.resEl.querySelector('#st-res-h'))?.focus();
  }

  /** Focuses the header or notice button with this label. @param {string} label */
  refocus(label) {
    for (const b of /** @type {NodeListOf<HTMLElement>} */ (this.el.querySelectorAll('.st-head button, .st-notices button'))) {
      if (b.textContent === label) { b.focus(); return; }
    }
  }

  /** A target of the summary. @param {string} id */
  target(id) { return this.d?.targets.find((/** @type {any} */ t) => t.id === id); }

  /** @param {any} l */
  async keysFor(l) {
    const ok = await enterKeys(l.id, l.waiting_keys.map((/** @type {string} */ id) => ({ id, name: this.name(id), scheme: this.target(id)?.scheme || 'https' })), l.shape === 'recurring');
    if (!ok) { this.refocus('Enter keys…'); return; }
    this.say.textContent = 'Keys accepted. Transfers resume with the next round.';
    await this.refresh();
    /** @type {HTMLElement|null} */ (this.panelEl.querySelector('h2'))?.focus();
  }

  async pauseSpeed() {
    const body = await askPause('Pause them while this campaign runs, so rounds and speed tests don’t compete for the line.');
    if (!body) { this.refocus('Pause scheduled speed tests…'); return; }
    try {
      const r = await apiWrite('POST', 'api/speed/config', body);
      this.say.textContent = r.pause?.paused ? `Scheduled speed tests ${pauseWords(r.pause)}.` : 'Scheduled speed tests paused.';
    } catch { this.say.textContent = 'Couldn’t pause the speed tests. Try again.'; }
    await this.refresh();
    // The notice that replaced the button, else the button again.
    /** @type {HTMLElement|null} */ (this.notesEl.querySelector('p.st-speed'))?.focus();
    if (!this.notesEl.contains(document.activeElement)) this.refocus('Pause scheduled speed tests…');
  }
}
