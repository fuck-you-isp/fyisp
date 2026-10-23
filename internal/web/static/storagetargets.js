// Details → Storage: the targets (api/storage, docs/STORAGE-API.md). A
// table of the S3-compatible endpoints the owner saved, each with Check
// (one unsigned request), Edit, Forget keys (when a campaign holds keys for
// it) and Delete; adding or editing turns the section into a form (as
// Speed's setup does), Escape or Cancel goes back. Keys belong to
// campaigns: the table only says whether one holds keys for a target.
// Plain ES module, no build step.
// @ts-check

import { h, apiWrite, dialog, clock } from './core.js';
import { classLong, statusWords, timings, hintWords } from './storagewords.js';

/** @typedef {{state:'none'|'memory'|'saved',access_key_last4:string|null,session_token:boolean,campaign:number|null}} StorageTargetKeys */
/** @typedef {{id:string,name:string,endpoint:string,scheme:string,host:string,port:number,region:string,bucket:string,addressing:string,has_ca:boolean,network:string,keys:StorageTargetKeys,in_use_by:number|null,last_check:import('./storagewords.js').StorageCheck|null,created:number,updated:number}} StorageTarget */
/** @typedef {{at:number,rev:number,targets:StorageTarget[]}} StorageSummary */
/** @typedef {{summary:()=>StorageSummary|null, reload:()=>Promise<void>, say:(t:string)=>void, render:()=>void}} TargetsHost */

/** As the server checks them (storage.Spec.normalize and s3test.Validate). */
const BUCKET_RE = /^[A-Za-z0-9][A-Za-z0-9._-]{1,61}[A-Za-z0-9]$/;
const REGION_RE = /^[A-Za-z0-9_-]{1,32}$/;
const MAX_TARGETS = 32;
const IN_USE = 'A live campaign uses this target: stop it first.';

/** Whether a campaign holds keys for t. @param {StorageTarget} t */
function hasKeys(t) { return !!t.keys && t.keys.state !== 'none'; }

/** "In memory · key …1234", "Saved · key …1234", "None". @param {StorageTarget} t */
export function keyWords(t) {
  if (!hasKeys(t)) return 'None';
  const k = t.keys;
  return `${k.state === 'saved' ? 'Saved' : 'In memory'}${k.access_key_last4 ? ` · key …${k.access_key_last4}` : ''}${k.session_token ? ' · with a session token' : ''}`;
}

/** "s3.us-east-1.amazonaws.com", "http://192.168.1.20:9000": the scheme only for plain http. @param {StorageTarget} t */
export function where(t) {
  const hp = t.port === (t.scheme === 'https' ? 443 : 80) ? t.host : `${t.host}:${t.port}`;
  return t.scheme === 'http' ? `http://${hp}` : hp;
}

/** Why a targets write failed, from the API's stable code. @param {any} e */
function writeError(e) {
  switch (e.body?.code) {
    case 'forbidden': return 'fyisp refused that. Reload the page and try again.';
    case 'not_found': return 'That target isn’t there any more. It may have been deleted in another tab.';
    case 'too_many': return `fyisp keeps at most ${MAX_TARGETS} targets. Delete one first.`;
    case 'changed': return 'This target changed in another tab. Check the form and save again.';
    case 'busy': return 'Another check is running. Try again in a few seconds.';
    case 'in_use': return IN_USE;
    case 'storage_file': return 'Storage tests are off: fyisp can’t use its storage-targets.json file. Fix or move it and restart fyisp.';
    case 'bad_request': return 'fyisp didn’t accept that setting.';
    case 'error': return 'fyisp couldn’t save its storage-targets.json file. Its log says why.';
  }
  if (e.status === 401) return ''; // the page reloads to sign in
  if (e.body?.code) return 'fyisp refused that. Reload the page and try again.';
  return 'Couldn’t reach fyisp. Try again.';
}

