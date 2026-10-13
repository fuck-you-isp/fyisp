// fyisp dashboard. Plain ES module, no build step. uPlot is loaded as a
// classic deferred script before this module runs (window.uPlot).
// @ts-check

/** @typedef {{id:string,title:string}} Group */
/** @typedef {{name:string,group:string,kinds:string[],interval_ms:number,layer?:string,trace?:boolean,provider?:string,provider_title?:string,provider_kind?:string,city?:string,country?:string,geo?:string,tags?:string[],home?:string}} TargetInfo */
/** @typedef {{target:string,kind:string,ratio:number,now_ms:number,normal_ms:number}} SlowItem */
/** @typedef {{kind:string,since?:string,summary?:string,targets?:string[],evidence?:Object<string,number>,slow?:SlowItem[],quiet?:boolean}} Verdict */
/** @typedef {{id:number,start:string,end?:string,kind:string,summary:string,targets?:string[],peak_loss:number}} Incident */
/** @typedef {{target:string,kind:string,interval:number,mean:(number|null)[],min:(number|null)[],max:(number|null)[],n:number[],lost:number[],gap:number[],lost_by:Object<string,number[]>}} SeriesData */
/** @typedef {{group:Group,tier:string,from:number,to:number,now:number,start:number,step:number,len:number,series:SeriesData[]}} PanelData */

import {
  meta, TOKEN, $, h, storageGet, storageSet, mqLight,
  effectiveTheme, cssCache, css, rgba, shade, fmtMs, fmtPct, fmtTime,
  fmtWhen, fmtDur, fmtStep, fmtInt, median, fetchJSON, tip, showTip,
  hideTip, whyButton, addAdmin, toLocalInput, fromLocalInput,
  useStatus, hatch, resetHatch, smooth,
} from './core.js';
import { Home, firstHome } from './home.js';
import { Speed } from './speed.js';

const uPlot = /** @type {any} */ (window).uPlot;

const RANGES = [['5m', 300], ['30m', 1800], ['1h', 3600], ['6h', 21600], ['24h', 86400], ['48h', 172800], ['7d', 604800], ['30d', 2592000], ['90d', 7776000]];
// Probe kinds, main kind first: shipped profiles measure every service with
// a TCP connect; HTTPS and ICMP appear only where a profile asks for them
// (the network path pings, --config files may request either).
const KINDS = ['tcp', 'https', 'icmp'];
const KIND_LABEL = { https: 'HTTPS', tcp: 'TCP', icmp: 'ICMP', udp: 'UDP' };
const KIND_TITLE = { tcp: 'TCP: time to open a connection', https: 'HTTPS: request sent → first response byte', icmp: 'ICMP echo (ping)', udp: 'UDP (STUN): median time to answer a burst of 5 requests' };
const KIND_DASH = { tcp: [], https: [6, 3], icmp: [1.5, 3], udp: [] };
// Every kind a chart can draw: the header's kinds plus UDP (STUN), which
// only the call-path panels show.
const CHART_KINDS = [...KINDS, 'udp'];
const REASONS = ['timeout', 'refused', 'reset', 'unreachable', 'dns', 'tls', 'http', 'no_network', 'other'];
const REASON_LABEL = { timeout: 'timeout', refused: 'refused', reset: 'reset', unreachable: 'unreachable', dns: 'DNS', tls: 'TLS', http: 'HTTP error', no_network: 'no network', other: 'other' };
const REFRESH_MS = 15000;
const VERDICT_MS = 5000;
const SYNC_KEY = 'fyisp';
const PATH_GROUP = 'path';

// Verdict kinds: headline, short badge, and what it means (the "?" tooltip).
/** @type {Object<string,{label:string,short:string,why:string}>} */
const VERDICTS = {
  ok: { label: 'All good', short: 'OK', why: 'Your router, your internet provider and the internet are all answering normally.' },
  warming_up: { label: 'Warming up', short: 'Warming up', why: 'fyisp has just started and needs a couple of minutes of measurements before it can say whose fault a problem is.' },
  lan: { label: 'Home network problem', short: 'Home network', why: 'Your router (the gateway) is not answering reliably, so the problem is most likely inside your home: Wi-Fi, cables or the router itself. Your internet provider looks fine from here.' },
  isp: { label: 'Internet provider problem', short: 'Provider', why: 'Your router answers fine, but the first hop on your internet provider\'s side (the ISP edge) does not. The problem is most likely in your provider\'s network.' },
  upstream: { label: 'Problem further out on the internet', short: 'Internet', why: 'Your router and your provider\'s first hop answer, but much of the internet (including the big anycast resolvers) does not. That is either your provider\'s links to the rest of the internet, or a wider outage.' },
  dns: { label: 'Name lookup (DNS) problem', short: 'Name lookup', why: 'The network path works, but name lookups are failing widely. The DNS resolver you use (often your router\'s or your provider\'s) is the likely culprit.' },
  service: { label: 'Some services aren\'t answering', short: 'One service', why: 'Your connection looks fine; only some services or regions aren\'t answering from here. That is most likely on their side, though a region can also be unreachable through your provider\'s routes.' },
  no_network: { label: 'No network', short: 'Offline', why: 'The device running fyisp has no route to the internet: Wi-Fi off, cable unplugged, or no network configured.' },
  udp_blocked: { label: 'Calls blocked (UDP)', short: 'Calls blocked', why: 'Your connection works, but UDP, which call audio and video use, gets no answers from several independent call services, not even on port 53. A firewall on this network (or on this computer) is blocking it. Calls fall back to slower TCP relays or fail.' },
  stun_blocked: { label: 'Call ports blocked (STUN)', short: 'Calls blocked', why: 'Your connection works and UDP gets through on port 53, but the ports calls use (STUN, e.g. 3478 and 19302) get no answers from several call services. A firewall is blocking them; calls may fall back to slower relays.' },
  udp_degraded: { label: 'Call quality degraded', short: 'Calls choppy', why: 'Your connection looks fine for browsing, but the UDP path call audio and video use is losing packets or jittery towards several call services. Calls may sound choppy.' },
};
// Shown instead of the label for a long-standing block (the verdict's quiet flag).
const QUIET_LABEL = { udp_blocked: 'This network blocks UDP', stun_blocked: 'This network blocks call ports' };
// The network path, left to right. key is the evidence prefix (<key>_loss, <key>_rtt_ms).
const LAYERS = [
  { id: 'gateway', key: 'gateway', label: 'Gateway', why: 'Your router: the first thing every packet goes through.' },
  { id: 'isp-edge', key: 'edge', label: 'ISP edge', why: 'The first hop on your ISP\'s side of the line.' },
  { id: 'anycast', key: 'anycast', label: 'Internet (anycast)', why: 'Big anycast resolvers (Cloudflare, Google) that are close to almost every ISP.' },
  { id: 'services', key: 'services', label: 'Services', why: 'The services and cloud regions in the panels below.' },
];
// Which layer each verdict kind blames.
/** @type {Object<string,string>} */
const BLAME = { lan: 'gateway', isp: 'isp-edge', upstream: 'anycast', dns: 'services', service: 'services' };

/**
 * Page state; everything but band lives in the URL hash. view: 'h' (Home),
 * 'calls' (Calls), 'o' (Overview), 'c' (charts) or 'speed' (Speed), null for Home; focus: a target shown alone
 * on its chart; os/oq/og/op/oc/om: the Overview's sort, filter text, region,
 * problems-only, heatmap cell and heatmap mode; ot: the Overview's Home-tile filter;
 * sr/sd/snf/ssw: Speed's selected row, span in days, "leave out backup servers" and the full test (sweep=1).
 * @type {{range:string, from:number|null, to:number|null, kinds:Set<string>, log:boolean, inv:string|null, band:boolean,
 *   view:string|null, focus:string|null, os:string, oq:string, og:string, op:boolean, oc:string, om:string, ot:string, sr:string, sd:number, snf:boolean, ssw:boolean}}
 */
const state = { range: '30m', from: null, to: null, kinds: new Set(['tcp']), log: false, inv: null, band: true,
  view: null, focus: null, os: '', oq: '', og: '', op: false, oc: '', om: '', ot: '', sr: '', sd: 7, snf: false, ssw: false };
/** @type {any} */
let status = null;
/** The kinds the header offers, main kind first: those of the profile's non-path targets (TCP alone for every shipped profile). */
let kindsOffered = ['tcp'];
function offeredKinds(/** @type {TargetInfo[]} */ targets) {
  const icmpOff = status && status.caps && status.caps.icmp === 'unavailable';
  const ks = KINDS.filter((k) => !(k === 'icmp' && icmpOff) && targets.some((t) => !t.layer && t.kinds.includes(k)));
  return ks.length ? ks : ['tcp'];
}
/** Keeps the selected kinds to the offered ones, with the main one when none is left. */
function fixKinds() {
  for (const k of [...state.kinds]) if (!kindsOffered.includes(k)) state.kinds.delete(k);
  if (!state.kinds.size) state.kinds.add(kindsOffered[0]);
}
/** @type {Panel[]} */
let panels = [];
// Profiles with more panels than this load only the panels near the viewport
// (big catalog profiles have 50+ panels).
const LAZY_PANELS = 12;
// At most this many panel requests in flight at once.
const PANEL_CONCURRENCY = 4;
let panelSlots = PANEL_CONCURRENCY;
/** @type {(() => void)[]} */
const panelWaiters = [];
async function panelSlot() {
  if (panelSlots > 0) { panelSlots--; return; }
  await new Promise((r) => panelWaiters.push(() => r(undefined)));
}
function panelRelease() {
  const next = panelWaiters.shift();
  if (next) next(); else panelSlots++;
}
let lastRefresh = 0;
let refreshing = false;
/** @type {Verdict|null} */
let verdict = null;
let lastVerdict = 0;
/** @type {Incident[]} */
let incidents = [];
let incidentsLoaded = false;

// ---------- theme ----------

function applyTheme(/** @type {string|null} */ t) {
  if (t === 'light' || t === 'dark') document.documentElement.dataset.theme = t;
  else delete document.documentElement.dataset.theme;
  const b = $('#theme');
  const next = effectiveTheme() === 'dark' ? 'light' : 'dark';
  b.textContent = '◐';
  b.setAttribute('aria-label', `Switch to ${next} theme`);
  b.title = `Switch to ${next} theme`;
}
function themeChanged() {
  cssCache.clear();
  resetHatch();
  for (const p of panels) p.render();
  renderLayers();
  if (inv && state.inv) { inv.sig = ''; inv.render(); }
  calls?.render();
  home?.themeChanged();
}

// ---------- colours ----------

// Colour follows the target's position in its group (never its rank on
// screen). Beyond 8 targets the same 8 hues repeat at other lightness steps;
// the legend, the tooltip and click-to-isolate carry identity.
const SHADES = [0, 0.35, -0.3, 0.6, -0.5];
function seriesColor(/** @type {number} */ i) {
  const base = css(`--s${(i % 8) + 1}`) || '#3987e5';
  return shade(base, SHADES[Math.floor(i / 8) % SHADES.length]);
}
function reasonColor(/** @type {string} */ r) { return css(`--r-${r}`) || '#999999'; }

// Glossary for Details: the short explanation of each technical term (ux-content §4).
// The same text is in the footer's Glossary; term() marks a word with it as a hover hint.
/** @type {Object<string,string>} */
const GLOSSARY = {
  mos: 'Call quality score from 1 (unusable) to 4.5 (excellent), estimated from delay, dropouts and jitter. Good ≥ 4.0, Fair ≥ 3.6, Poor ≥ 3.1.',
  jitter: 'How unevenly packets arrive. Steady below 10 ms; noticeable from 30 ms, when call apps add delay to smooth it out.',
  loss: 'Share of test messages that never came back. Above about 5%, people on a call start to hear gaps.',
  rtt: 'Time for a message to reach a server and come back, in milliseconds. Under 150 ms feels instant on a call.',
  stun: 'A tiny “what’s my address?” request that call apps send before a call. fyisp uses it to test the path call audio takes.',
  udp: 'The fast, no-retry way call audio and video travel. If it’s blocked, calls fall back to slower routes.',
  anycast: 'Big services (Cloudflare and Google DNS) that answer from a site near almost every provider. If they’re fine, the wider internet is fine.',
  'isp edge': 'The first piece of your internet provider’s equipment past your router.',
  p95: '95 out of 100 measurements were faster than this. Shows the slow moments the median hides.',
  bursts: '5 STUN requests sent quickly, 20 ms apart, like call audio packets. Loss and jitter are measured per burst.',
};
/** A technical word with its glossary text as a hint (the footer's Glossary has it for everyone). */
function term(/** @type {string} */ key, /** @type {string} */ text = key) {
  const tip = GLOSSARY[key.toLowerCase()];
  return tip ? h('abbr', { class: 'gl', title: tip, text }) : text;
}
function verdictColor(/** @type {string} */ kind) { return css(`--v-${kind}`) || css('--muted') || '#898781'; }

// ---------- URL hash state ----------

function readHash() {
  const p = new URLSearchParams(location.hash.slice(1));
  const r = p.get('r');
  if (r && RANGES.some(([k]) => k === r)) state.range = r;
  const f = Number(p.get('from')), t = Number(p.get('to'));
  if (f > 0 && t > f) { state.from = f; state.to = t; } else { state.from = state.to = null; }
  state.log = p.get('o') === '1';
  const ti = p.get('t');
  state.inv = ti && traceOn ? ti : null;
  const k = p.get('k');
  if (k != null) {
    const ks = k.split(',').filter((x) => KINDS.includes(x));
    state.kinds = new Set(ks.length ? ks : [kindsOffered[0]]);
  }
  fixKinds();
  const v = p.get('v');
  state.view = v === 'h' || v === 'o' || v === 'c' || v === 'calls' || v === 'speed' ? v : null;
  const ot = p.get('ot');
  state.ot = ot && /^[a-z]+$/.test(ot) ? ot : '';
  state.focus = p.get('f') || null;
  state.os = p.get('os') || '';
  state.oq = p.get('oq') || '';
  const og = p.get('og');
  state.og = og && GEOS.includes(og) ? og : '';
  state.op = p.get('op') === '1';
  state.oc = p.get('oc') || '';
  const om = p.get('om');
  state.om = om === 'r' || om === 'ms' ? om : '';
  // Speed: the selected row ('r:<region>' or 's:<server>'), the span, backup servers left out.
  const sr = p.get('sr');
  state.sr = sr && /^[rs]:[A-Za-z0-9._:-]{1,80}$/.test(sr) ? sr : '';
  state.sd = p.get('sd') === '30' ? 30 : 7;
  state.snf = p.get('snf') === '1';
  state.ssw = p.get('sweep') === '1';
}
/** The last hash the page applied (hashchange and popstate both fire on Back). */
let lastHash = '';
function hashString() {
  // Home's hash is empty: no Details range or kinds.
  if (!state.view || state.view === 'h') {
    if (!state.inv && !(state.focus && focusPanel())) return '';
  }
  const p = new URLSearchParams();
  p.set('r', state.range);
  if (state.from != null && state.to != null) { p.set('from', String(state.from)); p.set('to', String(state.to)); }
  p.set('k', [...state.kinds].join(','));
  if (state.log) p.set('o', '1');
  if (state.inv) p.set('t', state.inv);
  if (state.view) p.set('v', state.view);
  if (state.ot) p.set('ot', state.ot);
  if (state.focus) p.set('f', state.focus);
  if (state.os) p.set('os', state.os);
  if (state.oq) p.set('oq', state.oq);
  if (state.og) p.set('og', state.og);
  if (state.op) p.set('op', '1');
  if (state.oc) p.set('oc', state.oc);
  if (state.om) p.set('om', state.om);
  if (state.sr) p.set('sr', state.sr);
  if (state.sd === 30) p.set('sd', '30');
  if (state.snf) p.set('snf', '1');
  if (state.ssw) p.set('sweep', '1');
  return '#' + p.toString();
}
function writeHash() {
  const s = hashString();
  if (location.hash !== s) history.replaceState(history.state, '', s || location.pathname + location.search);
  lastHash = location.hash;
}
function isRelative() { return state.from == null; }
function rangeSecs() { return (RANGES.find(([k]) => k === state.range) || RANGES[1])[1]; }
function queryRange() {
  if (state.from != null && state.to != null) return { from: String(state.from), to: String(state.to) };
  return { from: 'now-' + state.range, to: 'now' };
}

// ---------- panels ----------

class Panel {
  /** @param {Group} group @param {TargetInfo[]} targets */
  constructor(group, targets) {
    this.group = group;
    this.targets = targets;
    /** @type {Set<string>} */
    this.hidden = new Set();
    /** @type {PanelData|null} */
    this.data = null;
    /** @type {any} */
    this.u = null;
    this.sig = '';
    /** @type {AbortController|null} */
    this.ctl = null;
    /** @type {{si:number,target:string,kind:string,role:string,sd:SeriesData|null,color:string}[]} */
    this.smap = [];
    this.view = [0, 1];
    this.hover = false;
    this.colors = new Map(targets.map((t, i) => [t.name, i]));
    this.isPath = group.id === PATH_GROUP;
    // visible: near the viewport (always true unless lazy). stale: needs a load once visible.
    this.visible = true;
    this.stale = true;

    this.meta = h('span', { class: 'meta' });
    this.csv = h('a', { href: '#', download: '', title: 'Download this panel as CSV', text: 'CSV' });
    this.chartEl = h('div', { class: 'chart' });
    this.emptyEl = h('div', { class: 'empty', text: 'Loading…' });
    this.chartEl.append(this.emptyEl);
    this.strip = /** @type {HTMLCanvasElement} */ (h('canvas', { class: 'strip', 'aria-hidden': 'true' }));
    this.stripLegend = h('div', { class: 'strip-legend' });
    this.legend = h('ul', { class: 'legend', 'aria-label': `${group.title} targets` });
    this.layers = this.isPath ? h('ol', { class: 'layers', 'aria-label': 'Network path status' }) : null;
    const pathTrace = this.isPath && traceOn && defaultTraceTarget()
      ? h('button', { class: 'tbtn', type: 'button', title: 'Trace the path hop by hop: where does loss start?', text: 'Investigate path', onclick: () => openInvestigate(defaultTraceTarget()) })
      : null;
    this.el = h('section', { class: this.isPath ? 'panel path' : 'panel', 'aria-label': group.title, id: 'panel-' + group.id },
      h('div', { class: 'panel-head' }, h('h2', { text: group.title }), this.meta, pathTrace, this.csv),
      this.layers,
      this.chartEl,
      h('div', { class: 'strip-wrap' }, this.strip),
      this.stripLegend,
      this.legend);
    this.strip.addEventListener('mousemove', (e) => this.stripHover(e));
    this.strip.addEventListener('mouseleave', hideTip);
    new ResizeObserver(() => this.resize()).observe(this.chartEl);
  }

  height() { return this.isPath ? 200 : this.targets.length > 12 ? 300 : 240; }

  /**
   * Probe kinds this panel shows. The network path shows its ICMP (or TCP
   * when ICMP is unavailable), and follows the header only when ICMP is
   * selected there, which needs a profile with ICMP services.
   * @returns {Set<string>}
   */
  kinds() {
    // A panel of UDP (STUN) targets only shows their UDP series.
    if (this.targets.length && this.targets.every((t) => t.kinds.includes('udp'))) return new Set(['udp']);
    if (!this.isPath) return state.kinds;
    const avail = new Set(this.targets.flatMap((t) => t.kinds));
    const icmpOff = status && status.caps && status.caps.icmp === 'unavailable';
    const sel = [...state.kinds].filter((k) => avail.has(k) && !(k === 'icmp' && icmpOff));
    if (sel.includes('icmp')) return new Set(sel);
    for (const k of ['icmp', 'tcp', 'https']) if (avail.has(k) && !(k === 'icmp' && icmpOff)) return new Set([k]);
    return new Set(avail);
  }

  /** The main kind on this chart (drawn solid, with a min–max band and the normal band): the first shown of TCP, HTTPS, ICMP; none on the path chart. */
  mainKind() {
    if (this.isPath) return null;
    const kinds = this.kinds();
    return KINDS.find((k) => kinds.has(k)) || (kinds.has('udp') ? 'udp' : null);
  }
  width() { return Math.max(200, this.chartEl.clientWidth); }

  async load() {
    this.ctl?.abort();
    const ctl = this.ctl = new AbortController();
    this.stale = false;
    const { from, to } = queryRange();
    const points = Math.min(2000, Math.max(50, Math.round(this.width())));
    const q = new URLSearchParams({ group: this.group.id, from, to, points: String(points) });
    this.csv.setAttribute('href', 'api/panel.csv?' + q);
    this.chartEl.classList.add('loading');
    try {
      await panelSlot();
      /** @type {PanelData} */
      let d;
      try {
        if (ctl.signal.aborted) return;
        d = await fetchJSON('api/panel?' + q, { signal: ctl.signal });
      } finally {
        panelRelease();
      }
      if (ctl.signal.aborted) return;
      this.data = d;
      this.render();
    } catch (e) {
      if (/** @type {any} */ (e).name === 'AbortError') return;
      this.emptyEl.textContent = `Could not load: ${/** @type {Error} */ (e).message}`;
      this.emptyEl.hidden = false;
    } finally {
      if (this.ctl === ctl) this.chartEl.classList.remove('loading');
    }
  }

  /** The target's "×N normal" ratio for the selected kinds (main kind first). */
  ratioFor(/** @type {string} */ name, /** @type {string[]} */ kinds) {
    for (const k of KINDS) {
      if (!kinds.includes(k)) continue;
      const b = baselines.get(name + '\0' + k);
      if (b && typeof b.ratio_now === 'number' && isFinite(b.ratio_now)) return { r: b.ratio_now, b };
    }
    return null;
  }

  /** The baseline band (median..p95) of the main kind's visible targets: at most BAND_MAX, else only the slow ones. */
  drawBand(/** @type {any} */ u) {
    const mk = this.mainKind();
    if (!baseOn || !state.band || !baselines.size || !mk) return;
    let list = this.targets
      .filter((t) => !this.hidden.has(t.name) && t.kinds.includes(mk))
      .map((t) => ({ t, b: baselines.get(t.name + '\0' + mk) }))
      .filter((x) => x.b);
    if (list.length > BAND_MAX) list = list.filter((x) => (x.b?.ratio_now || 0) >= SLOW_RATIO);
    if (!list.length) return;
    const ctx = u.ctx;
    const { left, top, width, height } = u.bbox;
    const dpr = window.devicePixelRatio || 1;
    ctx.save();
    ctx.beginPath();
    ctx.rect(left, top, width, height);
    ctx.clip();
    for (const { t, b } of list) {
      if (!b) continue;
      const c = seriesColor(/** @type {number} */ (this.colors.get(t.name)));
      const y0 = u.valToPos(b.median_ms, 'y', true), y1 = u.valToPos(Math.max(b.p95_ms, b.median_ms), 'y', true);
      ctx.fillStyle = rgba(c, (b.ratio_now || 0) >= SLOW_RATIO ? 0.14 : 0.09);
      ctx.fillRect(left, y1, width, Math.max(dpr, y0 - y1));
      ctx.strokeStyle = rgba(c, 0.45);
      ctx.lineWidth = dpr;
      ctx.setLineDash([2 * dpr, 4 * dpr]);
      ctx.beginPath();
      ctx.moveTo(left, Math.round(y0) + 0.5);
      ctx.lineTo(left + width, Math.round(y0) + 0.5);
      ctx.stroke();
    }
    ctx.restore();
  }

  /** Series layout: per target, per selected kind a mean line; the main kind adds a min–max band. */
  layout() {
    const d = /** @type {PanelData} */ (this.data);
    /** @type {Map<string, SeriesData>} */
    const byKey = new Map(d.series.map((s) => [s.target + '\0' + s.kind, s]));
    const smap = [];
    const kinds = this.kinds();
    const mk = this.mainKind();
    let si = 1;
    for (const t of this.targets) {
      const color = seriesColor(/** @type {number} */ (this.colors.get(t.name)));
      for (const k of CHART_KINDS) {
        if (!kinds.has(k) || !t.kinds.includes(k)) continue;
        const sd = byKey.get(t.name + '\0' + k) || null;
        smap.push({ si: si++, target: t.name, kind: k, role: 'mean', sd, color });
        if (k === mk) {
          smap.push({ si: si++, target: t.name, kind: k, role: 'max', sd, color });
          smap.push({ si: si++, target: t.name, kind: k, role: 'min', sd, color });
        }
      }
    }
    return smap;
  }

