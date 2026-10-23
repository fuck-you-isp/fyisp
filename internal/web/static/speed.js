// Details → Speed: scheduled speed tests (api/speed, docs/SPEEDTEST-API.md).
// Turning them on is a two-step page, not a wizard: what it costs (data, a
// busy line, calls that may stutter, who sees the address), then how to
// test. Once on, a header with the mode, the next test, Test now (with a
// server picker) and Turn off; tests wait their turn in one queue
// (speedqueue.js). Every write goes through the dashboard's write checks
// (apiWrite).
// Plain ES module, no build step.
// @ts-check

import { h, fetchShared, fmtMbps, apiWrite, dialog, tzParam, clock } from './core.js';
import { SpeedMap } from './speedmap.js';
import { SpeedQueue } from './speedqueue.js';
import { askPause, pauseWords, activePause } from './speedpause.js';

/** @typedef {{id:string,label:string,display?:string,provider:string,city:string,country:string,geo:string,area?:string,region:string|null,rtt_ms:number|null,picked:boolean,custom:boolean,capacity_mbps?:number,skipped?:boolean,skip_locked?:boolean,rest_until?:number|null}} SpeedServer */
/** @typedef {{hour:number,n:number,n_fallback:number,n_failed:number,n_capped:number,down_median_bps:number|null,up_median_bps:number|null,score_median:number|null,up_score_median:number|null,servers:Object<string,number>}} SpeedHour */
/** @typedef {{text:string,level:string,hours:number[],drop:number}} SpeedPattern */
/** @typedef {{id:string,kind:string,label:string,why?:string,note?:string,scheduled?:boolean,servers?:string[],server_now?:string,latest_at:number|null,n:number,busy:number,unreachable:number,failed:number,fallback:number,fallback_share?:number,fallback_share_24h?:number,capped_by_server?:number,down_median_bps:number|null,down_p10_bps:number|null,up_median_bps:number|null,up_p10_bps:number|null,best_down_bps:number|null,score_median?:number|null,learning?:boolean,dip:SpeedPattern|null,up_dip?:SpeedPattern|null,hours:SpeedHour[]}} SpeedRow */
/** @typedef {{n:number,lost:number,loss:number,p50_ms:number,p95_ms:number}} SpeedLat */
/** @typedef {{id:number,start:number,end:number,trigger:string,on_demand?:boolean,outcome:string,region:string|null,server:string,label:string,display?:string,provider?:string,city?:string,fallback:boolean,fallback_level?:string,attempts:number,down_bps:number|null,up_bps:number|null,down_s?:number,up_s?:number,capped_by_server?:boolean,score?:number|null,up_score?:number|null,bytes?:number,down_retrans:number|null,up_retrans:number|null,conn?:string,handoff?:{asn:number,name:string,isp_asn:number},latency?:{edge?:{idle:SpeedLat,down:SpeedLat,up:SpeedLat},gateway?:{idle:SpeedLat,down:SpeedLat,up:SpeedLat}},port?:number}} SpeedTest */
/** @typedef {{case:string,tier?:string,text:string,evidence:string,caveat?:string,hours:number[],drop:number,days:number,tests:number,regions:string[],providers:string[],network?:string,regions_judged?:number,providers_judged?:number,far_regions?:string[],slow_days?:number,day_count?:number,far_evening?:boolean,bridged?:boolean}} SpeedFinding */
/** @typedef {{entry?:number,kind?:string,started:number,direction:string,server:{id:string,label:string}|null,step?:number,steps?:number}} SpeedRunning */
/** @typedef {{at:number,tz:number,tz_name?:string,days:number,exclude_fallback:boolean,mode:string,modes:string[],enabled:boolean,set_by_flag?:boolean,interval_s:number,spacing_s:number,planned_share:number,warning:string|null,test_share_24h:number,next_at:number|null,next_row?:string|null,pause?:import('./speedpause.js').SpeedPauseState,running:SpeedRunning|null,plans?:{mode:string,tests_per_hour:number,share:number,est_gb_month:number}[],bytes_month?:number,bytes_24h?:number,connection?:{type:string,source:string},servers:SpeedServer[],rows:SpeedRow[],regions:{id:string,title:string,servers:string[],nearby?:string[],default?:boolean,providers?:string[]}[],per_region:SpeedRow[],per_server:SpeedRow[],latest:SpeedTest|null,pattern:SpeedPattern|null,finding?:SpeedFinding|null,under_load?:{text:string[]}|null}} SpeedSummary */

