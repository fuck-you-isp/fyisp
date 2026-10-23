// Details → Storage: checks of S3-compatible endpoints the owner adds
// (api/storage, docs/STORAGE-API.md). A header that says what this is,
// then the targets (storagetargets.js). Loaded the first time Storage is
// opened.
// Plain ES module, no build step.
// @ts-check

import { h, fetchJSON } from './core.js';
import { StorageTargets } from './storagetargets.js';

export class Storage {
  constructor() {
    /** @type {import('./storagetargets.js').StorageSummary|null} */
    this.d = null;
    this.err = '';
    this.seq = 0;
    this.say = h('p', { class: 'sr-only', role: 'status' });
    this.headEl = h('div', { class: 'sp-head st-head' });
    // One alert, kept across redraws (see render).
    this.errEl = h('p', { class: 'notice danger', role: 'alert' });
    this.targets = new StorageTargets({
      summary: () => this.d,
      reload: () => this.load(),
      say: (t) => { this.say.textContent = t; },
      render: () => this.render(),
    });
    this.el = h('section', { id: 'storage', class: 'storage', 'aria-label': 'Storage tests', hidden: true },
      this.say, this.headEl, this.targets.el);
  }

  /** Reads the summary. */
  async load() {
    const seq = ++this.seq;
    try {
      const d = /** @type {import('./storagetargets.js').StorageSummary} */ (await fetchJSON('api/storage'));
      if (seq !== this.seq) return;
      this.d = d;
      this.err = '';
    } catch (e) {
      if (seq !== this.seq) return;
      this.err = /** @type {any} */ (e).body?.code === 'storage_file'
        ? 'Storage tests are off: fyisp can’t use its storage-targets.json file (see fyisp’s log). Fix or move it and restart fyisp; fyisp never overwrites it.'
        : 'Couldn’t load the storage targets. They’ll show again when fyisp answers.';
    }
  }

  /** Details' refresh (on opening, after writes, every 15 s on screen). */
  async refresh() {
    if (this.targets.editing != null) return; // don't redraw under a form
    await this.load();
    this.render();
  }

  render() {
    const d = this.d;
    const n = d ? d.targets.length : 0;
    const add = () => this.targets.open('');
    if (this.errEl.textContent !== this.err) this.errEl.textContent = this.err;
    if (this.err) {
      if (!this.headEl.contains(this.errEl)) this.headEl.replaceChildren(h('h2', { text: 'Storage tests' }), this.errEl);
      this.targets.render();
      return;
    }
    const editing = this.targets.editing != null;
    if (!d) { this.headEl.replaceChildren(h('h2', { text: 'Storage tests' }), h('p', { class: 'muted', text: 'Loading…' })); return; }
    if (!n) {
      this.headEl.replaceChildren(
        h('h2', { text: 'Storage tests' }),
        h('p', { class: 'sp-lead', text: 'Check the S3-compatible storage endpoints you use: whether fyisp can reach each one, how long each step of a connection takes, and what to fix when one refuses.' }),
        h('ul', { class: 'sp-notes' },
          h('li', { text: 'Any S3-compatible endpoint: a cloud provider’s, or a server on your network.' }),
          h('li', { text: 'A check is one small request, sent only when you ask. No keys are needed.' }),
          h('li', { text: 'Endpoints see your internet address, as with any request.' })),
        editing ? null : h('div', { class: 'sp-acts' },
          h('button', { type: 'button', class: 'btn primary sp-go st-add', text: 'Add a target', onclick: add }),
          h('span', { class: 'muted small', text: 'Nothing is checked until you ask.' })),
        h('details', { class: 'sp-before' }, h('summary', { text: 'Where do I find the endpoint?' }),
          h('p', { text: 'Your storage provider lists it as the “S3 endpoint”, “S3 API URL” or “S3-compatible endpoint”, usually with a region. It looks like https://s3.<region>.<provider-domain>. For a MinIO or NAS on your network, use its address, e.g. http://192.168.1.20:9000.' })));
    } else {
      this.headEl.replaceChildren(h('div', { class: 'sp-on' },
        h('p', { class: 'sp-state' }, h('b', { text: 'Storage tests' }), ` · ${n} target${n === 1 ? '' : 's'}`),
        h('div', { class: 'sp-acts' })));
    }
    this.targets.render();
  }
}
