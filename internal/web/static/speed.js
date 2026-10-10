// Details → Speed: scheduled speed tests (api/speed, docs/SPEEDTEST-API.md).
// Turning them on is a two-step page, not a wizard: what it costs (data, a
// busy line, calls that may stutter, who sees the address), then how to
// test. Once on, a header with the mode, the next test, Test now (with a
// server picker) and Turn off. Every write goes through the same gate as
// notes and share; the share link is read-only. Plain ES module, no build
// step.
// @ts-check

import { h, fetchJSON, apiWrite, canWrite, dialog, MODE } from './core.js';
import { clock } from './home.js';

/** @typedef {{id:string,label:string,provider:string,city:string,country:string,geo:string,region:string,rtt_ms:number|null,picked:boolean,custom:boolean}} SpeedServer */
/** @typedef {{hour:number,n:number,down_median_bps:number|null,up_median_bps:number|null,down_norm:number|null}} SpeedHour */
/** @typedef {{text:string,level:string,hours:number[],drop:number}} SpeedPattern */
/** @typedef {{id:string,label:string,latest_at:number|null,n:number,busy:number,unreachable:number,failed:number,fallback:number,down_median_bps:number|null,down_p10_bps:number|null,up_median_bps:number|null,up_p10_bps:number|null,best_down_bps:number|null,dip:SpeedPattern|null,hours:SpeedHour[],fallback_share_24h?:number,fallback_note?:string}} SpeedRow */
/** @typedef {{id:number,start:number,end:number,trigger:string,outcome:string,region:string|null,server:string,label:string,fallback:boolean,attempts:number,down_bps:number|null,up_bps:number|null,idle_gateway_ms:number|null,loaded_gateway_ms:number|null,idle_call_ms:number|null,loaded_call_ms:number|null}} SpeedTest */
/** @typedef {{started:number,expected_end:number,direction:string,server:{id:string,label:string}|null,region:string|null}} SpeedRunning */
/** @typedef {{at:number,tz:number,days:number,mode:string,modes:string[],enabled:boolean,configurable:boolean,set_by_flag?:boolean,interval_s:number,spacing_s:number,planned_share:number,warning:string|null,masked_share_24h:number,next_at:number|null,next:{region:string|null,server:string}|null,running:SpeedRunning|null,servers:SpeedServer[],regions:{id:string,title:string,chain:string[]}[],per_region:SpeedRow[],per_server:SpeedRow[],latest:SpeedTest|null,pattern:SpeedPattern|null}} SpeedSummary */

// Seconds a test fills the line in each direction (a 1 s ramp-up, then 3 to 8 s), and a whole test with its pauses.
const DIR_S = 6, TEST_S = 15;
const MONTH_S = 30 * 86400;

/** The mode's name and one sentence. "nearby" and "nearest" are the same radio, whichever the server offers. */
const MODES = {
  regions: { name: 'Around the world', rec: true },
  nearby: { name: 'Near me, more often' },
  nearest: { name: 'Near me, more often' },
  custom: { name: 'My own servers' },
};

/** "248 Mbit/s"; one decimal under 10; Gbit/s from 1000. */
export function mbps(/** @type {number|null|undefined} */ bps) {
  if (bps == null) return '—';
  const m = bps / 1e6;
  if (m >= 1000) return `${(m / 1000).toFixed(1)} Gbit/s`;
  return `${m < 10 ? m.toFixed(1) : Math.round(m)} Mbit/s`;
}
/** "105 MB", "680 GB", "2.1 TB". */
function bytes(/** @type {number} */ b) {
  if (b >= 1e12) return `${(b / 1e12).toFixed(1)} TB`;
  if (b >= 1e9) return `${b >= 1e11 ? Math.round(b / 1e10) * 10 : Math.round(b / 1e9)} GB`;
  return `${Math.max(1, Math.round(b / 1e6))} MB`;
}
/** "every 15 minutes", "once an hour". */
function every(/** @type {number} */ s) {
  if (s === 3600) return 'once an hour';
  if (s % 3600 === 0) return `every ${s / 3600} hours`;
  return `every ${Math.round(s / 60)} minutes`;
}