/** The result of a check, as a bold first sentence and a detail. @param {StorageTarget} t @returns {[string, string, boolean]} [lead, detail, bad] */
export function checkWords(t) {
  const c = /** @type {import('./storagewords.js').StorageCheck} */ (t.last_check);
  const st = statusWords(c.http_status, c.s3_code);
  const times = timings(c);
  const more = [];
  let lead, bad = c.class !== 'ok';
  const s = c.http_status || 0;
  if (c.class === 'ok' && c.keys === 'none' && s >= 300 && s < 400) {
    lead = `Connected, but it answered with a redirect${st}.`;
    more.push(t.bucket ? 'The bucket may be in another region, or at another endpoint.' : 'The endpoint may be another address.');
  } else if (c.class === 'ok' && c.keys === 'none' && s === 404 && t.bucket) {
    lead = `Connected, but it answered${st}.`;
    more.push('The bucket may not exist at this endpoint, or its name is different.');
  } else if (c.class === 'ok') {
    lead = c.keys === 'work' ? 'Connected; the keys work.' : 'Connected.';
    if (c.keys === 'none' && (s === 401 || s === 403)) more.push(`It answered${st}, as expected without keys.`);
    else if (c.keys === 'none' && s >= 400) more.push(`It answered${st}.`);
  } else if (c.class === 'http_auth') {
    lead = `Connected, but the keys were refused${st}.`;
    more.push('Check the access key, the secret and that the key may list this bucket.');
  } else if (c.class === 'http_4xx' && c.s3_code === 'NoSuchBucket') {
    lead = `Connected, but that bucket doesn’t exist here${st}.`;
    more.push('Check the bucket name and region.');
  } else if (c.group === 'config' || c.group === 'provider') {
    lead = classLong(c.class).replace(/\.$/, '') + `${st}.`;
  } else {
    lead = classLong(c.class);
  }
  const hint = hintWords(c.hint);
  if (hint) more.push(hint);
  if (times) more.unshift(times + '.');
  if (c.during_test) more.push('A speed test was running, so these times may be high.');
  more.push(`Checked ${clock(c.at)}.`);
  return [lead, more.join(' '), bad];
}

export class StorageTargets {
  /** @param {TargetsHost} host */
  constructor(host) {
    this.host = host;
    /** null: the list; '': the add form; an id: that target's edit form. @type {string|null} */
    this.editing = null;
    /** Targets whose Check is running. @type {Set<string>} */
    this.checking = new Set();
    /** The target just saved (its row is marked and focused). */
    this.saved = '';
    this.err = '';
    // One alert, kept across redraws: its text changes only when the error does.
    this.errEl = h('p', { class: 'notice danger', role: 'alert', hidden: true });
    /** The list or the form (redrawn); the alert stays outside it. */
    this.body = h('div');
    this.el = h('section', { class: 'st-targets sp-body', 'aria-labelledby': 'st-targets-h' }, this.errEl, this.body);
  }

  /** Shows (or clears) the list's error. @param {string} msg */
  setErr(msg) {
    this.err = msg;
    if (this.errEl.textContent !== msg) this.errEl.textContent = msg;
    this.errEl.hidden = !msg;
  }

  /** The list, or the form. */
  render() {
    if (this.editing != null) return; // the form stays as typed until it closes
    const d = this.host.summary();
    const ts = d ? d.targets : [];
    if (!ts.length) { this.body.replaceChildren(); this.el.hidden = !this.err; return; }
    this.el.hidden = false;
    const rows = ts.flatMap((t) => this.row(t));
    this.body.replaceChildren(...[
      h('h2', { id: 'st-targets-h', text: 'Targets', tabindex: '-1' }),
      h('table', { class: 'sp-st st-list' },
        h('caption', { class: 'sr-only', text: 'Storage targets' }),
        h('thead', {}, h('tr', {}, ...['Name', 'Endpoint', 'Region', 'Bucket', 'Keys', 'Actions'].map((c) => h('th', { scope: 'col', text: c })))),
        h('tbody', {}, ...rows)),
      h('div', { class: 'sp-acts st-under' },
        h('button', { type: 'button', class: 'btn st-add', text: 'Add a target', disabled: ts.length >= MAX_TARGETS, onclick: () => this.open('') }),
        ts.length >= MAX_TARGETS ? h('span', { class: 'muted small', text: `At most ${MAX_TARGETS} targets.` }) : null,
        this.saved && ts.some((t) => t.id === this.saved && !t.last_check) ? h('span', { class: 'muted small', text: 'Check it now to see whether fyisp can reach it.' }) : null),
    ].filter((n) => n != null));
    this.setErr(this.err);
  }

