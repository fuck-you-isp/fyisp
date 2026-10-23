// Details → Storage: the new-campaign form (POST api/storage/campaign/start)
// and Run one round now (POST api/storage/run). Targets, on demand (now or
// at a time, a number of rounds or a while) or recurring (weekdays, a
// window, until a date), what each round tests, keys for the bucket
// targets (storagekeys.js), and the data estimate (GET part=estimate).
// Plain ES module, no build step.
// @ts-check

import { h, apiWrite, fetchJSON, dialog, toLocalInput, fromLocalInput, clock } from './core.js';
import { KeyFields } from './storagekeys.js';

/** @typedef {import('./storagetargets.js').StorageTarget} StorageTarget */

/** Transfer sizes offered (bytes). */
const SIZES = [0, 1, 4, 16, 64, 256, 1024].map((m) => m << 20);
const DAYS = ['Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat', 'Sun'];

/** "64 MiB", "1.5 GiB", "30 KiB": binary units (object sizes are typed in them). @param {number} b */
export function size(b) {
  if (!b) return '0 bytes';
  if (b >= 1 << 30) return `${(b / (1 << 30)).toFixed(1).replace(/\.0$/, '')} GiB`;
  if (b >= 1 << 20) return `${Math.round(b / (1 << 20))} MiB`;
  return `${Math.max(1, Math.round(b / 1024))} KiB`;
}