/** Why a speed-test write failed, from the API's stable code (never its wording). @param {any} e */
function speedError(e) {
  switch (e.body?.code) {
    case 'running': return 'A test is already running. Its result will appear here in a few seconds.';
    case 'forbidden': return 'Speed tests can only be changed from the device running fyisp.';
    case 'not_found': return 'That server isn’t in the list any more.';
    case 'bad_request': return 'fyisp didn’t accept that setting.';
  }
  if (e.status === 0) return ''; // the admin dialog was cancelled
  return 'Couldn’t reach fyisp. Try again.';
}

export class Speed {
  constructor() {
    /** @type {SpeedSummary|null} */
    this.d = null;
    this.err = '';
    this.seq = 0;
    /** @type {'idle'|'setup'} the switch-on panel, when on: Change… */
    this.panel = 'idle';
    this.pickMode = '';
    /** Bytes one test moved, from the estimate test or the latest ok test. @type {number|null} */
    this.perTest = null;
    this.estimating = false;
    /** The test this page started (Test now, Estimate): its start, the target's label, and what to say when it ends. */
    /** @type {{at:number,what:string,estimate:boolean}|null} */
    this.mine = null;
    this.result = '';
    this.resultLink = /** @type {Node|null} */ (null);
    this.note = '';
    this.lastStep = '';
    /** @type {number|undefined} */
    this.pollTimer = undefined;

    this.say = h('p', { class: 'sr-only', role: 'status' });
    this.headEl = h('div', { class: 'sp-head' });
    this.progEl = h('div', { class: 'sp-prog', hidden: true });
    this.bodyEl = h('div', { class: 'sp-body' });
    this.el = h('section', { id: 'speed', class: 'speed', 'aria-label': 'Speed tests', hidden: true },
      this.say, this.headEl, this.progEl, this.bodyEl);
  }

  /** Re-reads the summary (Details' refresh, every 30 s at most when idle). */
  async refresh() {
    const seq = ++this.seq;
    try {
      const d = /** @type {SpeedSummary} */ (await fetchJSON('api/speed?' + new URLSearchParams({ days: '7', tz: String(-new Date().getTimezoneOffset()) })));
      if (seq !== this.seq) return;
      this.d = d;
      this.err = '';
    } catch {
      if (seq !== this.seq) return;
      this.err = 'Couldn’t load the speed tests. They’ll show again when fyisp answers.';
    }
    if (this.d && this.d.running) this.poll();
    this.render();
  }

  /** While a test runs: the light part=running every 2 s, then the summary for the result. */
  poll() {
    if (this.pollTimer) return;
    const tick = async () => {
      this.pollTimer = undefined;
      if (this.el.hidden || !this.d) return;
      /** @type {SpeedRunning|null} */
      let r = null;
      try { r = (await fetchJSON('api/speed?part=running')).running; } catch { r = this.d.running; }
      this.d.running = r;
      if (r) {
        this.renderProgress();
        this.pollTimer = window.setTimeout(tick, 2000);
      } else {
        await this.refresh();
        this.finish();
      }
    };
    this.pollTimer = window.setTimeout(tick, 2000);
  }

  /** The test this page asked for has ended: say how it went. */
  finish() {
    const d = this.d, m = this.mine;
    if (!d || !m) { this.renderProgress(); return; }
    const t = d.latest;
    this.mine = null;
    this.lastStep = '';
    this.estimating = false;
    if (!t || t.start < m.at - 5000) { this.result = 'The test ended without a result. Try again.'; this.renderAll(); return; }
    if (t.outcome === 'ok' || t.down_bps != null) {
      this.perTest = testBytes(t);
      const row = d.per_server.find((x) => x.id === t.server);
      const pct = row && row.best_down_bps && t.down_bps != null ? ` ${Math.round(t.down_bps / row.best_down_bps * 100)}% of this server’s normal.` : '';
      const fb = t.fallback && t.region ? fallbackLine(d, t) : '';
      this.result = m.estimate
        ? `Your test used about ${bytes(this.perTest)}.`
        : `Done: ${mbps(t.down_bps)} down, ${t.up_bps != null ? mbps(t.up_bps).replace(' Mbit/s', '') : '—'} up.${pct}${fb ? ' ' + fb : ''}`;
    } else {
      this.result = t.outcome === 'busy' ? 'That server is busy with someone else’s test. We tried 3 ports.'
        : t.outcome === 'unreachable' ? 'That server didn’t answer.' : 'The test didn’t finish.';
    }
    this.say.textContent = this.result;
    this.renderAll();
  }