  /** A target's row heading (focus goes there after a save or a delete). @param {string} id */
  rowHead(id) { return /** @type {HTMLElement|null} */ (this.el.querySelector(`tr[data-id="${CSS.escape(id)}"] th`)); }

  /** A target's row, and its check result under it. @param {StorageTarget} t */
  row(t) {
    const busy = this.checking.has(t.id);
    const used = t.in_use_by != null;
    const out = [h('tr', { 'data-id': t.id, class: t.id === this.saved ? 'sel' : null },
      h('th', { scope: 'row', tabindex: '-1' }, h('b', { text: t.name }),
        t.network === 'private' ? h('span', { class: 'pill', text: 'on your network' }) : null,
        used ? h('span', { class: 'pill', text: 'in a live campaign' }) : null),
      h('td', { 'data-k': 'Endpoint', class: 'st-ep', text: where(t) }),
      h('td', { 'data-k': 'Region', text: t.region }),
      h('td', { 'data-k': 'Bucket', text: t.bucket || 'connection only' }),
      h('td', { 'data-k': 'Keys', class: 'st-keys', text: keyWords(t) }),
      h('td', { class: 'st-row-acts' },
        h('button', { type: 'button', class: 'btn sm', text: busy ? 'Checking…' : 'Check', 'aria-disabled': busy ? 'true' : null, 'aria-label': `${busy ? 'Checking' : 'Check'} ${t.name}`,
          'data-key': `check:${t.id}`, onclick: () => { if (!this.checking.has(t.id)) this.check(t); } }),
        h('button', { type: 'button', class: 'btn sm', text: 'Edit', 'aria-label': `Edit ${t.name}`, 'data-key': `edit:${t.id}`, onclick: () => this.open(t.id) }),
        hasKeys(t) ? h('button', { type: 'button', class: 'btn sm ghost', text: 'Forget keys…', 'aria-label': `Forget the keys for ${t.name}`, 'data-key': `forget:${t.id}`, onclick: () => this.forget(t) }) : null,
        // A target a live campaign uses can't be deleted: Delete says why instead of asking.
        h('button', { type: 'button', class: 'btn sm ghost', text: 'Delete…', 'aria-label': `Delete ${t.name}`, 'data-key': `delete:${t.id}`,
          title: used ? IN_USE : null, onclick: () => { if (used) { this.setErr(IN_USE); this.refocus(`delete:${t.id}`); } else this.remove(t); } })))];
    if (t.last_check) {
      const [lead, detail, bad] = checkWords(t);
      out.push(h('tr', { class: 'st-check' + (bad ? ' bad' : '') }, h('td', { colspan: '6' },
        h('p', { class: bad ? 'notice danger' : 'st-check-ok' }, h('b', { text: lead }), ' ', detail))));
    }
    return out;
  }

  /** Runs a check (one request, at most 20 s). @param {StorageTarget} t */
  async check(t) {
    this.checking.add(t.id);
    this.setErr('');
    this.saved = '';
    this.render();
    this.refocus(`check:${t.id}`);
    try {
      await apiWrite('POST', 'api/storage/check?' + new URLSearchParams({ id: t.id }));
      await this.host.reload();
      const nt = this.host.summary()?.targets.find((x) => x.id === t.id);
      if (nt && nt.last_check) {
        const [lead] = checkWords(nt);
        this.host.say(`${t.name}: ${lead}`);
      }
    } catch (e) {
      this.setErr(writeError(e));
    } finally {
      this.checking.delete(t.id);
      this.host.render();
      this.refocus(`check:${t.id}`);
    }
  }