  render() {
    const d = this.data;
    if (!d) return;
    this.view = isRelative() ? [d.from / 1000, d.to / 1000] : [/** @type {number} */ (state.from) / 1000, /** @type {number} */ (state.to) / 1000];
    this.meta.textContent = `${d.tier === 'raw' ? 'raw' : 'hourly'} · ${fmtStep(d.step)} buckets`;
    const smap = this.layout();
    const xs = new Float64Array(d.len);
    for (let i = 0; i < d.len; i++) xs[i] = (d.start + i * d.step) / 1000;
    const nulls = () => new Array(d.len).fill(null);
    const data = [xs, ...smap.map((m) => (m.sd ? m.sd[/** @type {'mean'|'min'|'max'} */ (m.role)] : nulls()))];
    const sig = effectiveTheme() + '|' + smap.map((m) => m.target + m.kind + m.role).join(',');
    this.smap = smap;
    if (!this.u || sig !== this.sig) {
      this.u?.destroy();
      this.u = this.makeChart(smap, data);
      this.sig = sig;
    } else {
      this.u.setData(data);
    }
    this.applyHidden();
    const any = d.series.some((s) => s.n.some((n) => n > 0) || s.lost.some((n) => n > 0));
    this.emptyEl.textContent = any ? '' : (status && status.ready === 0 ? 'Warming up: waiting for the first samples…' : 'No data in this range');
    this.emptyEl.hidden = any;
    this.renderLegend();
    this.drawStrip();
    if (this.isPath) this.renderLayers();
  }

  /** @param {any[]} smap @param {any[]} data */
  makeChart(smap, data) {
    const ink2 = css('--ink-2'), grid = css('--grid'), axis = css('--axis');
    const series = [{}];
    const bands = [];
    // One kind on screen needs no dash to tell kinds apart (the path panel's ICMP).
    const solo = new Set(smap.map((m) => m.kind)).size === 1;
    const mk = this.mainKind();
    for (const m of smap) {
      if (m.role === 'mean') {
        series.push({
          label: `${m.target} ${KIND_LABEL[/** @type {'https'} */ (m.kind)]}`,
          stroke: m.color, width: m.kind === mk ? 1.5 : 1.25, dash: solo ? [] : KIND_DASH[/** @type {'https'} */ (m.kind)],
          spanGaps: false, points: { show: false },
        });
      } else {
        series.push({ label: `${m.target} ${m.role}`, stroke: 'transparent', width: 0, spanGaps: false, points: { show: false } });
      }
    }
    for (let i = 0; i < smap.length; i++) {
      if (smap[i].role === 'max') bands.push({ series: [smap[i].si, smap[i + 1].si], fill: rgba(smap[i].color, 0.10) });
    }
    const self = this;
    const font = '11px system-ui, -apple-system, "Segoe UI", sans-serif';
    const opts = {
      width: this.width(),
      height: this.height(),
      padding: [8, 8, 0, 0],
      legend: { show: false },
      scales: {
        x: { time: true, range: () => self.view },
        y: { range: (/** @type {any} */ u, /** @type {number} */ min, /** @type {number} */ max) => [0, max > 0 ? max * 1.08 : 10] },
      },
      axes: [
        { stroke: ink2, font, grid: { stroke: grid, width: 1 }, ticks: { stroke: axis, width: 1, size: 4 } },
        { stroke: ink2, font, size: 48, label: 'ms', labelSize: 14, labelFont: font, grid: { stroke: grid, width: 1 }, ticks: { stroke: axis, width: 1, size: 4 } },
      ],
      series,
      bands,
      cursor: {
        sync: { key: SYNC_KEY, setSeries: false },
        drag: { x: true, y: false, setScale: false },
        points: { show: false },
        bind: {
          dblclick: () => () => { resetZoom(); return null; },
        },
      },
      hooks: {
        setSelect: [onChartSelect],
        setCursor: [(/** @type {any} */ u) => self.onCursor(u)],
        draw: [(/** @type {any} */ u) => { drawSpeedWindows(u); self.drawBand(u); self.drawIncidents(u); self.drawStrip(); }],
      },
    };
    this.emptyEl.remove();
    const u = new uPlot(opts, data, this.chartEl);
    this.chartEl.append(this.emptyEl);
    u.over.addEventListener('mouseenter', () => { self.hover = true; });
    u.over.addEventListener('mouseleave', () => { self.hover = false; hideTip(); });
    return u;
  }

  resize() {
    if (!this.u) return;
    const w = this.width();
    if (Math.abs(w - this.u.width) > 1) this.u.setSize({ width: w, height: this.height() });
  }

  applyHidden() {
    if (!this.u) return;
    this.u.batch(() => {
      for (const m of this.smap) {
        const show = !this.hidden.has(m.target);
        if (this.u.series[m.si].show !== show) this.u.setSeries(m.si, { show });
      }
    });
  }

  /** Series that count for legend, strip and tooltip: selected kinds, visible targets. */
  visibleSeries() {
    if (!this.data) return [];
    const kinds = this.kinds();
    return this.data.series.filter((s) => kinds.has(s.kind) && !this.hidden.has(s.target));
  }

  renderLegend() {
    const d = /** @type {PanelData} */ (this.data);
    const items = [];
    const sel = this.kinds();
    for (const t of this.targets) {
      const kinds = CHART_KINDS.filter((k) => sel.has(k) && t.kinds.includes(k));
      if (!kinds.length) continue;
      const ss = d.series.filter((s) => s.target === t.name && kinds.includes(s.kind));
      let n = 0, lost = 0;
      for (const s of ss) for (let i = 0; i < d.len; i++) { n += s.n[i]; lost += s.lost[i]; }
      const cur = kinds.map((k) => {
        const s = ss.find((x) => x.kind === k);
        let v = null;
        if (s) for (let i = d.len - 1; i >= Math.max(0, d.len - 4); i--) if (s.mean[i] != null) { v = s.mean[i]; break; }
        return (kinds.length > 1 ? KIND_LABEL[/** @type {'https'} */ (k)][0] + ' ' : '') + fmtMs(v);
      }).join(' / ') + ' ms';
      const lossF = n + lost > 0 ? lost / (n + lost) : NaN;
      const rb = this.ratioFor(t.name, kinds);
      const slow = rb && rb.r >= SLOW_RATIO ? h('span', {
        class: 'xnorm' + (rb.r >= 3 ? ' bad' : ''),
        title: `${t.name} ${KIND_LABEL[/** @type {'https'} */ (rb.b.kind)]}: ${fmtMs(rb.b.now_ms)} ms over the last 5 minutes vs ${fmtMs(rb.b.median_ms)} ms normal (median${rb.b.hour_of_day ? ' at this hour' : ''} over the past week)`,
        text: `${fmtRatio(rb.r)} normal`,
      }) : null;
      const sw = h('span', { class: 'sw' });
      sw.style.background = seriesColor(/** @type {number} */ (this.colors.get(t.name)));
      const li = h('li', {
        class: this.hidden.has(t.name) ? 'off' : '',
        title: `${t.name}: current RTT ${cur}, loss ${fmtPct(lossF)} over the range. Click to isolate, Ctrl/Cmd-click to toggle.`,
        tabindex: '0', role: 'button', 'aria-pressed': String(!this.hidden.has(t.name)),
        onclick: (/** @type {MouseEvent} */ e) => this.legendClick(t.name, e.ctrlKey || e.metaKey),
        onkeydown: (/** @type {KeyboardEvent} */ e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); this.legendClick(t.name, e.ctrlKey || e.metaKey); } },
      },
      sw,
      h('span', { class: 'name', text: t.name }),
      h('span', { class: 'val', text: cur }),
      slow,
      h('span', { class: 'loss' + (lossF > 0.005 ? ' bad' : ''), text: fmtPct(lossF) + ' loss' }),
      traceOn && t.trace ? traceButton(t.name, 'Trace') : null);
      items.push(li);
    }
    this.legend.replaceChildren(...items);
    // "×2.3 normal" badges need wider legend columns.
    this.legend.classList.toggle('wide', items.some((li) => li.querySelector('.xnorm')));
  }

  /** @param {string} name @param {boolean} toggle */
  legendClick(name, toggle) {
    const all = this.targets.map((t) => t.name);
    if (toggle) {
      if (this.hidden.has(name)) this.hidden.delete(name); else this.hidden.add(name);
    } else {
      const isolated = !this.hidden.has(name) && all.every((n) => n === name || this.hidden.has(n));
      this.hidden = isolated ? new Set() : new Set(all.filter((n) => n !== name));
    }
    this.applyHidden();
    this.renderLegend();
    this.drawStrip();
  }

  /** Per-bucket totals over the visible series. */
  bucketTotals(/** @type {number} */ i) {
    const t = { n: 0, lost: 0, gap: 0, by: /** @type {Object<string,number>} */ ({}) };
    for (const s of this.visibleSeries()) {
      t.n += s.n[i]; t.lost += s.lost[i]; t.gap += s.gap[i];
      for (const r in s.lost_by) t.by[r] = (t.by[r] || 0) + s.lost_by[r][i];
    }
    return t;
  }

  drawStrip() {
    const u = this.u, d = this.data;
    if (!u || !d) return;
    const dpr = window.devicePixelRatio || 1;
    const left = u.bbox.left / dpr, width = u.bbox.width / dpr, H = 22;
    const c = this.strip;
    c.style.left = left + 'px';
    c.style.width = width + 'px';
    if (c.width !== Math.round(width * dpr) || c.height !== Math.round(H * dpr)) {
      c.width = Math.round(width * dpr);
      c.height = Math.round(H * dpr);
    }
    const g = /** @type {CanvasRenderingContext2D} */ (c.getContext('2d'));
    g.setTransform(dpr, 0, 0, dpr, 0, 0);
    g.clearRect(0, 0, width, H);
    g.fillStyle = css('--grid');
    g.fillRect(0, H - 1, width, 1);
    const present = new Set();
    let anyGap = false;
    const step = d.step / 1000;
    for (let i = 0; i < d.len; i++) {
      const x0v = (d.start / 1000) + i * step;
      let x0 = u.valToPos(x0v, 'x'), x1 = u.valToPos(x0v + step, 'x');
      if (x1 < 0 || x0 > width) continue;
      x0 = Math.max(0, x0); x1 = Math.min(width, x1);
      let w = x1 - x0;
      if (w > 4) w -= 1; // hairline gap between wide buckets
      w = Math.max(1, w);
      const t = this.bucketTotals(i);
      const total = t.n + t.lost + t.gap;
      if (!total) continue;
      let y = H - 1;
      for (const r of REASONS) {
        const v = t.by[r];
        if (!v) continue;
        present.add(r);
        const bh = Math.max(2, (v / total) * (H - 1));
        g.fillStyle = reasonColor(r);
        g.fillRect(x0, y - bh, w, bh);
        y -= bh;
      }
      if (t.gap > 0) {
        anyGap = true;
        const bh = Math.max(2, (t.gap / total) * (H - 1));
        g.fillStyle = /** @type {CanvasPattern} */ (hatch(g));
        g.fillRect(x0, Math.max(0, y - bh), w, Math.min(bh, y));
      }
    }
    const kids = [];
    for (const r of REASONS) if (present.has(r)) kids.push(h('span', {}, h('span', { class: `chip r-${r}` }), REASON_LABEL[/** @type {'dns'} */ (r)]));
    if (anyGap) kids.push(h('span', {}, h('span', { class: 'chip hatch' }), 'not measured'));
    if (!kids.length) kids.push(h('span', { class: 'muted', text: 'No loss in range' }));
    kids.push(...speedLegend(/** @type {[number, number]} */ (this.view)));
    this.stripLegend.replaceChildren(...kids);
  }

  /** @param {MouseEvent} e */
  stripHover(e) {
    const d = this.data, u = this.u;
    if (!d || !u) return;
    const r = this.strip.getBoundingClientRect();
    const v = u.posToVal(e.clientX - r.left, 'x');
    const i = Math.floor((v * 1000 - d.start) / d.step);
    if (i < 0 || i >= d.len) { hideTip(); return; }
    const t = this.bucketTotals(i);
    const total = t.n + t.lost + t.gap;
    const nodes = [h('div', { class: 't', text: `${fmtTime(d.start + i * d.step, d.to - d.from)} · ${fmtStep(d.step)} bucket` })];
    const measured = t.n + t.lost;
    nodes.push(h('div', { text: `Loss: ${fmtPct(measured ? t.lost / measured : NaN)} (${t.lost} of ${measured} samples)` }));
    for (const rr of REASONS) {
      if (!t.by[rr]) continue;
      const sw = h('span', { class: `chip r-${rr}` });
      nodes.push(h('div', { class: 'row' }, sw, `${REASON_LABEL[/** @type {'dns'} */ (rr)]}: ${t.by[rr]}`));
    }
    if (t.gap) nodes.push(h('div', { class: 'row' }, h('span', { class: 'chip hatch' }), `not measured: ${t.gap} (${fmtPct(t.gap / total)})`));
    showTip(nodes, e.clientX, e.clientY);
  }

  /** Tooltip for the hovered chart: nearest target, its RTT or loss reason, bucket loss. */
  onCursor(/** @type {any} */ u) {
    if (!this.hover) return;
    const d = this.data;
    const i = u.cursor.idx;
    if (!d || i == null || u.cursor.left < 0) { hideTip(); return; }
    // Nearest visible mean line to the pointer; fall back to the first visible target.
    let best = null, bestDist = Infinity;
    for (const m of this.smap) {
      if (m.role !== 'mean' || this.hidden.has(m.target) || !m.sd) continue;
      const v = m.sd.mean[i];
      if (v == null) continue;
      const dist = Math.abs(u.valToPos(v, 'y') - u.cursor.top);
      if (dist < bestDist) { bestDist = dist; best = m; }
    }
    const nodes = [h('div', { class: 't', text: fmtTime(d.start + i * d.step, d.to - d.from) })];
    const kinds = this.kinds();
    const target = best ? best.target : (this.targets.find((t) => !this.hidden.has(t.name)) || this.targets[0])?.name;
    if (target) {
      const sw = h('span', { class: 'sw' });
      sw.style.background = seriesColor(/** @type {number} */ (this.colors.get(target)));
      nodes.push(h('div', { class: 'row' }, sw, h('b', { text: target })));
      let n = 0, lost = 0;
      for (const s of d.series) {
        if (s.target !== target || !kinds.has(s.kind)) continue;
        n += s.n[i]; lost += s.lost[i];
        let txt;
        if (s.mean[i] != null) {
          txt = `${fmtMs(s.mean[i])} ms`;
          if (s.min[i] != null && s.max[i] != null && s.n[i] > 1) txt += ` (${fmtMs(s.min[i])}–${fmtMs(s.max[i])})`;
        } else if (s.lost[i] > 0) {
          const rs = REASONS.filter((r) => s.lost_by[r] && s.lost_by[r][i] > 0).map((r) => REASON_LABEL[/** @type {'dns'} */ (r)]);
          txt = `lost: ${rs.join(', ') || 'unknown'}`;
        } else if (s.gap[i] > 0) {
          txt = 'not measured';
        } else {
          txt = 'no data';
        }
        nodes.push(h('div', { class: 'row' }, h('span', { class: 'k', text: KIND_LABEL[/** @type {'https'} */ (s.kind)] }), txt));
      }
      nodes.push(h('div', { class: 'row' }, h('span', { class: 'k', text: 'loss' }), n + lost ? `${fmtPct(lost / (n + lost))} in bucket` : '—'));
      const mk = this.mainKind();
      const nb = mk ? baselines.get(target + '\0' + mk) : null;
      if (nb) nodes.push(h('div', { class: 'row' }, h('span', { class: 'k', text: 'normal' }), `${fmtMs(nb.median_ms)}–${fmtMs(nb.p95_ms)} ms (median–p95)`));
    }
    // Other targets that lost samples in this bucket.
    const lossy = [];
    for (const s of this.visibleSeries()) {
      if (s.target === target || !s.lost[i]) continue;
      const r = REASONS.find((x) => s.lost_by[x] && s.lost_by[x][i] > 0) || 'other';
      lossy.push(`${s.target} ${KIND_LABEL[/** @type {'https'} */ (s.kind)]}: lost (${REASON_LABEL[/** @type {'dns'} */ (r)]})`);
    }
    if (lossy.length) {
      nodes.push(h('div', { class: 'sep' }));
      for (const l of lossy.slice(0, 5)) nodes.push(h('div', { text: l }));
      if (lossy.length > 5) nodes.push(h('div', { class: 'muted', text: `+${lossy.length - 5} more` }));
    }
    const hits = incidentsAt(d.start + i * d.step, d.start + (i + 1) * d.step);
    if (hits.length) {
      nodes.push(h('div', { class: 'sep' }));
      for (const inc of hits.slice(0, 3)) {
        const sw = h('span', { class: 'chip' });
        sw.style.background = verdictColor(inc.kind);
        nodes.push(h('div', { class: 'row inc' }, sw, `incident: ${inc.summary}`));
      }
    }
    nodes.push(...speedTipNodes(d.start + i * d.step, d.start + (i + 1) * d.step));
    const rect = u.over.getBoundingClientRect();
    showTip(nodes, rect.left + u.cursor.left, rect.top + u.cursor.top);
  }

  /** Incidents as faint vertical bands (drawn after the series), with a
   * stronger edge on top, coloured by verdict kind. */
  drawIncidents(/** @type {any} */ u) {
    if (!incidents.length) return;
    const ctx = u.ctx;
    const { left, top, width, height } = u.bbox;
    const vmin = u.scales.x.min, vmax = u.scales.x.max;
    const dpr = window.devicePixelRatio || 1;
    const now = Date.now();
    ctx.save();
    ctx.beginPath();
    ctx.rect(left, top, width, height);
    ctx.clip();
    for (const inc of incidents) {
      const a = Date.parse(inc.start) / 1000, b = (inc.end ? Date.parse(inc.end) : now) / 1000;
      if (!(b >= vmin && a <= vmax)) continue;
      let x0 = u.valToPos(a, 'x', true), x1 = u.valToPos(b, 'x', true);
      if (x1 - x0 < 2 * dpr) { const m = (x0 + x1) / 2; x0 = m - dpr; x1 = m + dpr; }
      const c = verdictColor(inc.kind);
      ctx.fillStyle = rgba(c, 0.12);
      ctx.fillRect(x0, top, x1 - x0, height);
      ctx.fillStyle = rgba(c, 0.75);
      ctx.fillRect(x0, top, x1 - x0, 2 * dpr);
    }
    ctx.restore();
  }

  /** Show only this target (the verdict banner's chips). */
  isolate(/** @type {string} */ name) {
    this.hidden = new Set(this.targets.map((t) => t.name).filter((n) => n !== name));
    this.applyHidden();
    this.renderLegend();
    this.drawStrip();
  }

  /** The layer strip above the path chart: Gateway → ISP edge → Internet → Services. */
  renderLayers() {
    if (!this.layers) return;
    const v = verdict && verdict.kind !== 'unknown' ? verdict : null;
    const ev = (v && v.evidence) || {};
    const blame = v ? blamedLayer(v, ev) : undefined;
    const kinds = this.kinds();
    const items = [];
    for (const L of LAYERS) {
      let stats = null, configured = true;
      if (L.id === 'services') {
        stats = servicesStats();
      } else {
        const names = new Set(this.targets.filter((t) => t.layer === L.id).map((t) => t.name));
        configured = names.size > 0;
        if (configured && this.data) stats = recentStats(this.data, (s) => names.has(s.target) && kinds.has(s.kind));
      }
      const eLoss = ev[L.key + '_loss'], eRtt = ev[L.key + '_rtt_ms'];
      const hasE = typeof eLoss === 'number' && isFinite(eLoss);
      const loss = hasE ? eLoss : stats ? stats.loss : NaN;
      const rtt = typeof eRtt === 'number' && isFinite(eRtt) ? eRtt : stats ? stats.rtt : null;
      let st = 'unknown', text;
      if (!configured) text = 'not set up';
      else if (!hasE && (!stats || stats.ever === 0)) text = this.data ? 'discovering…' : 'loading…';
      else if (!isFinite(loss)) text = 'no recent data';
      else {
        st = loss >= 0.5 ? 'down' : loss >= 0.05 ? 'warn' : 'up';
        text = st !== 'up' ? `${fmtPct(loss)} loss` : rtt != null ? `${fmtMs(rtt)} ms` : 'OK';
      }
      if (v && v.kind === 'no_network') { st = 'down'; text = 'unreachable'; }
      const blamed = blame === L.id;
      if (blamed && v) {
        // The blamed pill agrees with the verdict: never "OK" or a bare RTT.
        const soft = v.kind === 'service' || v.kind === 'dns';
        st = soft ? 'warn' : 'down';
        text = blamedText(v, L, ev, loss, rtt);
      }
      if (items.length) items.push(h('li', { class: 'arrow', 'aria-hidden': 'true', text: '→' }));
      items.push(h('li', { class: `layer st-${st}${blamed ? ' blame' : ''}`, title: `${L.label}: ${L.why}` },
        h('span', { class: 'ldot', 'aria-hidden': 'true' }),
        h('span', { class: 'ln', text: L.label }),
        h('span', { class: 'lv', text }),
        blamed ? h('span', { class: 'lb', text: 'likely cause' }) : null));
    }
    this.layers.replaceChildren(...items);
  }
}

/**
 * The layer a verdict blames. "upstream" is the anycast layer when most of
 * its targets are unhealthy, else the services (most of them are failing,
 * or the anycast layer is unknown).
 * @param {Verdict} v @param {Object<string,number>} ev
 */
function blamedLayer(v, ev) {
  if (v.kind === 'upstream') {
    const acBad = typeof ev.anycast_targets === 'number' && 2 * (ev.anycast_unhealthy || 0) > ev.anycast_targets;
    return acBad ? 'anycast' : 'services';
  }
  return BLAME[v.kind];
}

/**
 * The text of the layer pill the verdict blames, consistent with it:
 * service/dns count the targets the verdict names; path kinds show the loss
 * the engine judged (evidence), or the slow RTT, else "unhealthy".
 * @param {Verdict} v @param {{key:string}} L @param {Object<string,number>} ev
 * @param {number} loss @param {number|null} rtt
 */
function blamedText(v, L, ev, loss, rtt) {
  const n = Array.isArray(v.targets) ? v.targets.length : 0;
  if (v.kind === 'service') return n ? `${n} affected` : 'some affected';
  if (v.kind === 'dns') return n ? `${n} not resolving` : 'lookups failing';
  if (v.kind === 'upstream' && L.key === 'services') {
    const bad = ev.internet_unhealthy, all = ev.internet_targets;
    if (typeof bad === 'number' && typeof all === 'number' && all > 0) return `${bad} of ${all} failing`;
  }
  if (isFinite(loss) && loss >= 0.05) return `${fmtPct(loss)} loss`;
  if (typeof ev[L.key + '_baseline_ms'] === 'number' && rtt != null) return `${fmtMs(rtt)} ms, slow`;
  if (isFinite(loss) && loss > 0) return `${fmtPct(loss)} loss`;
  return 'unhealthy';
}

/**
 * Totals over the matching series: all samples in the range (ever), and
 * loss and mean RTT over the last few buckets that have data.
 * @param {PanelData} d @param {(s: SeriesData) => boolean} pred
 */
function recentStats(d, pred) {
  const ss = d.series.filter(pred);
  let ever = 0, n = 0, lost = 0, rttSum = 0, rttN = 0, used = 0;
  for (const s of ss) for (let i = 0; i < d.len; i++) ever += s.n[i] + s.lost[i];
  for (let i = d.len - 1; i >= 0 && used < 4; i--) {
    let bn = 0, bl = 0;
    for (const s of ss) { bn += s.n[i]; bl += s.lost[i]; }
    if (bn + bl === 0) continue;
    used++; n += bn; lost += bl;
    if (rttN === 0) for (const s of ss) { const m = s.mean[i]; if (m != null) { rttSum += m; rttN++; } }
  }
  return { ever, n, lost, loss: n + lost ? lost / (n + lost) : NaN, rtt: rttN ? rttSum / rttN : null };
}

