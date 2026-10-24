// Storage tests: the key fields of a campaign (the new-campaign form, Run
// one round now, Enter keys…). One fieldset per bucket target: access key,
// secret, an optional session token, and "Same keys as <the first>" for
// the others. Nothing is ever re-filled or shown again: the fields are
// cleared once read (clear()).
// Plain ES module, no build step.
// @ts-check

import { h } from './core.js';

/** @typedef {{access_key:string, secret:string, session_token?:string}} StorageKeys */
/** @typedef {{id:string, name:string, scheme?:string}} KeyTarget */

let seq = 0;

export class KeyFields {
  /**
   * @param {KeyTarget[]} targets the bucket targets that may get keys
   * @param {{optional?:boolean, onchange?:()=>void}} [o] optional: a target may be left without keys
   */
  constructor(targets, o = {}) {
    this.targets = targets;
    this.o = o;
    this.n = ++seq;
    /** @type {Map<string, {fs:HTMLElement, ak:HTMLInputElement, sec:HTMLInputElement, tok:HTMLInputElement, same:HTMLInputElement|null, err:HTMLElement}>} */
    this.f = new Map();
    this.el = h('div', { class: 'st-keys' }, ...targets.map((t, i) => this.fieldset(t, i)));
  }

  /** @param {KeyTarget} t @param {number} i */
  fieldset(t, i) {
    const id = (/** @type {string} */ k) => `st-k${this.n}-${t.id}-${k}`;
    const input = (/** @type {string} */ k, /** @type {string} */ type) => /** @type {HTMLInputElement} */ (h('input', {
      type, id: id(k), autocomplete: type === 'password' ? 'new-password' : 'off', spellcheck: 'false',
      'data-1p-ignore': 'true', 'data-lpignore': 'true', 'aria-describedby': id('err'),
      oninput: () => this.o.onchange?.(),
    }));
    const ak = input('ak', 'text'), sec = input('sec', 'password'), tok = input('tok', 'password');
    const err = h('p', { class: 'sp-note st-err', id: id('err'), hidden: true });
    const same = i > 0 ? /** @type {HTMLInputElement} */ (h('input', { type: 'checkbox', id: id('same'), onchange: () => { this.sameChanged(t.id); this.o.onchange?.(); } })) : null;
    const own = h('div', { class: 'st-keys-own' },
      h('div', { class: 'st-fld' }, h('label', { for: id('ak'), text: 'Access key ID' }), ak),
      h('div', { class: 'st-fld' }, h('label', { for: id('sec'), text: 'Secret access key' }), sec),
      t.scheme === 'http' ? null : h('details', { class: 'st-tok' }, h('summary', { text: 'Temporary credentials (session token)' }),
        h('div', { class: 'st-fld' }, h('label', { for: id('tok'), text: 'Session token' }), tok),
        h('p', { class: 'muted small', text: 'For temporary credentials from your provider. They stop working by themselves when they expire; rounds after that show “keys refused”.' })));
    const fs = h('fieldset', { class: 'st-keyset' }, h('legend', { text: `Keys for ${t.name}` }),
      same ? h('div', { class: 'st-check-row' }, same, h('label', { for: id('same'), text: ` Same keys as ${this.targets[0].name}` })) : null,
      own, err);
    this.f.set(t.id, { fs, ak, sec, tok, same, err });
    return fs;
  }

  /** @param {string} id */
  sameChanged(id) {
    const f = this.f.get(id);
    if (!f) return;
    const own = /** @type {HTMLElement} */ (f.fs.querySelector('.st-keys-own'));
    own.hidden = !!f.same?.checked;
  }

  /** Shows or clears a target's error. @param {string} id @param {string} msg */
  setError(id, msg) {
    const f = this.f.get(id);
    if (!f) return;
    f.err.textContent = msg;
    f.err.hidden = !msg;
    for (const x of [f.ak, f.sec]) if (msg) x.setAttribute('aria-invalid', 'true'); else x.removeAttribute('aria-invalid');
  }

  /** Takes what was typed in old's fields for the same targets, then empties old. @param {KeyFields} old */
  carry(old) {
    for (const [id, o] of old.f) {
      const f = this.f.get(id);
      if (f) { f.ak.value = o.ak.value; f.sec.value = o.sec.value; f.tok.value = o.tok.value; if (f.same && o.same) { f.same.checked = o.same.checked; this.sameChanged(id); } }
    }
    old.clear();
  }

  /** Clears every target's error. */
  clearErrors() { for (const t of this.targets) this.setError(t.id, ''); }

  /** Focuses a target's first field. @param {string} id */
  focus(id) { this.f.get(id)?.ak.focus(); }

  /**
   * The keys per target, or the first problem ([target, message]). A target
   * with both fields empty gets none (optional: allowed; else a problem).
   * @param {string[]} ids the targets asked for (others are left out)
   * @returns {{keys:Object<string, StorageKeys>}|{bad:[string, string]}}
   */
  read(ids) {
    /** @type {Object<string, StorageKeys>} */
    const out = {};
    for (const t of this.targets) {
      if (!ids.includes(t.id)) continue;
      const f = /** @type {NonNullable<ReturnType<typeof this.f.get>>} */ (this.f.get(t.id));
      let src = f;
      if (f.same?.checked) src = /** @type {any} */ (this.f.get(this.targets[0].id));
      const ak = src.ak.value.trim(), sec = src.sec.value, tok = src.tok.value.trim();
      if (!ak && !sec) {
        if (this.o.optional) continue;
        return { bad: [t.id, 'Enter the access key ID and the secret.'] };
      }
      if (!ak || !sec) return { bad: [t.id, 'Enter both the access key ID and the secret, or neither.'] };
      if (/\s/.test(ak) || ak.length > 128) return { bad: [t.id, 'The access key ID is up to 128 characters, without spaces.'] };
      if (sec.length > 256) return { bad: [t.id, 'The secret is up to 256 characters.'] };
      out[t.id] = tok ? { access_key: ak, secret: sec, session_token: tok } : { access_key: ak, secret: sec };
    }
    return { keys: out };
  }

  /** Targets with keys typed (or "same as"). */
  keyed() {
    return this.targets.filter((t) => {
      const f = this.f.get(t.id);
      if (!f) return false;
      if (f.same?.checked) { const first = this.f.get(this.targets[0].id); return !!first && !!first.ak.value.trim(); }
      return !!f.ak.value.trim();
    }).map((t) => t.id);
  }

  /** Empties every field (after a request, whatever its answer, or on cancel). */
  clear() {
    for (const f of this.f.values()) { f.ak.value = ''; f.sec.value = ''; f.tok.value = ''; }
  }

  /** Empties only the secrets (a refusal keeps the access keys to fix). */
  clearSecrets() {
    for (const f of this.f.values()) { f.sec.value = ''; f.tok.value = ''; }
  }
}