  render() { this.renderAll(); }

  renderAll() {
    this.renderHead();
    this.renderProgress();
    this.renderBody();
  }

  // ---------- header: off (the switch-on page) or on ----------

  renderHead() {
    const d = this.d;
    if (!d) {
      this.headEl.replaceChildren(h('p', { class: 'muted', text: this.err || 'Loading…' }));
      return;
    }
    if (MODE === 'public') {
      this.headEl.replaceChildren(h('p', { class: 'sp-state' }, d.enabled
        ? `Speed tests: on · ${modeName(d.mode)} · ${every(d.interval_s)}`
        : 'Speed tests are off on this connection.'));
      return;
    }
    if (!d.enabled || this.panel === 'setup') { fill(this.headEl, this.setupNodes()); return; }
    const flag = d.set_by_flag ? h('p', { class: 'muted small', text: 'Turned on by a start-up setting. If you turn it off here, it comes back on the next restart.' }) : null;
    const next = d.running ? 'testing now' : d.next_at ? `next test ${clock(d.next_at)}` : '';
    const can = canWrite();
    fill(this.headEl, [
      h('div', { class: 'sp-on' },
        h('p', { class: 'sp-state' }, h('b', { text: 'Speed tests: On' }), ` · ${modeName(d.mode)}${next ? ' · ' + next : ''}`),
        h('div', { class: 'sp-acts' },
          h('span', { class: 'sp-split' },
            h('button', { class: 'btn primary', type: 'button', text: 'Test now', disabled: !can || !!d.running, onclick: () => this.run('') }),
            h('button', { class: 'btn primary sp-more', type: 'button', 'aria-label': 'Choose a server to test', title: 'Choose a server', 'aria-haspopup': 'dialog', disabled: !can || !!d.running, text: '▾', onclick: () => this.pick() })),
          h('button', { class: 'btn', type: 'button', text: 'Change…', disabled: !can || !!d.set_by_flag, onclick: () => { this.panel = 'setup'; this.pickMode = d.mode; this.note = this.result = ''; this.renderAll(); /** @type {HTMLElement|null} */ (this.headEl.querySelector('h2'))?.focus(); } }),
          h('button', { class: 'btn ghost', type: 'button', text: 'Turn off', disabled: !can, onclick: () => this.setMode('off') }))),
      can ? null : h('p', { class: 'muted small', text: 'Speed tests can only be changed from the device running fyisp.' }),
      flag,
      d.warning ? h('p', { class: 'notice warn', text: d.warning }) : null,
      this.note ? h('p', { class: 'sp-note', text: this.note }) : null]);
  }