/** recentStats over every non-path panel (the "Services" layer). */
function servicesStats() {
  let ever = 0, n = 0, lost = 0;
  for (const p of panels) {
    if (p.isPath || !p.data) continue;
    const r = recentStats(p.data, (s) => state.kinds.has(s.kind));
    ever += r.ever; n += r.n; lost += r.lost;
  }
  // The Overview hides the service panels: use its window totals instead.
  if (!ever && ov && ov.data && overviewShown()) {
    for (const r of ov.data.rows) { ever += r.n + r.lost; n += r.n; lost += r.lost; }
  }
  // So does Calls: use the call apps' websites.
  if (!ever && calls && calls.live && callsShown()) {
    for (const a of calls.live.apps) {
      const k = a.web_samples || 0, l = Math.round(k * (a.web_loss || 0));
      ever += k; n += k - l; lost += l;
    }
  }
  return { ever, n, lost, loss: n + lost ? lost / (n + lost) : NaN, rtt: null };
}

/** Incidents overlapping [from, to) (milliseconds). */
function incidentsAt(/** @type {number} */ from, /** @type {number} */ to) {
  const now = Date.now();
  return incidents.filter((inc) => Date.parse(inc.start) < to && (inc.end ? Date.parse(inc.end) : now) >= from);
}

// ---------- investigate (per-hop traces) ----------

/** @typedef {{hop:number,ip?:string,no_reply?:boolean,private?:boolean,rdns?:string,asn?:number,owner?:string}} HopRef */
/** @typedef {HopRef & {n:number,lost:number,loss:number,min:number|null,mean:number|null,max:number|null,p95:number|null,jitter:number|null,loss_continues:boolean}} HopRow */
/** @typedef {{id:number,target:string,at:string,first_diff:number,from:HopRef[],to:HopRef[]}} RouteChange */
/** @typedef {{target:string,traced:boolean,from:number,to:number,now:number,isp_asn?:number,route:{since:string,hops:HopRef[]}|null,hops:HopRow[],changes:RouteChange[],investigation?:InvState}} TraceData */
/** @typedef {{target:string,state:string,rounds_done:number,rounds_max:number,interval_ms:number,started_at:number,finished_at?:number}} InvState */

const INV_REFRESH_MS = 5000;
const REAL_LOSS = 0.01; // loss below 1% is noise
const INV_SYNC = 'fyisp-inv';

let traceOn = false;
/** @type {TargetInfo[]} */
let allTargets = [];
/** @type {Investigate|null} */
let inv = null;

function targetInfo(/** @type {string} */ name) { return allTargets.find((t) => t.name === name); }

/** The target the path panel and path-level incidents open: a traced anycast target, else any traced one. */
function defaultTraceTarget() {
  const tr = allTargets.filter((t) => t.trace);
  return (tr.find((t) => t.layer === 'anycast') || tr[0] || allTargets.find((t) => !t.layer) || { name: '' }).name;
}

/** The target an outage-log row offers to trace: the first affected one we can trace, else the path. */
function incidentTraceTarget(/** @type {Incident} */ inc) {
  if (!traceOn) return '';
  for (const n of inc.targets || []) {
    const t = targetInfo(n);
    if (t) return n;
  }
  return defaultTraceTarget();
}

function traceButton(/** @type {string} */ name, /** @type {string} */ label) {
  return h('button', {
    class: 'tbtn', type: 'button', title: `Investigate ${name} hop by hop`, text: label,
    onclick: (/** @type {Event} */ e) => { e.stopPropagation(); openInvestigate(name); },
    onkeydown: (/** @type {Event} */ e) => e.stopPropagation(),
  });
}

function openInvestigate(/** @type {string} */ name) {
  if (!traceOn || !inv || !name) return;
  state.inv = name;
  writeHash();
  showView();
  window.scrollTo({ top: 0 });
  refreshAll();
}
function closeInvestigate() {
  state.inv = null;
  writeHash();
  showView();
  refreshAll();
}
/** Dashboard or Investigate view, per state.inv. */
function showView() {
  const on = !!(state.inv && inv);
  document.body.classList.toggle('investigating', on);
  const homeOn = currentView() === 'h' && !on;
  document.body.classList.toggle('home-on', homeOn);
  if (home) home.el.hidden = !homeOn;
  if (home) renderViewNav();
  if (inv) {
    inv.el.hidden = !on;
    if (on) inv.open(/** @type {string} */ (state.inv)); else inv.close();
  }
  updateRefreshLabel();
}

/**
 * Why an on-demand trace couldn't start, in plain words, from the API's
 * stable error code (never its wording): busy (no free slot: 4 in all, with
 * retry_after); unknown_target, unavailable, forbidden.
 * @param {any} e
 */
function invError(e) {
  const b = e.body || {};
  const s = Number(b.retry_after) || Number(e.retryAfter) || 0;
  const wait = s > 90 ? `in about ${Math.round(s / 60)} minutes` : s > 0 ? `in ${s} seconds` : 'in a little while';
  switch (b.code) {
    case 'busy': return 'Four traces are already running, which is the most at once. Try again in a few minutes.';
    case 'unknown_target': return 'Tracing isn’t available for this target.';
    case 'unavailable': return 'Tracing is turned off on this setup.';
    case 'forbidden': return /admin token/.test(String(b.error || '')) ? 'Tracing needs the admin token: open the link fyisp printed when it started.'
      : 'This page can’t start traces: open the dashboard at its own address.';
  }
  return e.status === 429 ? `Too many traces right now. Try again ${wait}.` : 'Couldn’t start the trace. Try again.';
}

function hopAddr(/** @type {HopRef} */ x) {
  if (x.no_reply) return 'no reply';
  if (x.private) return x.ip || 'private hop';
  return x.ip || '?';
}
function hopOwner(/** @type {HopRef} */ x) {
  if (x.asn) return `AS${x.asn}` + (x.owner ? ` ${x.owner}` : '');
  return x.owner || '';
}
/** One side of a route change: its network, or its address within one network. */
function changeSide(/** @type {HopRef|undefined} */ x, /** @type {HopRef|undefined} */ other) {
  if (!x) return 'nothing';
  if (x.asn && (!other || other.asn !== x.asn)) return `AS${x.asn}` + (x.owner ? ` (${x.owner})` : '');
  if (!x.asn && x.owner && (!other || other.owner !== x.owner)) return x.owner;
  return hopAddr(x);
}
function hopColor(/** @type {number} */ hop) { return seriesColor(hop - 1); }

class Investigate {
  constructor() {
    this.target = '';
    /** @type {TraceData|null} */
    this.data = null;
    /** @type {PanelData|null} */
    this.tl = null;
    /** @type {Set<number>} */
    this.sel = new Set();
    this.userSel = false;
    /** @type {any} */ this.rttU = null;
    /** @type {any} */ this.lossU = null;
    this.sig = '';
    /** @type {AbortController|null} */
    this.ctl = null;
    this.poking = false;
    this.invErr = '';
    /** @type {InvState|null} the on-demand trace of this target (running, or finished within the hour) */
    this.invState = null;
    // Start one on open, once the first load shows none is running.
    this.wantStart = false;
    /** Spoken only when a trace starts or finishes, never per round. */
    this.invSay = h('div', { class: 'sr-only', role: 'status', 'aria-live': 'polite', 'aria-atomic': 'true' });
    this.hover = false;
    /** @type {number[]} */
    this.hops = [];
    this.view = [0, 1];

    this.picker = /** @type {HTMLSelectElement} */ (h('select', { class: 'inv-pick', 'aria-label': 'Target to investigate' }));
    this.picker.addEventListener('change', () => openInvestigate(this.picker.value));
    this.titleEl = h('h2', { text: 'Investigate' });
    this.statusEl = h('span', { class: 'inv-status' });
    this.summary = h('div', { class: 'inv-summary', 'aria-live': 'polite' });
    this.tbody = h('tbody');
    const cols = [['Hop', 'hn'], ['Address', 'addr'], ['Network', 'own'], ['Loss', 'loss'], ['Avg', 'num'], ['Min', 'num'], ['Max', 'num'], ['P95', 'num'], ['Jitter', 'num'], ['Latency (min–max, avg)', 'bar']];
    this.table = h('table', { class: 'hops' },
      h('thead', {}, h('tr', {}, ...cols.map(([t, c]) => h('th', { class: c, scope: 'col', text: t })))),
      this.tbody);
    this.tlMeta = h('span', { class: 'meta' });
    this.rttEl = h('div', { class: 'chart inv-rtt' });
    this.lossEl = h('div', { class: 'chart inv-loss' });
    this.emptyEl = h('div', { class: 'empty', text: 'Loading…' });
    this.rttEl.append(this.emptyEl);
    this.chartLegend = h('ul', { class: 'legend inv-legend', 'aria-label': 'Hops on the timeline' });
    this.changesEl = h('div', { class: 'inv-changes' });
    this.el = h('section', { id: 'inv', class: 'inv', 'aria-label': 'Investigate a target hop by hop', hidden: true },
      this.invSay,
      h('div', { class: 'inv-head' },
        h('button', { class: 'btn', type: 'button', text: '← Dashboard', onclick: closeInvestigate }),
        this.titleEl, this.picker, this.statusEl),
      this.summary,
      h('section', { class: 'panel', 'aria-label': 'Hops' },
        h('div', { class: 'panel-head' }, h('h2', { text: 'Hops' }), h('span', { class: 'meta', text: 'Click a hop to add it to the timeline' })),
        h('div', { class: 'hops-wrap' }, this.table)),
      h('section', { class: 'panel', 'aria-label': 'Hop timeline' },
        h('div', { class: 'panel-head' }, h('h2', { text: 'Timeline' }), this.tlMeta),
        h('div', { class: 'inv-sub', text: 'Round-trip time (ms)' }),
        this.rttEl,
        h('div', { class: 'inv-sub', text: 'Loss (%)' }),
        this.lossEl,
        this.chartLegend),
      h('section', { class: 'panel', 'aria-label': 'Route changes' },
        h('div', { class: 'panel-head' }, h('h2', { text: 'Route changes' })),
        this.changesEl),
      h('details', { class: 'panel inv-help' },
        h('summary', { text: 'How to read this' }),
        h('p', { text: 'Each row is one router on the way to the destination (a hop), in order. Your router is hop 1; the rows marked "your ISP" belong to the same network as your ISP\'s first public hop.' }),
        h('p', {}, h('b', { text: 'Loss that does not continue is not real. ' }),
          'Routers forward your traffic in hardware but answer trace probes in software, at low priority and often rate-limited. A middle hop that drops replies while the hops after it (and the destination) do not is just busy answering: your traffic passes through it fine.'),
        h('p', {}, h('b', { text: 'Real loss continues to the destination. ' }),
          'When loss starts at one hop and every later hop shows at least as much, packets are really being dropped there, on that router or the link just before it. The first such hop is where to point the finger; if it is marked "your ISP", it is your ISP\'s network.'),
        h('p', { text: 'Latency works the same way: a spike at one middle hop that later hops do not show is the router being slow to reply, not slow to forward. The latency bar shows each hop\'s min–max range with a tick at the average.' })));
    new ResizeObserver(() => this.resize()).observe(this.rttEl);
  }

  /** @param {string} target */
  open(target) {
    if (target !== this.target) {
      this.target = target;
      this.data = null;
      this.tl = null;
      this.sel = new Set();
      this.userSel = false;
      this.invErr = '';
      this.invState = null;
      this.tbody.replaceChildren();
      this.summary.replaceChildren();
      this.changesEl.replaceChildren();
      this.chartLegend.replaceChildren();
      this.emptyEl.textContent = 'Loading…';
      this.emptyEl.hidden = false;
      this.rttU?.destroy(); this.lossU?.destroy();
      this.rttU = this.lossU = null;
      this.sig = '';
    }
    this.fillPicker();
    this.titleEl.textContent = 'Investigate';
    const t = targetInfo(target);
    this.wantStart = !!t && !t.trace;
    this.renderStatus();
  }

  close() { this.ctl?.abort(); }

  fillPicker() {
    const opts = [];
    const ok = (/** @type {TargetInfo} */ t) => !!t; // every target: anyone can start an on-demand trace
    const byGroup = new Map();
    for (const p of panels) byGroup.set(p.group.id, p.group.title);
    const groups = new Map();
    for (const t of allTargets) {
      if (!ok(t)) continue;
      const g = byGroup.get(t.group) || t.group;
      if (!groups.has(g)) groups.set(g, []);
      groups.get(g).push(t);
    }
    const traced = allTargets.filter((t) => t.trace);
    if (traced.length) {
      opts.push(h('optgroup', { label: 'Always-on traces' }, ...traced.map((t) => h('option', { value: t.name, text: t.name }))));
    }
    for (const [g, ts] of groups) {
      const rest = ts.filter((/** @type {TargetInfo} */ t) => !t.trace);
      if (rest.length) opts.push(h('optgroup', { label: `${g} (on demand)` }, ...rest.map((/** @type {TargetInfo} */ t) => h('option', { value: t.name, text: t.name }))));
    }
    if (!allTargets.some((t) => t.name === this.target && ok(t))) opts.unshift(h('option', { value: this.target, text: this.target }));
    this.picker.replaceChildren(...opts);
    this.picker.value = this.target;
  }

  /**
   * Starts an on-demand trace (POST api/investigate; the admin token where
   * the dashboard has one). It runs at most rounds_max rounds and stops by
   * itself, so it is never re-posted to keep it alive. On a running trace
   * the POST only returns its state.
   * @param {boolean} [auto] started by opening the view, not by a click
   */
  async poke(auto = false) {
    const target = this.target;
    if (this.poking) return;
    // Opening the view never asks for the admin token by itself: "Try again" does.
    if (auto && status && status.controls === 'admin' && !status.admin) {
      this.invErr = 'Tracing needs the admin token.';
      this.renderStatus();
      return;
    }
    this.poking = true;
    /** @type {Record<string,string>} */
    const headers = {};
    if (TOKEN) headers['X-FYISP-Token'] = TOKEN;
    if (!await addAdmin(headers, 'Tracing needs the admin token. Open the link fyisp printed when it started, or paste the token here.')) {
      this.poking = false;
      this.invErr = 'Tracing needs the admin token.';
      this.renderStatus();
      return;
    }
    try {
      /** @type {InvState} */
      const r = await fetchJSON('api/investigate?' + new URLSearchParams({ target }), { method: 'POST', headers }, 1);
      if (target !== this.target) return;
      this.invErr = '';
      this.setInv(r);
    } catch (e) {
      if (target !== this.target) return;
      this.invErr = invError(/** @type {any} */ (e));
    } finally {
      this.poking = false;
    }
    this.renderStatus();
    this.renderSummaryIfEmpty();
  }

  /** A new trace state; speaks only when a trace starts or finishes. @param {InvState|null|undefined} v */
  setInv(v) {
    const prev = this.invState;
    this.invState = v || null;
    if (!v) return;
    if (v.state === 'running' && (!prev || prev.state !== 'running' || prev.started_at !== v.started_at)) {
      this.invSay.textContent = `Tracing ${v.target}: up to ${v.rounds_max} rounds.`;
    } else if (v.state === 'finished' && prev && prev.state === 'running') {
      this.invSay.textContent = `Trace of ${v.target} finished: ${v.rounds_done} rounds.`;
    }
  }

  renderStatus() {
    const t = targetInfo(this.target);
    const d = this.data;
    const v = this.invState;
    const kids = [];
    if (t && t.trace) kids.push(h('span', { class: 'pill on', text: 'Always-on trace' }));
    else if (this.invErr) {
      kids.push(h('span', { class: 'pill err', text: this.invErr }));
      kids.push(h('button', { class: 'tbtn', type: 'button', text: 'Try again', onclick: () => { this.invErr = ''; this.poke(); } }));
    } else if (v && v.state === 'running') {
      kids.push(h('span', { class: 'pill on', title: `About every ${Math.round(v.interval_ms / 100) / 10} s; it stops by itself after ${v.rounds_max} rounds.`, text: `Tracing… ${v.rounds_done} of ${v.rounds_max} rounds` }));
    } else if (v && v.state === 'finished') {
      kids.push(h('span', { class: 'pill', text: `Finished: ${v.rounds_done} rounds${v.finished_at ? ` (ended ${fmtWhen(v.finished_at)})` : ''}` }));
      kids.push(h('button', { class: 'tbtn', type: 'button', text: 'Trace again', disabled: this.poking || null, onclick: () => this.poke() }));
    } else kids.push(h('span', { class: 'pill', text: 'Starting a trace…' }));
    if (d && d.route) kids.push(h('span', { class: 'muted small', text: `route unchanged since ${fmtWhen(Date.parse(d.route.since))}` }));
    this.statusEl.replaceChildren(...kids);
  }

  /** The "Tracing…" line in the summary while there are no hops yet. */
  renderSummaryIfEmpty() { if (this.data && !this.data.hops.length) this.renderSummary(); }

  setView(/** @type {number} */ from, /** @type {number} */ to) {
    this.view = [from / 1000, to / 1000];
    for (const u of [this.rttU, this.lossU]) u?.setScale('x', { min: this.view[0], max: this.view[1] });
  }

  /** The destination: the last hop of the current route, else the deepest hop seen. */
  dest() {
    const d = this.data;
    if (!d) return 0;
    if (d.route && d.route.hops.length) return d.route.hops.length;
    return d.hops.reduce((m, x) => Math.max(m, x.hop), 0);
  }

  /** The first hop whose loss is real (continues to the destination). */
  realStart() {
    const d = this.data;
    if (!d) return null;
    return d.hops.find((x) => x.loss_continues && x.loss >= REAL_LOSS) || null;
  }

  async load() {
    if (!this.target) return;
    this.ctl?.abort();
    const ctl = this.ctl = new AbortController();
    const target = this.target;
    const { from, to } = queryRange();
    this.rttEl.classList.add('loading');
    try {
      /** @type {TraceData} */
      const d = await fetchJSON('api/trace?' + new URLSearchParams({ target, from, to }), { signal: ctl.signal });
      if (ctl.signal.aborted || target !== this.target) return;
      this.data = d;
      // A response can predate our start: never forget a known trace.
      if (d.investigation) this.setInv(d.investigation);
      // Opening a target without an always-on trace starts one, unless one is running or just finished.
      if (this.wantStart) {
        this.wantStart = false;
        if (!d.investigation) this.poke(true);
      }
      const dest = this.dest();
      if (!this.userSel) {
        this.sel = new Set();
        const rs = this.realStart();
        if (rs && rs.hop !== dest) this.sel.add(rs.hop);
      }
      for (const x of [...this.sel]) if (x > Math.max(dest, 1)) this.sel.delete(x);
      this.hops = [...new Set([...this.sel, dest])].filter((x) => x > 0).sort((a, b) => a - b);
      this.renderTable();
      this.renderSummary();
      this.renderChanges();
      this.renderStatus();
      if (this.hops.length) {
        const points = Math.min(1000, Math.max(50, Math.round(this.width() / 2)));
        this.tl = await fetchJSON('api/trace/panel?' + new URLSearchParams({ target, hops: this.hops.join(','), from, to, points: String(points) }), { signal: ctl.signal });
        if (ctl.signal.aborted || target !== this.target) return;
      } else this.tl = null;
      this.renderCharts();
    } catch (e) {
      if (/** @type {any} */ (e).name === 'AbortError') return;
      this.emptyEl.textContent = `Could not load: ${/** @type {Error} */ (e).message}`;
      this.emptyEl.hidden = false;
    } finally {
      if (this.ctl === ctl) this.rttEl.classList.remove('loading');
    }
  }

  render() {
    if (!this.data) return;
    this.renderTable();
    this.renderSummary();
    this.renderChanges();
    this.renderCharts();
  }

  /** The verdict-like one-liner above the table. */
  renderSummary() {
    const d = /** @type {TraceData} */ (this.data);
    const dest = this.dest();
    const kids = [];
    if (!d.hops.length) {
      const t = targetInfo(this.target);
      const pending = t && !t.trace && !this.invErr && (!this.invState || this.invState.state === 'running');
      this.summary.className = 'inv-summary v-warming_up';
      kids.push(h('p', { class: 'is-head', text: pending ? 'Tracing… the first results appear within a few seconds.' : 'No trace data in this range.' }));
      this.summary.replaceChildren(...kids);
      return;
    }
    const rs = this.realStart();
    const destRows = d.hops.filter((x) => x.hop === dest);
    const destLoss = destRows.reduce((m, x) => Math.max(m, x.loss), 0);
    const destAvg = destRows.find((x) => x.mean != null)?.mean;
    const fake = d.hops.filter((x) => !x.loss_continues && x.n > 0 && x.loss >= REAL_LOSS && x.hop !== dest);
    let kind = 'ok', head;
    if (rs) {
      const isp = !!(rs.asn && d.isp_asn && rs.asn === d.isp_asn);
      kind = isp ? 'isp' : 'upstream';
      const net = hopOwner(rs) || hopAddr(rs);
      head = `Real loss starts at hop ${rs.hop}${net ? ` (${net}${isp ? ', your ISP' : ''})` : ''} and continues to the destination: ${fmtPct(destLoss)} lost there.`;
    } else if (destLoss >= REAL_LOSS) {
      kind = 'service';
      head = `The destination itself drops ${fmtPct(destLoss)} of probes, but no hop before it loses packets.`;
    } else {
      head = 'No real packet loss on this path in this range.';
    }
    this.summary.className = `inv-summary v-${kind}`;
    kids.push(h('p', { class: 'is-head', text: head }));
    const sub = [];
    if (destAvg != null) sub.push(`Destination round trip ${fmtMs(destAvg)} ms on average.`);
    if (fake.length) {
      const list = fake.slice(0, 3).map((x) => `hop ${x.hop} (${fmtPct(x.loss)})`).join(', ');
      sub.push(`${fake.length === 1 ? 'One hop drops' : `${fake.length} hops drop`} trace replies without it continuing: ${list}. That is router rate-limiting, not real loss.`);
    }
    if (sub.length) kids.push(h('p', { class: 'is-sub', text: sub.join(' ') }));
    this.summary.replaceChildren(...kids);
  }

