// Details → Storage: what a campaign points to (its finding), each
// target's card, the traceroutes to the providers, and the report (Copy
// report as Markdown, Download CSV) from api/storage?part=campaign and
// api/storage/report.
// Plain ES module, no build step.
// @ts-check

import { h, fmtMs, fmtMbps, copyText } from './core.js';
import { classShort } from './storagewords.js';
import { edge } from './storagegrid.js';

/** Each kicker's words. */
const KICKER = /** @type {Object<string,string>} */ ({
  path: 'Points to the path', line: 'Points to your line', provider: 'Points to the provider',
  config: 'Configuration problem', none: 'No problems found', early: 'Too early to tell',
});

/** @param {{kicker:string,text:string,evidence:string[]}} f */
export function findingSection(f) {
  return h('section', { class: 'sp-found sp-body st-found', 'aria-labelledby': 'st-found-h' },
    h('h3', { id: 'st-found-h', text: 'What we found' }),
    h('p', { class: 'sp-kicker', text: KICKER[f.kicker] || '' }),
    h('p', { class: 'sp-found-text', text: f.text }),
    f.evidence.length ? h('ul', { class: 'sp-found-x' }, ...f.evidence.map((t) => h('li', { text: t }))) : '');
}

/** "31.0 ms median · 48.2 ms at the 95th percentile". @param {{p50:number|null,p95:number|null}} p */
function pct(p) { return p.p50 == null ? '—' : `${fmtMs(p.p50)} ms median · ${fmtMs(p.p95)} ms at the 95th percentile`; }

/** One line of a transfer. @param {any} x @param {string} what */
function rate(x, what) {
  if (!x) return null;
  return h('p', {}, h('b', { text: `${what} ` }), x.median_bps == null ? `none worked (0 of ${x.n})` : `${fmtMbps(x.median_bps)} median · ${fmtMbps(x.p10_bps)} at the 10th percentile (${x.ok} of ${x.n} worked)`);
}

/** The targets' cards. @param {any[]} cards @param {(id:string)=>string} name @param {any[]} targets the campaign's */
export function cardsSection(cards, name, targets) {
  return h('section', { class: 'st-cards', 'aria-labelledby': 'st-cards-h' },
    h('h3', { id: 'st-cards-h', class: 'sr-only', text: 'Targets in this campaign' }),
    ...cards.map((c) => {
      const t = targets.find((x) => x.id === c.target);
      const classes = Object.entries(c.classes).sort((a, b) => b[1] - a[1]).map(([k, n]) => `${n} ${classShort(k)}`).join(' · ');
      const pill = c.failed ? h('span', { class: 'pill err', text: `${c.failed} failed` })
        : c.config ? h('span', { class: 'pill', text: 'Refused' }) : h('span', { class: 'pill on', text: 'No failures' });
      const worst = c.prefixes.find((/** @type {any} */ p) => p.failed);
      return h('article', { class: 'st-card sp-body', 'aria-labelledby': `st-card-${c.target}` },
        h('div', { class: 'st-card-head' }, h('h4', { id: `st-card-${c.target}`, text: name(c.target) }), pill),
        h('p', {}, h('b', { text: 'Fresh connections ' }), `${c.cold_failed} of ${c.cold_n} failed${c.cold_n ? ` (${Math.round(1000 * c.cold_failed / c.cold_n) / 10}%)` : ''}`),
        classes ? h('p', { class: 'muted small', text: classes }) : null,
        h('p', {}, h('b', { text: 'Connect ' }), pct(c.connect_ms)),
        h('p', {}, h('b', { text: 'Encryption ' }), pct(c.tls_ms)),
        h('p', {}, h('b', { text: 'First byte ' }), pct(c.ttfb_ms)),
        rate(c.down, 'Download'), rate(c.up, 'Upload'),
        !c.down && !c.up ? h('p', { class: 'muted small', text: t?.mode === 'bucket' ? 'No transfers yet.' : 'Connection checks only (no keys).' }) : null,
        c.prefix_count ? h('p', { class: 'muted small', text: `${c.prefix_count} provider address range${c.prefix_count === 1 ? '' : 's'}${worst ? `; ${worst.failed} failure${worst.failed === 1 ? '' : 's'} on ${edge(worst.prefix)}` : ''}` }) : null);
    }));
}

/** The traceroutes, newest first. @param {any[]} traces @param {(id:string)=>string} name */
export function tracesSection(traces, name) {
  if (!traces.length) return null;
  return h('section', { class: 'st-traces sp-body', 'aria-labelledby': 'st-traces-h' },
    h('h3', { id: 'st-traces-h', text: 'Routes to the providers' }),
    h('p', { class: 'muted small', text: 'Traced after the first round and after a connection failure, at most once per address range every 10 minutes. Routers on your network and your provider’s are named, not addressed.' }),
    h('ul', { class: 'st-trace-list' }, ...traces.slice().reverse().map((t) => h('li', {},
      h('b', { text: `${name(t.target)} · ${new Date(t.at).toLocaleString([], { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' })}` }),
      ` (${t.trigger === 'failure' ? 'after a failure' : 'first round'}) to ${edge(t.edge_prefix)}`,
      t.route_changed ? h('span', { class: 'pill err', text: 'route changed' }) : null,
      h('ol', { class: 'st-hops' }, ...t.hops.map((/** @type {any} */ hp) => h('li', {
        text: hp.ip ? `${hp.ip}${hp.asn ? ` · AS${hp.asn}${hp.owner ? ` ${hp.owner}` : ''}` : ''}` : hp.owner ? `${hp.owner}${hp.asn ? ` (AS${hp.asn})` : ''}` : '* (no answer)',
      })), h('li', { text: `the provider, ${edge(t.edge_prefix)}` }))))));
}

/** Copy report (Markdown, hosts only when ticked) and Download CSV, with a status line under them. @param {number} id */
export function reportSection(id) {
  const hosts = /** @type {HTMLInputElement} */ (h('input', { type: 'checkbox', id: 'st-rep-hosts' }));
  const ta = /** @type {HTMLTextAreaElement} */ (h('textarea', { class: 'st-rep-text', rows: '8', readonly: true, hidden: true, 'aria-label': 'The report' }));
  const say = h('p', { class: 'muted small st-rep-say', role: 'status' });
  const copy = async () => {
    say.textContent = ''; // "Copied." again is announced again
    let text;
    try {
      const res = await fetch('api/storage/report?' + new URLSearchParams({ campaign: String(id), format: 'md', hosts: hosts.checked ? '1' : '0' }), { credentials: 'same-origin', cache: 'no-store' });
      if (!res.ok) throw new Error(String(res.status));
      text = await res.text();
    } catch { say.textContent = 'Couldn’t get the report. Try again.'; return; }
    copyText(text, ta, say);
  };
  return h('details', { class: 'sp-before st-report' }, h('summary', { text: 'Copy report or download CSV' }),
    h('div', { class: 'st-check-row' }, hosts, h('label', { for: 'st-rep-hosts', text: ' Include endpoint hosts and bucket names' })),
    h('p', { class: 'muted small', text: 'Off by default: host names can contain account IDs. The report names your routers and your provider’s, without their addresses; your internet address is never included.' }),
    h('div', { class: 'sp-acts' },
      h('button', { type: 'button', class: 'btn', text: 'Copy report', onclick: copy }),
      h('a', { class: 'btn', href: 'api/storage/report?' + new URLSearchParams({ campaign: String(id), format: 'csv' }), download: `fyisp-storage-${id}.csv`, text: 'Download CSV' })),
    say, ta);
}