  /** The switch-on page (off), or Change… (on): what it costs, then how to test. */
  setupNodes() {
    const d = /** @type {SpeedSummary} */ (this.d);
    const on = d.enabled;
    const can = canWrite();
    const modes = d.modes.filter((m) => m !== 'off' && m in MODES);
    if (!this.pickMode || !modes.includes(this.pickMode)) this.pickMode = modes.includes('regions') ? 'regions' : modes[0] || '';
    const providers = [...new Set(d.servers.filter((s) => !s.custom).map((s) => s.provider))];
    const who = providers.length > 3 ? `${providers.slice(0, 3).join(', ')} and others` : providers.join(' and ');
    const est = this.perTest;
    const per10 = monthBytes(d, 'regions', 10e6 / 8 * DIR_S);
    const data = est != null
      ? `Your test used about ${bytes(est)}. ${modeName(this.pickMode)} would use about ${bytes(monthBytes(d, this.pickMode, est))} a month.`
      : `Uses a lot of data. About ${bytes(per10)} a month for every 10 Mbit/s of download plus upload: a 100/20 connection uses about ${bytes(per10 * 12)}.`;
    const tph = testsPerHour(d, this.pickMode);
    const busyMin = tph * TEST_S / 60;
    const radios = modes.map((m) => {
      const id = `sp-mode-${m}`;
      const gb = est != null ? `≈ ${bytes(monthBytes(d, m, est))} a month` : '';
      return h('div', { class: 'sp-radio' },
        h('input', { type: 'radio', name: 'sp-mode', id, value: m, checked: m === this.pickMode, disabled: !!d.set_by_flag, onchange: () => { this.pickMode = m; this.renderHead(); refocusRadio(m); } }),
        h('label', { for: id },
          h('b', { text: MODES[/** @type {'regions'} */ (m)].name + (MODES[/** @type {'regions'} */ (m)].rec ? ' (recommended)' : '') }),
          h('span', { class: 'sp-radio-text', text: modeSentence(d, m) }),
          gb ? h('span', { class: 'sp-gb', text: gb }) : null));
    });
    const go = h('button', { class: 'btn primary sp-go', type: 'button', text: on ? 'Save' : 'Turn on speed tests', disabled: !can || !this.pickMode || !!d.set_by_flag, onclick: () => this.setMode(this.pickMode) });
    return [
      h('h2', { tabindex: '-1', text: on ? 'Change how speed tests run' : 'Speed tests are off' }),
      on ? null : h('p', { class: 'sp-lead', text: 'Find out whether your connection slows down at certain times of day, and whether that’s your provider’s own network or its links to particular parts of the internet.' }),
      h('details', { class: 'sp-before', open: true },
        h('summary', { text: on ? 'What it costs' : 'Before you turn this on' }),
        h('ul', { class: 'sp-disc' },
          h('li', {}, h('span', { text: data }), ' ',
            can && !this.estimating ? h('button', { class: 'btn sm', type: 'button', text: est != null ? 'Estimate again' : 'Estimate for my connection', onclick: () => this.estimate() }) : null,
            this.estimating ? h('span', { class: 'muted', text: 'Testing (about 20 seconds)…' }) : null),
          h('li', { text: `Your line is busy about ${fmtMinutes(busyMin)} each hour, in ${Math.round(tph)} short bursts (about ${Math.max(1, Math.round(busyMin / 60 * 100))}% of the time).` }),
          h('li', { text: 'A call during a test may stutter for a few seconds. We don’t count those seconds against your connection.' }),
          h('li', { text: `Tests go to public test servers run by hosting companies (${who}). They see your internet address.` }),
          h('li', { text: 'Not for capped, mobile or hotspot connections. If this device uses a work VPN, the tests measure the VPN, not your home internet.' }))),
      h('fieldset', { class: 'sp-modes' }, h('legend', { text: 'How to test' }), ...radios),
      d.set_by_flag ? h('p', { class: 'muted small', text: 'Set by a start-up setting: change it where fyisp is started.' }) : null,
      h('div', { class: 'sp-go-row' }, go,
        on ? h('button', { class: 'btn ghost', type: 'button', text: 'Cancel', onclick: () => { this.panel = 'idle'; this.renderAll(); } })
          : h('span', { class: 'muted small', text: 'Nothing is tested until you turn this on.' })),
      can ? null : h('p', { class: 'muted small', text: 'Speed tests can only be changed from the device running fyisp.' }),
      this.note ? h('p', { class: 'sp-note', text: this.note }) : null,
      this.result && !this.mine ? h('p', { class: 'sp-result', text: this.result }) : null,
    ];
  }

  /** @param {string} mode */
  async setMode(mode) {
    const d = this.d;
    if (!d) return;
    const why = mode === 'off' ? 'Turning off speed tests needs the admin token. Open the link fyisp printed when it started, or paste the token here.'
      : 'Turning on speed tests needs the admin token. Open the link fyisp printed when it started, or paste the token here.';
    try {
      const r = await apiWrite('POST', 'api/speed/config', { mode }, why);
      d.mode = r.mode;
      d.enabled = r.mode !== 'off';
      d.next_at = r.next_at;
      this.panel = 'idle';
      this.result = '';
      this.note = mode === 'off' ? 'Off. Results so far are kept.' : `On: ${modeName(r.mode)}. The first test runs in about ${r.next_at ? fmtMinutes(Math.max(1, (r.next_at - Date.now()) / 60e3)) : 'a few minutes'}.`;
    } catch (e) {
      this.note = speedError(e);
    }
    this.say.textContent = this.note;
    this.renderAll();
    // The button that was pressed is gone: keep the keyboard on the page.
    /** @type {HTMLElement|null} */ (this.headEl.querySelector('.sp-acts .btn, .sp-go'))?.focus();
    this.refresh();
  }