  renderTable() {
    const d = /** @type {TraceData} */ (this.data);
    const dest = this.dest();
    const rows = [];
    // Bar scale: up to the largest max, but not beyond 1.5x the largest P95 (one outlier would squash every bar).
    let top = 0, p95 = 0;
    for (const x of d.hops) { if (x.max != null) top = Math.max(top, x.max); if (x.p95 != null) p95 = Math.max(p95, x.p95); }
    if (p95 > 0) top = Math.min(top, p95 * 1.5);
    top = top > 0 ? top * 1.05 : 1;
    const pos = (/** @type {number} */ v) => `${Math.min(100, Math.max(0, (v / top) * 100)).toFixed(2)}%`;
    let lastHop = 0;
    for (const x of d.hops) {
      const first = x.hop !== lastHop;
      lastHop = x.hop;
      const selected = x.hop === dest || this.sel.has(x.hop);
      const sw = h('span', { class: 'hsw' + (selected ? ' on' : '') });
      if (selected) sw.style.background = hopColor(x.hop);
      const isp = !!(x.asn && d.isp_asn && x.asn === d.isp_asn);
      const addr = h('td', { class: 'addr', 'data-label': 'Address' },
        h('div', { class: 'ip' + (x.private || x.no_reply ? ' muted' : ''), text: hopAddr(x) },
          x.private ? h('span', { class: 'masked', text: ' private' }) : null),
        x.rdns ? h('div', { class: 'rdns', title: x.rdns, text: x.rdns }) : null);
      const own = h('td', { class: 'own', 'data-label': 'Network' },
        hopOwner(x) ? h('span', { text: hopOwner(x) }) : h('span', { class: 'muted', text: x.private ? 'private network' : '—' }),
        isp ? h('span', { class: 'isp', title: 'Same network (ASN) as your ISP\'s first public hop', text: 'your ISP' }) : null);
      // A router that never answers trace probes in this range is normal
      // (many are configured that way): neutral "no reply", not 100% loss.
      const silent = x.n === 0 && x.lost > 0 && x.hop !== dest;
      const lossCell = h('td', { class: 'loss', 'data-label': 'Loss' },
        silent
          ? h('span', { class: 'lv muted', title: 'This router does not answer trace probes. That is common and harmless: later hops still answer.', text: 'no reply' })
          : h('span', { class: 'lv' + (x.loss >= 0.05 ? ' bad' : x.loss >= REAL_LOSS ? ' warn' : ''), text: x.n + x.lost ? fmtPct(x.loss) : '—' }));
      if (silent) {
        // nothing more to say
      } else if (x.loss > 0 && x.loss_continues && x.loss >= REAL_LOSS) {
        lossCell.append(h('span', { class: 'lbadge real', title: 'Every later hop, up to the destination, loses at least as much: packets are really dropped here.', text: 'real loss, continues to destination' }));
      } else if (x.loss > 0 && !x.loss_continues && x.hop !== dest && !x.no_reply) {
        lossCell.append(h('span', { class: 'lnote', title: 'Later hops do not show this loss: the router only answers trace probes at a limited rate. Your traffic is not affected.', text: 'not real — router rate-limiting' }));
      }
      const bar = h('div', { class: 'lbar' });
      if (x.min != null && x.max != null) {
        const rng = h('span', { class: 'rng' });
        rng.style.left = pos(x.min);
        rng.style.width = `calc(${pos(x.max)} - ${pos(x.min)} + 2px)`;
        bar.append(rng);
        if (x.mean != null) { const t = h('span', { class: 'avg' }); t.style.left = pos(x.mean); bar.append(t); }
        if (x.max > top) bar.append(h('span', { class: 'over', title: `max ${fmtMs(x.max)} ms`, text: '›' }));
      }
      const num = (/** @type {string} */ l, /** @type {number|null} */ v) => h('td', { class: 'num', 'data-label': l, text: x.n > 0 ? fmtMs(v) : '—' });
      const tr = h('tr', {
        class: (selected ? 'sel' : '') + (first ? '' : ' alt') + (x.hop === dest ? ' dest' : ''),
        tabindex: '0', 'aria-selected': String(selected),
        title: x.hop === dest ? 'The destination is always on the timeline' : selected ? 'Click to remove from the timeline' : 'Click to add to the timeline',
        onclick: () => this.toggle(x.hop),
        onkeydown: (/** @type {KeyboardEvent} */ e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); this.toggle(x.hop); } },
      },
      h('td', { class: 'hn', 'data-label': 'Hop' }, sw, first ? String(x.hop) : ''),
      addr, own, lossCell,
      num('Avg', x.mean), num('Min', x.min), num('Max', x.max), num('P95', x.p95), num('Jitter', x.jitter),
      h('td', { class: 'bar', 'data-label': 'Latency' }, bar));
      rows.push(tr);
    }
    if (!rows.length) rows.push(h('tr', {}, h('td', { class: 'none muted', colspan: '10', text: 'No hops in this range.' })));
    this.tbody.replaceChildren(...rows);
  }

  toggle(/** @type {number} */ hop) {
    if (hop === this.dest()) return;
    this.userSel = true;
    if (this.sel.has(hop)) this.sel.delete(hop); else this.sel.add(hop);
    this.renderTable();
    this.load();
  }

  renderChanges() {
    const d = /** @type {TraceData} */ (this.data);
    if (!d.changes.length) {
      this.changesEl.replaceChildren(h('p', { class: 'muted o-empty', text: 'No route changes in this range.' }));
      return;
    }
    this.changesEl.replaceChildren(...d.changes.map((c) => {
      const at = Date.parse(c.at);
      const i = c.first_diff - 1;
      const a = c.from[i], b = c.to[i];
      return h('button', {
        class: 'c-row', type: 'button', title: 'Zoom to this route change',
        onclick: () => zoomTo(at - 15 * 60e3, Math.min(Date.now(), at + 15 * 60e3)),
      },
      h('span', { class: 'c-when', text: fmtWhen(at) }),
      h('span', { class: 'c-what', text: `route changed at hop ${c.first_diff}: ${changeSide(a, b)} → ${changeSide(b, a)}` }),
      h('span', { class: 'c-len muted', text: `${c.from.length} → ${c.to.length} hops` }));
    }));
  }

  width() { return Math.max(200, this.rttEl.clientWidth); }

  resize() {
    const w = this.width();
    for (const [u, ht] of [[this.rttU, 220], [this.lossU, 110]]) if (u && Math.abs(w - u.width) > 1) u.setSize({ width: w, height: ht });
  }

  renderCharts() {
    const d = this.tl;
    const td = this.data;
    if (!d || !td) {
      this.rttU?.destroy(); this.lossU?.destroy(); this.rttU = this.lossU = null; this.sig = '';
      this.emptyEl.textContent = td && !td.hops.length ? 'No trace data in this range' : 'Loading…';
      this.emptyEl.hidden = false;
      this.chartLegend.replaceChildren();
      return;
    }
    this.view = isRelative() ? [d.from / 1000, d.to / 1000] : [/** @type {number} */ (state.from) / 1000, /** @type {number} */ (state.to) / 1000];
    this.tlMeta.textContent = `${fmtStep(d.step)} buckets`;
    const xs = new Float64Array(d.len);
    for (let i = 0; i < d.len; i++) xs[i] = (d.start + i * d.step) / 1000;
    const series = d.series;
    const rtt = [xs, ...series.map((s) => s.mean)];
    const loss = [xs, ...series.map((s) => s.n.map((n, i) => (n + s.lost[i] ? (100 * s.lost[i]) / (n + s.lost[i]) : null)))];
    const sig = effectiveTheme() + '|' + series.map((s) => /** @type {any} */ (s).hop).join(',');
    if (!this.rttU || sig !== this.sig) {
      this.rttU?.destroy(); this.lossU?.destroy();
      this.emptyEl.remove();
      this.rttU = this.makeChart(this.rttEl, series, rtt, 220, 'ms', false);
      this.lossU = this.makeChart(this.lossEl, series, loss, 110, '%', true);
      this.rttEl.append(this.emptyEl);
      this.sig = sig;
    } else {
      this.rttU.setData(rtt);
      this.lossU.setData(loss);
    }
    const any = series.some((s) => s.n.some((n) => n > 0) || s.lost.some((n) => n > 0));
    this.emptyEl.textContent = any ? '' : 'No trace data in this range';
    this.emptyEl.hidden = any;
    const dest = this.dest();
    const byHop = new Map(td.hops.map((x) => [x.hop, x]));
    this.chartLegend.replaceChildren(...series.map((s) => {
      const hop = /** @type {any} */ (s).hop;
      const x = byHop.get(hop);
      const sw = h('span', { class: 'sw' });
      sw.style.background = hopColor(hop);
      return h('li', {
        title: hop === dest ? 'The destination is always shown' : 'Click to remove from the timeline',
        tabindex: '0', role: 'button',
        onclick: () => this.toggle(hop),
        onkeydown: (/** @type {KeyboardEvent} */ e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); this.toggle(hop); } },
      }, sw,
      h('span', { class: 'name', text: `Hop ${hop}${hop === dest ? ' · destination' : ''}` }),
      h('span', { class: 'val', text: x ? hopAddr(x) : '' }));
    }));
  }

  /**
   * @param {HTMLElement} el @param {SeriesData[]} series @param {any[]} data
   * @param {number} height @param {string} unit @param {boolean} isLoss
   */
  makeChart(el, series, data, height, unit, isLoss) {
    const ink2 = css('--ink-2'), grid = css('--grid'), axis = css('--axis');
    const font = '11px system-ui, -apple-system, "Segoe UI", sans-serif';
    const self = this;
    const stepped = uPlot.paths && uPlot.paths.stepped ? uPlot.paths.stepped({ align: 1 }) : undefined;
    const opts = {
      width: this.width(),
      height,
      padding: [8, 8, 0, 0],
      legend: { show: false },
      scales: {
        x: { time: true, range: () => self.view },
        y: { range: (/** @type {any} */ u, /** @type {number} */ min, /** @type {number} */ max) => (isLoss ? [0, Math.max(10, Math.min(100, max * 1.15))] : [0, max > 0 ? max * 1.08 : 10]) },
      },
      axes: [
        { stroke: ink2, font, grid: { stroke: grid, width: 1 }, ticks: { stroke: axis, width: 1, size: 4 } },
        { stroke: ink2, font, size: 48, label: unit, labelSize: 14, labelFont: font, grid: { stroke: grid, width: 1 }, ticks: { stroke: axis, width: 1, size: 4 } },
      ],
      series: [{}, ...series.map((s) => {
        const c = hopColor(/** @type {any} */ (s).hop);
        return isLoss
          ? { label: `hop ${/** @type {any} */ (s).hop}`, stroke: c, width: 1.25, fill: rgba(c, 0.12), paths: stepped, spanGaps: false, points: { show: false } }
          : { label: `hop ${/** @type {any} */ (s).hop}`, stroke: c, width: 1.5, spanGaps: false, points: { show: false } };
      })],
      cursor: {
        sync: { key: INV_SYNC, setSeries: false },
        drag: { x: true, y: false, setScale: false },
        points: { show: false },
        bind: { dblclick: () => () => { resetZoom(); return null; } },
      },
      hooks: {
        setSelect: [(/** @type {any} */ u) => {
          const w = u.select.width;
          if (w > 4) zoomTo(Math.round(u.posToVal(u.select.left, 'x') * 1000), Math.round(u.posToVal(u.select.left + w, 'x') * 1000));
          u.setSelect({ left: 0, top: 0, width: 0, height: 0 }, false);
        }],
        setCursor: [(/** @type {any} */ u) => self.onCursor(u)],
        draw: [(/** @type {any} */ u) => { drawSpeedWindows(u); self.drawChanges(u, !isLoss); }],
      },
    };
    const u = new uPlot(opts, data, el);
    u.over.addEventListener('mouseenter', () => { self.hover = true; });
    u.over.addEventListener('mouseleave', () => { self.hover = false; hideTip(); });
    return u;
  }

  /** Route changes as dashed vertical lines (labelled on the RTT chart). */
  drawChanges(/** @type {any} */ u, /** @type {boolean} */ label) {
    const td = this.data;
    if (!td || !td.changes.length) return;
    const ctx = u.ctx;
    const { left, top, width, height } = u.bbox;
    const dpr = window.devicePixelRatio || 1;
    const c = css('--v-upstream') || '#f09a3e';
    ctx.save();
    ctx.beginPath();
    ctx.rect(left, top, width, height);
    ctx.clip();
    ctx.strokeStyle = c;
    ctx.fillStyle = c;
    ctx.lineWidth = 1.5 * dpr;
    ctx.setLineDash([4 * dpr, 3 * dpr]);
    ctx.font = `${11 * dpr}px system-ui, sans-serif`;
    for (const ch of td.changes) {
      const t = Date.parse(ch.at) / 1000;
      if (t < u.scales.x.min || t > u.scales.x.max) continue;
      const x = Math.round(u.valToPos(t, 'x', true)) + 0.5;
      ctx.beginPath();
      ctx.moveTo(x, top);
      ctx.lineTo(x, top + height);
      ctx.stroke();
      if (label) {
        const txt = `route change (hop ${ch.first_diff})`;
        const w = ctx.measureText(txt).width;
        const tx = x + 4 * dpr + w > left + width ? x - 4 * dpr - w : x + 4 * dpr;
        ctx.fillText(txt, tx, top + 12 * dpr);
      }
    }
    ctx.restore();
  }

  onCursor(/** @type {any} */ u) {
    if (!this.hover) return;
    const d = this.tl;
    const i = u.cursor.idx;
    if (!d || i == null || u.cursor.left < 0) { hideTip(); return; }
    const nodes = [h('div', { class: 't', text: fmtTime(d.start + i * d.step, d.to - d.from) })];
    for (const s of d.series) {
      const hop = /** @type {any} */ (s).hop;
      const sw = h('span', { class: 'sw' });
      sw.style.background = hopColor(hop);
      const tot = s.n[i] + s.lost[i];
      const rttTxt = s.mean[i] != null ? `${fmtMs(s.mean[i])} ms` : tot ? 'no reply' : 'no data';
      nodes.push(h('div', { class: 'row' }, sw, h('span', { class: 'k', text: `hop ${hop}` }), `${rttTxt}${tot ? ` · ${fmtPct(s.lost[i] / tot)} loss` : ''}`));
    }
    const td = this.data;
    if (td) {
      const b0 = d.start + i * d.step, b1 = b0 + d.step;
      for (const ch of td.changes) {
        const t = Date.parse(ch.at);
        if (t >= b0 && t < b1) nodes.push(h('div', { class: 'row inc' }, `route changed at hop ${ch.first_diff}`));
      }
    }
    nodes.push(...speedTipNodes(d.start + i * d.step, d.start + (i + 1) * d.step));
    const rect = u.over.getBoundingClientRect();
    showTip(nodes, rect.left + u.cursor.left, rect.top + u.cursor.top);
  }
}

/** Redraws every chart (the "normal" band or the speed-test marks changed). */
function redrawCharts() {
  for (const p of panels) p.u?.redraw(false);
  if (inv) { inv.rttU?.redraw(false); inv.lossU?.redraw(false); }
}

// ---------- speed-test windows ----------

/** @typedef {{start:number,end:number,origin?:string}} SpeedWindow */

// false once this fyisp answers 404 (no speed tests): stop asking.
let speedOn = true;
/** Details → Speed, when this fyisp has speed tests. @type {Speed|null} */
let speed = null;

/** Whether this fyisp has speed tests: the light part=running answers 404 when not. */
async function speedAvailable() {
  try {
    await fetchJSON('api/speed?part=running');
    return true;
  } catch (e) {
    if (/** @type {any} */ (e).status === 404) { speedOn = false; return false; }
    return true; // another error: show the tab, which says it can't load
  }
}
/** @type {SpeedWindow[]} */
let speedWins = [];
// Whether charts mark speed-test windows: off by default (a test every few
// minutes would cover the chart), toggled from the strip legend, remembered.
let speedShow = false;
try { speedShow = localStorage.getItem('fyisp.speedShow') === '1'; } catch { /* no storage */ }
function toggleSpeedShow() {
  speedShow = !speedShow;
  try { localStorage.setItem('fyisp.speedShow', speedShow ? '1' : '0'); } catch { /* no storage */ }
  redrawCharts();
}

/** Speed-test windows in the view: probes inside are measured as usual (a spike may be the test). */
async function loadSpeedWindows() {
  if (!speedOn) return;
  const { from, to } = queryRange();
  try {
    const d = await fetchJSON('api/speed?' + new URLSearchParams({ part: 'windows', from, to }));
    speedWins = Array.isArray(d.windows) ? d.windows : [];
  } catch (e) {
    if (/** @type {any} */ (e).status === 404) { speedOn = false; speedWins = []; redrawCharts(); }
    return;
  }
  redrawCharts();
}

/**
 * Speed tests as marks along the top of the chart; a test wide enough to see
 * (zoomed in) also gets a faint band. Thin full-height bands would read as a
 * grid: with a test every 88 s they'd cover a day view.
 */
function drawSpeedWindows(/** @type {any} */ u) {
  if (!speedShow || !speedWins.length) return;
  const ctx = u.ctx;
  const { left, top, width, height } = u.bbox;
  const vmin = u.scales.x.min, vmax = u.scales.x.max;
  const dpr = window.devicePixelRatio || 1;
  const c = css('--muted') || '#898781';
  // Merge windows that touch on screen, each at least 2 px wide.
  /** @type {[number, number][]} */
  const spans = [];
  for (const w of speedWins) {
    if (w.end / 1000 < vmin || w.start / 1000 > vmax) continue;
    let x0 = u.valToPos(w.start / 1000, 'x', true), x1 = u.valToPos(w.end / 1000, 'x', true);
    if (x1 - x0 < 2 * dpr) { const m = (x0 + x1) / 2; x0 = m - dpr; x1 = m + dpr; }
    const last = spans[spans.length - 1];
    if (last && x0 <= last[1] + dpr) last[1] = Math.max(last[1], x1); else spans.push([x0, x1]);
  }
  ctx.save();
  ctx.beginPath();
  ctx.rect(left, top, width, height);
  ctx.clip();
  for (const [x0, x1] of spans) {
    if (x1 - x0 >= 6 * dpr) {
      ctx.fillStyle = rgba(c, 0.10);
      ctx.fillRect(x0, top, x1 - x0, height);
    }
    // A solid grey tick along the top.
    ctx.fillStyle = c;
    ctx.fillRect(x0, top, x1 - x0, 4 * dpr);
  }
  ctx.restore();
}

/**
 * Legend entries for speed-test windows, only when the chart's range holds
 * one.
 * @param {[number, number]} view seconds
 */
function speedLegend(view) {
  if (!view || !speedWins.length) return [];
  const inView = speedWins.filter((w) => w.end / 1000 >= view[0] && w.start / 1000 <= view[1]);
  if (!inView.length) return [];
  const toggle = h('span', {
    class: 'sp-toggle' + (speedShow ? '' : ' off'), tabindex: '0', role: 'button', 'aria-pressed': String(speedShow),
    title: speedShow ? 'Hide speed-test marks' : 'Show when speed tests ran (they can raise latency while they run)',
    onclick: toggleSpeedShow,
    onkeydown: (/** @type {KeyboardEvent} */ e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); toggleSpeedShow(); } },
  }, h('span', { class: 'chip sp-tick', 'aria-hidden': 'true' }), speedShow ? 'speed tests (shown)' : 'show speed tests');
  return [toggle];
}

/** Tooltip line for a bucket that overlaps a speed test. */
function speedTipNodes(/** @type {number} */ a, /** @type {number} */ b) {
  if (!speedShow) return [];
  const hit = speedWins.filter((w) => w.start < b && w.end > a);
  if (!hit.length) return [];
  const n = hit.length;
  return [h('div', { class: 'sep' }), h('div', { class: 'row inc', text: `${n === 1 ? 'speed test' : `${n} speed tests`} running` })];
}

/** A select (drag) on a chart zooms all charts to it. */
function onChartSelect(/** @type {any} */ u) {
  const w = u.select.width;
  if (w > 4) zoomTo(Math.round(u.posToVal(u.select.left, 'x') * 1000), Math.round(u.posToVal(u.select.left + w, 'x') * 1000));
  u.setSelect({ left: 0, top: 0, width: 0, height: 0 }, false);
}

// ---------- baselines ----------

/** @typedef {{target:string,kind:string,median_ms:number,p95_ms:number,loss:number,samples:number,hour_of_day:boolean,now_ms?:number,ratio_now?:number}} BaselineData */

let baseOn = false;
const SLOW_RATIO = 1.5;
const BAND_MAX = 6; // bands for at most this many visible targets (then only slow ones)
state.band = storageGet('fyisp-band') !== '0';

/** @type {Map<string, BaselineData>} */
let baselines = new Map();
let baseKey = '';
let baseAt = 0;

/** Every series' normal in one request, at most once a minute per view. */
async function loadBaselines() {
  if (!baseOn) return;
  const at = isRelative() ? 'now' : String(state.to);
  if (at === baseKey && Date.now() - baseAt < 60e3) return;
  baseKey = at;
  baseAt = Date.now();
  try {
    const b = await fetchJSON('api/baselines?' + new URLSearchParams({ at }));
    baselines = new Map((b.series || []).map((/** @type {BaselineData} */ x) => [x.target + '\0' + x.kind, x]));
  } catch {
    baseAt = 0;
    return;
  }
  for (const p of panels) if (p.data) { p.renderLegend(); p.u?.redraw(false); }
}

function fmtRatio(/** @type {number} */ r) { return '×' + (r >= 10 ? r.toFixed(0) : r.toFixed(1)); }

// ---------- overview (large profiles) ----------

/** @typedef {{target:string,state:string,now_ms?:number,n:number,lost:number,loss:number,reason?:string,normal_ms?:number,normal_p95_ms?:number,ratio?:number,hour_of_day?:boolean}} OvRow */
/** @typedef {{targets:number,measured:number,with_normal:number,slow:number,lossy:number,failing:number,providers:number,providers_affected:number,geos:number,geos_affected:number}} OvSummary */
/** @typedef {{from:number,to:number,kind:string,capped?:boolean,summary:OvSummary,rows:OvRow[]}} OvData */

// Profiles with more non-path targets than this open on the Overview.
const OVERVIEW_MIN = 150;
const FAIL_LOSS = 0.2; // the verdict engine's lossBad
const VERY_SLOW = 3; // the verdict engine's spikeFactor
// Loss needs evidence: at least 2 lost of at least 10 samples (as the server's row states).
const LOSS_MIN_LOST = 2, LOSS_MIN_SEEN = 10;
const GEOS = ['na', 'sa', 'eu', 'me', 'af', 'as', 'oc', 'global'];
/** @type {Object<string,string>} */
const GEO_TITLE = { na: 'North America', sa: 'South America', eu: 'Europe', me: 'Middle East', af: 'Africa', as: 'Asia', oc: 'Oceania', global: 'Global', '': '—' };
/** @type {Object<string,string>} */
const GEO_SHORT = { na: 'NA', sa: 'SA', eu: 'EU', me: 'ME', af: 'AF', as: 'AS', oc: 'OC', global: 'GL', '': '—' };
const PROVIDER_KINDS = [['hyperscaler', 'Hyperscalers'], ['cloud', 'Clouds'], ['cdn', 'CDNs'], ['dns', 'DNS'], ['service', 'Services'], ['game', 'Games'], ['gaming-platform', 'Gaming platforms'], ['', 'Other']];
/** @type {Object<string,{label:string,rank:number}>} */
const OV_STATES = {
  failing: { label: 'Failing', rank: 0 }, lossy: { label: 'Lossy', rank: 1 }, very_slow: { label: 'Very slow', rank: 2 },
  slow: { label: 'Slow', rank: 3 }, ok: { label: 'OK', rank: 4 }, unmeasured: { label: 'Not measured', rank: 5 },
};
const PROBLEMS = new Set(['failing', 'lossy', 'very_slow', 'slow']);
const RATIO_BINS = [[1.25, '<1.25×'], [1.5, '1.25–1.5×'], [VERY_SLOW, '1.5–3×'], [Infinity, '≥3×']];
const MS_BINS = [[30, '<30'], [60, '30–60'], [100, '60–100'], [200, '100–200'], [Infinity, '≥200 ms']];
// Table columns: id, header, class, sortable.
const OV_COLS = [['target', 'Target', 't'], ['provider', 'Provider', 'pv sm-off'], ['loc', 'Location', 'loc sm-off'], ['region', 'Region', 'rg sm-off'],
  ['now', 'Now', 'num'], ['normal', 'Normal', 'num sm-off'], ['ratio', '×Normal', 'num'], ['loss', 'Loss', 'num'], ['state', 'Status', 'st sm-off']];
// Heatmap geometry (px): a provider label, then one cell per region.
const HM_LABEL = 150, HM_CELL = 16, HM_GAP = 2, HM_BLOCK_GAP = 24;

let bigProfile = false;
/** @type {Overview|null} */
let ov = null;

/** The view on screen: Home ('h', the default for every profile), or in Details Calls ('calls'), the Overview ('o') or the chart wall ('c'). Focus is always charts. */
function currentView() {
  if (state.focus && focusPanel()) return 'c';
  if (state.view === 'calls' && !callsOn) return detailsView();
  if (state.view === 'speed' && !speed) return detailsView();
  return state.view || 'h';
}
/** Details' own view: Calls when the profile has a call path or call apps, the Overview for big profiles, else the charts. */
function detailsView() { return callsOn ? 'calls' : bigProfile ? 'o' : 'c'; }
function overviewShown() { return !!ov && currentView() === 'o' && !(state.inv && inv); }
function speedShown() { return !!speed && currentView() === 'speed' && !(state.inv && inv); }
/** The kind the Overview shows: the first selected of TCP, HTTPS, ICMP. */
function ovKind() { return KINDS.find((k) => state.kinds.has(k)) || kindsOffered[0]; }
function focusPanel() { return state.focus ? panels.find((p) => !p.isPath && p.targets.some((t) => t.name === state.focus)) || null : null; }
/** Panels that are on screen and may load: the path panel always, the others per view and focus. */
function panelShown(/** @type {Panel} */ p) {
  if (currentView() === 'h' || currentView() === 'speed') return false; // Details loads its charts only when opened; Speed has none
  if (p.isPath) return true;
  if (currentView() !== 'c') return false;
  const fp = focusPanel();
  return !fp || fp === p;
}
function loadStale() { for (const p of panels) if (p.visible && p.stale && panelShown(p)) p.load(); }
/** Worst first: state, then loss, then ratio. */
function bySeverity(/** @type {OvRow} */ a, /** @type {OvRow} */ b) {
  return (OV_STATES[a.state]?.rank ?? 9) - (OV_STATES[b.state]?.rank ?? 9) || b.loss - a.loss || (b.ratio ?? 0) - (a.ratio ?? 0) || a.target.localeCompare(b.target);
}
function geoOf(/** @type {TargetInfo} */ t) { return t.geo || ''; }
/** Category names for the Overview's ot= filter (the v0.6–v0.8 Home's tiles; a #ot= link still opens one). */
/** @type {Object<string,string>} */
const HOME_TILE = { calls: 'Calls', work: 'Work apps', web: 'Websites', other: 'Other services' };
/** A target's category, as the server assigns it (profile.HomeCategory via /api/profile). */
function homeCategory(/** @type {TargetInfo} */ t) { return t.home || ''; }
function locationOf(/** @type {TargetInfo} */ t) { return [t.city, t.country].filter(Boolean).join(', '); }
function windowWords(/** @type {OvData} */ d) {
  return isRelative() ? `last ${fmtDur(Number(rangeSecs()) * 1000)}` : `${fmtWhen(d.from)} – ${fmtWhen(d.to)}`;
}