// Seconds a typical test fills the line in each direction (a ramp-up of up to 1 s, then 3 to 10 s), and a whole test with its pauses.
const DIR_S = 6, TEST_S = 15;
const MONTH_S = 30 * 86400;

/** The mode's name and one sentence. */
const MODES = {
  regions: { name: 'Around the world', def: true },
  nearby: { name: 'Near me, more often' },
  custom: { name: 'My own servers' },
};

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
    case 'forbidden': return 'fyisp refused that. Reload the page and try again.';
    case 'not_found': return 'That server isn’t in the list any more.';
    case 'skipped': return 'You skipped that server. Turn it back on under Every server to test it.';
    case 'bad_request': return 'fyisp didn’t accept that setting.';
  }
  if (e.status === 401) return ''; // the page reloads to sign in
  return 'Couldn’t reach fyisp. Try again.';
}

export class Speed {
  /** @param {import('./speedmap.js').SpeedHost} host */
  constructor(host) {
    this.host = host;
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
    this.result = '';
    this.note = '';

    this.say = h('p', { class: 'sr-only', role: 'status' });
    this.headEl = h('div', { class: 'sp-head' });
    this.queue = new SpeedQueue({
      summary: () => this.d,
      done: (e, tag) => this.finish(e, tag),
      say: (t) => { this.say.textContent = t; },
      // A new test or full test replaces the last result.
      started: () => { this.result = ''; this.renderProgress(); },
      sweepHash: () => host.sweep(),
    });
    this.progEl = h('div', { class: 'sp-prog', hidden: true });
    this.bodyEl = h('div', { class: 'sp-body' });
    this.map = new SpeedMap(host);
    this.el = h('section', { id: 'speed', class: 'speed', 'aria-label': 'Speed tests', hidden: true },
      this.say, this.headEl, this.progEl, this.queue.el, this.bodyEl, this.map.el, this.queue.sweepEl);
  }

  /** Re-reads the summary (Details' refresh, every 30 s at most when idle). */
  async refresh() {
    const seq = ++this.seq;
    try {
      const q = new URLSearchParams({ days: String(this.host.days()), tz: tzParam() });
      if (this.host.nofb()) q.set('exclude_fallback', '1');
      const d = /** @type {SpeedSummary} */ (await fetchShared('api/speed?' + q));
      if (seq !== this.seq) return;
      this.d = d;
      this.err = '';
    } catch {
      if (seq !== this.seq) return;
      this.err = 'Couldn’t load the speed tests. They’ll show again when fyisp answers.';
    }
    this.render();
    this.queue.load();
  }