/** The viewer's IANA zone. */
export function zone() {
  try { return Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC'; } catch { return 'UTC'; }
}

/** The API's field names, in words. */
const FIELDS = /** @type {Object<string,string>} */ ({
  targets: 'the targets', interval_s: 'the interval', cold: 'the number of fresh connections', warm: 'the number of reused requests',
  down_bytes: 'the download size', up_bytes: 'the upload size', start_at: 'the start time (at most 30 days ahead)', count: 'the number of rounds',
  duration_s: 'the duration (at most 7 days, and at least one interval)', days: 'the days', from: 'the window', tz: 'this computer’s time zone',
  until: 'the end date', keys: 'the keys', shape: 'the kind of campaign',
});

/** Why a campaign write failed, from the API's code. @param {any} e */
export function campaignError(e) {
  switch (e.body?.code) {
    case 'forbidden': return 'fyisp refused that. Reload the page and try again.';
    case 'not_found': return 'That campaign isn’t live any more.';
    case 'active_campaign': return 'Another campaign is live, or the schedule’s window covers that time. Stop it first, or pick another time.';
    case 'schedule_exists': return 'You already have a schedule. Remove it first.';
    case 'bad_state': return 'The campaign changed meanwhile. Look at its state and try again.';
    case 'not_waiting': return 'Those targets aren’t waiting for keys any more.';
    case 'keys_refused': return 'The keys were refused.';
    case 'interval_too_short': return `A round takes longer than that: choose every ${Math.ceil((e.body.min_interval_s || 60) / 60)} minutes or more, or fewer requests and smaller sizes.`;
    case 'bad_request': return FIELDS[e.body.field] ? `fyisp didn’t accept ${FIELDS[e.body.field]}.` : 'fyisp didn’t accept that setting.';
    case 'storage_file': return 'Storage tests are off: fyisp can’t use its storage-targets.json file.';
    case 'busy': return 'Keys are being tried for another request. Try again in a few seconds.';
  }
  if (e.status === 401) return '';
  if (e.body?.code) return 'fyisp refused that. Reload the page and try again.';
  return 'Couldn’t reach fyisp. Try again.';
}

/** A labelled number input. @param {string} id @param {string} label @param {number} value @param {number} min @param {number} max */
function num(id, label, value, min, max) {
  const input = /** @type {HTMLInputElement} */ (h('input', { type: 'number', id, value: String(value), min: String(min), max: String(max), step: '1', inputmode: 'numeric' }));
  return { input, el: h('div', { class: 'st-fld st-num' }, h('label', { for: id, text: label }), input) };
}

/** A transfer size select. @param {string} id @param {string} label @param {number} value */
function sizeSelect(id, label, value) {
  const sel = /** @type {HTMLSelectElement} */ (h('select', { id }, ...SIZES.map((b) => h('option', { value: String(b), text: b ? size(b) : 'None', selected: b === value }))));
  return { sel, el: h('div', { class: 'st-fld' }, h('label', { for: id, text: label }), sel) };
}

/** A radio with its label. @param {string} name @param {string} value @param {string} label @param {boolean} checked */
function radio(name, value, label, checked, extra = '') {
  const id = `${name}-${value}`;
  return h('div', { class: 'sp-radio' }, h('input', { type: 'radio', name, id, value, checked }),
    h('label', { for: id }, h('b', { text: label }), extra ? h('span', { class: 'muted small', text: ' ' + extra }) : null));
}

export class CampaignForm {
  /**
   * @param {StorageTarget[]} targets
   * @param {{scheduleExists:boolean, done:(c:any|null, words:string)=>void}} host
   */
  constructor(targets, host) {
    this.targets = targets;
    this.host = host;
    this.busy = false;
    this.estSeq = 0;
    /** @type {ReturnType<typeof setTimeout>|undefined} */
    this.estTimer = undefined;
    this.err = h('p', { class: 'notice danger', role: 'alert', tabindex: '-1', hidden: true });
    this.est = h('div', { class: 'st-est', 'aria-live': 'polite' });
    this.picks = targets.map((t) => /** @type {HTMLInputElement} */ (h('input', { type: 'checkbox', id: `st-c-t-${t.id}`, value: t.id, checked: true, onchange: () => this.changed(true) })));
    this.cold = num('st-c-cold', 'Fresh connections per target', 10, 0, 20);
    this.warm = num('st-c-warm', 'Requests on a reused connection', 5, 0, 10);
    this.down = sizeSelect('st-c-down', 'Download size', 64 << 20);
    this.up = sizeSelect('st-c-up', 'Upload size', 16 << 20);
    this.every = num('st-c-every', 'Every (minutes)', 5, 1, 1440);
    this.count = num('st-c-count', 'Rounds', 24, 1, 1000);
    this.hours = num('st-c-hours', 'Hours', 2, 0, 168);
    this.mins = num('st-c-mins', 'Minutes', 0, 0, 59);
    this.at = /** @type {HTMLInputElement} */ (h('input', { type: 'datetime-local', id: 'st-c-at', value: toLocalInput(Date.now() + 3600e3) }));
    this.from = /** @type {HTMLInputElement} */ (h('input', { type: 'time', id: 'st-c-from', value: '19:00' }));
    this.to = /** @type {HTMLInputElement} */ (h('input', { type: 'time', id: 'st-c-to', value: '23:00' }));
    this.until = /** @type {HTMLInputElement} */ (h('input', { type: 'date', id: 'st-c-until', value: new Date(Date.now() + 30 * 86400e3).toISOString().slice(0, 10) }));
    /** @type {boolean[]} */
    this.days = [true, true, true, true, true, false, false];
    this.dayBtns = DAYS.map((d, i) => h('button', { type: 'button', 'aria-pressed': String(this.days[i]), text: d, onclick: () => { this.days[i] = !this.days[i]; this.drawDays(); this.changed(); } }));
    this.keys = new KeyFields([], { optional: true });
    this.keysWrap = h('div');
    this.el = this.build();
    this.redrawKeys();
    this.changed();
  }

  drawDays() { this.dayBtns.forEach((b, i) => b.setAttribute('aria-pressed', String(this.days[i]))); }

  /** @param {string} name */
  pick(name) { return /** @type {HTMLInputElement|null} */ (this.el.querySelector(`input[name="${name}"]:checked`))?.value || ''; }

  build() {
    const tz = zone();
    const targets = h('fieldset', { class: 'st-tpick' }, h('legend', { text: 'Targets' }),
      ...this.targets.map((t, i) => h('div', { class: 'st-check-row' }, this.picks[i],
        h('label', { for: this.picks[i].id }, h('b', { text: ` ${t.name}` }),
          h('span', { class: 'muted small', text: ` ${t.bucket ? `bucket ${t.bucket}` : 'connection only'}${t.network === 'private' ? ' · on your network' : ''}` })))));
    const shape = h('fieldset', { class: 'sp-modes', onchange: () => this.changed() }, h('legend', { text: 'Kind' }),
      radio('st-c-shape', 'on_demand', 'On demand', true, 'A number of rounds, or for a while.'),
      radio('st-c-shape', 'recurring', 'Recurring', false, this.host.scheduleExists ? 'You already have a schedule. Remove it first.' : 'Inside a time window on chosen days, until a date or until you remove it.'));
    if (this.host.scheduleExists) /** @type {HTMLInputElement} */ (shape.querySelector('#st-c-shape-recurring')).disabled = true;
    this.onDemand = h('div', { class: 'st-od' },
      h('fieldset', { class: 'sp-modes', onchange: () => this.changed() }, h('legend', { text: 'When' }),
        radio('st-c-when', 'now', 'Start now', true), radio('st-c-when', 'at', 'Start at', false),
        h('div', { class: 'st-fld' }, h('label', { for: 'st-c-at', text: 'Start time' }), this.at)),
      h('fieldset', { class: 'sp-modes', onchange: () => this.changed() }, h('legend', { text: 'How long' }),
        radio('st-c-long', 'count', 'A number of rounds', true), this.count.el,
        radio('st-c-long', 'duration', 'For a while', false), h('div', { class: 'st-pair' }, this.hours.el, this.mins.el)));
    this.recurring = h('div', { class: 'st-rec', hidden: true },
      h('fieldset', { class: 'st-days' }, h('legend', { text: 'Days' }),
        h('div', { class: 'seg', role: 'group', 'aria-label': 'Days' }, ...this.dayBtns),
        h('div', { class: 'st-quick' },
          h('button', { type: 'button', class: 'btn sm ghost', text: 'Weekdays', onclick: () => { this.days = [true, true, true, true, true, false, false]; this.drawDays(); this.changed(); } }),
          h('button', { type: 'button', class: 'btn sm ghost', text: 'Every day', onclick: () => { this.days = this.days.map(() => true); this.drawDays(); this.changed(); } }))),
      h('div', { class: 'st-pair', oninput: () => this.changed() },
        h('div', { class: 'st-fld' }, h('label', { for: 'st-c-from', text: 'From' }), this.from),
        h('div', { class: 'st-fld' }, h('label', { for: 'st-c-to', text: 'To' }), this.to)),
      h('p', { class: 'muted small st-over', text: `Times are this computer’s local time (${tz}).` }),
      h('fieldset', { class: 'sp-modes', onchange: () => this.changed() }, h('legend', { text: 'Until' }),
        radio('st-c-until', 'date', 'A date', true), h('div', { class: 'st-fld' }, h('label', { for: 'st-c-until', text: 'Last day' }), this.until),
        radio('st-c-until', 'none', 'Until I remove it', false)));
    const what = h('fieldset', { class: 'st-what', oninput: () => this.changed() }, h('legend', { text: 'Each round' }),
      h('div', { class: 'st-pair' }, this.cold.el, this.warm.el),
      h('div', { class: 'st-pair' }, this.down.el, this.up.el),
      h('p', { class: 'muted small', text: 'Fresh connections are a second apart, each with its own name lookup and handshake. Downloads and uploads run only for targets you give keys; uploads are deleted at once.' }));
    this.everyHelp = h('p', { class: 'sp-note st-err', id: 'st-c-every-err', hidden: true });
    this.every.input.setAttribute('aria-describedby', 'st-c-every-err');
    const every = h('div', { oninput: () => this.changed() }, this.every.el, this.everyHelp);
    this.start = h('button', { type: 'submit', class: 'btn primary sp-go', text: 'Start campaign' });
    return h('form', { class: 'st-form st-cform', novalidate: true, onsubmit: (/** @type {Event} */ e) => { e.preventDefault(); this.submit(); } },
      targets, shape, this.onDemand, this.recurring, every, what, this.keysWrap, this.est, this.err,
      h('div', { class: 'sp-acts' }, this.start, h('button', { type: 'button', class: 'btn ghost', text: 'Cancel', onclick: () => this.host.done(null, '') }),
        h('span', { class: 'muted small', text: 'Nothing runs until you start it.' })));
  }

  /** The chosen targets. */
  chosen() { return this.picks.filter((p) => p.checked).map((p) => p.value); }

  redrawKeys() {
    const bucket = this.targets.filter((t) => t.bucket && this.chosen().includes(t.id));
    const rec = this.pick('st-c-shape') === 'recurring';
    const old = this.keys;
    this.keys = new KeyFields(bucket.map((t) => ({ id: t.id, name: t.name, scheme: t.scheme })), { optional: true, onchange: () => this.changed() });
    this.keys.carry(old);
    const untilNone = rec && this.pick('st-c-until') === 'none';
    this.keysWrap.replaceChildren(!bucket.length ? '' : h('fieldset', { class: 'st-keysfs' }, h('legend', { text: 'Keys for this campaign (optional)' }),
      h('p', { class: 'muted small', text: rec
        ? `Saved on this computer (in fyisp’s data folder, readable only by fyisp’s user) ${untilNone ? 'until you remove the schedule' : 'until the schedule ends'}, then deleted. Never shown again.`
        : 'Kept in memory only, for this campaign. Never saved and never shown again; forgotten when it ends, is stopped, or fyisp restarts.' }),
      untilNone ? h('p', { class: 'notice', text: 'With no end date the keys stay saved until you remove the schedule. Consider temporary credentials or an end date.' }) : null,
      this.keys.el,
      h('p', { class: 'muted small', text: 'A target left without keys gets connection checks only. Give keys that may only list, get, put and delete under fyisp-test/ in a test bucket.' })));
  }

  /** The spec as the API takes it (keys apart). @returns {Object<string, any>} */
  spec() {
    const rec = this.pick('st-c-shape') === 'recurring';
    const n = (/** @type {{input:HTMLInputElement}} */ f) => Math.round(Number(f.input.value) || 0);
    /** @type {Object<string, any>} */
    const s = { targets: this.chosen(), shape: rec ? 'recurring' : 'on_demand', interval_s: n(this.every) * 60, cold: n(this.cold), warm: n(this.warm),
      down_bytes: Number(this.down.sel.value), up_bytes: Number(this.up.sel.value) };
    if (!rec) {
      if (this.pick('st-c-when') === 'at') s.start_at = fromLocalInput(this.at.value);
      if (this.pick('st-c-long') === 'count') s.count = n(this.count);
      else s.duration_s = (n(this.hours) * 60 + n(this.mins)) * 60;
    } else {
      s.days = this.days.map((on, i) => (on ? i + 1 : 0)).filter(Boolean);
      s.from = this.from.value;
      s.to = this.to.value;
      s.tz = zone();
      if (this.pick('st-c-until') === 'date' && this.until.value) s.until = new Date(`${this.until.value}T23:59:59`).getTime();
    }
    return s;
  }

  /** Something changed: the shape's fields, the keys (targets), the estimate. @param {boolean} [targets] */
  changed(targets = false) {
    const rec = this.pick('st-c-shape') === 'recurring';
    this.onDemand.hidden = rec;
    this.recurring.hidden = !rec;
    this.start.textContent = rec ? 'Start schedule' : 'Start campaign';
    if (targets || this.lastShape !== rec + this.pick('st-c-until')) {
      this.lastShape = rec + this.pick('st-c-until');
      this.redrawKeys();
    }
    const over = /** @type {HTMLElement} */ (this.recurring.querySelector('.st-over'));
    if (rec && this.to.value && this.from.value && this.to.value <= this.from.value) {
      over.textContent = `Runs overnight, ending at ${this.to.value} the next day. Times are this computer’s local time (${zone()}).`;
    } else over.textContent = `Times are this computer’s local time (${zone()}).`;
    clearTimeout(this.estTimer);
    this.estTimer = setTimeout(() => this.estimate(), 250);
  }

  async estimate() {
    const s = this.spec();
    const seq = ++this.estSeq;
    if (!s.targets.length || !s.interval_s) { this.est.replaceChildren(); return; }
    const q = new URLSearchParams({ part: 'estimate', targets: s.targets.join(','), shape: s.shape, interval_s: String(s.interval_s),
      cold: String(s.cold), warm: String(s.warm), down_bytes: String(s.down_bytes), up_bytes: String(s.up_bytes) });
    const keyed = this.keys.keyed();
    if (keyed.length) q.set('keyed', keyed.join(','));
    if (s.count) q.set('count', String(s.count));
    if (s.duration_s) q.set('duration_s', String(s.duration_s));
    if (s.shape === 'recurring') { q.set('days', s.days.join(',') || '1'); q.set('from', s.from || '19:00'); q.set('to', s.to || '23:00'); }
    let e;
    try { e = await fetchJSON('api/storage?' + q); } catch { if (seq === this.estSeq) this.est.replaceChildren(); return; }
    if (seq !== this.estSeq) return;
    const lines = [h('p', {}, h('b', { text: 'Each round: ' }), `${size(e.per_round.down)} down and ${size(e.per_round.up)} up${keyed.length ? ` (${keyed.length} target${keyed.length === 1 ? '' : 's'} with keys)` : ' (connection checks only)'}; about ${Math.max(1, Math.round(e.round_s_est))} s.`)];
    if (e.total) lines.push(h('p', { text: `This campaign: ${e.rounds} rounds, about ${size(e.total.down)} down and ${size(e.total.up)} up.` }));
    if (e.per_window) lines.push(h('p', { text: `Each window: ${e.rounds} rounds, about ${size(e.per_window.down)} down and ${size(e.per_window.up)} up. The next 30 days: about ${size(e.per_30d.down)} down and ${size(e.per_30d.up)} up.` }));
    const short = s.interval_s < e.min_interval_s;
    this.everyHelp.hidden = !short;
    this.everyHelp.textContent = short ? `A round takes about ${Math.round(e.round_s_est)} s with these targets and sizes: choose every ${Math.ceil(e.min_interval_s / 60)} minutes or more, or fewer requests and smaller sizes.` : '';
    if (short) this.every.input.setAttribute('aria-invalid', 'true'); else this.every.input.removeAttribute('aria-invalid');
    this.est.replaceChildren(h('h3', { text: 'Data' }), ...lines);
  }

  /** @param {string} msg */
  fail(msg) {
    this.err.textContent = msg;
    this.err.hidden = !msg;
    if (msg) this.err.focus();
  }

  async submit() {
    if (this.busy) return;
    this.fail('');
    const s = this.spec();
    if (!s.targets.length) { this.fail('Pick at least one target.'); return; }
    if (s.shape === 'recurring' && !s.days.length) { this.fail('Pick at least one day.'); return; }
    if (s.start_at != null && !(s.start_at > Date.now())) { this.fail('Pick a start time in the future.'); this.at.focus(); return; }
    this.keys.clearErrors();
    const r = this.keys.read(s.targets);
    if ('bad' in r) { this.keys.setError(...r.bad); this.keys.focus(r.bad[0]); return; }
    s.keys = r.keys;
    this.busy = true;
    const label = this.start.textContent;
    this.start.textContent = Object.keys(r.keys).length ? 'Checking keys…' : 'Starting…';
    this.start.setAttribute('aria-disabled', 'true');
    try {
      const out = await apiWrite('POST', 'api/storage/campaign/start', s);
      this.keys.clear();
      const c = out.campaign;
      const first = c.start_at && c.start_at > Date.now() + 30e3 ? `The first round is at ${clock(c.start_at)}.` : 'The first round is starting now.';
      this.host.done(c, s.shape === 'recurring' ? 'Schedule on.' : `Campaign started: ${first}`);
    } catch (e) {
      const b = /** @type {any} */ (e).body || {};
      this.keys.clearSecrets();
      if (b.code === 'keys_refused' && Array.isArray(b.targets)) {
        for (const id of b.targets) this.keys.setError(id, 'These keys were refused. Check them, or clear them to test connections only. Type the secret again.');
        this.keys.focus(b.targets[0]);
      } else this.fail(campaignError(e));
    } finally {
      this.busy = false;
      this.start.textContent = label;
      this.start.removeAttribute('aria-disabled');
    }
  }
}

/**
 * Run one round now: a dialog with the targets and optional keys; resolves
 * with the campaign started, or null.
 * @param {StorageTarget[]} targets
 */
export async function runOnce(targets) {
  const picks = targets.map((t) => /** @type {HTMLInputElement} */ (h('input', { type: 'checkbox', id: `st-r-${t.id}`, value: t.id, checked: true })));
  const bucket = targets.filter((t) => t.bucket);
  const keys = new KeyFields(bucket.map((t) => ({ id: t.id, name: t.name, scheme: t.scheme })), { optional: true });
  const err = h('p', { class: 'notice danger', role: 'alert', hidden: true });
  const fields = [h('fieldset', {}, h('legend', { text: 'Targets' }), ...targets.map((t, i) => h('div', { class: 'st-check-row' }, picks[i], h('label', { for: picks[i].id, text: ` ${t.name}` })))),
    bucket.length ? h('fieldset', { class: 'st-keysfs' }, h('legend', { text: 'Keys (optional)' }),
      h('p', { class: 'muted small', text: 'Leave empty to test connections only. Kept in memory for this round only.' }), keys.el) : '',
    err];
  /** @type {HTMLElement|undefined} */
  let focus;
  try {
    for (;;) {
      const v = await dialog({
        title: 'Run one round now',
        body: [h('p', { text: `10 fresh connections and 5 requests on a reused one per target, and with keys a ${size(64 << 20)} download and a ${size(16 << 20)} upload. It goes first in the test queue, after the test running now.` })],
        fields, buttons: [{ label: 'Cancel', value: 'cancel' }, { label: 'Run round', value: 'ok', primary: true }], focus,
      });
      if (v !== 'ok') return null;
      const ids = picks.filter((p) => p.checked).map((p) => p.value);
      err.hidden = true;
      keys.clearErrors();
      if (!ids.length) { err.textContent = 'Pick at least one target.'; err.hidden = false; continue; }
      const r = keys.read(ids);
      if ('bad' in r) { keys.setError(...r.bad); focus = /** @type {HTMLElement} */ (keys.f.get(r.bad[0])?.ak); continue; }
      try {
        const out = await apiWrite('POST', 'api/storage/run', { targets: ids, keys: r.keys });
        return out.campaign;
      } catch (e) {
        const b = /** @type {any} */ (e).body || {};
        keys.clearSecrets();
        if (b.code === 'keys_refused' && Array.isArray(b.targets)) {
          for (const id of b.targets) keys.setError(id, 'These keys were refused. Type the secret again, or clear the keys.');
          focus = keys.f.get(b.targets[0])?.ak;
        } else { err.textContent = campaignError(e); err.hidden = false; }
      }
    }
  } finally {
    keys.clear();
  }
}

/**
 * Enter keys for a campaign's targets waiting for them: resolves true when
 * fyisp took them.
 * @param {number} campaign @param {{id:string,name:string,scheme?:string}[]} waiting @param {boolean} saved a schedule's (saved until it ends)
 */
export async function enterKeys(campaign, waiting, saved) {
  const keys = new KeyFields(waiting, {});
  const err = h('p', { class: 'notice danger', role: 'alert', hidden: true });
  /** @type {HTMLElement|undefined} */
  let focus;
  try {
    for (;;) {
      const v = await dialog({
        title: 'Enter keys for this campaign',
        body: [h('p', { text: saved ? 'Saved on this computer, readable only by fyisp’s user, until the schedule ends. Never shown again.' : 'Kept in memory only, for this campaign, like before. Never shown again.' })],
        fields: [keys.el, err], buttons: [{ label: 'Later', value: 'cancel' }, { label: 'Use these keys', value: 'ok', primary: true }], focus,
      });
      if (v !== 'ok') return false;
      keys.clearErrors();
      const r = keys.read(waiting.map((w) => w.id));
      if ('bad' in r) { keys.setError(...r.bad); focus = keys.f.get(r.bad[0])?.ak; continue; }
      try {
        await apiWrite('POST', 'api/storage/campaign/keys?' + new URLSearchParams({ id: String(campaign) }), { keys: r.keys });
        return true;
      } catch (e) {
        const b = /** @type {any} */ (e).body || {};
        keys.clearSecrets();
        if (b.code === 'keys_refused' && Array.isArray(b.targets)) {
          for (const id of b.targets) keys.setError(id, 'These keys were refused. Type the secret again.');
          focus = keys.f.get(b.targets[0])?.ak;
        } else { err.textContent = campaignError(e); err.hidden = false; }
      }
    }
  } finally {
    keys.clear();
  }
}