/**
 * The breadth line: how many targets are worse than normal at once. It
 * corroborates the verdict and never overrides it.
 * @param {OvSummary} s @param {string} when
 */
function breadthText(s, when) {
  const bad = s.slow + s.lossy + s.failing;
  const lossy = s.lossy + s.failing;
  const pl = (/** @type {number} */ n, /** @type {string} */ w) => `${fmtInt(n)} ${w}${n === 1 ? '' : 's'}`;
  if (!s.measured) return { strong: false, text: `No target has been measured yet (${when}).` };
  if (s.with_normal < s.targets / 2) {
    // Too early for "normal": loss only.
    if (!lossy) return { strong: false, text: `None of the ${fmtInt(s.measured)} measured targets are losing packets, ${when}.` };
    return { strong: false, text: `${fmtInt(lossy)} of ${fmtInt(s.measured)} targets are losing packets, ${when}.` };
  }
  if (!bad) return { strong: false, text: `All ${fmtInt(s.measured)} measured targets are within their normal, ${when}.` };
  if (bad >= s.measured / 4 && s.providers_affected >= 3 && s.geos_affected >= 2) {
    return {
      strong: true,
      text: `${fmtInt(bad)} of ${fmtInt(s.measured)} targets (${Math.round((bad / s.measured) * 100)}%) across ${fmtInt(s.providers_affected)} of ${pl(s.providers, 'provider')} in ${pl(s.geos_affected, 'region')} got slower or lost packets at the same time. ` +
        'When this many unrelated networks degrade at once, the common factor is your side: your connection or your ISP\'s routes.',
    };
  }
  const where = [pl(s.providers_affected, 'provider')];
  if (s.geos) where.push(pl(s.geos_affected, 'region'));
  return { strong: false, text: `${fmtInt(bad)} of ${fmtInt(s.measured)} targets are worse than normal (${where.join(', ')}), ${when}.` };
}

/** @typedef {{t:TargetInfo,row:OvRow|null,tr:HTMLElement,cells:Object<string,HTMLElement>}} OvEntry */
/** @typedef {{id:string,title:string,kind:string,geos:Map<string,string[]>}} OvProvider */

class Overview {
  constructor() {
    /** @type {OvData|null} */
    this.data = null;
    /** @type {AbortController|null} */
    this.ctl = null;
    this.kind = '';
    /** @type {Map<string, OvEntry>} */
    this.entries = new Map();
    /** @type {string[]} */
    this.order = [];
    /** @type {Map<string, HTMLElement>} heatmap cells by provider + '\0' + geo */
    this.cells = new Map();
    this.hmCols = 0;
    /** @type {OvProvider[]} */
    this.provs = [];
    /** @type {HTMLElement|null} */
    this.noneRow = null;
    /** @type {number|null} */
    this.saveScroll = null;
    this.err = '';

    this.meta = h('span', { class: 'meta' });
    this.breadth = h('p', { class: 'ov-breadth', 'aria-live': 'polite' });
    this.modeSeg = h('div', { class: 'seg sm', role: 'group', 'aria-label': 'Heatmap colour' });
    this.hmLegend = h('div', { class: 'hm-legend' });
    this.hm = h('div', { class: 'hm' });
    this.hmWrap = h('div', { class: 'hm-wrap' }, this.hm);
    this.filterIn = /** @type {HTMLInputElement} */ (h('input', { type: 'search', class: 'ov-q', placeholder: 'Filter: target, provider, city, country', 'aria-label': 'Filter targets' }));
    this.filterIn.addEventListener('input', () => { state.oq = this.filterIn.value.trim(); writeHash(); this.applyRows(); });
    this.chips = h('div', { class: 'seg sm ov-geos', role: 'group', 'aria-label': 'Region' });
    this.probBtn = h('button', { class: 'btn sm', type: 'button', text: 'Problems only', onclick: () => { state.op = !state.op; writeHash(); this.renderControls(); this.applyRows(); } });
    this.cellChip = h('span', { class: 'ov-cell' });
    this.count = h('span', { class: 'ov-count muted small', 'aria-live': 'polite' });
    this.thead = h('tr');
    this.tbody = h('tbody');
    this.table = h('table', { class: 'ovt' }, h('thead', {}, this.thead), this.tbody);
    this.tableBox = h('div', { class: 'ovt-box' },
      h('div', { class: 'ov-controls' }, this.filterIn, this.chips, this.probBtn, this.cellChip, this.count),
      h('div', { class: 'ovt-wrap' }, this.table));
    this.el = h('section', { class: 'panel overview', id: 'overview', 'aria-label': 'Overview of every target' },
      h('div', { class: 'panel-head' }, h('h2', { text: 'Overview' }), this.meta),
      this.breadth,
      h('div', { class: 'hm-head' }, h('span', { class: 'hm-title', text: 'Providers by region' }), this.modeSeg, this.hmLegend),
      this.hmWrap,
      this.tableBox);
    this.hm.addEventListener('mousemove', (e) => this.hover(e));
    this.hm.addEventListener('mouseleave', hideTip);
    this.hm.addEventListener('click', (e) => this.click(e));
    new ResizeObserver(() => { if (this.hmCols && this.blocks() !== this.hmCols) this.layout(); }).observe(this.hmWrap);
  }

  /** Non-path targets that have this kind, by name. */
  targetsOf(/** @type {string} */ kind) { return allTargets.filter((t) => !t.layer && t.kinds.includes(kind)); }

  async load() {
    this.ctl?.abort();
    const ctl = this.ctl = new AbortController();
    const kind = ovKind();
    const { from, to } = queryRange();
    this.el.classList.add('loading');
    try {
      /** @type {OvData} */
      const d = await fetchJSON('api/overview?' + new URLSearchParams({ from, to, kind }), { signal: ctl.signal });
      if (ctl.signal.aborted) return;
      this.data = d;
      this.err = '';
      if (d.kind !== this.kind) this.build(d.kind);
      this.render();
    } catch (e) {
      if (/** @type {any} */ (e).name === 'AbortError') return;
      this.err = `Could not load the overview: ${/** @type {Error} */ (e).message}`;
      this.renderBreadth();
    } finally {
      if (this.ctl === ctl) this.el.classList.remove('loading');
    }
  }

  /** (Re)builds the table rows and the heatmap for a kind; refreshes then update them in place. */
  build(/** @type {string} */ kind) {
    this.kind = kind;
    this.entries = new Map();
    for (const t of this.targetsOf(kind)) {
      const cells = {
        now: h('td', { class: 'num' }), normal: h('td', { class: 'num sm-off' }), ratio: h('td', { class: 'num' }),
        loss: h('td', { class: 'num' }), state: h('td', { class: 'st sm-off' }),
      };
      const geo = geoOf(t);
      const tr = h('tr', { onclick: () => openFocus(t.name) },
        h('td', { class: 't' },
          h('button', { class: 'ov-name', type: 'button', title: `Show ${t.name} on its chart`, text: t.name }),
          h('span', { class: 'sub', text: `${t.provider_title || ''} · ${GEO_SHORT[geo]}` })),
        h('td', { class: 'pv sm-off', text: t.provider_title || '' }),
        h('td', { class: 'loc sm-off', text: locationOf(t) || '—' }),
        h('td', { class: 'rg sm-off', text: GEO_TITLE[geo] }),
        cells.now, cells.normal, cells.ratio, cells.loss, cells.state,
        h('td', { class: 'act sm-off' }, traceOn && t.trace ? traceButton(t.name, 'Investigate') : null));
      this.entries.set(t.name, { t, row: null, tr, cells });
    }
    this.order = [];
    this.tbody.replaceChildren();
    this.layout();
  }

  /** Heatmap blocks side by side that fit the width. */
  blocks() {
    const w = this.hmWrap.clientWidth;
    const bw = HM_LABEL + this.geoCols().length * (HM_CELL + HM_GAP);
    return Math.max(1, Math.floor((w + HM_BLOCK_GAP) / (bw + HM_BLOCK_GAP)));
  }

  geoCols() {
    const cols = [...GEOS];
    if ([...this.entries.values()].some((e) => !geoOf(e.t))) cols.push('');
    return cols;
  }

  /** Providers grouped by kind (catalog order of kinds), alphabetical within: stable across refreshes. */
  providers() {
    /** @type {Map<string, OvProvider>} */
    const m = new Map();
    for (const e of this.entries.values()) {
      const id = e.t.provider || e.t.group;
      let p = m.get(id);
      if (!p) m.set(id, p = { id, title: e.t.provider_title || id, kind: e.t.provider_kind || '', geos: new Map() });
      const g = geoOf(e.t);
      if (!p.geos.has(g)) p.geos.set(g, []);
      /** @type {string[]} */ (p.geos.get(g)).push(e.t.name);
    }
    const kindIdx = (/** @type {string} */ k) => { const i = PROVIDER_KINDS.findIndex(([x]) => x === k); return i < 0 ? PROVIDER_KINDS.length : i; };
    return [...m.values()].sort((a, b) => kindIdx(a.kind) - kindIdx(b.kind) || a.title.localeCompare(b.title));
  }

  /** Lays the heatmap out in blocks of provider rows, each with the region header. */
  layout() {
    this.provs = this.providers();
    const cols = this.geoCols();
    // Rows: a kind heading before each kind's first provider.
    /** @type {({kind:string}|{p:OvProvider})[]} */
    const rows = [];
    let lastKind = null;
    const kinds = new Set(this.provs.map((p) => p.kind));
    for (const p of this.provs) {
      if (p.kind !== lastKind && kinds.size > 1) rows.push({ kind: p.kind });
      lastKind = p.kind;
      rows.push({ p });
    }
    const nb = this.hmCols = Math.min(this.blocks(), Math.max(1, Math.ceil(rows.length / 8)));
    const per = Math.ceil(rows.length / nb);
    this.cells = new Map();
    const blocks = [];
    for (let b = 0; b < nb; b++) {
      const part = rows.slice(b * per, (b + 1) * per);
      if (!part.length) break;
      const el = h('div', { class: 'hm-block', role: 'presentation' });
      el.style.gridTemplateColumns = `minmax(0, ${HM_LABEL}px) repeat(${cols.length}, ${HM_CELL}px)`;
      el.append(h('span', { class: 'hm-corner' }), ...cols.map((g) => h('span', { class: 'hm-g', title: GEO_TITLE[g] + (g === 'global' ? ' (anycast)' : ''), text: GEO_SHORT[g] })));
      for (const r of part) {
        if ('kind' in r) {
          el.append(h('span', { class: 'hm-kind', text: (PROVIDER_KINDS.find(([k]) => k === r.kind) || ['', 'Other'])[1] }));
          continue;
        }
        const p = r.p;
        el.append(h('button', { class: 'hm-l', type: 'button', 'data-p': p.id, title: `Show only ${p.title} in the table`, text: p.title }));
        for (const g of cols) {
          const c = h('span', { class: 'hm-c', 'data-p': p.id, 'data-g': g });
          if (p.geos.has(g)) this.cells.set(p.id + '\0' + g, c);
          el.append(c);
        }
      }
      blocks.push(el);
    }
    this.hm.replaceChildren(...blocks);
    this.hm.style.gridTemplateColumns = `repeat(${blocks.length}, max-content)`;
    if (this.data) this.renderHeatmap();
  }

  /** Heatmap colour mode: "vs normal" once half the targets have a normal, else latency; the hash overrides. */
  mode() {
    const s = this.data?.summary;
    const normalOK = !!s && s.with_normal > 0;
    if (state.om === 'r' && normalOK) return 'r';
    if (state.om === 'ms') return 'ms';
    return s && s.with_normal >= s.targets / 2 && normalOK ? 'r' : 'ms';
  }

  render() {
    const d = /** @type {OvData} */ (this.data);
    for (const e of this.entries.values()) e.row = null;
    for (const r of d.rows) { const e = this.entries.get(r.target); if (e) e.row = r; }
    const span = d.to - d.from;
    this.meta.textContent = `${KIND_LABEL[/** @type {'https'} */ (d.kind)]} · ${fmtInt(d.rows.length)} targets · mean over the ${windowWords(d)}`;
    this.renderBreadth();
    this.renderHead(span);
    for (const e of this.entries.values()) this.renderRow(e);
    this.renderControls();
    this.applyRows();
    this.renderHeatmap();
  }

  renderBreadth() {
    const d = this.data;
    if (this.err || !d) {
      this.breadth.className = 'ov-breadth err';
      this.breadth.textContent = this.err || 'Loading…';
      return;
    }
    const b = breadthText(d.summary, windowWords(d));
    this.breadth.className = 'ov-breadth' + (b.strong ? ' strong' : '');
    /** @type {(string|Node)[]} */
    const kids = [b.text];
    // The banner judges the last minute with stricter thresholds; say so, so
    // that "All good" above and "N targets lost packets" here do not read
    // as a contradiction.
    if (verdict && verdict.kind !== 'unknown') {
      const what = d.summary.with_normal < d.summary.targets / 2 ? '1% loss or more' : '1% loss or more, or 1.5× their normal';
      const kind = KIND_LABEL[/** @type {'https'} */ (d.kind)];
      kids.push(h('span', { class: 'muted', text: ` This line covers ${kind} over the whole range and counts ${what}; the verdict above judges every kind over the last minute.` }));
    }
    // Rows still too new to judge can already show heavy loss in the table:
    // say so, so "none are losing packets" never sits next to a 100% row.
    const early = d.rows.filter((r) => r.state === 'unmeasured' && r.lost > 0).length;
    if (early) kids.push(h('span', { text: ` ${fmtInt(early)} more ${early === 1 ? 'is' : 'are'} losing packets, but ${early === 1 ? 'it has' : 'they have'} too few checks yet to judge (Status “Not measured”).` }));
    const un = d.summary.targets - d.summary.measured - early;
    if (un > 0) kids.push(h('span', { class: 'muted', text: ` ${fmtInt(un)} not measured.` }));
    if (d.capped) kids.push(h('span', { class: 'muted', text: ` The Overview covers at most the last 30 days of the range.` }));
    this.breadth.replaceChildren(...kids);
  }

  renderHead(/** @type {number} */ span) {
    const [sc, desc] = this.sortKey();
    this.thead.replaceChildren(...OV_COLS.map(([id, label, cls]) => {
      let text = label;
      if (id === 'now') text = `${isRelative() ? 'Now' : 'Mean'} (${fmtDur(span)})`;
      const on = sc === id;
      return h('th', { class: cls, scope: 'col', 'aria-sort': on ? (desc ? 'descending' : 'ascending') : null },
        h('button', { type: 'button', class: 'ov-sort', title: `Sort by ${label}`, onclick: () => this.sortBy(id) }, text, on ? h('span', { class: 'arr', 'aria-hidden': 'true', text: desc ? ' ▾' : ' ▴' }) : null));
    }), h('th', { class: 'act sm-off', scope: 'col' }));
  }

  /** [column, descending]; '' is the default severity order. @returns {[string, boolean]} */
  sortKey() {
    const s = state.os || '';
    return s.startsWith('-') ? [s.slice(1), true] : [s, false];
  }

  sortBy(/** @type {string} */ id) {
    const [sc, desc] = this.sortKey();
    // Numbers sort largest first on the first click, text A to Z.
    const numeric = ['now', 'normal', 'ratio', 'loss'].includes(id);
    state.os = sc === id ? (desc ? id : '-' + id) : (numeric ? '-' + id : id);
    writeHash();
    if (this.data) { this.renderHead(this.data.to - this.data.from); this.applyRows(); }
  }

  renderRow(/** @type {OvEntry} */ e) {
    const r = e.row, c = e.cells;
    const st = r ? r.state : 'unmeasured';
    e.tr.className = `st-${st}`;
    c.now.textContent = r && r.now_ms != null ? fmtMs(r.now_ms) : '—';
    c.normal.textContent = r && r.normal_ms != null ? fmtMs(r.normal_ms) : '—';
    c.normal.title = r && r.normal_ms != null
      ? `Normal: median ${fmtMs(r.normal_ms)} ms, p95 ${fmtMs(r.normal_p95_ms)} ms over the past week${r.hour_of_day ? ', same hour of day' : ''}`
      : 'No normal yet (needs about a day of data)';
    c.ratio.textContent = r && r.ratio != null ? fmtRatio(r.ratio) : '—';
    c.ratio.className = 'num' + (r && r.ratio != null && r.ratio >= VERY_SLOW ? ' bad' : r && r.ratio != null && r.ratio >= SLOW_RATIO ? ' warn' : '');
    c.loss.textContent = r && r.n + r.lost ? fmtPct(r.loss) : '—';
    // Colour follows the state: loss without enough evidence yet stays plain.
    c.loss.className = 'num' + (st === 'failing' ? ' bad' : st === 'lossy' ? ' warn' : '');
    c.loss.title = r && r.lost ? `${r.lost} of ${r.n + r.lost} samples lost, mostly ${REASON_LABEL[/** @type {'dns'} */ (r.reason || 'other')] || r.reason}` : '';
    const chip = c.state.firstElementChild;
    if (!chip || chip.getAttribute('data-st') !== st) c.state.replaceChildren(h('span', { class: `ovst st-${st}`, 'data-st': st, text: OV_STATES[st]?.label || st }));
  }

  renderControls() {
    const present = new Set([...this.entries.values()].map((e) => geoOf(e.t)));
    const geos = [...GEOS, ''].filter((g) => present.has(g));
    this.chips.hidden = geos.length < 2;
    this.chips.replaceChildren(h('button', { type: 'button', text: 'All', 'aria-pressed': String(!state.og), onclick: () => this.setGeo('') }),
      ...geos.filter((g) => g).map((g) => h('button', { type: 'button', text: GEO_SHORT[g], title: GEO_TITLE[g], 'aria-pressed': String(state.og === g), onclick: () => this.setGeo(g) })));
    this.probBtn.setAttribute('aria-pressed', String(state.op));
    if (this.filterIn.value.trim() !== state.oq) this.filterIn.value = state.oq;
    const [pid, g] = this.cellFilter();
    if (pid) {
      const p = (this.provs || []).find((x) => x.id === pid);
      this.cellChip.replaceChildren(h('button', {
        class: 'btn sm on', type: 'button', title: 'Clear this filter',
        text: `${p ? p.title : pid}${g != null ? ' · ' + GEO_TITLE[g] : ''} ×`, onclick: () => this.setCell(''),
      }));
    } else this.cellChip.replaceChildren();
    if (state.ot) {
      this.cellChip.append(h('button', {
        class: 'btn sm on', type: 'button', title: 'Show every target',
        text: `${HOME_TILE[state.ot] || state.ot} ×`, onclick: () => { state.ot = ''; writeHash(); this.renderControls(); this.applyRows(); },
      }));
    }
    const m = this.mode();
    const noNormal = !this.data || this.data.summary.with_normal === 0;
    this.modeSeg.replaceChildren(
      h('button', { type: 'button', text: 'vs normal', 'aria-pressed': String(m === 'r'), disabled: noNormal, title: noNormal ? 'No normal yet: it needs about a day of data' : 'Colour by how much slower than normal', onclick: () => this.setMode('r') }),
      h('button', { type: 'button', text: 'latency', 'aria-pressed': String(m === 'ms'), title: 'Colour by round-trip time', onclick: () => this.setMode('ms') }));
    this.renderLegend(m);
  }

  renderLegend(/** @type {string} */ m) {
    const sw = (/** @type {string} */ cls, /** @type {string} */ label, /** @type {string} */ glyph = '') => h('span', { class: 'hm-li' }, h('span', { class: `hm-c ${cls}`, text: glyph }), label);
    const kids = m === 'r'
      ? RATIO_BINS.map(([, l], i) => sw(`r${i}`, /** @type {string} */ (l)))
      : MS_BINS.map(([, l], i) => sw(`l${i}`, /** @type {string} */ (l)));
    kids.push(sw(m === 'r' ? 'r0 dot' : 'l0 dot', 'loss ≥1%'), sw('fail', 'loss ≥20%', '×'), sw('unm', 'not measured'));
    if (m === 'r') kids.push(sw('nonorm', 'no normal yet', '·'));
    this.hmLegend.replaceChildren(...kids);
  }

  setGeo(/** @type {string} */ g) { state.og = g; writeHash(); this.renderControls(); this.applyRows(); }
  setMode(/** @type {string} */ m) { state.om = m; writeHash(); this.renderControls(); this.renderHeatmap(); }
  setCell(/** @type {string} */ v) { state.oc = v; writeHash(); this.renderControls(); this.applyRows(); this.renderHeatmap(); }

  /** The heatmap filter: [provider, region] (region undefined: the whole provider). */
  cellFilter() {
    if (!state.oc) return ['', undefined];
    const i = state.oc.lastIndexOf(':');
    if (i < 0) return [state.oc, undefined];
    const g = state.oc.slice(i + 1);
    return [state.oc.slice(0, i), g === '*' ? undefined : g];
  }

  /** Filters and sorts the table rows, moving rows only when the order changed. */
  applyRows() {
    const q = state.oq.toLowerCase();
    const [pid, pg] = this.cellFilter();
    const [sc, desc] = this.sortKey();
    /** @type {OvEntry[]} */
    const shown = [];
    let total = 0;
    for (const e of this.entries.values()) {
      total++;
      const t = e.t, st = e.row ? e.row.state : 'unmeasured';
      let ok = true;
      if (q) ok = [t.name, t.provider_title, t.city, t.country].some((x) => x && x.toLowerCase().includes(q));
      if (ok && state.og) ok = geoOf(t) === state.og;
      if (ok && state.ot) ok = homeCategory(t) === state.ot;
      if (ok && state.op) ok = PROBLEMS.has(st);
      if (ok && pid) ok = (t.provider || t.group) === pid && (pg === undefined || geoOf(t) === pg);
      e.tr.hidden = !ok;
      if (ok) shown.push(e);
    }
    this.count.textContent = `Showing ${fmtInt(shown.length)} of ${fmtInt(total)}`;
    const all = [...this.entries.values()];
    all.sort(this.comparator(sc, desc));
    const names = all.map((e) => e.t.name);
    if (names.length !== this.order.length || names.some((n, i) => n !== this.order[i])) {
      const ae = document.activeElement;
      this.tbody.append(...all.map((e) => e.tr));
      this.order = names;
      if (ae instanceof HTMLElement && this.tbody.contains(ae)) ae.focus({ preventScroll: true });
    }
    if (!shown.length && total) {
      if (!this.noneRow) this.noneRow = h('tr', { class: 'none' }, h('td', { colspan: String(OV_COLS.length + 1), class: 'muted', text: 'No target matches the filters.' }));
      this.tbody.append(this.noneRow);
    } else this.noneRow?.remove();
  }