  /** Focuses the button with this data-key ("check:<id>"), if it is still there. @param {string} key */
  refocus(key) {
    /** @type {HTMLElement|null} */ (this.el.querySelector(`button[data-key="${CSS.escape(key)}"]`))?.focus();
  }

  /** Forgets the keys a campaign holds for t, after asking. @param {StorageTarget} t */
  async forget(t) {
    const v = await dialog({
      title: `Forget the keys for ${t.name}?`,
      body: [h('p', { text: t.keys.state === 'saved'
        ? 'The schedule’s saved keys for it are deleted now. Its rounds test connections only until you enter keys again.'
        : 'The campaign’s keys for it are forgotten now. Its rounds test connections only until you enter keys again.' })],
      buttons: [{ label: 'Cancel', value: 'cancel' }, { label: 'Forget keys', value: 'ok', primary: true }],
      safe: true,
    });
    if (v !== 'ok') { this.refocus(`forget:${t.id}`); return; }
    try {
      await apiWrite('POST', 'api/storage/target/forget-keys?' + new URLSearchParams({ id: t.id }));
      this.setErr('');
      this.host.say(`Forgot the keys for ${t.name}.`);
    } catch (e) {
      this.setErr(writeError(e));
    }
    await this.host.reload();
    this.host.render();
    if (this.el.querySelector(`button[data-key="forget:${CSS.escape(t.id)}"]`)) this.refocus(`forget:${t.id}`);
    else this.rowHead(t.id)?.focus();
  }

  /** Deletes a target, after asking. @param {StorageTarget} t */
  async remove(t) {
    const v = await dialog({
      title: `Delete ${t.name}?`,
      body: [h('p', { text: 'Its settings are deleted now. Nothing in the bucket is touched.' })],
      buttons: [{ label: 'Cancel', value: 'cancel' }, { label: 'Delete target', value: 'ok', primary: true }],
      safe: true,
    });
    if (v !== 'ok') { this.refocus(`delete:${t.id}`); return; }
    const ts = this.host.summary()?.targets || [];
    const i = ts.findIndex((x) => x.id === t.id);
    const next = ts[i + 1] || ts[i - 1];
    let ok = false;
    try {
      await apiWrite('POST', 'api/storage/target/delete?' + new URLSearchParams({ id: t.id }));
      this.setErr('');
      this.saved = '';
      this.host.say(`Deleted ${t.name}.`);
      ok = true;
    } catch (e) {
      this.setErr(writeError(e));
    }
    await this.host.reload();
    this.host.render();
    if (!ok) { this.refocus(`delete:${t.id}`); return; }
    const row = next && this.rowHead(next.id);
    if (row) row.focus(); else /** @type {HTMLElement|null} */ (document.querySelector('.st-add'))?.focus();
  }

  /** Opens the add form ('') or a target's edit form. @param {string} id */
  open(id) {
    const t = id ? this.host.summary()?.targets.find((x) => x.id === id) : null;
    if (id && !t) return;
    this.editing = id;
    this.err = '';
    this.saved = '';
    this.host.render(); // the header leaves out its Add button while a form is open
    this.el.hidden = false;
    this.body.replaceChildren(...new TargetForm(t || null, {
      done: (saved, words) => this.closeForm(saved, words),
    }).nodes());
    /** @type {HTMLElement|null} */ (this.el.querySelector('h2'))?.focus();
  }

  /** @param {string} saved the target saved ('' cancelled) @param {string} words */
  async closeForm(saved, words) {
    const opener = this.editing ? `edit:${this.editing}` : '';
    this.editing = null;
    if (saved) {
      this.saved = saved;
      this.host.say(words);
      await this.host.reload();
      this.host.render();
      this.rowHead(saved)?.focus();
      return;
    }
    this.host.render();
    if (opener) this.refocus(opener);
    else /** @type {HTMLElement|null} */ (document.querySelector('.st-add'))?.focus();
  }
}