  // ---------- Test now, the picker, progress ----------

  /** Starts a test: '' the nearest servers, else a server or region id. @param {string} id @param {string} [what] */
  async run(id, what = '', estimate = false) {
    const why = 'Running a speed test needs the admin token. Open the link fyisp printed when it started, or paste the token here.';
    this.result = '';
    this.note = '';
    try {
      await apiWrite('POST', 'api/speed/run' + (id ? '?' + new URLSearchParams({ id }) : ''), undefined, why);
      this.mine = { at: Date.now(), what, estimate };
      if (estimate) this.estimating = true;
    } catch (e) {
      const err = /** @type {any} */ (e);
      if (err.body?.code === 'running') this.note = speedError(e);
      else { this.note = speedError(e); this.estimating = false; this.say.textContent = this.note; this.renderAll(); return; }
    }
    // Show the progress at once; the poll fills it in.
    if (this.d && !this.d.running) this.d.running = { started: Date.now(), expected_end: Date.now() + 20e3, direction: 'picking', server: null, region: null };
    this.renderAll();
    this.poll();
    /** @type {HTMLElement|null} */ (this.progEl.querySelector('.sp-prog-title'))?.focus();
  }

  estimate() { this.run('', '', true); }

  /** The server picker: every catalog server by test region, plus "any server" per region; never hostnames or ports. */
  async pick() {
    const d = this.d;
    if (!d) return;
    const rows = new Map(d.per_server.map((r) => [r.id, r]));
    const inChain = new Set(d.regions.flatMap((r) => r.chain));
    const byId = new Map(d.servers.map((s) => [s.id, s]));
    /** @type {{title:string,items:{id:string,label:string,sub:string,find:string}[]}[]} */
    const groups = d.regions.map((rg) => ({
      title: rg.title,
      items: [{ id: rg.id, label: `Any ${rg.title} server`, sub: 'the first free one, as the schedule does', find: rg.title },
        ...rg.chain.map((id) => byId.get(id)).filter((s) => !!s).map((s) => serverItem(/** @type {SpeedServer} */ (s), rows.get(/** @type {SpeedServer} */ (s).id), rg.title))],
    }));
    const other = d.servers.filter((s) => !inChain.has(s.id));
    if (other.length) groups.push({ title: 'Other', items: other.map((s) => serverItem(s, rows.get(s.id), 'Other')) });
    const search = /** @type {HTMLInputElement} */ (h('input', { type: 'search', id: 'sp-find', placeholder: 'City, country or provider', autocomplete: 'off' }));
    const list = h('div', { class: 'sp-pick-list' });
    let first = true;
    for (const g of groups) {
      list.append(h('fieldset', { class: 'sp-pick-group' }, h('legend', { text: g.title }),
        ...g.items.map((it) => {
          const rid = `sp-pick-${it.id.replace(/[^A-Za-z0-9_-]/g, '_')}`;
          const input = h('input', { type: 'radio', name: 'sp-pick', id: rid, value: it.id, checked: first });
          first = false;
          return h('div', { class: 'sp-pick-row', 'data-find': it.find.toLowerCase() }, input,
            h('label', { for: rid }, h('span', { class: 'sp-pick-name', text: it.label }), it.sub ? h('span', { class: 'muted small', text: it.sub }) : null));
        })));
    }
    const none = h('p', { class: 'muted', text: 'No server matches.', hidden: true });
    search.addEventListener('input', () => {
      const q = search.value.trim().toLowerCase();
      let any = false;
      for (const g of /** @type {NodeListOf<HTMLElement>} */ (list.querySelectorAll('.sp-pick-group'))) {
        let shown = 0;
        for (const r of /** @type {NodeListOf<HTMLElement>} */ (g.querySelectorAll('.sp-pick-row'))) {
          r.hidden = !!q && !(r.dataset.find || '').includes(q);
          if (!r.hidden) shown++;
        }
        g.hidden = !shown;
        any = any || shown > 0;
      }
      none.hidden = any;
      // Keep the choice on a visible row.
      const sel = /** @type {HTMLInputElement|null} */ (list.querySelector('input:checked'));
      if (!sel || /** @type {HTMLElement} */ (sel.closest('.sp-pick-row')).hidden) {
        const v = /** @type {HTMLInputElement|null} */ (list.querySelector('.sp-pick-row:not([hidden]) input'));
        if (v) v.checked = true;
      }
    });
    const v = await dialog({
      title: 'Test one server',
      fields: [h('label', { class: 'fld', for: 'sp-find' }, h('span', { text: 'Find' }), search), list, none],
      buttons: [{ label: 'Cancel', value: 'cancel' }, { label: 'Test this server', value: 'ok', primary: true }],
      focus: search,
    });
    if (v !== 'ok') return;
    const sel = /** @type {HTMLInputElement|null} */ (list.querySelector('input:checked'));
    if (!sel) return;
    const name = byId.get(sel.value)?.label || d.regions.find((r) => r.id === sel.value)?.title || sel.value;
    this.run(sel.value, name);
  }