  /** @param {string} col @param {boolean} desc @returns {(a: OvEntry, b: OvEntry) => number} */
  comparator(col, desc) {
    const row = (/** @type {OvEntry} */ e) => e.row || { target: e.t.name, state: 'unmeasured', n: 0, lost: 0, loss: 0 };
    if (!col) return (a, b) => bySeverity(row(a), row(b));
    /** @type {(e: OvEntry) => string|number|null|undefined} */
    const key = {
      target: (/** @type {OvEntry} */ e) => e.t.name.toLowerCase(),
      provider: (/** @type {OvEntry} */ e) => (e.t.provider_title || '').toLowerCase(),
      loc: (/** @type {OvEntry} */ e) => locationOf(e.t).toLowerCase() || null,
      region: (/** @type {OvEntry} */ e) => { const i = GEOS.indexOf(geoOf(e.t)); return i < 0 ? null : i; },
      now: (/** @type {OvEntry} */ e) => e.row?.now_ms,
      normal: (/** @type {OvEntry} */ e) => e.row?.normal_ms,
      ratio: (/** @type {OvEntry} */ e) => e.row?.ratio,
      loss: (/** @type {OvEntry} */ e) => (e.row && e.row.n + e.row.lost ? e.row.loss : null),
      state: (/** @type {OvEntry} */ e) => OV_STATES[row(e).state]?.rank,
    }[col] || ((/** @type {OvEntry} */ e) => e.t.name);
    return (a, b) => {
      const x = key(a), y = key(b);
      // Missing values last in either direction.
      if (x == null || y == null) return x == null && y == null ? bySeverity(row(a), row(b)) : x == null ? 1 : -1;
      const c = x < y ? -1 : x > y ? 1 : 0;
      return (desc ? -c : c) || bySeverity(row(a), row(b));
    };
  }

  /** Aggregates one heatmap cell. */
  cellStats(/** @type {string} */ pid, /** @type {string} */ g) {
    const p = (this.provs || []).find((x) => x.id === pid);
    const names = p?.geos.get(g) || [];
    const rows = names.map((n) => this.entries.get(n)?.row).filter((r) => r != null);
    const measured = /** @type {OvRow[]} */ (rows).filter((r) => r.state !== 'unmeasured');
    let n = 0, lost = 0;
    for (const r of measured) { n += r.n; lost += r.lost; }
    const ratios = measured.flatMap((r) => (r.ratio != null ? [r.ratio] : []));
    const nows = measured.flatMap((r) => (r.now_ms != null ? [r.now_ms] : []));
    const normals = measured.flatMap((r) => (r.normal_ms != null ? [r.normal_ms] : []));
    const worst = [.../** @type {OvRow[]} */ (rows)].sort(bySeverity)[0];
    return {
      p, names, rows: /** @type {OvRow[]} */ (rows), measured, loss: n + lost ? lost / (n + lost) : 0,
      judged: lost >= LOSS_MIN_LOST && n + lost >= LOSS_MIN_SEEN,
      lossyAny: measured.some((r) => r.state === 'lossy' || r.state === 'failing'),
      ratio: median(ratios), now: median(nows), normal: median(normals), worst,
    };
  }

  renderHeatmap() {
    if (!this.data) return;
    const m = this.mode();
    const [pid, pg] = this.cellFilter();
    for (const [k, el] of this.cells) {
      const [id, g] = k.split('\0');
      const c = this.cellStats(id, g);
      let cls = 'hm-c has', glyph = '';
      if (!c.rows.length) cls = 'hm-c';
      else if (!c.measured.length) cls += ' unm';
      else if (c.judged && c.loss >= FAIL_LOSS) { cls += ' fail'; glyph = '×'; }
      else if (m === 'r') {
        if (isNaN(c.ratio)) { cls += ' nonorm'; glyph = '·'; }
        else cls += ' r' + RATIO_BINS.findIndex(([x]) => c.ratio < /** @type {number} */ (x));
      } else if (isNaN(c.now)) cls += ' unm';
      else cls += ' l' + MS_BINS.findIndex(([x]) => c.now < /** @type {number} */ (x));
      if (c.measured.length && !(c.judged && c.loss >= FAIL_LOSS) && (c.lossyAny || (c.judged && c.loss >= REAL_LOSS))) cls += ' dot';
      if (pid === id && (pg === undefined || pg === g)) cls += ' sel';
      if (el.className !== cls) el.className = cls;
      if (el.textContent !== glyph) el.textContent = glyph;
    }
    for (const l of this.hm.querySelectorAll('.hm-l')) l.classList.toggle('sel', pid === l.getAttribute('data-p') && pg === undefined);
  }

  /** @param {MouseEvent} e */
  hover(e) {
    const c = /** @type {HTMLElement|null} */ (/** @type {HTMLElement} */ (e.target).closest('.hm-c.has'));
    if (!c) { hideTip(); return; }
    showTip(this.tipNodes(/** @type {string} */ (c.getAttribute('data-p')), /** @type {string} */ (c.getAttribute('data-g'))), e.clientX, e.clientY);
  }

  tipNodes(/** @type {string} */ pid, /** @type {string} */ g) {
    const c = this.cellStats(pid, g);
    const nodes = [h('div', { class: 't', text: `${c.p ? c.p.title : pid} · ${GEO_TITLE[g]}` })];
    nodes.push(h('div', { text: `${c.names.length} target${c.names.length === 1 ? '' : 's'}${c.measured.length < c.rows.length ? `, ${c.measured.length} measured` : ''}` }));
    if (!c.measured.length) nodes.push(h('div', { class: 'muted', text: 'Not measured in this range' }));
    else {
      let now = `now ${fmtMs(c.now)} ms`;
      if (!isNaN(c.normal)) now += ` vs normal ${fmtMs(c.normal)} ms`;
      if (!isNaN(c.ratio)) now += ` (${fmtRatio(c.ratio)})`;
      nodes.push(h('div', { text: now + (c.names.length > 1 ? ' (medians)' : '') }));
      if (this.mode() === 'r' && isNaN(c.ratio)) nodes.push(h('div', { class: 'muted', text: 'no normal yet (needs about a day of data)' }));
      nodes.push(h('div', { text: `loss ${fmtPct(c.loss)}` }));
    }
    if (c.worst && c.names.length > 1) {
      const w = c.worst;
      const bits = [OV_STATES[w.state]?.label || w.state];
      if (w.now_ms != null) bits.push(`${fmtMs(w.now_ms)} ms`);
      if (w.ratio != null) bits.push(fmtRatio(w.ratio));
      if (w.lost) bits.push(`${fmtPct(w.loss)} loss`);
      nodes.push(h('div', { class: 'sep' }), h('div', { class: 'row' }, h('span', { class: 'k', text: 'worst' }), `${w.target}: ${bits.join(', ')}`));
    }
    return nodes;
  }

  /** A cell filters the table to its provider and region; a label to the provider. Again: clear. */
  click(/** @type {MouseEvent} */ e) {
    const el = /** @type {HTMLElement|null} */ (/** @type {HTMLElement} */ (e.target).closest('.hm-l, .hm-c.has'));
    if (!el) return;
    const pid = /** @type {string} */ (el.getAttribute('data-p'));
    const v = el.classList.contains('hm-l') ? pid : `${pid}:${el.getAttribute('data-g')}`;
    this.setCell(state.oc === v ? '' : v);
    if (el.classList.contains('hm-c') && matchMedia('(hover: none)').matches) {
      e.stopPropagation(); // keep the tooltip open on touch screens
      showTip(this.tipNodes(pid, /** @type {string} */ (el.getAttribute('data-g'))), e.clientX, e.clientY);
    }
    if (state.oc) {
      const r = this.tableBox.getBoundingClientRect();
      if (r.top > window.innerHeight) this.tableBox.scrollIntoView({ block: 'start', behavior: smooth() });
    }
  }
}

// ---------- calls (call quality) ----------

/** @typedef {{state:(string|null)[],mos:(number|null)[],rtt_ms:(number|null)[],jitter_ms:(number|null)[],loss:(number|null)[]}} CallsSeries */
/** @typedef {{at:number,state:string,mos:number|null,rtt_ms:number|null,jitter_ms:number|null,loss:number|null}} CallsMinute */
/** @typedef {{state:string,mos:number|null,rtt_ms:number|null,rtt_p95_ms:number|null,jitter_ms:number|null,loss:number|null,samples:number,unknown?:number,factor?:string,worst?:CallsMinute,minutes:CallsSeries}} CallsGroup */
/** @typedef {Partial<CallsGroup> & {app:string,udp:string,region?:string,web_ms?:number|null,web_loss?:number|null,web_samples?:number}} CallsApp */
/** @typedef {{source:string,app?:string,kind?:string,summary?:string,factor?:string}} CallsCause */
/** @typedef {{from:number,to:number,start:number,step:number,state:string,quiet?:boolean,cause?:CallsCause,path?:CallsGroup,apps:CallsApp[]}} CallsData */

const CALLS_LIVE_MS = 15000; // the top card ("updated 4 s ago")
const CALLS_DAY_MS = 60000; // the working-hours strip and the sparklines
const CALLS_LIVE_WINDOW = '5m';
// Working hours, local time, Monday to Friday.
const WORK_FROM = 9, WORK_TO = 17;
/** Call states: the label (rows, strip) and the top card's headline. */
/** @type {Object<string,{label:string,head:string}>} */
const CALL_STATES = {
  good: { label: 'Good', head: 'Good for calls' },
  fair: { label: 'Fair', head: 'Fair for calls' },
  poor: { label: 'Poor', head: 'Poor for calls' },
  unusable: { label: 'Unusable', head: 'Calls will break up' },
  blocked: { label: 'Blocked', head: 'Calls are blocked on this network' },
  not_enough_data: { label: 'Not enough data yet', head: 'Not enough data yet' },
  server_not_answering: { label: 'This test server isn\'t answering; your other call checks look normal', head: 'The test servers aren\'t answering; your other call checks look normal' },
};
const BAD_CALL = new Set(['poor', 'unusable']);
/** @type {Object<string,string>} */
const APP_TITLE = { teams: 'Teams', meet: 'Google Meet', zoom: 'Zoom', slack: 'Slack', discord: 'Discord', whatsapp: 'WhatsApp', webex: 'Webex' };
// What a verdict means for a call: the top card's cause line.
/** @type {Object<string,string>} */
const CALL_CAUSE = {
  lan: 'your Wi-Fi or router is dropping packets',
  isp: 'your ISP is dropping packets',
  upstream: 'there is trouble beyond your ISP',
  dns: 'name lookups (DNS) are failing, so joining a call may fail',
  no_network: 'this computer has no network connection',
  udp_blocked: 'this network (or a firewall on this computer) blocks UDP; calls fall back to slower relays or fail',
  stun_blocked: 'this network blocks the ports calls use (STUN); calls may fall back to slower relays',
  udp_degraded: 'the UDP path to call services is losing packets or jittery',
};
// Where a problem was, per incident kind ("What happened at …?").
/** @type {Object<string,string>} */
const CALL_WHERE = {
  lan: 'in your home network (Wi-Fi or router)',
  isp: 'at your ISP edge; Wi-Fi/router fine',
  upstream: 'beyond your ISP; Wi-Fi/router and ISP edge fine',
  dns: 'while name lookups (DNS) were failing; the network path was fine',
  no_network: 'with no network connection at all',
  udp_blocked: 'while UDP was blocked on this network',
  stun_blocked: 'while the call ports (STUN) were blocked',
  udp_degraded: 'on the UDP path to call services; Wi-Fi/router and ISP edge fine',
  service: 'while some services had trouble; your connection was fine',
};
// The incident that best explains a bad call, most specific first.
const CALL_BLAME_ORDER = ['no_network', 'lan', 'isp', 'upstream', 'udp_blocked', 'stun_blocked', 'udp_degraded', 'dns', 'service'];

/** @type {Calls|null} */
let calls = null;
let callsOn = false; // the profile has call-path (udp-path) or app targets

function callsShown() { return !!calls && currentView() === 'calls' && !(state.inv && inv); }
function callLabel(/** @type {string} */ st) { return (CALL_STATES[st] || { label: st }).label; }
function hhmm(/** @type {number} */ ms) { return new Date(ms).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }); }
function hh(/** @type {number} */ hour) { return String(hour).padStart(2, '0'); }
/** "steady (jitter 3 ms)", by how much the jitter is. */
function jitterWords(/** @type {number|null|undefined} */ j) {
  if (j == null || !isFinite(j)) return '';
  const ms = `${fmtMs(j)} ms`;
  if (j < 10) return `steady (jitter ${ms})`;
  if (j < 30) return `some jitter (${ms})`;
  return `jittery (${ms})`;
}
/** An Azure region as words: "swedencentral" → "Sweden Central", "eastus2" → "East US 2". */
function regionName(/** @type {string} */ r) {
  return r.replace(/(north|south|east|west|central)/g, ' $1 ').replace(/(\d+)/g, ' $1').trim().split(/\s+/)
    .map((w) => (w === 'us' || w === 'uk' || w === 'uae' ? w.toUpperCase() : w[0].toUpperCase() + w.slice(1))).join(' ');
}
/** Today's working hours as [from, to] (ms), or null on a weekend. */
function workHours(/** @type {number} */ now) {
  const d = new Date(now);
  if (d.getDay() === 0 || d.getDay() === 6) return null;
  const a = new Date(d), b = new Date(d);
  a.setHours(WORK_FROM, 0, 0, 0);
  b.setHours(WORK_TO, 0, 0, 0);
  return [a.getTime(), b.getTime()];
}
/** The state colour token of a call state. */
function callColor(/** @type {string|null} */ st) {
  switch (st) {
    case 'good': return css('--c-good');
    case 'fair': return css('--c-fair');
    case 'poor': return css('--c-poor');
    case 'unusable': return css('--c-bad');
  }
  return '';
}
/** The minute index of t in d's series (may be out of range). */
function minuteIdx(/** @type {CallsData} */ d, /** @type {number} */ t) { return Math.floor((t - d.start) / d.step); }
/** One minute of a group's series, or null when not scored. */
function minuteAt(/** @type {CallsData} */ d, /** @type {CallsGroup|undefined} */ g, /** @type {number} */ i) {
  if (!g || i < 0 || i >= g.minutes.state.length || !g.minutes.state[i]) return null;
  const m = g.minutes;
  return { at: d.start + i * d.step, state: /** @type {string} */ (m.state[i]), mos: m.mos[i], rtt_ms: m.rtt_ms[i], jitter_ms: m.jitter_ms[i], loss: m.loss[i] };
}
/**
 * Runs of minutes in [from, to) whose state is in want, merging gaps of up
 * to 2 minutes; each with its worst minute.
 * @param {CallsData} d @param {CallsGroup|undefined} g @param {number} from @param {number} to @param {Set<string>} want
 */
function callRuns(d, g, from, to, want) {
  /** @type {{a:number,b:number,worst:CallsMinute}[]} */
  const runs = [];
  if (!g) return runs;
  for (let i = Math.max(0, minuteIdx(d, from)); i < g.minutes.state.length; i++) {
    const m = minuteAt(d, g, i);
    if (!m || m.at >= to || !want.has(m.state)) continue;
    const last = runs[runs.length - 1];
    if (last && m.at - last.b <= 2 * d.step) {
      last.b = m.at + d.step;
      if ((m.mos ?? 9) < (last.worst.mos ?? 9)) last.worst = m;
    } else runs.push({ a: m.at, b: m.at + d.step, worst: m });
  }
  return runs;
}
/** The highest MOS a jitter allows (callq.JitterCapMOS): 3.9 from 30 ms, 3.5 from 50 ms, 3.0 from 100 ms. */
function jitterCapMOS(/** @type {number} */ jit) { return jit >= 100 ? 3.0 : jit >= 50 ? 3.5 : jit >= 30 ? 3.9 : Infinity; }
function runText(/** @type {{a:number,b:number}} */ r) { return r.b - r.a <= 60e3 ? hhmm(r.a) : `${hhmm(r.a)}–${hhmm(r.b)}`; }
/** What made a minute bad, in words: "8% packet loss", "jitter up to 45 ms" or "a slow round trip (310 ms)". */
function minuteWhat(/** @type {CallsMinute} */ m, /** @type {string} */ factor) {
  if (factor === 'jitter' && m.jitter_ms != null) return `jitter up to ${fmtMs(m.jitter_ms)} ms`;
  if (factor === 'latency' && m.rtt_ms != null) return `a slow round trip (${fmtMs(m.rtt_ms)} ms)`;
  if (m.loss != null && m.loss > 0) return `${fmtPct(m.loss)} packet loss`;
  if (m.jitter_ms != null && m.jitter_ms >= 30) return `jitter up to ${fmtMs(m.jitter_ms)} ms`;
  return m.rtt_ms != null ? `a slow round trip (${fmtMs(m.rtt_ms)} ms)` : 'no answers';
}
/** The factor (loss, jitter, latency) that costs a minute its band, as callq.Minute.Factor: "jitter" when a jitter cap set the band. */
function minuteFactor(/** @type {CallsMinute} */ m) {
  if (m.mos != null && Math.abs(m.mos - jitterCapMOS(m.jitter_ms ?? 0)) < 0.005) return 'jitter';
  const rtt = m.rtt_ms ?? 0, jit = m.jitter_ms ?? 0, loss = (m.loss ?? 0) * 100;
  const eff = rtt / 2 + 2 * jit + 10;
  const delay = eff < 160 ? eff / 40 : (eff - 120) / 10;
  const lossPen = 2 * Math.min(loss, 5) + 2.5 * Math.max(loss - 5, 0);
  const jitPen = eff > 0 ? (delay * 2 * jit) / eff : 0;
  if (lossPen >= jitPen && lossPen >= delay - jitPen) return 'loss';
  return jitPen >= delay - jitPen ? 'jitter' : 'latency';
}

class Calls {
  constructor() {
    /** @type {CallsData|null} */
    this.live = null;
    /** @type {CallsData|null} */
    this.day = null;
    this.liveAt = 0;
    this.dayAt = 0;
    this.liveBusy = false;
    this.dayBusy = false;
    this.err = '';
    /** @type {number|null} "What happened at …?": the picked time; null is the last 60 minutes */
    this.askAt = null;
    this.askSeq = 0;
    /** @type {Set<string>} expanded app rows */
    this.open = new Set();

    this.dot = h('span', { class: 'cq-dot', 'aria-hidden': 'true' });
    this.headEl = h('span', { class: 'cq-state' });
    this.statsEl = h('span', { class: 'cq-stats' });
    this.causeEl = h('p', { class: 'cq-cause' });
    this.updatedEl = h('span', { class: 'cq-updated muted small' });
    this.top = h('section', { class: 'panel cq-top', 'aria-label': 'Call quality now' },
      h('div', { class: 'cq-line' }, this.dot, this.headEl, this.statsEl,
        whyButton('Your call path', 'fyisp sends tiny STUN requests over UDP, the way call audio and video travel, to Google, Cloudflare and Twilio servers every few seconds. They answer from the edge nearest to you, so this measures your connection to the nearest edge, not the other people on the call.')),
      this.causeEl,
      h('div', { class: 'cq-meta' }, this.updatedEl));

    this.stripSum = h('p', { class: 'cq-strip-sum' });
    this.strip = /** @type {HTMLCanvasElement} */ (h('canvas', { class: 'cq-strip', role: 'img' }));
    this.hours = h('div', { class: 'cq-hours', 'aria-hidden': 'true' });
    for (let x = WORK_FROM; x <= WORK_TO; x++) this.hours.append(h('span', { text: hh(x) }));
    const chip = (/** @type {string} */ st) => h('span', {}, h('span', { class: `chip cs-${st}` }), callLabel(st));
    this.legend = h('div', { class: 'strip-legend cq-legend' }, chip('good'), chip('fair'), chip('poor'), chip('unusable'),
      h('span', {}, h('span', { class: 'chip cs-none' }), 'no data'));
    this.stripBox = h('section', { class: 'panel cq-day', 'aria-label': 'Call quality during working hours' },
      h('div', { class: 'panel-head' }, h('h2', { text: 'Working hours' }), h('span', { class: 'meta', text: `Mon–Fri ${hh(WORK_FROM)}–${hh(WORK_TO)}, local time` })),
      this.stripSum,
      h('div', { class: 'cq-strip-wrap' }, this.strip),
      this.hours,
      this.legend);
    this.strip.addEventListener('mousemove', (e) => this.stripHover(e));
    this.strip.addEventListener('mouseleave', hideTip);
    this.strip.addEventListener('click', (e) => {
      const t = this.stripTime(e);
      if (t != null) { this.askAt = t; this.renderAskInput(); this.ask(); }
    });

    this.askIn = /** @type {HTMLInputElement} */ (h('input', { type: 'datetime-local', 'aria-label': 'Around this time' }));
    this.askIn.addEventListener('change', () => { const t = fromLocalInput(this.askIn.value); this.askAt = isFinite(t) ? t : null; this.ask(); });
    this.askLast = h('button', { class: 'btn sm', type: 'button', text: 'Last 60 min', onclick: () => { this.askAt = null; this.renderAskInput(); this.ask(); } });
    this.answer = h('p', { class: 'cq-answer', 'aria-live': 'polite' });
    this.askLinks = h('div', { class: 'cq-links' });
    this.askBox = h('section', { class: 'panel cq-ask', 'aria-label': 'What happened at a time' },
      h('div', { class: 'panel-head' }, h('h2', { text: 'What happened at …?' })),
      h('div', { class: 'cq-ask-in' }, h('label', { class: 'fld' }, h('span', { text: 'Around (±15 min)' }), this.askIn), this.askLast),
      this.answer, this.askLinks);

    this.appList = h('ul', { class: 'cq-apps-list' });
    this.appsBox = h('section', { class: 'panel cq-apps', 'aria-label': 'Call quality by app' },
      h('div', { class: 'panel-head' }, h('h2', { text: 'By app' }), h('span', { class: 'meta', text: 'last 5 minutes' })),
      this.appList);

    this.el = h('section', { id: 'calls', class: 'calls', 'aria-label': 'Calls', hidden: true },
      this.top, this.stripBox, this.askBox, this.appsBox);
    new ResizeObserver(() => this.drawStrip()).observe(this.strip);
  }

  /** Loads both windows now (a view switch or a refresh). */
  refresh() { return Promise.all([this.loadLive(), this.loadDay()]); }

  /** Every second: reload what is due while Calls is on screen; tick the "updated" age. */
  tick() {
    if (!callsShown() || document.hidden) return;
    const now = Date.now();
    if (now - this.liveAt >= CALLS_LIVE_MS) this.loadLive();
    if (now - this.dayAt >= CALLS_DAY_MS) this.loadDay();
    this.renderUpdated();
  }

  async loadLive() {
    if (this.liveBusy) return;
    this.liveBusy = true;
    this.liveAt = Date.now();
    try {
      this.live = await fetchJSON('api/calls?' + new URLSearchParams({ from: 'now-' + CALLS_LIVE_WINDOW }));
      this.err = '';
    } catch (e) {
      this.err = /** @type {Error} */ (e).message;
      this.liveAt = Date.now() - CALLS_LIVE_MS + 5000; // retry in 5 s
    } finally {
      this.liveBusy = false;
    }
    this.renderTop();
    this.renderApps();
    renderLayers(); // the Services pill counts the call apps' websites here
  }

  /** The day window: today's working hours so far and the last hour. */
  dayRange() {
    const now = Date.now();
    let from = now - 3600e3;
    const wh = workHours(now);
    if (wh && wh[0] < from) from = wh[0];
    return { from: Math.floor(from / 60e3) * 60e3, to: now };
  }

  async loadDay() {
    if (this.dayBusy) return;
    this.dayBusy = true;
    this.dayAt = Date.now();
    const { from } = this.dayRange();
    try {
      this.day = await fetchJSON('api/calls?' + new URLSearchParams({ from: String(from), to: 'now' }));
    } catch {
      this.dayAt = Date.now() - CALLS_DAY_MS + 10000;
    } finally {
      this.dayBusy = false;
    }
    this.renderDay();
    this.renderApps();
    if (this.askAt == null) this.ask();
  }

  render() {
    this.renderTop();
    this.renderDay();
    this.renderApps();
  }

  renderUpdated() {
    if (!this.live) { this.updatedEl.textContent = this.err ? '' : 'Loading…'; return; }
    const age = Math.max(0, Math.round((Date.now() - this.liveAt) / 1000));
    this.updatedEl.textContent = this.err ? `Could not update: ${this.err}` : `updated ${age < 2 ? 'just now' : `${age} s ago`}`;
  }