/** The add or edit form: fields, live preview, validation, Save and Cancel. */
class TargetForm {
  /** @param {StorageTarget|null} t @param {{done:(saved:string, words:string)=>void}} host */
  constructor(t, host) {
    this.t = t;
    this.host = host;
    this.busy = false;
    const v = t || { name: '', endpoint: '', region: '', bucket: '', addressing: 'virtual-host', has_ca: false };
    this.start = JSON.stringify([v.name, v.endpoint, v.region, v.bucket, v.addressing]);
    /** @type {Object<string, HTMLInputElement|HTMLTextAreaElement>} */
    this.f = {
      name: /** @type {HTMLInputElement} */ (h('input', { type: 'text', id: 'st-f-name', value: v.name, maxlength: '64', autocomplete: 'off', 'aria-describedby': 'st-f-name-help st-f-name-err' })),
      endpoint: /** @type {HTMLInputElement} */ (h('input', { type: 'url', id: 'st-f-endpoint', value: v.endpoint, inputmode: 'url', spellcheck: 'false', autocomplete: 'off', placeholder: 'https://s3.<region>.<provider>', 'aria-describedby': 'st-f-endpoint-help st-f-endpoint-err' })),
      region: /** @type {HTMLInputElement} */ (h('input', { type: 'text', id: 'st-f-region', value: v.region, spellcheck: 'false', autocomplete: 'off', placeholder: 'us-east-1', 'aria-describedby': 'st-f-region-help st-f-region-err' })),
      bucket: /** @type {HTMLInputElement} */ (h('input', { type: 'text', id: 'st-f-bucket', value: v.bucket, spellcheck: 'false', autocomplete: 'off', 'aria-describedby': 'st-f-bucket-help st-f-bucket-err' })),
      ca: /** @type {HTMLTextAreaElement} */ (h('textarea', { id: 'st-f-ca', rows: '6', spellcheck: 'false', class: 'mono', 'aria-describedby': 'st-f-ca-help st-f-ca-err' })),
    };
    /** "keep" (edit, a CA is set), "" (none), or the pasted PEM in the textarea. */
    this.caMode = t && t.has_ca ? 'keep' : '';
    this.addr = v.addressing;
    this.preview = h('p', { class: 'muted small st-preview' });
    this.dotNote = h('p', { class: 'notice', hidden: true, text: 'Bucket names with dots break the certificate check when the bucket is in the host name. Choose “In the path”.' });
    this.formErr = h('p', { class: 'notice danger', role: 'alert', tabindex: '-1', hidden: true });
    for (const el of Object.values(this.f)) el.addEventListener('input', () => this.live());
  }

  /** A field's error line. @param {string} k */
  errEl(k) { return h('p', { class: 'sp-note st-err', id: `st-f-${k}-err`, hidden: true }); }