  renderProgress() {
    const d = this.d;
    const r = d && d.running;
    if (!r) {
      this.progEl.hidden = !(this.result && this.d?.enabled && this.panel !== 'setup');
      if (!this.progEl.hidden) {
        this.progEl.replaceChildren(h('p', { class: 'sp-result' }, this.result, ' ',
          /^That server/.test(this.result) && canWrite() ? h('button', { class: 'btn sm', type: 'button', text: 'Try another server', onclick: () => this.pick() }) : null));
      }
      return;
    }
    this.progEl.hidden = false;
    const step = r.direction === 'picking' || !r.server ? 'find' : r.direction === 'download' ? 'down' : 'up';
    const who = r.server ? r.server.label : this.mine?.what || '';
    const title = !this.mine ? `A test is running${who ? ` with ${who}` : ''}. Its result will appear here.`
      : who ? `Testing with ${who}…` : 'Testing…';
    const order = ['find', 'down', 'up'];
    const names = { find: 'Finding a free server', down: 'Download', up: 'Upload' };
    const at = order.indexOf(step);
    const total = Math.max(1, r.expected_end - r.started);
    const pct = Math.min(99, Math.max(2, Math.round((Date.now() - r.started) / total * 100)));
    fill(this.progEl, [
      h('p', { class: 'sp-prog-title', tabindex: '-1', text: title }),
      h('ol', { class: 'sp-steps' }, ...order.map((s, i) => h('li', { class: i < at ? 'done' : i === at ? 'now' : '' },
        h('span', { class: 'sp-step-mark', 'aria-hidden': 'true', text: i < at ? '✓' : i === at ? '●' : '○' }),
        names[/** @type {'find'} */ (s)],
        h('span', { class: 'sr-only', text: i < at ? ' (done)' : i === at ? ' (now)' : '' })))),
      h('progress', { class: 'sp-bar', max: '100', value: String(pct), 'aria-label': 'Test progress' }),
      h('p', { class: 'muted small', text: `About ${Math.max(5, Math.round((r.expected_end - Date.now()) / 1000))} seconds left. Calls may stutter briefly.` }),
      this.note ? h('p', { class: 'sp-note', text: this.note }) : null]);
    // Announce step changes only, for a test this page started.
    if (this.mine && step !== this.lastStep) {
      this.lastStep = step;
      this.say.textContent = step === 'find' ? title : `${names[step]}…`;
    }
  }

  // ---------- body (phase b fills in the heatmap and the rest) ----------