  renderTop() {
    const d = this.live;
    this.renderUpdated();
    if (!d) {
      this.top.className = 'panel cq-top cs-not_enough_data';
      this.headEl.textContent = this.err ? 'Could not load the call quality' : 'Checking your call path…';
      this.statsEl.textContent = '';
      this.causeEl.textContent = this.err;
      return;
    }
    const st = d.state;
    const quiet = st === 'blocked' && !!d.quiet;
    this.top.className = `panel cq-top cs-${/^[a-z_]+$/.test(st) ? st : 'other'}${quiet ? ' quiet' : ''}`;
    const info = CALL_STATES[st] || { label: st, head: st };
    this.headEl.textContent = quiet ? 'This network blocks call traffic (UDP)' : info.head;
    const p = d.path;
    const bits = [];
    if (p && p.samples > 0 && st !== 'blocked') {
      if (p.rtt_ms != null) bits.push(`${fmtMs(p.rtt_ms)} ms`);
      if (p.loss != null) bits.push(`${fmtPct(p.loss)} loss`);
      const jw = jitterWords(p.jitter_ms);
      if (jw) bits.push(jw);
    }
    this.statsEl.textContent = bits.length ? '· ' + bits.join(' · ') : '';
    this.statsEl.title = p && p.rtt_p95_ms != null ? `Over the last 5 minutes: median round trip ${fmtMs(p.rtt_ms)} ms, p95 ${fmtMs(p.rtt_p95_ms)} ms; ${p.samples} bursts of 5 requests` : '';
    this.causeEl.textContent = this.causeText(d);
    this.causeEl.hidden = !this.causeEl.textContent;
  }

  /** The top card's second line: why it is not good. */
  causeText(/** @type {CallsData} */ d) {
    const st = d.state, c = d.cause, p = d.path;
    if (st === 'good') return '';
    if (st === 'not_enough_data') return 'fyisp needs a few minutes of measurements of your call path.';
    if (st === 'server_not_answering') return 'The call-path test servers stopped answering, while your other call checks look normal.';
    if (st === 'blocked') {
      const why = CALL_CAUSE[c?.kind || 'udp_blocked'];
      return d.quiet ? `It has been this way for hours (common on hotel and office networks): ${why}.` : `Blocked: ${why}.`;
    }
    const label = callLabel(st);
    if (c && c.source !== 'path' && c.kind && CALL_CAUSE[c.kind]) return `${label}: ${CALL_CAUSE[c.kind]}.`;
    if (c && c.source === 'app') {
      // A measured app scores worse than the path: name it (its row below says the same).
      const a = d.apps.find((x) => x.app === c.app);
      const name = APP_TITLE[c.app || ''] || c.app;
      if (a && a.worst) return `${label}: ${minuteWhat(a.worst, c.factor || a.factor || '')} to ${name} (worst minute ${hhmm(a.worst.at)}).`;
      return `${label}: ${name} scores ${label.toLowerCase()}.`;
    }
    if (p && p.worst) {
      const w = p.worst;
      return `${label}: ${minuteWhat(w, c?.factor || p.factor || '')} on your call path (worst minute ${hhmm(w.at)}).`;
    }
    return '';
  }

  renderDay() {
    const d = this.day;
    const now = Date.now();
    const wh = workHours(now);
    this.stripBox.hidden = false;
    if (!wh) {
      this.stripSum.textContent = `No working hours today (Mon–Fri ${hh(WORK_FROM)}–${hh(WORK_TO)}).`;
      this.strip.parentElement?.setAttribute('hidden', '');
      this.hours.hidden = this.legend.hidden = true;
      return;
    }
    this.strip.parentElement?.removeAttribute('hidden');
    this.hours.hidden = this.legend.hidden = false;
    const head = `Today ${hh(WORK_FROM)}–${hh(WORK_TO)}: `;
    if (now < wh[0]) this.stripSum.textContent = head + `starts at ${hh(WORK_FROM)}:00.`;
    else if (!d) this.stripSum.textContent = head + 'loading…';
    else {
      const to = Math.min(now, wh[1]);
      /** @type {Object<string,number>} */
      const n = { good: 0, fair: 0, poor: 0, unusable: 0 };
      let scored = 0;
      for (let t = wh[0]; t < to; t += 60e3) {
        const m = minuteAt(d, d.path, minuteIdx(d, t));
        if (!m) continue;
        scored++;
        n[m.state] = (n[m.state] || 0) + 1;
      }
      if (!scored) this.stripSum.textContent = head + 'not enough data yet.';
      else {
        const pct = (/** @type {number} */ x) => `${Math.round((100 * x) / scored)}%`;
        // Bands that round to 0% are left out.
        const parts = [];
        if (Math.round((100 * n.good) / scored)) parts.push(`Good ${pct(n.good)}`);
        if (Math.round((100 * n.fair) / scored)) parts.push(`Fair ${pct(n.fair)}`);
        const runs = callRuns(d, d.path, wh[0], to, BAD_CALL);
        for (const r of runs.slice(0, 3)) parts.push(`${callLabel(r.worst.state)} ${runText(r)}`);
        if (runs.length > 3) parts.push(`+${runs.length - 3} more`);
        this.stripSum.textContent = head + parts.join(' · ');
      }
    }
    this.strip.setAttribute('aria-label', this.stripSum.textContent || '');
    this.drawStrip();
  }

  drawStrip() {
    const wh = workHours(Date.now());
    const c = this.strip;
    const W = c.clientWidth, H = 20;
    if (!wh || !W) return;
    const dpr = window.devicePixelRatio || 1;
    if (c.width !== Math.round(W * dpr) || c.height !== Math.round(H * dpr)) { c.width = Math.round(W * dpr); c.height = Math.round(H * dpr); }
    const g = /** @type {CanvasRenderingContext2D} */ (c.getContext('2d'));
    g.setTransform(dpr, 0, 0, dpr, 0, 0);
    g.clearRect(0, 0, W, H);
    const total = (wh[1] - wh[0]) / 60e3;
    const now = Date.now();
    g.fillStyle = css('--surface-2');
    g.fillRect(0, 0, W, H);
    const d = this.day;
    const none = css('--nodata');
    for (let i = 0; i < total; i++) {
      const t = wh[0] + i * 60e3;
      if (t >= now) break;
      const m = d ? minuteAt(d, d.path, minuteIdx(d, t)) : null;
      const x0 = (i / total) * W, x1 = ((i + 1) / total) * W;
      g.fillStyle = m ? callColor(m.state) || none : none;
      g.fillRect(x0, m ? 0 : H - 4, Math.max(1, x1 - x0 + 0.5), m ? H : 4);
    }
    // Hour lines.
    g.fillStyle = css('--page');
    for (let x = 1; x < WORK_TO - WORK_FROM; x++) g.fillRect(Math.round((x / (WORK_TO - WORK_FROM)) * W), 0, 1, H);
    if (now > wh[0] && now < wh[1]) {
      g.fillStyle = css('--ink');
      g.fillRect(Math.round(((now - wh[0]) / (wh[1] - wh[0])) * W) - 1, 0, 2, H);
    }
  }

  /** The minute under the pointer on the strip (ms), or null. */
  stripTime(/** @type {MouseEvent} */ e) {
    const wh = workHours(Date.now());
    if (!wh) return null;
    const r = this.strip.getBoundingClientRect();
    const f = (e.clientX - r.left) / r.width;
    if (f < 0 || f >= 1) return null;
    const t = wh[0] + Math.floor(f * ((wh[1] - wh[0]) / 60e3)) * 60e3;
    return t < Date.now() ? t : null;
  }

  stripHover(/** @type {MouseEvent} */ e) {
    const t = this.stripTime(e);
    const d = this.day;
    if (t == null || !d) { hideTip(); return; }
    const m = minuteAt(d, d.path, minuteIdx(d, t));
    const nodes = [h('div', { class: 't', text: `${hhmm(t)}–${hhmm(t + 60e3)}` })];
    if (!m) nodes.push(h('div', { class: 'muted', text: 'Not enough data for this minute' }));
    else {
      const sw = h('span', { class: `chip cs-${m.state}` });
      nodes.push(h('div', { class: 'row' }, sw, h('b', { text: callLabel(m.state) }), m.mos != null ? ` · MOS ${m.mos.toFixed(2)}` : ''));
      nodes.push(h('div', { text: [m.rtt_ms != null ? `${fmtMs(m.rtt_ms)} ms` : '', m.loss != null ? `${fmtPct(m.loss)} loss` : '', m.jitter_ms != null ? `jitter ${fmtMs(m.jitter_ms)} ms` : ''].filter(Boolean).join(' · ') }));
    }
    nodes.push(h('div', { class: 'muted', text: 'Click: what happened at this time?' }));
    showTip(nodes, e.clientX, e.clientY);
  }

  renderAskInput() { this.askIn.value = this.askAt == null ? '' : toLocalInput(this.askAt); }

  /** "What happened at …?": one sentence naming the layer at fault, from the call minutes and the incidents. */
  async ask() {
    const seq = ++this.askSeq;
    const now = Date.now();
    const a = this.askAt == null ? now - 3600e3 : this.askAt - 15 * 60e3;
    const b = this.askAt == null ? now : Math.min(now, this.askAt + 15 * 60e3);
    if (!(b > a)) { this.answer.textContent = 'Pick a time in the past.'; this.askLinks.replaceChildren(); return; }
    let d = this.day;
    /** @type {Incident[]} */
    let incs = [];
    try {
      const [cd, list] = await Promise.all([
        d && d.from <= a + 60e3 && d.to >= b - 60e3 ? d : fetchJSON('api/calls?' + new URLSearchParams({ from: String(Math.floor(a)), to: String(Math.ceil(b)) })),
        verdict && verdict.kind === 'unknown' ? [] : fetchJSON('api/incidents?' + new URLSearchParams({ from: String(Math.floor(a)), to: String(Math.ceil(b)) })),
      ]);
      d = cd;
      incs = Array.isArray(list) ? list : [];
    } catch (e) {
      if (seq === this.askSeq) this.answer.textContent = `Could not load: ${/** @type {Error} */ (e).message}`;
      return;
    }
    if (seq !== this.askSeq || !d) return;
    const r = explainCalls(d, a, b, incs);
    this.answer.className = `cq-answer cs-${r.state}`;
    this.answer.textContent = r.text;
    this.askLinks.replaceChildren(h('button', { class: 'tbtn', type: 'button', text: 'Show on the charts', onclick: () => showCallsChart(r.from - 10 * 60e3, Math.min(Date.now(), r.to + 10 * 60e3)) }));
  }

  renderApps() {
    const d = this.live;
    if (!d) { this.appList.replaceChildren(h('li', { class: 'muted cq-app-none', text: 'Loading…' })); return; }
    const items = [];
    for (const app of d.apps) {
      if (app.udp !== 'measured') continue;
      const st = app.state || 'not_enough_data';
      const title = APP_TITLE[app.app] || app.app;
      const stats = [];
      if (st !== 'blocked' && st !== 'server_not_answering' && (app.samples || 0) > 0) {
        if (app.rtt_ms != null) stats.push(`${fmtMs(app.rtt_ms)} ms`);
        if (app.loss != null) stats.push(`${fmtPct(app.loss)} loss`);
      }
      const det = /** @type {HTMLDetailsElement} */ (h('details', { class: `cq-app cs-${st}` },
        h('summary', {},
          h('span', { class: 'cq-app-name' }, h('b', { text: title }), app.region ? h('span', { class: 'muted', text: ` via ${regionName(app.region)}` }) : null),
          h('span', { class: `cq-badge cs-${st}`, text: callLabel(st) }),
          h('span', { class: 'cq-app-stats', text: stats.join(' · ') })),
        this.appDetail(app)));
      det.open = this.open.has(app.app);
      det.addEventListener('toggle', () => { if (det.open) this.open.add(app.app); else this.open.delete(app.app); });
      items.push(h('li', {}, det));
    }
    // Apps whose own call server is not measured (Teams with its relay removed or not answering): their calls take the call path.
    for (const app of d.apps.filter((x) => x.udp === 'shared')) {
      const st = d.path ? d.path.state : 'not_enough_data';
      const web = (app.web_samples || 0) === 0 ? '' : app.web_ms == null ? ' Its website is not answering right now.' : ` Website reachable, ${fmtMs(app.web_ms)} ms.`;
      const own = app.own_down ? 'Its own call server isn\'t answering; ' : '';
      items.push(h('li', { class: 'cq-app cq-other' }, h('b', { text: APP_TITLE[app.app] || app.app }),
        h('p', { class: 'muted', text: `${own}${own ? 'calls' : 'Calls'} use your call path (${callLabel(st)}).${web}` })));
    }
    const other = d.apps.filter((x) => x.udp === 'not_measurable');
    if (other.length) {
      const web = other.flatMap((x) => (x.web_ms != null ? [x.web_ms] : []));
      const reach = other.some((x) => (x.web_samples || 0) > 0);
      const allLost = reach && other.every((x) => x.web_ms == null);
      const webText = !reach ? '' : allLost ? ' Their websites are not answering right now.' : ` Websites reachable, ${fmtMs(median(web))} ms.`;
      items.push(h('li', { class: 'cq-app cq-other' },
        h('b', { text: other.map((x) => APP_TITLE[x.app] || x.app).join(', ') }),
        h('p', { class: 'muted', text: 'These apps pick a server per meeting, so they can\'t be tested ahead of a call. If your call path is Good they usually are too.' + webText })));
    }
    if (!items.length) items.push(h('li', { class: 'muted cq-app-none', text: 'No call apps in this profile.' }));
    this.appList.replaceChildren(...items);
  }

  /** An app row's expanded part: MOS, jitter, p95 and the last hour's sparkline. */
  appDetail(/** @type {CallsApp} */ app) {
    /** @type {(string|Node)[]} */
    const facts = [];
    const sep = () => { if (facts.length) facts.push(' · '); };
    if (app.mos != null) { sep(); facts.push(term('MOS'), ` ${app.mos.toFixed(2)} (worst minute)`); }
    if (app.jitter_ms != null) { sep(); facts.push(term('jitter'), ` ${fmtMs(app.jitter_ms)} ms`); }
    if (app.rtt_p95_ms != null) { sep(); facts.push(term('p95'), ` ${fmtMs(app.rtt_p95_ms)} ms`); }
    sep();
    facts.push(term('bursts', `${app.samples || 0} bursts`), app.unknown ? `, ${app.unknown} failed otherwise (not counted as loss)` : '');
    const box = h('div', { class: 'cq-app-more' }, h('p', { class: 'small' }, ...facts));
    const d = this.day;
    const g = d && d.apps.find((x) => x.app === app.app);
    if (d && g && g.minutes) {
      const spark = /** @type {HTMLCanvasElement} */ (h('canvas', { class: 'cq-spark', role: 'img', 'aria-label': `${APP_TITLE[app.app] || app.app}: call quality per minute over the last hour` }));
      box.append(spark, h('span', { class: 'muted small', text: 'Last hour, one bar per minute: taller is better.' }));
      requestAnimationFrame(() => drawSpark(spark, d, /** @type {CallsGroup} */ (g)));
    }
    return box;
  }
}

/** A sparkline of the last hour's per-minute MOS, coloured by state. */
function drawSpark(/** @type {HTMLCanvasElement} */ c, /** @type {CallsData} */ d, /** @type {CallsGroup} */ g) {
  const W = c.clientWidth || 240, H = 28;
  const dpr = window.devicePixelRatio || 1;
  c.width = Math.round(W * dpr);
  c.height = Math.round(H * dpr);
  const ctx = /** @type {CanvasRenderingContext2D} */ (c.getContext('2d'));
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  const n = 60, end = minuteIdx(d, Date.now());
  const w = W / n;
  for (let k = 0; k < n; k++) {
    const m = minuteAt(d, g, end - n + 1 + k);
    if (!m || m.mos == null) { ctx.fillStyle = css('--grid'); ctx.fillRect(k * w, H - 2, Math.max(1, w - 1), 2); continue; }
    const bh = Math.max(2, ((m.mos - 1) / 3.5) * H);
    ctx.fillStyle = callColor(m.state);
    ctx.fillRect(k * w, H - bh, Math.max(1, w - 1), bh);
  }
}

/**
 * One sentence for [a, b): the worst stretch of bad call minutes and the layer
 * an overlapping incident blames ("14:10–14:16: 8% packet loss at your ISP
 * edge; Wi-Fi/router fine"), or that calls were fine.
 * @param {CallsData} d @param {number} a @param {number} b @param {Incident[]} incs
 * @returns {{text:string,state:string,from:number,to:number}}
 */
function explainCalls(d, a, b, incs) {
  const p = d.path;
  const span = `${hhmm(a)}–${hhmm(b)}`;
  const overlapping = (/** @type {number} */ x, /** @type {number} */ y) => incs.filter((i) => Date.parse(i.start) < y && (i.end ? Date.parse(i.end) : Date.now()) > x);
  const blame = (/** @type {Incident[]} */ list) => {
    for (const k of CALL_BLAME_ORDER) { const i = list.find((x) => x.kind === k); if (i) return i; }
    return null;
  };
  let scored = 0;
  /** @type {CallsMinute|null} */
  let worst = null;
  for (let t = a; t < b; t += 60e3) {
    const m = minuteAt(d, p, minuteIdx(d, t));
    if (!m) continue;
    scored++;
    if (!worst || (m.mos ?? 9) < (worst.mos ?? 9)) worst = m;
  }
  const runs = callRuns(d, p, a, b, BAD_CALL);
  if (runs.length) {
    const r = runs.reduce((x, y) => ((y.worst.mos ?? 9) < (x.worst.mos ?? 9) ? y : x));
    const inc = blame(overlapping(r.a, r.b));
    const w = r.worst;
    if (inc && (inc.kind === 'udp_blocked' || inc.kind === 'stun_blocked')) {
      return { text: `${runText(r)}: ${inc.kind === 'udp_blocked' ? 'UDP was' : 'the call ports (STUN) were'} blocked on this network; calls fall back to slower relays or fail.`, state: 'blocked', from: r.a, to: r.b };
    }
    const where = inc ? CALL_WHERE[inc.kind] || 'while fyisp saw a problem' : 'on your call path; Wi-Fi/router and ISP edge looked fine';
    const more = runs.length > 1 ? ` (${runs.length - 1} more bad stretch${runs.length > 2 ? 'es' : ''} in this range)` : '';
    return { text: `${runText(r)}: ${minuteWhat(w, minuteFactor(w))} ${where}${more}.`, state: w.state, from: r.a, to: r.b };
  }
  const inc = blame(overlapping(a, b).filter((i) => i.kind !== 'service'));
  if (!scored) {
    if (inc) return { text: `${span}: no call-path measurements, but ${inc.summary.replace(/^./, (c) => c.toLowerCase())}`, state: 'not_enough_data', from: a, to: b };
    return { text: `${span}: no call-path measurements in this range.`, state: 'not_enough_data', from: a, to: b };
  }
  const fair = callRuns(d, p, a, b, new Set(['fair']));
  const w = /** @type {CallsMinute} */ (worst);
  if (fair.length) {
    const r = fair[0];
    return { text: `${span}: calls were mostly fine; Fair at ${runText(r)} (${minuteWhat(r.worst, minuteFactor(r.worst))})${inc ? ' ' + CALL_WHERE[inc.kind] : ''}.`, state: 'fair', from: r.a, to: r.b };
  }
  const total = Math.round((b - a) / 60e3);
  const part = scored < total * 0.8 ? ` in the ${scored} of ${total} minutes measured` : ' all along';
  return { text: `${span}: calls would have been fine. Good${part} (worst minute ${hhmm(w.at)}, MOS ${(w.mos ?? 0).toFixed(1)}).`, state: 'good', from: a, to: b };
}

/** "Show on the charts": the charts view zoomed to [from, to], scrolled to the call-path panel. */
function showCallsChart(/** @type {number} */ from, /** @type {number} */ to) {
  state.view = 'c';
  state.focus = null;
  applyView();
  zoomTo(from, to);
  const p = panels.find((x) => !x.isPath && x.targets.some((t) => (t.tags || []).includes('udp-path')));
  if (!p) return;
  requestAnimationFrame(() => {
    const bar = $('.top');
    const top = p.el.getBoundingClientRect().top + window.scrollY - (getComputedStyle(bar).position === 'sticky' ? bar.offsetHeight : 0) - 8;
    window.scrollTo({ top: Math.max(0, top) });
  });
}

// ---------- Home and the Home | Details switch ----------

/** Home (made once the first answer is in or given up on). @type {Home|null} */
let home = null;
/** Re-reads the hash and applies it (hashchange, popstate, and Home's pushState links). */
let onNav = () => {};
function homeShown() { return !!home && currentView() === 'h' && !(state.inv && inv); }

/** The header's Home | Details switch. */
function renderViewNav() {
  const nav = $('#viewnav');
  nav.replaceChildren(
    h('button', { type: 'button', text: 'Home', 'aria-current': homeShown() ? 'page' : null, onclick: () => { if (!homeShown()) { state.inv = null; goHome(); } } }),
    h('button', { type: 'button', text: 'Details', 'aria-current': !homeShown() ? 'page' : null, onclick: () => { if (homeShown()) openDetails(''); } }));
}

/**
 * Opens Details with pushState, so Back returns to Home. hash pre-sets the
 * Details state (a tile's filter, a zoomed range); empty opens Details' own view.
 * @param {string} hash
 */
function openDetails(hash) {
  if (!hash) {
    state.view = detailsView();
    state.focus = null;
    hash = hashString();
  }
  history.pushState({ fyispHome: true }, '', hash);
  onNav();
  window.scrollTo({ top: 0 });
}
/** The hash openDetails('') would open: Home's "Technical details →" link. */
function detailsHash() {
  const v = state.view, f = state.focus;
  state.view = detailsView();
  state.focus = null;
  const s = hashString();
  state.view = v;
  state.focus = f;
  return s;
}
/** Back to Home: Back when Details was opened from Home, else a new entry. */
function goHome() {
  if (history.state && history.state.fyispHome) { history.back(); return; }
  state.view = null;
  state.focus = null;
  history.pushState(null, '', hashString() || location.pathname + location.search);
  onNav();
}

// ---------- view switch & focus ----------

const viewSeg = h('div', { class: 'seg', role: 'group', 'aria-label': 'View' });
const viewBar = h('div', { class: 'viewbar' }, viewSeg, h('span', { class: 'muted small vb-hint' }));
const focusBar = h('div', { class: 'focusbar', role: 'status', hidden: true });
/** The target focus mode has isolated on its chart, and its chart's first load. */
let focused = '';
let focusReady = Promise.resolve();

function setView(/** @type {string} */ v) {
  state.view = v;
  state.focus = null;
  writeHash();
  applyView();
  if (v === 'o') ov?.load(); else if (v === 'calls') calls?.refresh(); else if (v === 'speed') speed?.refresh(); else loadStale();
}

function renderViewBar() {
  const v = currentView();
  viewSeg.replaceChildren(
    callsOn ? h('button', { type: 'button', text: 'Calls', 'aria-pressed': String(v === 'calls'), title: 'Is your connection good for calls?', onclick: () => setView('calls') }) : null,
    h('button', { type: 'button', text: 'Overview', 'aria-pressed': String(v === 'o'), title: 'Every target in one table and heatmap', onclick: () => setView('o') }),
    h('button', { type: 'button', text: 'Charts', 'aria-pressed': String(v === 'c' && !focusPanel()), title: 'One chart per panel', onclick: () => setView('c') }),
    speed ? h('button', { type: 'button', text: 'Speed', 'aria-pressed': String(v === 'speed'), title: 'Download and upload speed through the day', onclick: () => setView('speed') }) : null);
  const hint = /** @type {HTMLElement} */ (viewBar.lastElementChild);
  const n = panels.filter((p) => !p.isPath).length;
  hint.textContent = v === 'speed' ? 'Measured with short download and upload tests to public test servers'
    : v === 'calls' ? 'Measured with tiny STUN tests over UDP, the path call audio and video take'
    : v === 'o' ? `${fmtInt(allTargets.filter((t) => !t.layer).length)} targets; click a row to see its chart` : `${n} chart panel${n === 1 ? '' : 's'}`;
}