  /**
   * A test this page queued has ended: say how it went. tag 'estimate' feeds the
   * data estimate; a sweep's results show in its own section.
   * @param {import('./speedqueue.js').QueueEntry} e @param {string} tag
   */
  async finish(e, tag) {
    if (tag === 'estimate') this.estimating = false;
    if (e.kind === 'sweep') return;
    if (e.state !== 'done') {
      this.result = e.state === 'dropped' ? 'The test waited too long in the queue and was dropped. Try again.' : 'The test was cancelled.';
      this.say.textContent = this.result;
      this.renderAll();
      return;
    }
    await this.refresh();
    const d = this.d;
    const t = d && d.latest && e.tests.includes(d.latest.id) ? d.latest : null;
    if (!d || !t) { this.result = 'Done. The result is in the tests below.'; this.say.textContent = this.result; this.renderAll(); return; }
    if (t.outcome === 'ok' || t.down_bps != null) {
      this.perTest = t.bytes || testBytes(t);
      const pct = t.score != null ? ` (${Math.round(t.score * 100)}% of normal)` : '';
      const fb = t.fallback && t.region ? fallbackLine(d, t) : '';
      const up = t.up_bps != null ? `, ${fmtMbps(t.up_bps)} up` : '';
      this.result = tag === 'estimate'
        ? `Your test used about ${bytes(this.perTest)}.`
        : `Done: ${t.label}, ${fmtMbps(t.down_bps)} down${up}${pct}.${t.up_bps == null ? ' The upload didn’t finish.' : ''}${fb ? ' ' + fb : ''}`;
    } else {
      this.result = t.outcome === 'cancelled' ? 'The test was cancelled.' : t.outcome === 'busy' ? 'That server is busy with someone else’s test. We tried 3 times.'
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
    if (!d.enabled || this.panel === 'setup') {
      fill(this.headEl, this.setupNodes());
      // Escape leaves Change… (not the switch-on page), back to the button that opened it.
      this.headEl.onkeydown = d.enabled ? (/** @type {KeyboardEvent} */ e) => { if (e.key === 'Escape') { e.preventDefault(); this.closeSetup(); } } : null;
      return;
    }
    this.headEl.onkeydown = null;
    const flag = d.set_by_flag ? h('p', { class: 'muted small', text: 'Turned on by a start-up setting. If you turn it off here, it comes back on the next restart.' }) : null;
    const paused = activePause(d.pause);
    const next = this.queue.q?.running || d.running ? 'testing now' : paused ? `scheduled tests ${pauseWords(paused)}` : d.next_at ? `next test ${clock(d.next_at)}` : '';
    fill(this.headEl, [
      h('div', { class: 'sp-on' },
        h('p', { class: 'sp-state' }, h('b', { text: 'Speed tests: On' }), ` · ${modeName(d.mode)}${next ? ' · ' + next : ''}`,
          d.bytes_month ? h('span', { class: 'muted small sp-used', text: ` · Data used: ${d.bytes_24h ? `${bytes(d.bytes_24h)} today, ` : ''}${bytes(d.bytes_month)} this month` }) : null),
        h('div', { class: 'sp-acts' },
          h('span', { class: 'sp-split' },
            h('button', { class: 'btn primary', type: 'button', text: 'Test now', onclick: () => this.run('') }),
            h('button', { class: 'btn primary sp-more', type: 'button', 'aria-label': 'Choose a server to test', title: 'Choose a server', 'aria-haspopup': 'dialog', text: '▾', onclick: () => this.pick() })),
          h('button', { class: 'btn', type: 'button', text: 'Change…', disabled: !!d.set_by_flag, onclick: () => { this.panel = 'setup'; this.pickMode = d.mode; this.note = this.result = ''; this.renderAll(); /** @type {HTMLElement|null} */ (this.headEl.querySelector('h2'))?.focus(); } }),
          paused
            ? h('button', { class: 'btn', type: 'button', text: 'Resume schedule', onclick: () => this.setPause(false) })
            : h('button', { class: 'btn', type: 'button', text: 'Pause schedule…', 'aria-haspopup': 'dialog', onclick: () => this.setPause(true) }),
          h('button', { class: 'btn ghost', type: 'button', text: 'Turn off', onclick: () => this.setMode('off') }))),
      flag,
      d.warning ? h('p', { class: 'notice warn', text: d.warning }) : null,
      this.note ? h('p', { class: 'sp-note', text: this.note }) : null]);
  }

  /** The switch-on page (off), or Change… (on): what it costs, then how to test. */
  setupNodes() {
    const d = /** @type {SpeedSummary} */ (this.d);
    const on = d.enabled;
    const modes = d.modes.filter((m) => m !== 'off' && m in MODES);
    if (!this.pickMode || !modes.includes(this.pickMode)) this.pickMode = modes.includes('regions') ? 'regions' : modes[0] || '';
    const providers = [...new Set(d.servers.filter((s) => !s.custom).map((s) => s.provider))];
    const who = providers.length > 3 ? `${providers.slice(0, 3).join(', ')} and others` : providers.join(' and ');
    const est = this.perTest;
    const data = est != null
      ? `Your test used about ${bytes(est)}. ${modeName(this.pickMode)} would use about ${bytes(monthBytes(d, this.pickMode, est))} a month.`
      : 'How much data this uses depends on your line’s speed; each test runs up to 10 seconds each way.';
    const tph = testsPerHour(d, this.pickMode);
    const plan = d.plans && d.plans.find((p) => p.mode === this.pickMode);
    const busyMin = plan ? plan.share * 60 : tph * TEST_S / 60;
    const radios = modes.map((m) => {
      const id = `sp-mode-${m}`;
      // The server's estimate for this line until an estimate test measured it.
      const plan = d.plans && d.plans.find((p) => p.mode === m);
      const gb = est != null ? `≈ ${bytes(monthBytes(d, m, est))} a month` : plan ? `≈ ${bytes(plan.est_gb_month * 1e9)} a month on this line` : '';
      return h('div', { class: 'sp-radio' },
        h('input', { type: 'radio', name: 'sp-mode', id, value: m, checked: m === this.pickMode, disabled: !!d.set_by_flag, onchange: () => { this.pickMode = m; this.renderHead(); refocusRadio(m); } }),
        h('label', { for: id },
          h('b', { text: MODES[/** @type {'regions'} */ (m)].name + (MODES[/** @type {'regions'} */ (m)].def ? ' (the default)' : '') }),
          h('span', { class: 'sp-radio-text', text: modeSentence(d, m) }),
          gb ? h('span', { class: 'sp-gb', text: gb }) : null));
    });
    const go = h('button', { class: 'btn primary sp-go', type: 'button', text: on ? 'Save' : 'Turn on speed tests', disabled: !this.pickMode || !!d.set_by_flag, onclick: () => this.setMode(this.pickMode) });
    return [
      h('h2', { tabindex: '-1', text: on ? 'Change how speed tests run' : 'Speed tests are off' }),
      on ? null : h('p', { class: 'sp-lead', text: 'Find out whether your connection slows down at certain times of day, and whether that’s your provider’s own network or its links to particular parts of the internet.' }),
      h('details', { class: 'sp-before', open: true },
        h('summary', { text: on ? 'What it costs' : 'Before you turn this on' }),
        h('ul', { class: 'sp-disc' },
          h('li', {}, h('span', { text: data }), ' ',
            !this.estimating ? h('button', { class: 'btn sm', type: 'button', text: est != null ? 'Estimate again' : 'Estimate for my connection', onclick: () => this.estimate() }) : null,
            this.estimating ? h('span', { class: 'muted', text: 'Testing (about 20 seconds)…' }) : null),
          h('li', { text: `Your line is busy about ${fmtMinutes(busyMin)} each hour, in ${Math.round(tph)} short bursts (about ${Math.max(1, Math.round(busyMin / 60 * 100))}% of the time).` }),
          h('li', { text: 'A call during a test may stutter for a few seconds. We don’t count those seconds against your connection.' }),
          h('li', { text: `Tests go to public test servers run by hosting companies (${who}). They see your internet address.` }),
          h('li', { text: 'Not for capped, mobile or hotspot connections. If this device uses a work VPN, the tests measure the VPN, not your home internet.' }))),
      h('fieldset', { class: 'sp-modes' }, h('legend', { text: 'How to test' }), ...radios),
      d.set_by_flag ? h('p', { class: 'muted small', text: 'Set by a start-up setting: change it where fyisp is started.' }) : null,
      h('div', { class: 'sp-go-row' }, go,
        on ? h('button', { class: 'btn ghost', type: 'button', text: 'Cancel', onclick: () => this.closeSetup() })
          : h('span', { class: 'muted small', text: 'Nothing is tested until you turn this on.' })),
      this.note ? h('p', { class: 'sp-note', text: this.note }) : null,
      this.result ? h('p', { class: 'sp-result', text: this.result }) : null,
    ];
  }

  /** Leaves Change… without saving; the keyboard goes back to Change…. */
  closeSetup() {
    this.panel = 'idle';
    this.renderAll();
    /** @type {HTMLElement|null} */ ([...this.headEl.querySelectorAll('button')].find((b) => b.textContent === 'Change…') || null)?.focus();
  }

  /** @param {string} mode */
  async setMode(mode) {
    const d = this.d;
    if (!d) return;
    try {
      const r = await apiWrite('POST', 'api/speed/config', { mode });
      d.mode = r.mode;
      d.enabled = r.mode !== 'off';
      d.next_at = r.next_at;
      this.panel = 'idle';
      this.result = '';
      const paused = activePause(d.pause);
      this.note = mode === 'off' ? 'Off. Results so far are kept.'
        : `On: ${modeName(r.mode)}. ${paused ? `Scheduled tests are ${pauseWords(paused)}.` : firstTest(r.next_at, false)}`;
    } catch (e) {
      this.note = speedError(e);
    }
    this.say.textContent = this.note;
    this.renderAll();
    // The button that was pressed is gone: keep the keyboard on the page.
    /** @type {HTMLElement|null} */ (this.headEl.querySelector('.sp-acts .btn, .sp-go'))?.focus();
    this.refresh();
  }

  /** Pauses (asking how long) or resumes the schedule. @param {boolean} on */
  async setPause(on) {
    const d = this.d;
    if (!d) return;
    const body = on ? await askPause() : { pause: false };
    if (!body) {
      /** @type {HTMLElement|null} */ ([...this.headEl.querySelectorAll('button')].find((b) => b.textContent === 'Pause schedule…') || null)?.focus();
      return;
    }
    try {
      const r = await apiWrite('POST', 'api/speed/config', body);
      d.pause = r.pause;
      d.next_at = r.next_at;
      this.note = r.pause.paused ? `Scheduled tests ${pauseWords(r.pause)}.` : `Scheduled tests resumed.${r.next_at ? ` Next test at about ${clock(r.next_at)}.` : ''}`;
    } catch (e) {
      this.note = speedError(e);
    }
    this.say.textContent = this.note;
    this.renderAll();
    await this.refresh();
    // The button that was pressed is gone: keep the keyboard on its replacement.
    /** @type {HTMLElement|null} */ ([...this.headEl.querySelectorAll('button')].find((b) => /schedule/.test(b.textContent || '')) || null)?.focus();
  }

  // ---------- Test now, the picker, progress ----------

  /** Queues a test: '' the nearest servers, else a server or region id. @param {string} id */
  async run(id, estimate = false) {
    this.result = '';
    this.note = '';
    if (estimate) { this.estimating = true; this.renderHead(); }
    const opener = document.activeElement && document.activeElement.textContent;
    await this.queue.start('single', id, estimate ? 'estimate' : 'test');
    if (estimate && this.queue.note) { this.estimating = false; this.renderHead(); }
    this.renderAll();
    // The re-render replaced the button that was pressed: keep the keyboard on the request's status, else on that button again.
    if (!this.el.contains(document.activeElement) || document.activeElement === document.body) {
      const to = /** @type {HTMLElement|null} */ (this.queue.el.querySelector('.sp-mine-t'))
        || [...this.headEl.querySelectorAll('button')].find((b) => b.textContent === opener) || null;
      to?.focus();
    }
  }

  estimate() { this.run('', true); }

  /** The server picker: every catalog server by test region, plus "any server" per region; never hostnames or ports. */
  async pick() {
    const d = this.d;
    if (!d) return;
    const rows = new Map(d.per_server.map((r) => [r.id, r]));
    const inChain = new Set(d.regions.flatMap((r) => r.servers));
    const byId = new Map(d.servers.map((s) => [s.id, s]));
    /** @type {{title:string,items:{id:string,label:string,sub:string,find:string}[]}[]} */
    const groups = d.regions.map((rg) => ({
      title: rg.title,
      items: [{ id: rg.id, label: `Any ${rg.title} server`, sub: 'the first free one, as the schedule does', find: rg.title },
        ...rg.servers.map((id) => byId.get(id)).filter((s) => !!s).map((s) => serverItem(/** @type {SpeedServer} */ (s), rows.get(/** @type {SpeedServer} */ (s).id), rg.title))],
    }));
    const other = d.servers.filter((s) => !inChain.has(s.id));
    // Servers outside the test regions, by continent.
    const areas = [...new Set(other.map((s) => s.area || 'Other'))];
    for (const a of areas) groups.push({ title: a === 'Other' ? 'Other' : `${a} (other cities)`, items: other.filter((s) => (s.area || 'Other') === a).map((s) => serverItem(s, rows.get(s.id), a)) });
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
    this.run(sel.value);
  }

  /** The last result of a test this page asked for. */
  renderProgress() {
    this.progEl.hidden = !(this.result && this.d?.enabled && this.panel !== 'setup');
    if (this.progEl.hidden) return;
    this.progEl.replaceChildren(h('p', { class: 'sp-result' }, this.result, ' ',
      /^That server/.test(this.result) ? h('button', { class: 'btn sm', type: 'button', text: 'Try another server', onclick: () => this.pick() }) : null));
  }

  // ---------- body: the latest test, then the evidence ----------

  renderBody() {
    const d = this.d;
    const evidence = !!d && !!d.latest && this.panel !== 'setup';
    this.map.el.hidden = !evidence;
    if (evidence) this.map.render(/** @type {SpeedSummary} */ (d));
    if (!d || (!d.enabled && !d.latest) || this.panel === 'setup') { this.bodyEl.replaceChildren(); this.bodyEl.hidden = true; return; }
    this.bodyEl.hidden = false;
    const t = d.latest;
    const lines = [];
    if (t) {
      const region = t.region ? d.regions.find((r) => r.id === t.region)?.title : '';
      const res = t.outcome === 'ok' ? `${fmtMbps(t.down_bps)} down, ${fmtMbps(t.up_bps)} up`
        : t.outcome === 'busy' ? 'server busy' : t.outcome === 'unreachable' ? 'didn’t answer' : t.outcome === 'cancelled' ? 'cancelled' : 'didn’t finish';
      lines.push(h('p', {}, h('b', { text: 'Latest test: ' }), `${clock(t.start)} · ${[region, t.label].filter(Boolean).join(' · ')} · ${res}${t.trigger !== 'schedule' ? ' (on demand)' : ''}`));
    }
    if (d.enabled && d.test_share_24h > 0) {
      lines.push(h('p', { class: 'muted small', text: `Tests filled the line ${Math.max(1, Math.round(d.test_share_24h * 100))}% of the last 24 hours.` }));
    }
    this.bodyEl.replaceChildren(...lines);
  }
}

/** When the first test after turning on runs, in whole minutes (never "½ minute"). @param {number|null|undefined} at @param {boolean} running */
function firstTest(at, running) {
  if (running) return 'The first test is running now.';
  if (!at) return 'The first test starts within the hour.';
  const m = Math.round((at - Date.now()) / 60e3);
  if ((at - Date.now()) < 60e3) return 'The first test starts in under a minute.';
  return `The first test starts at about ${clock(at)} (in ${m} minute${m === 1 ? '' : 's'}).`;
}

/** replaceChildren, skipping the parts that don't apply. @param {Element} el @param {(Node|null)[]} kids */
function fill(el, kids) { el.replaceChildren(.../** @type {Node[]} */ (kids.filter((k) => k))); }

/** @param {string} m */
function modeName(m) { return (MODES[/** @type {'regions'} */ (m)] || { name: m }).name; }

/** Tests per hour in mode m (each target once per interval). @param {SpeedSummary} d @param {string} m */
function testsPerHour(d, m) {
  const plan = d.plans && d.plans.find((p) => p.mode === m);
  if (plan) return plan.tests_per_hour;
  const per = m === 'regions' ? d.regions.filter((r) => r.default).length
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
  if (m === 'regions') {
    // The default regions; each is tested once per (count ÷ tests an hour) hours.
    const n = d.regions.filter((r) => r.default).length;
    const tph = testsPerHour(d, m);
    return `${n} regions, each ${tph ? every(Math.round(3600 * n / tph)) : ev}. Best for telling your provider’s network apart from its links to other networks.`;
  }
  if (m === 'nearby') return `Servers near you from several providers, more often: ${Math.round(testsPerHour(d, m))} tests an hour.`;
  return `iperf3 servers you run, given with --speedtest-server, each ${ev}.`;
}

/** "London was busy, so we used Manchester." @param {SpeedSummary} d @param {SpeedTest} t */
function fallbackLine(d, t) {
  const rg = d.regions.find((r) => r.id === t.region);
  const first = rg && d.servers.find((s) => s.id === rg.servers[0]);
  return first ? `${first.label} was busy, so we used ${t.label}.` : '';
}

/** @param {SpeedServer} s @param {SpeedRow|undefined} row @param {string} group */
function serverItem(s, row, group) {
  const bits = [];
  if (row && row.down_median_bps != null) bits.push(`usually ${fmtMbps(row.down_median_bps)}`);
  if (row && row.latest_at) bits.push(`last ${clock(row.latest_at)}`);
  if (s.skipped) bits.push('skipped');
  else if (s.rest_until && s.rest_until > Date.now()) bits.push(`resting until ${clock(s.rest_until)} (busy several times)`);
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