  renderBody() {
    const d = this.d;
    if (!d || (!d.enabled && !d.latest) || this.panel === 'setup') { this.bodyEl.replaceChildren(); return; }
    const t = d.latest;
    const lines = [];
    if (t) {
      const region = t.region ? d.regions.find((r) => r.id === t.region)?.title : '';
      const res = t.outcome === 'ok' ? `${mbps(t.down_bps)} down, ${mbps(t.up_bps)} up`
        : t.outcome === 'busy' ? 'server busy' : t.outcome === 'unreachable' ? 'didn’t answer' : 'didn’t finish';
      lines.push(h('p', {}, h('b', { text: 'Latest test: ' }), `${clock(t.start)} · ${[region, t.label].filter(Boolean).join(' · ')} · ${res}${t.trigger === 'manual' ? ' (manual)' : ''}`));
    }
    if (d.enabled && d.masked_share_24h > 0) {
      lines.push(h('p', { class: 'muted small', text: `Tests filled the line ${Math.max(1, Math.round(d.masked_share_24h * 100))}% of the last 24 hours. Those seconds aren’t counted on the other charts.` }));
    }
    this.bodyEl.replaceChildren(...lines);
  }
}

/** replaceChildren, skipping the parts that don't apply. @param {Element} el @param {(Node|null)[]} kids */
function fill(el, kids) { el.replaceChildren(.../** @type {Node[]} */ (kids.filter((k) => k))); }

/** @param {string} m */
function modeName(m) { return (MODES[/** @type {'regions'} */ (m)] || { name: m }).name; }

/** Tests per hour in mode m (each target once per interval). @param {SpeedSummary} d @param {string} m */
function testsPerHour(d, m) {
  const per = m === 'regions' ? d.regions.length || 9
    : m === 'nearest' ? 1 // the 2 nearest, alternating
      : m === 'nearby' ? 4
        : Math.max(1, d.servers.filter((s) => s.custom).length);
  return per * 3600 / (d.interval_s || 900);
}
/** Bytes a month in mode m, at perTest bytes a test. @param {SpeedSummary} d @param {string} m @param {number} perTest */
function monthBytes(d, m, perTest) { return testsPerHour(d, m) * (MONTH_S / 3600) * perTest; }
/** Bytes a test moved: both directions at their measured rates. @param {SpeedTest} t */
function testBytes(t) { return ((t.down_bps || 0) + (t.up_bps || 0)) / 8 * DIR_S; }

/** @param {SpeedSummary} d @param {string} m */
function modeSentence(d, m) {
  const ev = every(d.interval_s || 900);
  if (m === 'regions') return `${d.regions.length || 9} regions, each ${ev}. Best for telling your provider’s network apart from its links to other networks.`;
  if (m === 'nearest') return `The 2 nearest servers in turn, a test ${ev}.`;
  if (m === 'nearby') return `4 nearby servers, each ${ev}.`;
  return `iperf3 servers you run, given with --speedtest-server, each ${ev}.`;
}

/** "London was busy, so we used Manchester." @param {SpeedSummary} d @param {SpeedTest} t */
function fallbackLine(d, t) {
  const rg = d.regions.find((r) => r.id === t.region);
  const first = rg && d.servers.find((s) => s.id === rg.chain[0]);
  return first ? `${first.label} was busy, so we used ${t.label}.` : '';
}

/** @param {SpeedServer} s @param {SpeedRow|undefined} row @param {string} group */
function serverItem(s, row, group) {
  const bits = [];
  if (row && row.down_median_bps != null) bits.push(`usually ${mbps(row.down_median_bps)}`);
  if (row && row.latest_at) bits.push(`last ${clock(row.latest_at)}`);
  if (row && row.busy && !row.n) bits.push('busy last time');
  else if (row && row.unreachable && !row.n) bits.push('didn’t answer last time');
  return { id: s.id, label: s.label, sub: bits.join(' · '), find: `${s.label} ${s.provider} ${s.city} ${s.country} ${group}` };
}

/** "1½ minutes", "9 minutes". @param {number} m */
function fmtMinutes(m) {
  if (m < 1) return 'under a minute';
  const r = Math.round(m * 2) / 2;
  const whole = Math.floor(r), half = r - whole ? '½' : '';
  return `${whole || ''}${half} minute${r === 1 ? '' : 's'}`;
}

/** After a radio re-render, keep the keyboard on the chosen radio. @param {string} m */
function refocusRadio(m) { /** @type {HTMLElement|null} */ (document.getElementById(`sp-mode-${m}`))?.focus(); }