  /** @returns {Node[]} */
  nodes() {
    const t = this.t;
    const fld = (/** @type {string} */ k, /** @type {string} */ label, /** @type {string} */ help) => h('div', { class: 'st-fld' },
      h('label', { for: `st-f-${k}`, text: label }), this.f[k],
      h('p', { class: 'muted small', id: `st-f-${k}-help`, text: help }), this.errEl(k));
    this.radios = h('fieldset', { class: 'sp-modes st-addr' }, h('legend', { text: 'Bucket address' }),
      ...[['virtual-host', 'In the host name'], ['path', 'In the path']].map(([v, label]) => h('div', { class: 'sp-radio' },
        h('input', { type: 'radio', name: 'st-addr', id: `st-addr-${v}`, value: v, checked: this.addr === v, onchange: () => { this.addr = v; this.live(); } }),
        h('label', { for: `st-addr-${v}` }, h('b', { text: label }), ' ', h('span', { class: 'muted small st-addr-ex', 'data-v': v })))),
      this.dotNote);
    const caFile = h('input', { type: 'file', id: 'st-f-ca-file', accept: '.pem,.crt,.cer,application/x-pem-file', onchange: (/** @type {Event} */ e) => this.readCA(/** @type {HTMLInputElement} */ (e.target)) });
    this.caFile = caFile;
    const caWrap = h('div', { class: 'st-ca-body' });
    this.redrawCA(caWrap, false);
    const ca = h('details', { class: 'sp-before st-ca', open: this.caMode === 'keep' ? true : null },
      h('summary', { text: 'Uses its own certificate authority (self-hosted)' }), caWrap);
    const form = h('form', { class: 'st-form', novalidate: true, onsubmit: (/** @type {Event} */ e) => { e.preventDefault(); this.save(); } },
    fld('name', 'Name', t ? 'Renaming keeps this target’s history.' : 'Your label for this endpoint.'),
    fld('endpoint', 'Endpoint', 'https://host, or http:// for a server on your network. Your provider lists it as the S3 endpoint or S3 API URL.'),
    fld('region', 'Region (optional)', 'As your provider names it, e.g. us-east-1 or auto. Used to sign requests; us-east-1 when empty.'),
    fld('bucket', 'Bucket (optional)', 'Leave empty to test connections only.'),
    this.radios, ca, this.preview,
    t && hasKeys(t) ? h('p', { class: 'notice st-keys-note' }, h('b', { text: 'A campaign holds keys for this target. ' }),
      'Saving a change to the endpoint, region, bucket, addressing or certificate authority deletes them: its rounds test connections only until you enter keys again. A new name keeps them.') : null,
    this.formErr,
    h('div', { class: 'sp-acts' },
      h('button', { type: 'submit', class: 'btn primary st-save', text: t ? 'Save changes' : 'Save target' }),
      h('button', { type: 'button', class: 'btn ghost', text: 'Cancel', onclick: () => this.cancel() })));
    this.live();
    // Escape anywhere in it, the heading included, cancels.
    return [h('div', { onkeydown: (/** @type {KeyboardEvent} */ e) => { if (e.key === 'Escape' && !e.defaultPrevented) { e.preventDefault(); this.cancel(); } } },
      h('h2', { id: 'st-targets-h', tabindex: '-1', text: t ? `Edit ${t.name}` : 'Add a storage target' }), form)];
  }

  /** The PEM textarea and file picker. @param {HTMLElement} caFile */
  caFields(caFile) {
    return [h('label', { for: 'st-f-ca', text: 'CA certificate (PEM)' }), this.f.ca,
      h('p', { class: 'muted small', id: 'st-f-ca-help', text: 'Paste the certificate that signed your server’s certificate, starting with -----BEGIN CERTIFICATE-----. fyisp never skips the certificate check.' }),
      h('label', { class: 'small', for: 'st-f-ca-file', text: 'or choose a .pem file ' }), caFile, this.errEl('ca')];
  }

  /** The CA part for caMode: kept (Replace, Remove), to be removed (Keep it), or the fields. @param {HTMLElement} wrap @param {boolean} focus */
  redrawCA(wrap, focus = true) {
    const set = (/** @type {string} */ mode) => () => { this.caMode = mode; this.redrawCA(wrap); };
    if (this.caMode === 'keep') {
      wrap.replaceChildren(h('p', { class: 'st-ca-set' }, 'A certificate authority is set. ',
        h('button', { type: 'button', class: 'btn sm', text: 'Replace', onclick: set('') }), ' ',
        h('button', { type: 'button', class: 'btn sm ghost', text: 'Remove', onclick: set('remove') })));
    } else if (this.caMode === 'remove') {
      wrap.replaceChildren(h('p', { class: 'st-ca-set' }, 'The certificate authority is removed when you save; the system’s are used. ',
        h('button', { type: 'button', class: 'btn sm', text: 'Keep it', onclick: set('keep') })));
    } else {
      wrap.replaceChildren(...this.caFields(/** @type {HTMLElement} */ (this.caFile)));
    }
    if (focus) /** @type {HTMLElement|null} */ (wrap.querySelector('textarea, button'))?.focus();
  }