/** Applies the view and focus to the page: which panels show, the focus bar, isolation. */
function applyView() {
  const v = currentView();
  // Investigate (#t=) is part of Details: it hides Home, whatever the view.
  const homeOn = v === 'h' && !(state.inv && inv);
  document.body.classList.toggle('home-on', homeOn);
  if (home) home.el.hidden = !homeOn;
  if (v !== 'h') document.title = 'Details · fyisp';
  renderViewNav();
  document.body.classList.toggle('ov-on', v === 'o');
  document.body.classList.toggle('calls-on', v === 'calls');
  document.body.classList.toggle('speed-on', v === 'speed');
  if (ov) ov.el.hidden = v !== 'o';
  if (speed) speed.el.hidden = v !== 'speed';
  if (calls) calls.el.hidden = v !== 'calls';
  const fp = focusPanel();
  for (const p of panels) {
    p.el.classList.toggle('off-view', !panelShown(p));
    p.el.classList.toggle('focused', p === fp);
  }
  if (focused && focused !== (fp ? state.focus : '')) {
    // Leaving focus: show every target of that panel again.
    const old = panels.find((p) => p.targets.some((t) => t.name === focused));
    if (old) { old.hidden = new Set(); old.applyHidden(); if (old.data) { old.renderLegend(); old.drawStrip(); } }
    focused = '';
  }
  if (fp && state.focus && focused !== state.focus) {
    const name = focused = state.focus;
    if (fp.data) fp.isolate(name);
    focusReady = fp.load().then(() => { if (focused === name && fp.data) fp.isolate(name); });
  }
  renderFocusBar(fp);
  renderViewBar();
  if (v === 'o' && ov && ov.saveScroll != null) {
    const y = ov.saveScroll;
    ov.saveScroll = null;
    requestAnimationFrame(() => window.scrollTo({ top: y }));
  }
}

function renderFocusBar(/** @type {Panel|null} */ fp) {
  focusBar.hidden = !fp;
  if (!fp || !state.focus) return;
  const t = targetInfo(state.focus);
  const where = [t?.provider_title || fp.group.title, t && t.geo ? GEO_TITLE[t.geo] : ''].filter(Boolean).join(' · ');
  focusBar.replaceChildren(
    h('span', { class: 'fb-what' }, h('b', { text: state.focus }), ` in ${where}`),
    h('span', { class: 'fb-acts' },
      h('button', { class: 'btn sm', type: 'button', text: 'Show all charts', onclick: () => closeFocus(false) }),
      h('button', { class: 'btn sm primary', type: 'button', text: '← Back to overview', onclick: () => closeFocus(true) })));
}

/** Row click: the target's chart only, isolated; Back returns to the Overview. */
function openFocus(/** @type {string} */ name) {
  if (!panels.some((p) => !p.isPath && p.targets.some((t) => t.name === name))) return;
  if (currentView() === 'o' && ov) ov.saveScroll = window.scrollY;
  state.focus = name;
  state.view = 'c';
  history.pushState({ fyispFocus: true }, '', hashString());
  lastHash = location.hash;
  applyView();
  requestAnimationFrame(scrollToFocus);
}

/** Scrolls the focus bar to the top; again once the chart has loaded, unless the user scrolled meanwhile. */
function scrollToFocus() {
  const bar = $('.top');
  const top = Math.max(0, focusBar.getBoundingClientRect().top + window.scrollY - (getComputedStyle(bar).position === 'sticky' ? bar.offsetHeight : 0) - 8);
  window.scrollTo({ top });
  focusReady.then(() => {
    if (Math.abs(window.scrollY - top) > 2 || !focusPanel()) return;
    const again = Math.max(0, focusBar.getBoundingClientRect().top + window.scrollY - (getComputedStyle(bar).position === 'sticky' ? bar.offsetHeight : 0) - 8);
    if (Math.abs(again - top) > 2) window.scrollTo({ top: again });
  });
}

function closeFocus(/** @type {boolean} */ toOverview) {
  if (toOverview && history.state && history.state.fyispFocus) { history.back(); return; }
  state.focus = null;
  state.view = toOverview ? 'o' : 'c';
  writeHash();
  applyView();
  if (toOverview) ov?.load(); else loadStale();
}

// ---------- zoom & refresh ----------

function zoomTo(/** @type {number} */ from, /** @type {number} */ to) {
  if (to - from < 30e3) { const mid = (from + to) / 2; from = mid - 15e3; to = mid + 15e3; }
  state.from = Math.round(from); state.to = Math.round(to);
  writeHash();
  for (const p of panels) { p.view = [from / 1000, to / 1000]; p.u?.setScale('x', { min: p.view[0], max: p.view[1] }); }
  inv?.setView(from, to);
  renderControls();
  refreshAll();
}
function resetZoom() {
  if (state.from == null) return;
  state.from = state.to = null;
  writeHash();
  renderControls();
  refreshAll();
}

async function refreshAll() {
  lastRefresh = Date.now();
  refreshing = true;
  updateRefreshLabel();
  try {
    if (state.inv && inv) await Promise.all([loadStatus(), loadVerdict(), loadSpeedWindows(), inv.load()]);
    // Home makes two requests; the status (for the notices) at most twice a minute.
    else if (homeShown()) await Promise.all([Date.now() - statusAt > 30e3 ? loadStatus() : null, /** @type {Home} */ (home).refresh()]);
    else {
      // Hidden panels (the Overview is showing, or focus) load when shown.
      await Promise.all([loadStatus(), loadVerdict(), loadIncidents(), loadSpeedWindows(), overviewShown() ? /** @type {Overview} */ (ov).load() : null,
        callsShown() ? /** @type {Calls} */ (calls).refresh() : null,
        speedShown() ? /** @type {Speed} */ (speed).refresh() : null,
        ...panels.map((p) => (p.visible && panelShown(p) ? p.load() : (p.stale = true, null)))]).then(loadBaselines);
    }
  } finally {
    refreshing = false;
    updateRefreshLabel();
    renderLayers();
  }
}

function renderLayers() { for (const p of panels) if (p.isPath) p.renderLayers(); }

// ---------- verdict banner ----------

/** Scroll to the panel that has this target and show only it. */
function focusTarget(/** @type {string} */ name) {
  const p = panels.find((x) => x.targets.some((t) => t.name === name));
  if (!p) return;
  if (!panelShown(p)) { openFocus(name); return; }
  p.isolate(name);
  const bar = $('.top');
  const top = p.el.getBoundingClientRect().top + window.scrollY - (getComputedStyle(bar).position === 'sticky' ? bar.offsetHeight : 0) - 8;
  window.scrollTo({ top: Math.max(0, top), behavior: smooth() });
}

// The banner shows at most this many target chips, then "+N more".
const VERDICT_CHIPS = 8;
let verdictChipsOpen = false;

/** "+N more" on a big profile: the Overview, problems only, scrolled into view. */
function showProblems() {
  if (!ov) return;
  const o = ov;
  state.op = true;
  state.oc = '';
  const scroll = () => requestAnimationFrame(() => o.tableBox.scrollIntoView({ block: 'start' }));
  if (currentView() !== 'o') {
    state.view = 'o';
    state.focus = null;
    writeHash();
    applyView();
    o.load().then(scroll);
  } else {
    writeHash();
    o.renderControls();
    o.applyRows();
    scroll();
  }
}

function renderVerdict() {
  const el = $('#verdict');
  const v = verdict;
  const on = !!v && v.kind !== 'unknown';
  el.hidden = !on;
  $('#outages').hidden = !on;
  if (!on || !v) return;
  const info = VERDICTS[v.kind] || { label: v.kind, short: v.kind, why: '' };
  // A long-standing UDP block is information, not an alarm.
  const quiet = !!v.quiet && v.kind in QUIET_LABEL;
  el.className = `verdict v-${/^[a-z_]+$/.test(v.kind) ? v.kind : 'other'}${quiet ? ' quiet' : ''}`;
  const kids = [h('div', { class: 'v-head' },
    h('span', { class: 'v-dot', 'aria-hidden': 'true' }),
    h('span', { class: 'v-kind', text: quiet ? QUIET_LABEL[/** @type {'udp_blocked'} */ (v.kind)] : info.label }),
    info.why ? whyButton(info.label, info.why) : null)];
  kids.push(h('p', { class: 'v-summary', text: v.summary || info.why }));
  const since = v.since ? Date.parse(v.since) : NaN;
  const meta = [];
  if (isFinite(since) && since > 0) meta.push(h('span', { text: `since ${fmtWhen(since)} (${fmtDur(Date.now() - since)})` }));
  if (v.targets && v.targets.length) {
    meta.push(h('span', { class: 'v-aff', title: 'Targets the verdict counts over the last minute', text: 'Affected now:' }));
    const all = v.targets.length <= VERDICT_CHIPS + 1 || verdictChipsOpen;
    for (const name of all ? v.targets : v.targets.slice(0, VERDICT_CHIPS)) {
      meta.push(h('button', { class: 'tchip', type: 'button', title: `Show ${name} on its chart`, text: name, onclick: () => focusTarget(name) }));
    }
    if (!all) {
      const more = v.targets.length - VERDICT_CHIPS;
      // Big profiles: the Overview lists them all (problems only); else expand in place.
      meta.push(ov && bigProfile
        ? h('button', { class: 'tchip more', type: 'button', title: 'Show every target with a problem in the Overview', text: `+${more} more`, onclick: showProblems })
        : h('button', { class: 'tchip more', type: 'button', title: 'Show all affected targets', text: `+${more} more`, onclick: () => { verdictChipsOpen = true; renderVerdict(); } }));
    }
  }
  if (meta.length) kids.push(h('div', { class: 'v-meta' }, ...meta));
  if (v.slow && v.slow.length) {
    kids.push(h('div', { class: 'v-meta v-slow' },
      h('span', { class: 'v-slow-l', text: 'Slower than your normal:' }),
      ...v.slow.map((x) => h('button', {
        class: 'tchip slow', type: 'button', text: `${x.target} ${fmtRatio(x.ratio)}`,
        title: `${x.target}: ${fmtMs(x.now_ms)} ms now vs ${fmtMs(x.normal_ms)} ms normal. Show it on its chart.`,
        onclick: () => focusTarget(x.target),
      }))));
  }
  el.replaceChildren(...kids);
}

async function loadVerdict() {
  lastVerdict = Date.now();
  const prev = verdict && verdict.kind;
  try {
    verdict = await fetchJSON('api/verdict');
  } catch {
    return;
  }
  renderVerdict();
  renderLayers();
  if (ov && ov.data && overviewShown()) ov.renderBreadth();
  // A new verdict usually means a new (or closed) incident, and a new top card.
  if (prev != null && verdict && prev !== verdict.kind) { loadIncidents(); if (callsShown()) calls?.loadLive(); }
}

// ---------- outage log ----------

async function loadIncidents() {
  if (verdict && verdict.kind === 'unknown') { incidents = []; return; }
  const { from, to } = queryRange();
  try {
    const list = await fetchJSON('api/incidents?' + new URLSearchParams({ from, to }));
    incidents = Array.isArray(list) ? list : [];
    incidentsLoaded = true;
  } catch {
    return;
  }
  renderOutages();
  for (const p of panels) p.u?.redraw(false);
}

function renderOutages() {
  const n = incidents.length;
  const ongoing = incidents.filter((i) => !i.end).length;
  $('#outages-count').textContent = incidentsLoaded
    ? `· ${n >= 500 ? '500+' : n} incident${n === 1 ? '' : 's'} in range${ongoing ? ` · ${ongoing} ongoing` : ''}`
    : '';
  const list = $('#outages-list');
  if (!n) {
    list.replaceChildren(h('p', { class: 'muted o-empty', text: 'No incidents in this range.' }));
    return;
  }
  const now = Date.now();
  list.replaceChildren(...incidents.map((inc) => {
    const a = Date.parse(inc.start), b = inc.end ? Date.parse(inc.end) : now;
    const info = VERDICTS[inc.kind] || { label: inc.kind, short: inc.kind };
    const kind = /^[a-z_]+$/.test(inc.kind) ? inc.kind : 'other';
    const row = h('button', {
      class: 'o-row' + (inc.end ? '' : ' ongoing'), type: 'button',
      title: 'Zoom all charts to this incident',
      onclick: () => zoomTo(a - 5 * 60e3, Math.min(Date.now(), b + 5 * 60e3)),
    },
    h('span', { class: 'o-when' }, fmtWhen(a), ' – ', inc.end ? fmtWhen(b) : h('b', { text: 'ongoing' })),
    h('span', { class: 'o-dur', text: fmtDur(b - a) }),
    h('span', { class: `kbadge v-${kind}`, text: info.short }),
    h('span', { class: 'o-sum', text: inc.summary }),
    h('span', { class: 'o-peak', title: 'Worst one-minute loss', text: inc.peak_loss >= 0.995 ? 'worst minute: nothing got through' : `worst minute: ${fmtPct(inc.peak_loss)} lost` }));
    const tt = incidentTraceTarget(inc);
    const tb = tt ? h('button', {
      class: 'tbtn o-trace', type: 'button', title: `Trace ${tt} hop by hop during this incident`, text: `Trace ${tt}`,
      onclick: () => { zoomTo(a - 5 * 60e3, Math.min(Date.now(), b + 5 * 60e3)); openInvestigate(tt); },
    }) : null;
    if (!tb) return h('div', { class: 'o-item' }, row);
    return h('div', { class: 'o-item' }, row, h('span', { class: 'o-acts' }, tb));
  }));
}

function updateRefreshLabel() {
  const el = $('#refresh');
  if (refreshing) el.textContent = 'Updating…';
  else if (!isRelative()) el.textContent = 'Zoomed · auto-refresh off';
  else if (document.hidden) el.textContent = 'Paused';
  else el.textContent = `Auto-refresh ${refreshMs() / 1000}s`;
}

/** The Investigate view refreshes faster than the dashboard. */
function refreshMs() { return state.inv ? INV_REFRESH_MS : REFRESH_MS; }

setInterval(() => {
  calls?.tick();
  home?.tick();
  if (document.hidden || refreshing || homeShown()) return;
  if (isRelative() && Date.now() - lastRefresh >= refreshMs()) refreshAll();
  // The banner is always "now", even when zoomed; it is cheap to poll.
  else if (Date.now() - lastVerdict >= VERDICT_MS) loadVerdict();
}, 1000);
document.addEventListener('visibilitychange', () => {
  updateRefreshLabel();
  if (document.hidden || homeShown()) return;
  if (isRelative() && Date.now() - lastRefresh >= refreshMs()) refreshAll();
  else if (Date.now() - lastVerdict >= VERDICT_MS) loadVerdict();
});

// ---------- header controls ----------

function renderControls() {
  const ranges = $('#ranges');
  ranges.replaceChildren(...RANGES.map(([k]) => h('button', {
    type: 'button', text: k, 'aria-pressed': String(isRelative() && state.range === k),
    onclick: () => { state.range = /** @type {string} */ (k); state.from = state.to = null; writeHash(); renderControls(); refreshAll(); },
  })));
  if (!isRelative()) {
    ranges.append(h('button', { type: 'button', 'aria-pressed': 'true', title: 'Reset zoom (or double-click a chart)', text: 'Zoomed ×', onclick: resetZoom }));
  }
  // Only the kinds the profile's services have; one kind needs no switch.
  const kindsEl = $('#kinds');
  kindsEl.hidden = kindsOffered.length < 2;
  kindsEl.replaceChildren(...kindsOffered.map((k) => h('button', {
    type: 'button', text: KIND_LABEL[/** @type {'https'} */ (k)], 'aria-pressed': String(state.kinds.has(k)),
    title: KIND_TITLE[/** @type {'https'} */ (k)],
    onclick: () => {
      if (state.kinds.has(k)) { if (state.kinds.size > 1) state.kinds.delete(k); } else state.kinds.add(k);
      writeHash(); renderControls();
      for (const p of panels) p.render();
      renderLayers();
      if (overviewShown() && ov && ovKind() !== ov.kind) ov.load();
    },
  })));
  renderTools();
  updateRefreshLabel();
}

/** Header tools: the "normal" band toggle. */
function renderTools() {
  const el = $('#tools');
  const kids = [];
  if (baseOn) {
    kids.push(h('button', {
      class: 'btn ghost', type: 'button', text: 'Normal band', 'aria-pressed': String(state.band),
      title: 'Shade each target\'s normal latency (median to p95 over the past week) on the charts',
      onclick: () => { state.band = !state.band; storageSet('fyisp-band', state.band ? '1' : '0'); renderTools(); redrawCharts(); },
    }));
  }
  el.replaceChildren(...kids);
  el.hidden = !kids.length;
}

/** Takes the token of an "open this link" URL (#admin=…) out of the address bar and history entry. */
function takeAdminFragment() {
  const p = new URLSearchParams(location.hash.slice(1));
  const t = p.get('admin');
  if (t == null) return '';
  p.delete('admin');
  const rest = p.toString();
  history.replaceState(history.state, '', location.pathname + location.search + (rest ? '#' + rest : ''));
  return t;
}

/** Exchanges the link's admin token for the HttpOnly session cookie. @param {string} t */
async function adminLogin(t) {
  try {
    await fetchJSON('api/admin/session', { method: 'POST', headers: { 'X-FYISP-Token': TOKEN, 'X-FYISP-Admin': t } }, 1);
    try { sessionStorage.removeItem('fyisp-admin'); } catch { /* ignore */ }
  } catch (e) {
    adminLinkErr = /** @type {any} */ (e).status === 403
      ? 'This link is not valid any more. Open the newest link from fyisp\'s log.'
      : 'Could not use this link: ' + /** @type {any} */ (e).message;
  }
}
/** Why an "open this link" URL did not work (shown as a notice). */
let adminLinkErr = '';

function renderNotices() {
  const el = $('#notices');
  const out = [];
  if (!status) { el.replaceChildren(); return; }
  if (adminLinkErr) out.push(h('div', { class: 'notice danger' }, adminLinkErr));
  if (status.controls === 'disabled') {
    out.push(h('div', { class: 'notice' }, 'Speed-test controls are off: the dashboard is reachable from your network and fyisp was started with --admin-token=off.'));
  }
  if (status.targets && status.ready < status.targets) {
    out.push(h('div', { class: 'notice' }, h('b', { text: 'Warming up: ' }), `${status.ready}/${status.targets} targets reporting.`));
  }
  const caps = status.caps || {};
  const missing = [];
  if (caps.icmp === 'unavailable') missing.push('ICMP');
  if (caps.tcp === false) missing.push('TCP');
  if (caps.https === false) missing.push('HTTPS');
  if (missing.length) {
    out.push(h('div', { class: 'notice' }, h('b', { text: `${missing.join(', ')} unavailable` }),
      ` on this machine; those probes are not running.${caps.icmp_hint ? ' ' + caps.icmp_hint : ''}`));
  }
  const st = status.store || {};
  if (st.last_flush_err) out.push(h('div', { class: 'notice danger' }, h('b', { text: 'Storage error: ' }), st.last_flush_err));
  if (st.healthy === false) out.push(h('div', { class: 'notice danger' }, h('b', { text: 'Storage degraded' }), ': recent data may be missing.'));
  el.replaceChildren(...out);
}

/** When the status was last loaded. */
let statusAt = 0;
async function loadStatus() {
  try {
    status = await fetchJSON('api/status');
    statusAt = Date.now();
  } catch {
    return;
  }
  renderNotices();
  renderTools();
}

// ---------- boot ----------

async function main() {
  useStatus(() => status);
  applyTheme(storageGet('fyisp-theme'));
  $('#theme').addEventListener('click', () => {
    const next = effectiveTheme() === 'dark' ? 'light' : 'dark';
    storageSet('fyisp-theme', next);
    applyTheme(next);
    themeChanged();
  });
  mqLight.addEventListener('change', () => { applyTheme(storageGet('fyisp-theme')); themeChanged(); });
  // Taps elsewhere close a tooltip opened by tapping (touch screens have no hover).
  document.addEventListener('click', hideTip);
  const adminTok = takeAdminFragment();
  readHash();
  const log = /** @type {HTMLDetailsElement} */ ($('#outages'));
  log.open = state.log;
  log.addEventListener('toggle', () => { if (state.log !== log.open) { state.log = log.open; writeHash(); } });
  document.addEventListener('keydown', (e) => {
    if (e.key !== 'Escape') return;
    hideTip(); // tooltips close on Escape (WCAG 1.4.13)
  });
  // Back from focus mode restores the Overview's scroll position itself.
  if ('scrollRestoration' in history) history.scrollRestoration = 'manual';
  onNav = () => {
    if (location.hash === lastHash) return;
    lastHash = location.hash;
    readHash(); log.open = state.log;
    renderControls(); applyView(); showView(); refreshAll();
  };
  window.addEventListener('hashchange', onNav);
  window.addEventListener('popstate', onNav);
  if (adminTok) await adminLogin(adminTok);
  // Home's first answer, fetched alongside the profile (null after a few seconds without
  // one: Home then polls on its own). Details doesn't wait for it.
  const first = firstHome();
  const [st, prof] = await Promise.all([fetchJSON('api/status').catch(() => null), fetchJSON('api/profile')]);
  status = st;
  if (st) statusAt = Date.now();
  traceOn = !!(prof.features && prof.features.trace);
  baseOn = !!(prof.features && prof.features.baselines);
  allTargets = prof.targets;
  // Which target profile(s) this run measures, unless the default.
  const pb = $('#profile-badge');
  if (prof.name && prof.name !== 'default') {
    pb.hidden = false;
    pb.textContent = 'Profile: ' + prof.name;
    pb.title = `${prof.targets.length} targets in ${prof.groups.length} panels`;
  }
  kindsOffered = offeredKinds(prof.targets);
  readHash();
  renderControls();
  renderNotices();
  /** @type {TargetInfo[]} */
  const targets = prof.targets;
  // The network path comes first, full width.
  const groups = [...prof.groups].sort((/** @type {Group} */ a, /** @type {Group} */ b) => Number(b.id === PATH_GROUP) - Number(a.id === PATH_GROUP));
  panels = groups.map((/** @type {Group} */ g) => new Panel(g, targets.filter((t) => t.group === g.id)));
  bigProfile = targets.filter((t) => !t.layer).length > OVERVIEW_MIN;
  callsOn = targets.some((t) => (t.tags || []).some((x) => x === 'udp-path' || x.startsWith('app:')));
  ov = new Overview();
  calls = callsOn ? new Calls() : null;
  // features.speed says it at once; older builds without it are probed.
  const hasSpeed = prof.features && typeof prof.features.speed === 'boolean' ? prof.features.speed : await speedAvailable();
  if (!hasSpeed) speedOn = false;
  speed = hasSpeed ? new Speed({
    sel: () => state.sr,
    setSel: (v) => { state.sr = v; writeHash(); },
    days: () => state.sd,
    setDays: (n) => { state.sd = n; writeHash(); speed?.refresh(); },
    nofb: () => state.snf,
    setNofb: (b) => { state.snf = b; writeHash(); speed?.refresh(); },
    refresh: () => speed?.refresh(),
    sweep: () => state.ssw,
  }) : null;
  // The view switch, then Calls, the network path, the Overview or the charts: DOM order is visual order.
  const path = panels.filter((p) => p.isPath).map((p) => p.el);
  $('#panels').replaceChildren(viewBar, focusBar, calls ? calls.el : '', speed ? speed.el : '', ...path, ov.el, ...panels.filter((p) => !p.isPath).map((p) => p.el));
  if (panels.length > LAZY_PANELS && 'IntersectionObserver' in window) {
    const io = new IntersectionObserver((entries) => {
      for (const e of entries) {
        const p = panels.find((x) => x.el === e.target);
        if (!p) continue;
        p.visible = e.isIntersecting;
        if (p.visible && p.stale && panelShown(p) && !(state.inv && inv)) p.load();
      }
    }, { rootMargin: '800px 0px' });
    for (const p of panels) { p.visible = false; io.observe(p.el); }
  }
  uPlot.sync(SYNC_KEY);
  if (traceOn) {
    inv = new Investigate();
    $('#panels').after(inv.el);
  }
  if (currentView() !== 'h' || (state.inv && inv)) {
    // Details first; Home joins when its first answer is in.
    applyView();
    showView();
    writeHash();
    const loaded = refreshAll();
    makeHome(await first);
    applyView();
    showView();
    // Back on Home already (the switch, or Back) while the first answer was out.
    if (homeShown()) /** @type {Home} */ (home).refresh();
    await loaded;
    return;
  }
  makeHome(await first);
  applyView();
  showView();
  writeHash();
  await refreshAll();
}

/** Builds Home. @param {import('./home.js').HomeNow|null} first its first answer, if in */
function makeHome(first) {
  home = new Home({ openDetails, detailsHash, status: () => status, speedOn: () => !!speed }, first);
  $('#notices').before(home.el);
}

main().catch((e) => {
  $('#notices').replaceChildren(h('div', { class: 'notice danger' }, h('b', { text: 'Could not start: ' }), String(e && e.message || e)));
});