  /** Reads a chosen .pem file into the textarea. @param {HTMLInputElement} input */
  readCA(input) {
    const file = input.files && input.files[0];
    if (!file) return;
    if (file.size > 64 << 10) { this.fieldErr('ca', 'That file is over 64 KiB; it isn’t a certificate.'); return; }
    file.text().then((s) => { this.f.ca.value = s; this.live(); });
  }

  /** The live preview, the addressing examples and the dotted-bucket note. */
  live() {
    const ep = this.f.endpoint.value.trim().replace(/\/+$/, '');
    const bucket = this.f.bucket.value.trim();
    let u = null;
    try { u = ep ? new URL(ep) : null; } catch { /* not yet a URL */ }
    const host = u ? u.host : 'host';
    const scheme = u ? u.protocol : 'https:';
    const ex = {
      'virtual-host': `${bucket || 'bucket'}.${host}`,
      path: `${host}/${bucket || 'bucket'}`,
    };
    for (const s of /** @type {NodeListOf<HTMLElement>} */ (this.radios?.querySelectorAll('.st-addr-ex') || [])) s.textContent = ex[/** @type {'path'|'virtual-host'} */ (s.dataset.v)];
    if (this.radios) {
      this.radios.hidden = !bucket;
      for (const r of /** @type {NodeListOf<HTMLInputElement>} */ (this.radios.querySelectorAll('input'))) r.disabled = !bucket;
    }
    this.dotNote.hidden = !(bucket.includes('.') && this.addr === 'virtual-host');
    const to = !u ? '' : bucket ? `${scheme}//${this.addr === 'path' ? `${u.host}/${bucket}` : `${bucket}.${u.host}`}/` : `${scheme}//${u.host}/`;
    this.preview.textContent = to ? `Requests go to: ${to}` : '';
  }

  /** Shows a field's error and marks it. @param {string} k @param {string} msg */
  fieldErr(k, msg) {
    const el = /** @type {HTMLElement|null} */ (document.getElementById(`st-f-${k}-err`));
    const f = this.f[k];
    if (el) { el.textContent = msg; el.hidden = !msg; }
    if (f) { if (msg) f.setAttribute('aria-invalid', 'true'); else f.removeAttribute('aria-invalid'); }
  }

  /** The first problem the browser can see, before asking fyisp. @returns {[string, string]|null} */
  check() {
    const name = this.f.name.value.trim(), ep = this.f.endpoint.value.trim(), region = this.f.region.value.trim(), bucket = this.f.bucket.value.trim();
    if (!name) return ['name', 'Give the target a name.'];
    let u = null;
    try { u = new URL(ep); } catch { /* below */ }
    if (!u || (u.protocol !== 'https:' && u.protocol !== 'http:') || !u.hostname) return ['endpoint', 'Enter the address as https://host or https://host:port.'];
    if (u.username || u.password || (u.pathname && u.pathname !== '/') || u.search || u.hash || ep.endsWith('?')) return ['endpoint', 'Leave out the path; just https://host[:port].'];
    if (region && !REGION_RE.test(region)) return ['region', 'Use the region your provider shows, e.g. us-east-1 or auto (letters, digits, - and _).'];
    if (bucket && !BUCKET_RE.test(bucket)) return ['bucket', 'Bucket names are 3 to 63 letters, digits, dots, hyphens or underscores.'];
    if (bucket && this.addr === 'virtual-host' && /^\d+\.\d+\.\d+\.\d+$/.test(u.hostname)) return ['bucket', 'An endpoint given as an IP address needs the bucket in the path.'];
    const ca = this.f.ca.value.trim();
    if (this.caMode === '' && ca && !ca.includes('-----BEGIN CERTIFICATE-----')) return ['ca', 'That isn’t a PEM certificate.'];
    return null;
  }

  /** The words for a field fyisp refused. @param {any} e @returns {[string, string]|null} */
  serverField(e) {
    const b = e.body || {};
    const name = this.f.name.value.trim();
    if (b.code === 'name_taken') return ['name', `Another target is already called ${name}.`];
    if (b.code === 'addr_refused') return ['endpoint', 'fyisp doesn’t connect to that address: it is link-local, reserved or a documentation address, or the name resolves both to your network and to the internet.'];
    if (b.code !== 'bad_request' || !b.field) return null;
    switch (b.field) {
      case 'name': return ['name', 'Give the target a name of up to 64 characters, without control characters.'];
      case 'endpoint': return ['endpoint', this.f.endpoint.value.trim().startsWith('http://')
        ? 'fyisp couldn’t use that address. Check that the name resolves to an IPv4 address; plain http:// is allowed only for a server on your network.'
        : 'fyisp couldn’t use that address. Check that the name resolves to an IPv4 address.'];
      case 'region': return ['region', 'Use the region your provider shows, e.g. us-east-1 or auto (letters, digits, - and _).'];
      case 'bucket': return ['bucket', 'Bucket names are 3 to 63 letters, digits, dots, hyphens or underscores.'];
      case 'ca_pem': return ['ca', 'That isn’t a PEM certificate.'];
      case 'addressing': return ['bucket', 'Choose where the bucket goes in the address.'];
    }
    return null;
  }

  /** The request body: every field for a new target, the changed ones for an edit. */
  body() {
    /** @type {Object<string,string>} */
    const b = {
      name: this.f.name.value.trim(), endpoint: this.f.endpoint.value.trim(), region: this.f.region.value.trim(),
      bucket: this.f.bucket.value.trim(), addressing: this.f.bucket.value.trim() ? this.addr : 'virtual-host',
    };
    if (this.caMode === 'remove') b.ca_pem = '';
    else if (this.caMode === '' && (this.f.ca.value.trim() || !this.t)) b.ca_pem = this.f.ca.value.trim();
    return b;
  }

  async save() {
    if (this.busy) return;
    for (const k of Object.keys(this.f)) this.fieldErr(k, '');
    this.formErr.hidden = true;
    const bad = this.check();
    if (bad) { this.fieldErr(...bad); this.f[bad[0]].focus(); return; }
    this.busy = true;
    const btn = /** @type {HTMLButtonElement|null} */ (document.querySelector('.st-save'));
    btn?.setAttribute('aria-disabled', 'true');
    try {
      const t = this.t;
      const r = t
        ? await apiWrite('POST', 'api/storage/target/update?' + new URLSearchParams({ id: t.id }), this.body())
        : await apiWrite('POST', 'api/storage/target/add', this.body());
      this.host.done(r.target.id, r.keys_cleared ? `Saved ${r.target.name}. The keys a campaign held for it were deleted.` : `Saved ${r.target.name}.`);
    } catch (e) {
      const f = this.serverField(e);
      if (f) { this.fieldErr(...f); this.f[f[0]].focus(); } else {
        const msg = writeError(e);
        this.formErr.textContent = msg;
        this.formErr.hidden = !msg;
        if (msg) this.formErr.focus();
      }
    } finally {
      this.busy = false;
      btn?.removeAttribute('aria-disabled');
    }
  }

  async cancel() {
    const now = JSON.stringify([this.f.name.value, this.f.endpoint.value, this.f.region.value, this.f.bucket.value, this.addr]);
    const was = /** @type {HTMLElement|null} */ (document.activeElement);
    if (now !== this.start || this.f.ca.value.trim() || this.caMode === 'remove') {
      const v = await dialog({
        title: this.t ? 'Discard your changes?' : 'Discard this target?',
        buttons: [{ label: 'Keep editing', value: 'cancel' }, { label: 'Discard', value: 'ok', primary: true }],
        safe: true,
      });
      if (v !== 'ok') { (was && was.isConnected ? was : this.f.name).focus(); return; }
    }
    this.host.done('', '');
  }
}
