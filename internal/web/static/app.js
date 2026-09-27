// fyisp dashboard. Plain ES module, no build step. uPlot is loaded as a
// classic deferred script before this module runs (window.uPlot).
// @ts-check

/** @typedef {{id:string,title:string}} Group */
/** @typedef {{name:string,group:string,kinds:string[],interval_ms:number,layer?:string,trace?:boolean,provider?:string,provider_title?:string,provider_kind?:string,city?:string,country?:string,geo?:string}} TargetInfo */
/** @typedef {{target:string,kind:string,ratio:number,now_ms:number,normal_ms:number}} SlowItem */
/** @typedef {{kind:string,since?:string,summary?:string,targets?:string[],evidence?:Object<string,number>,slow?:SlowItem[]}} Verdict */
/** @typedef {{id:number,start:string,end?:string,kind:string,summary:string,targets?:string[],peak_loss:number}} Incident */
/** @typedef {{target:string,kind:string,interval:number,mean:(number|null)[],min:(number|null)[],max:(number|null)[],n:number[],lost:number[],gap:number[],lost_by:Object<string,number[]>}} SeriesData */
/** @typedef {{group:Group,tier:string,from:number,to:number,now:number,start:number,step:number,len:number,series:SeriesData[]}} PanelData */

const uPlot = /** @type {any} */ (window).uPlot;

const meta = (/** @type {string} */ n) => document.querySelector(`meta[name="${n}"]`)?.getAttribute('content') || '';
const MODE = meta('fyisp-mode') === 'public' ? 'public' : 'local';
const TOKEN = meta('fyisp-token');

const RANGES = [['5m', 300], ['30m', 1800], ['1h', 3600], ['6h', 21600], ['24h', 86400], ['48h', 172800], ['7d', 604800], ['30d', 2592000], ['90d', 7776000]];
const KINDS = ['https', 'tcp', 'icmp'];
const KIND_LABEL = { https: 'HTTPS', tcp: 'TCP', icmp: 'ICMP' };
const KIND_DASH = { https: [], tcp: [6, 3], icmp: [1.5, 3] };
const REASONS = ['timeout', 'refused', 'reset', 'unreachable', 'dns', 'tls', 'http', 'no_network', 'other'];
const REASON_LABEL = { timeout: 'timeout', refused: 'refused', reset: 'reset', unreachable: 'unreachable', dns: 'DNS', tls: 'TLS', http: 'HTTP error', no_network: 'no network', other: 'other' };
const REFRESH_MS = 15000;
const VERDICT_MS = 5000;
const SYNC_KEY = 'fyisp';
const PATH_GROUP = 'path';

// Verdict kinds: headline, short badge, and what it means (the "?" tooltip).
/** @type {Object<string,{label:string,short:string,why:string}>} */
const VERDICTS = {
  ok: { label: 'All good', short: 'OK', why: 'Your router, your ISP and the internet are all answering normally.' },
  warming_up: { label: 'Warming up', short: 'Warming up', why: 'fyisp has just started and needs a couple of minutes of measurements before it can say whose fault a problem is.' },
  lan: { label: 'Home network problem', short: 'LAN', why: 'Your router (the gateway) is not answering reliably, so the problem is inside your home: Wi-Fi, cables or the router itself. Your ISP is not to blame for this one.' },
  isp: { label: 'ISP problem', short: 'ISP', why: 'Your router answers fine, but the first hop on your ISP\'s side does not. The problem is in your ISP\'s network.' },
  upstream: { label: 'Internet problem beyond your ISP', short: 'Upstream', why: 'Your router and your ISP\'s first hop answer, but most of the internet (including the big anycast resolvers) does not: your ISP\'s upstream links or peering are failing.' },
  dns: { label: 'DNS problem', short: 'DNS', why: 'The network path works, but name lookups are failing widely. The DNS resolver you use (often your router\'s or your ISP\'s) is the likely culprit.' },
  service: { label: 'Some services are down', short: 'Service', why: 'Your connection is fine; only some services or regions are unhealthy. That is on their side, not yours or your ISP\'s.' },
  no_network: { label: 'No network', short: 'No network', why: 'This machine has no route to the internet: Wi-Fi off, cable unplugged, or no network configured.' },
};
// The network path, left to right. key is the evidence prefix (<key>_loss, <key>_rtt_ms).
const LAYERS = [
  { id: 'gateway', key: 'gateway', label: 'Gateway', why: 'Your router: the first thing every packet goes through.' },
  { id: 'isp-edge', key: 'edge', label: 'ISP edge', why: 'The first hop on your ISP\'s side of the line.' },
  { id: 'anycast', key: 'anycast', label: 'Internet (anycast)', why: 'Big anycast resolvers (Cloudflare, Google, Quad9) that are close to almost every ISP.' },
  { id: 'services', key: 'services', label: 'Services', why: 'The services and cloud regions in the panels below.' },
];
// Which layer each verdict kind blames.
/** @type {Object<string,string>} */
const BLAME = { lan: 'gateway', isp: 'isp-edge', upstream: 'anycast', dns: 'services', service: 'services' };

/**
 * Page state; everything but band lives in the URL hash. view: 'o' (Overview)
 * or 'c' (charts), null for the profile's default; focus: a target shown alone
 * on its chart; os/oq/og/op/oc/om: the Overview's sort, filter text, region,
 * problems-only, heatmap cell and heatmap mode.
 * @type {{range:string, from:number|null, to:number|null, kinds:Set<string>, log:boolean, notes:boolean, reports:boolean, inv:string|null, band:boolean,
 *   view:string|null, focus:string|null, os:string, oq:string, og:string, op:boolean, oc:string, om:string}}
 */
const state = { range: '30m', from: null, to: null, kinds: new Set(['https']), log: false, notes: false, reports: false, inv: null, band: true,
  view: null, focus: null, os: '', oq: '', og: '', op: false, oc: '', om: '' };
/** @type {any} */
let status = null;
/** @type {Panel[]} */
let panels = [];
// Profiles with more panels than this load only the panels near the viewport
// (big catalog profiles have 50+ panels; the public link is rate limited).
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

const $ = (/** @type {string} */ s) => /** @type {HTMLElement} */ (document.querySelector(s));

/**
 * @param {string} tag
 * @param {Object<string, any>} [props]
 * @param {...(Node|string|null|undefined|false)} kids
 */
function h(tag, props, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (v == null || v === false) continue;
    if (k === 'class') el.className = v;
    else if (k === 'text') el.textContent = v;
    else if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else el.setAttribute(k, v === true ? '' : String(v));
  }
  for (const c of kids) if (c != null && c !== false) el.append(c);
  return el;
}

// ---------- theme ----------

function storageGet(/** @type {string} */ k) { try { return localStorage.getItem(k); } catch { return null; } }
function storageSet(/** @type {string} */ k, /** @type {string} */ v) { try { localStorage.setItem(k, v); } catch { /* private mode */ } }

const mqLight = window.matchMedia('(prefers-color-scheme: light)');
function effectiveTheme() {
  const t = document.documentElement.dataset.theme;
  if (t === 'light' || t === 'dark') return t;
  return mqLight.matches ? 'light' : 'dark';
}
function applyTheme(/** @type {string|null} */ t) {
  if (t === 'light' || t === 'dark') document.documentElement.dataset.theme = t;
  else delete document.documentElement.dataset.theme;
  const b = $('#theme');
  b.textContent = effectiveTheme() === 'dark' ? 'Light' : 'Dark';
  b.setAttribute('aria-label', `Switch to ${b.textContent.toLowerCase()} theme`);
}
/** @type {Map<string,string>} */
const cssCache = new Map();
function css(/** @type {string} */ name) {
  let v = cssCache.get(name);
  if (v == null) {
    v = getComputedStyle(document.documentElement).getPropertyValue(name).trim();
    cssCache.set(name, v);
  }
  return v;
}
function themeChanged() {
  cssCache.clear();
  hatchPattern = null;
  for (const p of panels) p.render();
  renderLayers();
  if (inv && state.inv) { inv.sig = ''; inv.render(); }
}

// ---------- colours ----------

/** @param {string} hex @param {number} a */
function rgba(hex, a) {
  const m = /^#?([0-9a-f]{6})$/i.exec(hex);
  if (!m) return hex;
  const n = parseInt(m[1], 16);
  return `rgba(${n >> 16},${(n >> 8) & 255},${n & 255},${a})`;
}
/** Mix a colour towards white (t>0) or black (t<0). */
function shade(/** @type {string} */ hex, /** @type {number} */ t) {
  const m = /^#?([0-9a-f]{6})$/i.exec(hex);
  if (!m) return hex;
  const n = parseInt(m[1], 16);
  const f = (/** @type {number} */ c) => Math.round(t > 0 ? c + (255 - c) * t : c * (1 + t));
  const r = f(n >> 16), g = f((n >> 8) & 255), b = f(n & 255);
  return '#' + ((1 << 24) | (r << 16) | (g << 8) | b).toString(16).slice(1);
}
// Colour follows the target's position in its group (never its rank on
// screen). Beyond 8 targets the same 8 hues repeat at other lightness steps;
// the legend, the tooltip and click-to-isolate carry identity.
const SHADES = [0, 0.35, -0.3, 0.6, -0.5];
function seriesColor(/** @type {number} */ i) {
  const base = css(`--s${(i % 8) + 1}`) || '#3987e5';
  return shade(base, SHADES[Math.floor(i / 8) % SHADES.length]);
}
function reasonColor(/** @type {string} */ r) { return css(`--r-${r}`) || '#999999'; }

// ---------- formatting ----------

function fmtMs(/** @type {number|null|undefined} */ v) {
  if (v == null || !isFinite(v)) return '—';
  if (v < 10) return v.toFixed(2);
  if (v < 100) return v.toFixed(1);
  return v.toFixed(0);
}
function fmtPct(/** @type {number} */ f) {
  if (!isFinite(f)) return '—';
  if (f === 0) return '0%';
  if (f < 0.001) return '<0.1%';
  return (f * 100).toFixed(f < 0.1 ? 1 : 0) + '%';
}
function fmtTime(/** @type {number} */ ms, /** @type {number} */ spanMs) {
  const d = new Date(ms);
  const time = d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: spanMs <= 6 * 3600e3 ? '2-digit' : undefined });
  if (spanMs <= 24 * 3600e3) return time;
  return d.toLocaleDateString([], { month: 'short', day: 'numeric' }) + ' ' + time;
}
/** Clock time; with the date when not today. */
function fmtWhen(/** @type {number} */ ms) {
  const d = new Date(ms);
  const time = d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
  if (d.toDateString() === new Date().toDateString()) return time;
  return d.toLocaleDateString([], { month: 'short', day: 'numeric' }) + ' ' + time;
}
/** A duration in words: "45 s", "13 min", "2 h 5 min", "3 d 4 h". */
function fmtDur(/** @type {number} */ ms) {
  const s = Math.max(0, Math.round(ms / 1000));
  if (s < 60) return `${s} s`;
  const m = Math.round(s / 60);
  if (m < 60) return `${m} min`;
  const hh = Math.floor(m / 60), mm = m % 60;
  if (hh < 48) return mm ? `${hh} h ${mm} min` : `${hh} h`;
  const dd = Math.floor(hh / 24), rh = hh % 24;
  return rh ? `${dd} d ${rh} h` : `${dd} d`;
}
function verdictColor(/** @type {string} */ kind) { return css(`--v-${kind}`) || css('--muted') || '#898781'; }
function fmtStep(/** @type {number} */ ms) {
  if (ms < 60e3) return `${Math.round(ms / 1e3)}s`;
  if (ms < 3600e3) return `${Math.round(ms / 60e3)}m`;
  if (ms < 86400e3) return `${Math.round(ms / 3600e3)}h`;
  return `${Math.round(ms / 86400e3)}d`;
}

// ---------- URL hash state ----------

function readHash() {
  const p = new URLSearchParams(location.hash.slice(1));
  const r = p.get('r');
  if (r && RANGES.some(([k]) => k === r)) state.range = r;
  const f = Number(p.get('from')), t = Number(p.get('to'));
  if (f > 0 && t > f) { state.from = f; state.to = t; } else { state.from = state.to = null; }
  state.log = p.get('o') === '1';
  state.notes = p.get('n') === '1';
  state.reports = p.get('rp') === '1';
  const ti = p.get('t');
  state.inv = ti && traceOn ? ti : null;
  const k = p.get('k');
  if (k != null) {
    const ks = k.split(',').filter((x) => KINDS.includes(x));
    state.kinds = new Set(ks.length ? ks : ['https']);
  }
  const v = p.get('v');
  state.view = v === 'o' || v === 'c' ? v : null;
  state.focus = p.get('f') || null;
  state.os = p.get('os') || '';
  state.oq = p.get('oq') || '';
  const og = p.get('og');
  state.og = og && GEOS.includes(og) ? og : '';
  state.op = p.get('op') === '1';
  state.oc = p.get('oc') || '';
  const om = p.get('om');
  state.om = om === 'r' || om === 'ms' ? om : '';
}
/** The last hash the page applied (hashchange and popstate both fire on Back). */
let lastHash = '';
function hashString() {
  const p = new URLSearchParams();
  p.set('r', state.range);
  if (state.from != null && state.to != null) { p.set('from', String(state.from)); p.set('to', String(state.to)); }
  p.set('k', [...state.kinds].join(','));
  if (state.log) p.set('o', '1');
  if (state.notes) p.set('n', '1');
  if (state.reports) p.set('rp', '1');
  if (state.inv) p.set('t', state.inv);
  if (state.view) p.set('v', state.view);
  if (state.focus) p.set('f', state.focus);
  if (state.os) p.set('os', state.os);
  if (state.oq) p.set('oq', state.oq);
  if (state.og) p.set('og', state.og);
  if (state.op) p.set('op', '1');
  if (state.oc) p.set('oc', state.oc);
  if (state.om) p.set('om', state.om);
  return '#' + p.toString();
}
function writeHash() {
  const s = hashString();
  if (location.hash !== s) history.replaceState(history.state, '', s);
  lastHash = location.hash;
}
function isRelative() { return state.from == null; }
function rangeSecs() { return (RANGES.find(([k]) => k === state.range) || RANGES[1])[1]; }
function queryRange() {
  if (state.from != null && state.to != null) return { from: String(state.from), to: String(state.to) };
  return { from: 'now-' + state.range, to: 'now' };
}

// ---------- fetch ----------

/**
 * GET/POST JSON with a small retry on 429/503 (the public link is rate limited).
 * @param {string} url @param {RequestInit} [init] @param {number} [tries]
 */
async function fetchJSON(url, init, tries = 3) {
  for (let i = 0; ; i++) {
    const res = await fetch(url, { credentials: 'same-origin', cache: 'no-store', ...init });
    if ((res.status === 429 || res.status === 503) && i < tries - 1) {
      const wait = Math.min(5, Number(res.headers.get('Retry-After')) || 1) * 1000 * (1 + Math.random());
      await new Promise((r) => setTimeout(r, wait));
      if (init?.signal?.aborted) throw new DOMException('aborted', 'AbortError');
      continue;
    }
    const body = await res.json().catch(() => ({}));
    if (!res.ok) throw Object.assign(new Error(body.error || `HTTP ${res.status}`), { status: res.status, body });
    return body;
  }
}

// ---------- tooltip ----------

const tip = $('#tip');
function showTip(/** @type {Node[]} */ nodes, /** @type {number} */ x, /** @type {number} */ y) {
  tip.replaceChildren(...nodes);
  tip.hidden = false;
  const r = tip.getBoundingClientRect();
  let left = x + 14, top = y + 14;
  if (left + r.width > window.innerWidth - 8) left = x - r.width - 14;
  if (top + r.height > window.innerHeight - 8) top = y - r.height - 14;
  tip.style.left = Math.max(8, left) + 'px';
  tip.style.top = Math.max(8, top) + 'px';
}
function hideTip() { tip.hidden = true; }

// ---------- panels ----------

let hatchPattern = /** @type {CanvasPattern|null} */ (null);
function hatch(/** @type {CanvasRenderingContext2D} */ ctx) {
  if (hatchPattern) return hatchPattern;
  const c = document.createElement('canvas');
  const dpr = window.devicePixelRatio || 1;
  c.width = c.height = Math.round(6 * dpr);
  const g = /** @type {CanvasRenderingContext2D} */ (c.getContext('2d'));
  g.strokeStyle = css('--hatch');
  g.lineWidth = 1.2 * dpr;
  g.beginPath();
  for (let o = -c.width; o <= c.width; o += c.width) { g.moveTo(o, c.height); g.lineTo(o + c.width, 0); }
  g.stroke();
  hatchPattern = ctx.createPattern(c, 'repeat');
  return hatchPattern;
}

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
   * Probe kinds this panel shows. Path targets are ICMP/TCP only: they follow
   * the header's TCP/ICMP selection and fall back to ICMP (or TCP when ICMP
   * is unavailable) when only HTTPS is selected.
   * @returns {Set<string>}
   */
  kinds() {
    if (!this.isPath) return state.kinds;
    const avail = new Set(this.targets.flatMap((t) => t.kinds));
    const icmpOff = status && status.caps && status.caps.icmp === 'unavailable';
    const sel = [...state.kinds].filter((k) => avail.has(k) && !(k === 'icmp' && icmpOff));
    if (sel.some((k) => k !== 'https')) return new Set(sel);
    for (const k of ['icmp', 'tcp', 'https']) if (avail.has(k) && !(k === 'icmp' && icmpOff)) return new Set([k]);
    return new Set(avail);
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

  /** The target's "×N normal" ratio for the selected kinds (HTTPS first). */
  ratioFor(/** @type {string} */ name, /** @type {string[]} */ kinds) {
    for (const k of ['https', 'tcp', 'icmp']) {
      if (!kinds.includes(k)) continue;
      const b = baselines.get(name + '\0' + k);
      if (b && typeof b.ratio_now === 'number' && isFinite(b.ratio_now)) return { r: b.ratio_now, b };
    }
    return null;
  }

  /** The baseline band (median..p95) of visible HTTPS targets: at most BAND_MAX, else only the slow ones. */
  drawBand(/** @type {any} */ u) {
    if (!baseOn || !state.band || !baselines.size || !this.kinds().has('https')) return;
    let list = this.targets
      .filter((t) => !this.hidden.has(t.name) && t.kinds.includes('https'))
      .map((t) => ({ t, b: baselines.get(t.name + '\0https') }))
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

  /** Series layout: per target, per selected kind a mean line; HTTPS adds a min–max band. */
  layout() {
    const d = /** @type {PanelData} */ (this.data);
    /** @type {Map<string, SeriesData>} */
    const byKey = new Map(d.series.map((s) => [s.target + '\0' + s.kind, s]));
    const smap = [];
    const kinds = this.kinds();
    let si = 1;
    for (const t of this.targets) {
      const color = seriesColor(/** @type {number} */ (this.colors.get(t.name)));
      for (const k of KINDS) {
        if (!kinds.has(k) || !t.kinds.includes(k)) continue;
        const sd = byKey.get(t.name + '\0' + k) || null;
        smap.push({ si: si++, target: t.name, kind: k, role: 'mean', sd, color });
        if (k === 'https') {
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
    for (const m of smap) {
      if (m.role === 'mean') {
        series.push({
          label: `${m.target} ${KIND_LABEL[/** @type {'https'} */ (m.kind)]}`,
          stroke: m.color, width: m.kind === 'https' ? 1.5 : 1.25, dash: solo ? [] : KIND_DASH[/** @type {'https'} */ (m.kind)],
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
        draw: [(/** @type {any} */ u) => { self.drawBand(u); self.drawIncidents(u); drawNotes(u); self.drawStrip(); }],
      },
    };
    this.emptyEl.remove();
    const u = new uPlot(opts, data, this.chartEl);
    this.chartEl.append(this.emptyEl);
    u.over.addEventListener('mouseenter', () => { self.hover = true; });
    u.over.addEventListener('mouseleave', () => { self.hover = false; hideTip(); });
    bindNoteGestures(u);
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
      const kinds = KINDS.filter((k) => sel.has(k) && t.kinds.includes(k));
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
      const nb = kinds.has('https') ? baselines.get(target + '\0https') : null;
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
    nodes.push(...noteTipNodes(notesNear(u, u.cursor.left)));
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
  return { ever, n, lost, loss: n + lost ? lost / (n + lost) : NaN, rtt: null };
}

/** Incidents overlapping [from, to) (milliseconds). */
function incidentsAt(/** @type {number} */ from, /** @type {number} */ to) {
  const now = Date.now();
  return incidents.filter((inc) => Date.parse(inc.start) < to && (inc.end ? Date.parse(inc.end) : now) >= from);
}

// ---------- investigate (per-hop traces) ----------

/** @typedef {{hop:number,ip?:string,no_reply?:boolean,private?:boolean,masked?:boolean,rdns?:string,asn?:number,owner?:string}} HopRef */
/** @typedef {HopRef & {n:number,lost:number,loss:number,min:number|null,mean:number|null,max:number|null,p95:number|null,jitter:number|null,loss_continues:boolean}} HopRow */
/** @typedef {{id:number,target:string,at:string,first_diff:number,from:HopRef[],to:HopRef[]}} RouteChange */
/** @typedef {{target:string,traced:boolean,from:number,to:number,now:number,isp_asn?:number,route:{since:string,hops:HopRef[]}|null,hops:HopRow[],changes:RouteChange[],investigate_until?:number}} TraceData */

const INV_REFRESH_MS = 5000;
const KEEPALIVE_MS = 60000;
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
  return (tr.find((t) => t.layer === 'anycast') || tr[0] || (MODE === 'local' ? allTargets.find((t) => !t.layer) : null) || { name: '' }).name;
}

/** The target an outage-log row offers to trace: the first affected one we can trace, else the path. */
function incidentTraceTarget(/** @type {Incident} */ inc) {
  if (!traceOn) return '';
  for (const n of inc.targets || []) {
    const t = targetInfo(n);
    if (t && (t.trace || MODE === 'local')) return n;
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
  if (inv) {
    inv.el.hidden = !on;
    if (on) inv.open(/** @type {string} */ (state.inv)); else inv.close();
  }
  updateRefreshLabel();
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
    this.lastPoke = 0;
    this.poking = false;
    this.invErr = '';
    this.invUntil = 0;
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
        h('p', { text: 'Latency works the same way: a spike at one middle hop that later hops do not show is the router being slow to reply, not slow to forward. The latency bar shows each hop\'s min–max range with a tick at the average.' }),
        MODE === 'public' ? h('p', { class: 'muted', text: 'Public view: addresses inside the home and the ISP\'s private network are hidden, the ISP\'s first public address is shortened, and router names are hidden for the first public hops.' }) : null));
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
      this.invUntil = 0;
      this.lastPoke = 0;
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
    this.renderStatus();
    this.keepalive();
  }

  close() { this.ctl?.abort(); }

  fillPicker() {
    const opts = [];
    const ok = (/** @type {TargetInfo} */ t) => t.trace || MODE === 'local';
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
    if (MODE === 'local') {
      for (const [g, ts] of groups) {
        const rest = ts.filter((/** @type {TargetInfo} */ t) => !t.trace);
        if (rest.length) opts.push(h('optgroup', { label: `${g} (on demand)` }, ...rest.map((/** @type {TargetInfo} */ t) => h('option', { value: t.name, text: t.name }))));
      }
    }
    if (!allTargets.some((t) => t.name === this.target && ok(t))) opts.unshift(h('option', { value: this.target, text: this.target }));
    this.picker.replaceChildren(...opts);
    this.picker.value = this.target;
  }

  /** On-demand trace for a target without an always-on trace: POST now and every 60 s while open. */
  keepalive() {
    const t = targetInfo(this.target);
    if (MODE !== 'local' || !t || t.trace || this.poking || document.hidden) return;
    if (status && status.controls === 'disabled') {
      this.invErr = 'on-demand traces are disabled because the dashboard is reachable from your network and fyisp was started without --admin-token';
      this.renderStatus();
      return;
    }
    if (Date.now() - this.lastPoke < KEEPALIVE_MS) return;
    this.poke();
  }

  async poke() {
    const target = this.target;
    this.poking = true;
    this.lastPoke = Date.now();
    /** @type {Record<string,string>} */
    const headers = { 'X-FYISP-Token': TOKEN };
    if (status && status.controls === 'admin') headers['X-FYISP-Admin'] = adminHeader();
    try {
      const r = await fetchJSON('api/investigate?' + new URLSearchParams({ target }), { method: 'POST', headers }, 1);
      if (target !== this.target) return;
      this.invUntil = r.until;
      this.invErr = '';
    } catch (e) {
      if (target !== this.target) return;
      const err = /** @type {any} */ (e);
      if (err.status === 403 && status && status.controls === 'admin') { try { sessionStorage.removeItem('fyisp-admin'); } catch { /* ignore */ } }
      this.invErr = err.message;
    } finally {
      this.poking = false;
    }
    this.renderStatus();
  }

  renderStatus() {
    const t = targetInfo(this.target);
    const d = this.data;
    const kids = [];
    if (t && t.trace) kids.push(h('span', { class: 'pill on', text: 'Always-on trace' }));
    else if (MODE === 'local') {
      if (this.invErr) kids.push(h('span', { class: 'pill err', text: `On-demand trace failed: ${this.invErr}` }));
      else if (this.invUntil) kids.push(h('span', { class: 'pill on', title: 'The trace keeps running while this view is open, and stops about 2 minutes after you leave it.', text: 'On-demand trace running' }));
      else kids.push(h('span', { class: 'pill', text: 'Starting on-demand trace…' }));
    } else kids.push(h('span', { class: 'pill', text: 'No always-on trace for this target' }));
    if (d && d.route) kids.push(h('span', { class: 'muted small', text: `route unchanged since ${fmtWhen(Date.parse(d.route.since))}` }));
    this.statusEl.replaceChildren(...kids);
  }

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
      if (d.investigate_until) this.invUntil = d.investigate_until;
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
      const pending = MODE === 'local' && t && !t.trace && !this.invErr;
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
          x.masked ? h('span', { class: 'masked', title: 'Shortened on the public link', text: ' (shortened)' }) : null,
          x.private && MODE === 'local' ? h('span', { class: 'masked', text: ' private' }) : null),
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
        draw: [(/** @type {any} */ u) => { self.drawChanges(u, !isLoss); drawNotes(u); }],
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
    const rect = u.over.getBoundingClientRect();
    showTip(nodes, rect.left + u.cursor.left, rect.top + u.cursor.top);
  }
}

// ---------- write helper ----------

/** POST/PUT/DELETE JSON with the CSRF (and admin) token. @param {string} method @param {string} url @param {any} [body] */
async function apiWrite(method, url, body) {
  /** @type {Record<string,string>} */
  const headers = { 'X-FYISP-Token': TOKEN };
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  if (status && status.controls === 'admin') headers['X-FYISP-Admin'] = adminHeader();
  try {
    return await fetchJSON(url, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) }, 1);
  } catch (e) {
    const err = /** @type {any} */ (e);
    if (err.status === 403 && status && status.controls === 'admin') { try { sessionStorage.removeItem('fyisp-admin'); } catch { /* ignore */ } }
    throw e;
  }
}

/** Owner controls (notes, reports) are usable: local and not disabled by a LAN bind without --admin-token. */
function canWrite() { return MODE === 'local' && !!status && status.controls !== 'disabled'; }

/** The current view as absolute milliseconds. */
function viewRange() {
  if (state.from != null && state.to != null) return { from: state.from, to: state.to };
  const to = Date.now();
  return { from: to - rangeSecs() * 1000, to };
}

/** 'YYYY-MM-DDTHH:MM' in local time, for <input type="datetime-local">. */
function toLocalInput(/** @type {number} */ ms) {
  const d = new Date(ms);
  const p = (/** @type {number} */ n) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}`;
}
function fromLocalInput(/** @type {string} */ s) { const t = s ? new Date(s).getTime() : NaN; return isFinite(t) ? t : NaN; }

async function copyText(/** @type {string} */ text, /** @type {HTMLElement} */ btn) {
  const label = btn.textContent;
  try { await navigator.clipboard.writeText(text); btn.textContent = 'Copied'; } catch { window.prompt('Copy this link:', text); }
  setTimeout(() => { btn.textContent = label; }, 1500);
}

// ---------- notes (annotations) ----------

/** @typedef {{id:number,at:string,end?:string,text:string,public:boolean,created?:string,updated?:string}} Note */

let notesOn = false;
/** @type {Note[]} */
let notes = [];
let notesLoaded = false;

async function loadNotes() {
  if (!notesOn) return;
  const { from, to } = queryRange();
  try {
    const list = await fetchJSON('api/annotations?' + new URLSearchParams({ from, to }));
    notes = Array.isArray(list) ? list : [];
    notesLoaded = true;
  } catch {
    return;
  }
  renderNotes();
  redrawCharts();
}

function redrawCharts() {
  for (const p of panels) p.u?.redraw(false);
  if (inv) { inv.rttU?.redraw(false); inv.lossU?.redraw(false); }
}

function noteSpan(/** @type {Note} */ n) {
  const a = Date.parse(n.at);
  return { a, b: n.end ? Date.parse(n.end) : a };
}
function noteColor(/** @type {Note} */ n) { return n.public ? (css('--accent') || '#3987e5') : (css('--note') || '#c3c2b7'); }

/** Notes whose marker (or range) is under css-pixel x on this chart. */
function notesNear(/** @type {any} */ u, /** @type {number} */ px) {
  const out = [];
  for (const n of notes) {
    const { a, b } = noteSpan(n);
    const x0 = u.valToPos(a / 1000, 'x'), x1 = u.valToPos(b / 1000, 'x');
    if ((px >= x0 - 6 && px <= x0 + 10) || (b > a && px >= x0 && px <= x1)) out.push(n);
  }
  return out;
}

/** Note markers: a flag and a dashed line at the note's time; a faint band for a range. */
function drawNotes(/** @type {any} */ u) {
  if (!notes.length) return;
  const ctx = u.ctx;
  const { left, top, width, height } = u.bbox;
  const vmin = u.scales.x.min, vmax = u.scales.x.max;
  const dpr = window.devicePixelRatio || 1;
  ctx.save();
  ctx.beginPath();
  ctx.rect(left, top, width, height);
  ctx.clip();
  for (const n of notes) {
    const { a, b } = noteSpan(n);
    if (b / 1000 < vmin || a / 1000 > vmax) continue;
    const c = noteColor(n);
    const x = Math.round(u.valToPos(a / 1000, 'x', true)) + 0.5;
    if (b > a) {
      const x1 = u.valToPos(b / 1000, 'x', true);
      ctx.fillStyle = rgba(c, 0.07);
      ctx.fillRect(x, top, x1 - x, height);
      ctx.fillStyle = rgba(c, 0.55);
      ctx.fillRect(x, top + height - 2 * dpr, x1 - x, 2 * dpr);
    }
    ctx.strokeStyle = rgba(c, 0.55);
    ctx.lineWidth = dpr;
    ctx.setLineDash([3 * dpr, 3 * dpr]);
    ctx.beginPath();
    ctx.moveTo(x, top + 12 * dpr);
    ctx.lineTo(x, top + height);
    ctx.stroke();
    ctx.setLineDash([]);
    ctx.strokeStyle = c;
    ctx.lineWidth = 1.5 * dpr;
    ctx.beginPath();
    ctx.moveTo(x, top);
    ctx.lineTo(x, top + 13 * dpr);
    ctx.stroke();
    ctx.fillStyle = c;
    ctx.beginPath();
    ctx.moveTo(x, top);
    ctx.lineTo(x + 10 * dpr, top + 3.5 * dpr);
    ctx.lineTo(x, top + 7 * dpr);
    ctx.closePath();
    ctx.fill();
  }
  ctx.restore();
}

/** Tooltip rows for notes under the cursor. */
function noteTipNodes(/** @type {Note[]} */ list) {
  const nodes = [];
  if (!list.length) return nodes;
  nodes.push(h('div', { class: 'sep' }));
  for (const n of list.slice(0, 3)) {
    const flag = h('span', { class: 'nflag' + (n.public ? ' pub' : '') });
    nodes.push(h('div', { class: 'row note' }, flag, h('span', { class: 'ntext', text: n.text }),
      MODE === 'local' ? h('span', { class: 'nbadge' + (n.public ? ' pub' : ''), text: n.public ? 'public' : 'private' }) : null));
  }
  if (list.length > 3) nodes.push(h('div', { class: 'muted', text: `+${list.length - 3} more notes` }));
  return nodes;
}

/**
 * Chart gestures for notes: right-click (or long-press) adds a note at that
 * time, Shift-drag adds one over a range (instead of zooming), clicking a
 * flag edits it. Local and writable only.
 * @param {any} u
 */
function bindNoteGestures(u) {
  const over = /** @type {HTMLElement} */ (u.over);
  const timeAt = (/** @type {number} */ clientX) => Math.round(u.posToVal(clientX - over.getBoundingClientRect().left, 'x') * 1000);
  over.addEventListener('mousedown', (e) => { u._noteDrag = e.shiftKey && notesOn && canWrite(); }, true);
  over.addEventListener('contextmenu', (e) => {
    if (!notesOn || !canWrite()) return;
    e.preventDefault();
    openNoteEditor(null, e.clientX, e.clientY, { at: timeAt(e.clientX) });
  });
  over.addEventListener('click', (e) => {
    if (!notesOn || !canWrite()) return;
    const r = over.getBoundingClientRect();
    if (e.clientY - r.top > 18) return;
    const hit = notesNear(u, e.clientX - r.left)[0];
    if (hit) { e.stopPropagation(); openNoteEditor(hit, e.clientX, e.clientY); }
  });
  /** @type {number} */ let timer = 0;
  let sx = 0, sy = 0;
  const cancel = () => { clearTimeout(timer); timer = 0; };
  over.addEventListener('pointerdown', (e) => {
    if (e.pointerType !== 'touch' || !notesOn || !canWrite()) return;
    sx = e.clientX; sy = e.clientY;
    cancel();
    timer = window.setTimeout(() => { timer = 0; hideTip(); openNoteEditor(null, sx, sy, { at: timeAt(sx) }); }, 600);
  });
  over.addEventListener('pointermove', (e) => { if (timer && Math.hypot(e.clientX - sx, e.clientY - sy) > 10) cancel(); });
  over.addEventListener('pointerup', cancel);
  over.addEventListener('pointercancel', cancel);
}

/** A select (drag) on a chart: zoom, or with Shift a note over that range. */
function onChartSelect(/** @type {any} */ u) {
  const w = u.select.width;
  if (w > 4) {
    const a = Math.round(u.posToVal(u.select.left, 'x') * 1000), b = Math.round(u.posToVal(u.select.left + w, 'x') * 1000);
    if (u._noteDrag) {
      const r = u.over.getBoundingClientRect();
      openNoteEditor(null, r.left + u.select.left + w, r.top + 20, { at: a, end: b });
    } else zoomTo(a, b);
  }
  u._noteDrag = false;
  u.setSelect({ left: 0, top: 0, width: 0, height: 0 }, false);
}

const notePop = h('div', { id: 'note-pop', class: 'pop', role: 'dialog', 'aria-label': 'Note', hidden: true });

function closeNoteEditor() { notePop.hidden = true; notePop.replaceChildren(); }

/**
 * The note popover: add (note null) or edit. preset gives the time(s) of a new note.
 * @param {Note|null} note @param {number} x @param {number} y @param {{at:number,end?:number}} [preset]
 */
function openNoteEditor(note, x, y, preset) {
  hideTip();
  const at = note ? Date.parse(note.at) : (preset ? preset.at : Date.now());
  const end = note ? (note.end ? Date.parse(note.end) : NaN) : (preset && preset.end ? preset.end : NaN);
  const atIn = /** @type {HTMLInputElement} */ (h('input', { type: 'datetime-local', value: toLocalInput(at), required: true, 'aria-label': 'When' }));
  const endIn = /** @type {HTMLInputElement} */ (h('input', { type: 'datetime-local', value: isFinite(end) ? toLocalInput(end) : '', 'aria-label': 'Until (optional)' }));
  const text = /** @type {HTMLTextAreaElement} */ (h('textarea', { rows: '3', maxlength: '500', placeholder: 'What happened? e.g. "Zoom call dropped", "ISP ticket #123 opened"', 'aria-label': 'Note text' }));
  text.value = note ? note.text : '';
  const pub = /** @type {HTMLInputElement} */ (h('input', { type: 'checkbox' }));
  pub.checked = !!(note && note.public);
  const err = h('div', { class: 'perr', role: 'alert' });
  const save = /** @type {HTMLButtonElement} */ (h('button', { class: 'btn primary', type: 'submit', text: note ? 'Save' : 'Add note' }));
  const form = h('form', { class: 'pop-form' },
    h('div', { class: 'pop-title', text: note ? 'Edit note' : preset && preset.end ? 'Note for this range' : 'Add a note' }),
    h('label', { class: 'fld' }, h('span', { text: 'When' }), atIn),
    h('label', { class: 'fld' }, h('span', { text: 'Until' }), endIn),
    text,
    h('label', { class: 'chk', title: 'Public notes are shown on your public link and in redacted reports. Private notes stay on this machine.' }, pub, ' Show on my public link'),
    err,
    h('div', { class: 'pop-actions' },
      note ? h('button', { class: 'btn ghost danger', type: 'button', text: 'Delete', onclick: () => deleteNote(note) }) : null,
      h('span', { class: 'grow' }),
      h('button', { class: 'btn ghost', type: 'button', text: 'Cancel', onclick: closeNoteEditor }),
      save));
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    const a = fromLocalInput(atIn.value), b = fromLocalInput(endIn.value);
    const t = text.value.trim();
    if (!isFinite(a)) { err.textContent = 'Pick a time.'; return; }
    if (!t) { err.textContent = 'Write something.'; text.focus(); return; }
    if (endIn.value && !(b > a)) { err.textContent = '"Until" must be after "When".'; return; }
    /** @type {any} */
    const body = { at: new Date(a).toISOString(), text: t, public: pub.checked };
    if (endIn.value) body.end = new Date(b).toISOString();
    save.disabled = true;
    try {
      await apiWrite(note ? 'PUT' : 'POST', note ? `api/annotations/${note.id}` : 'api/annotations', body);
      closeNoteEditor();
      await loadNotes();
    } catch (e2) {
      err.textContent = `Could not save: ${/** @type {Error} */ (e2).message}`;
      save.disabled = false;
    }
  });
  form.addEventListener('keydown', (e) => { if (e.key === 'Escape') closeNoteEditor(); });
  notePop.replaceChildren(form);
  notePop.hidden = false;
  const r = notePop.getBoundingClientRect();
  let left = x + 8, top = y + 8;
  if (left + r.width > window.innerWidth - 8) left = Math.max(8, x - r.width - 8);
  if (top + r.height > window.innerHeight - 8) top = Math.max(8, window.innerHeight - r.height - 8);
  notePop.style.left = left + 'px';
  notePop.style.top = top + 'px';
  text.focus();
}

async function deleteNote(/** @type {Note} */ n) {
  if (!window.confirm(`Delete this note?\n\n${n.text}`)) return;
  try {
    await apiWrite('DELETE', `api/annotations/${n.id}`);
    closeNoteEditor();
    await loadNotes();
  } catch (e) {
    window.alert(`Could not delete: ${/** @type {Error} */ (e).message}`);
  }
}

function renderNotes() {
  const box = $('#notes');
  box.hidden = !notesOn;
  if (!notesOn) return;
  const n = notes.length;
  $('#notes-count').textContent = notesLoaded ? `· ${n} in range` : '';
  const list = $('#notes-list');
  const kids = [];
  if (!n) {
    kids.push(h('p', { class: 'muted o-empty', text: MODE === 'public' ? 'No public notes in this range.' : 'No notes in this range. Right-click a chart (long-press on a phone) to add one at that time, or Shift-drag for a range.' }));
  }
  for (const note of [...notes].reverse()) {
    const { a, b } = noteSpan(note);
    const row = h('button', {
      class: 'o-row n-row', type: 'button', title: 'Zoom all charts to this note',
      onclick: () => zoomTo(a - 15 * 60e3, Math.min(Date.now(), Math.max(b, a) + 15 * 60e3)),
    },
    h('span', { class: 'o-when' }, fmtWhen(a), b > a ? ` – ${fmtWhen(b)}` : ''),
    h('span', { class: 'nflag' + (note.public ? ' pub' : ''), 'aria-hidden': 'true' }),
    h('span', { class: 'o-sum n-text', text: note.text }),
    MODE === 'local' ? h('span', { class: 'nbadge' + (note.public ? ' pub' : ''), title: note.public ? 'Shown on your public link' : 'Only on this machine', text: note.public ? 'public' : 'private' }) : null);
    const acts = canWrite() ? h('span', { class: 'n-acts' },
      h('button', { class: 'tbtn', type: 'button', text: 'Edit', onclick: (/** @type {MouseEvent} */ e) => { e.stopPropagation(); const r = /** @type {HTMLElement} */ (e.currentTarget).getBoundingClientRect(); openNoteEditor(note, r.left - 300, r.bottom); } }),
      h('button', { class: 'tbtn danger', type: 'button', text: 'Delete', onclick: (/** @type {MouseEvent} */ e) => { e.stopPropagation(); deleteNote(note); } })) : null;
    kids.push(h('div', { class: 'o-item' }, row, acts));
  }
  list.replaceChildren(...kids);
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

/** The view on screen: the Overview ('o') or the chart wall ('c'). Focus is always charts. */
function currentView() {
  if (state.focus && focusPanel()) return 'c';
  return state.view || (bigProfile ? 'o' : 'c');
}
function overviewShown() { return !!ov && currentView() === 'o' && !(state.inv && inv); }
/** The kind the Overview shows: the first selected of HTTPS, TCP, ICMP. */
function ovKind() { return KINDS.find((k) => state.kinds.has(k)) || 'https'; }
function focusPanel() { return state.focus ? panels.find((p) => !p.isPath && p.targets.some((t) => t.name === state.focus)) || null : null; }
/** Panels that are on screen and may load: the path panel always, the others per view and focus. */
function panelShown(/** @type {Panel} */ p) {
  if (p.isPath) return true;
  if (currentView() === 'o') return false;
  const fp = focusPanel();
  return !fp || fp === p;
}
function loadStale() { for (const p of panels) if (p.visible && p.stale && panelShown(p)) p.load(); }
function fmtInt(/** @type {number} */ n) { return n.toLocaleString('en-US'); }
function median(/** @type {number[]} */ v) {
  if (!v.length) return NaN;
  const s = [...v].sort((a, b) => a - b), m = s.length >> 1;
  return s.length % 2 ? s[m] : (s[m - 1] + s[m]) / 2;
}
/** Worst first: state, then loss, then ratio. */
function bySeverity(/** @type {OvRow} */ a, /** @type {OvRow} */ b) {
  return (OV_STATES[a.state]?.rank ?? 9) - (OV_STATES[b.state]?.rank ?? 9) || b.loss - a.loss || (b.ratio ?? 0) - (a.ratio ?? 0) || a.target.localeCompare(b.target);
}
function geoOf(/** @type {TargetInfo} */ t) { return t.geo || ''; }
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
      kids.push(h('span', { class: 'muted', text: ` This line covers the whole range and counts ${what}; the verdict above judges only the last minute.` }));
    }
    const un = d.summary.targets - d.summary.measured;
    if (un) kids.push(h('span', { class: 'muted', text: ` ${fmtInt(un)} not measured.` }));
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
    c.loss.className = 'num' + (r && r.loss >= FAIL_LOSS ? ' bad' : r && r.loss >= REAL_LOSS ? ' warn' : '');
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
      if (r.top > window.innerHeight) this.tableBox.scrollIntoView({ block: 'start', behavior: 'smooth' });
    }
  }
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
  if (v === 'o') ov?.load(); else loadStale();
}

function renderViewBar() {
  const v = currentView();
  viewSeg.replaceChildren(
    h('button', { type: 'button', text: 'Overview', 'aria-pressed': String(v === 'o'), title: 'Every target in one table and heatmap', onclick: () => setView('o') }),
    h('button', { type: 'button', text: 'Charts', 'aria-pressed': String(v === 'c' && !focusPanel()), title: 'One chart per panel', onclick: () => setView('c') }));
  const hint = /** @type {HTMLElement} */ (viewBar.lastElementChild);
  const n = panels.filter((p) => !p.isPath).length;
  hint.textContent = v === 'o' ? `${fmtInt(allTargets.filter((t) => !t.layer).length)} targets; click a row to see its chart` : `${n} chart panel${n === 1 ? '' : 's'}`;
}

/** Applies the view and focus to the page: which panels show, the focus bar, isolation. */
function applyView() {
  const v = currentView();
  document.body.classList.toggle('ov-on', v === 'o');
  if (ov) ov.el.hidden = v !== 'o';
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

// ---------- reports ----------

/** @typedef {{id:string,title:string,from:string,to:string,created:string,public:boolean,bytes:number,redacted:boolean}} ReportMeta */

let reportsOn = false;
/** @type {ReportMeta[]} */
let reports = [];

function shareURL() {
  const sh = status && status.share;
  return sh && sh.phase === 'connected' && sh.url ? String(sh.url) : '';
}
function reportPublicURL(/** @type {ReportMeta} */ r) {
  const base = shareURL();
  return base && r.public && r.redacted ? base.replace(/\/?$/, '/') + 'r/' + r.id : '';
}
function fmtBytes(/** @type {number} */ n) { return n < 1024 ? `${n} B` : n < 1048576 ? `${(n / 1024).toFixed(0)} KB` : `${(n / 1048576).toFixed(1)} MB`; }

async function loadReports() {
  if (!reportsOn) return;
  try {
    const list = await fetchJSON('api/reports');
    reports = Array.isArray(list) ? list : [];
  } catch {
    return;
  }
  renderReports();
}

/** Open / Download / Copy public link / Publish / Delete for one report. */
function reportActions(/** @type {ReportMeta} */ r, /** @type {() => void} */ after, big = false) {
  const cls = big ? 'btn' : 'tbtn';
  const kids = [
    h('a', { class: cls + (big ? ' primary' : ''), href: `api/reports/${r.id}`, target: '_blank', rel: 'noopener', text: 'Open' }),
    h('a', { class: cls, href: `api/reports/${r.id}?download=1`, download: '', text: 'Download' }),
  ];
  const link = reportPublicURL(r);
  if (r.public && r.redacted) {
    const copy = h('button', {
      class: cls, type: 'button', text: 'Copy public link', disabled: !link,
      title: link ? link : 'Start your public link (top of the page) to get this report\'s address',
      onclick: () => copyText(link, copy),
    });
    kids.push(copy);
  }
  if (canWrite()) {
    if (r.redacted) {
      kids.push(h('button', {
        class: cls, type: 'button', text: r.public ? 'Unpublish' : 'Publish',
        title: r.public ? 'Stop serving this snapshot on your public link' : 'Serve this redacted snapshot on your public link',
        onclick: async () => {
          try { await apiWrite('PUT', `api/reports/${r.id}`, { public: !r.public }); } catch (e) { window.alert(/** @type {Error} */ (e).message); }
          await loadReports();
          after();
        },
      }));
    }
    kids.push(h('button', {
      class: cls + ' danger', type: 'button', text: 'Delete',
      onclick: async () => {
        if (!window.confirm(`Delete the report "${r.title}"?`)) return;
        try { await apiWrite('DELETE', `api/reports/${r.id}`); } catch (e) { window.alert(/** @type {Error} */ (e).message); }
        await loadReports();
        after();
      },
    }));
  }
  return h('span', { class: 'r-acts' + (big ? ' big' : '') }, ...kids);
}

function renderReports() {
  const box = $('#reports');
  box.hidden = !reportsOn;
  if (!reportsOn) return;
  $('#reports-count').textContent = `· ${reports.length} saved`;
  const list = $('#reports-list');
  if (!reports.length) {
    list.replaceChildren(h('p', { class: 'muted o-empty', text: 'No reports yet. "Create report" builds a self-contained page (prints to PDF) for the current range; "Report" on an outage builds one for that incident.' }));
    return;
  }
  list.replaceChildren(...reports.map((r) => h('div', { class: 'r-item' },
    h('div', { class: 'r-main' },
      h('span', { class: 'r-title', text: r.title }),
      h('span', { class: 'r-meta muted' }, `${fmtWhen(Date.parse(r.from))} – ${fmtWhen(Date.parse(r.to))} · created ${fmtWhen(Date.parse(r.created))} · ${fmtBytes(r.bytes)}`),
      h('span', { class: 'r-badges' },
        h('span', { class: 'nbadge' + (r.redacted ? '' : ' warn'), title: r.redacted ? 'No private addresses; public notes only' : 'Includes private addresses, host names and private notes', text: r.redacted ? 'redacted' : 'private details' }),
        r.public && r.redacted ? h('span', { class: 'nbadge pub', title: 'Served on your public link while sharing is on', text: 'on public link' }) : null)),
    reportActions(r, renderReports))));
}

/** @type {HTMLDialogElement|null} */
let reportDlg = null;

/**
 * The "Create report" dialog. range defaults to the current view.
 * @param {{from:number,to:number,title?:string}} [preset]
 */
function openReportDialog(preset) {
  const rng = preset || viewRange();
  const dlg = reportDlg || /** @type {HTMLDialogElement} */ (h('dialog', { class: 'dlg', 'aria-label': 'Create report' }));
  if (!reportDlg) { document.body.append(dlg); reportDlg = dlg; }
  const title = /** @type {HTMLInputElement} */ (h('input', { type: 'text', maxlength: '200', value: rng.title || `Connection report: ${fmtWhen(rng.from)} – ${fmtWhen(rng.to)}` }));
  const fromIn = /** @type {HTMLInputElement} */ (h('input', { type: 'datetime-local', value: toLocalInput(rng.from), required: true }));
  const toIn = /** @type {HTMLInputElement} */ (h('input', { type: 'datetime-local', value: toLocalInput(Math.min(rng.to, Date.now())), required: true }));
  const priv = /** @type {HTMLInputElement} */ (h('input', { type: 'checkbox' }));
  const pub = /** @type {HTMLInputElement} */ (h('input', { type: 'checkbox' }));
  const pubHint = h('span', { class: 'hint' });
  const syncPub = () => {
    pub.disabled = priv.checked;
    if (priv.checked) pub.checked = false;
    pubHint.textContent = priv.checked
      ? 'Reports with private details cannot be published; they stay on this machine.'
      : shareURL() ? 'Anyone with your public link can open it.' : 'It will be served once you start your public link.';
  };
  priv.addEventListener('change', syncPub);
  syncPub();
  const err = h('div', { class: 'perr', role: 'alert' });
  const go = /** @type {HTMLButtonElement} */ (h('button', { class: 'btn primary', type: 'submit', text: 'Create report' }));
  const form = h('form', { class: 'dlg-form', method: 'dialog' },
    h('h2', { text: 'Create report' }),
    h('p', { class: 'muted small', text: 'A self-contained page with the verdicts, outage log, charts, statistics and notes for this range. Open it to print or save as PDF.' }),
    h('label', { class: 'fld' }, h('span', { text: 'Title' }), title),
    h('div', { class: 'fld-row' },
      h('label', { class: 'fld' }, h('span', { text: 'From' }), fromIn),
      h('label', { class: 'fld' }, h('span', { text: 'To' }), toIn)),
    h('label', { class: 'chk' }, priv, h('span', {}, ' Include private details', h('span', { class: 'hint', text: 'Addresses, router names and private notes. Leave unchecked for a redacted report you can send to your ISP.' }))),
    h('label', { class: 'chk' }, pub, h('span', {}, ' Publish on my public link', pubHint)),
    err,
    h('div', { class: 'pop-actions' },
      h('span', { class: 'grow' }),
      h('button', { class: 'btn ghost', type: 'button', text: 'Cancel', onclick: () => dlg.close() }),
      go));
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    const a = fromLocalInput(fromIn.value), b = fromLocalInput(toIn.value);
    if (!(isFinite(a) && isFinite(b) && b > a)) { err.textContent = '"To" must be after "From".'; return; }
    go.disabled = true;
    go.textContent = 'Building…';
    err.textContent = '';
    try {
      /** @type {ReportMeta} */
      const r = await apiWrite('POST', 'api/reports', {
        from: new Date(a).toISOString(), to: new Date(b).toISOString(), title: title.value.trim(), redact: !priv.checked, public: pub.checked,
      });
      await loadReports();
      showReportDone(dlg, r);
    } catch (e2) {
      err.textContent = `Could not build the report: ${/** @type {Error} */ (e2).message}`;
      go.disabled = false;
      go.textContent = 'Create report';
    }
  });
  dlg.replaceChildren(form);
  if (!dlg.open) dlg.showModal();
  title.focus();
  title.select();
}

function showReportDone(/** @type {HTMLDialogElement} */ dlg, /** @type {ReportMeta} */ r) {
  const refresh = () => {
    const cur = reports.find((x) => x.id === r.id);
    if (!cur) { dlg.close(); return; }
    showReportDone(dlg, cur);
  };
  dlg.replaceChildren(h('div', { class: 'dlg-form' },
    h('h2', { text: 'Report ready' }),
    h('p', { class: 'r-title', text: r.title }),
    h('p', { class: 'muted small' }, `${fmtWhen(Date.parse(r.from))} – ${fmtWhen(Date.parse(r.to))} · ${fmtBytes(r.bytes)} · `,
      r.redacted ? 'redacted' : 'includes private details', r.public && r.redacted ? ' · on your public link' : ''),
    r.public && r.redacted && !shareURL() ? h('p', { class: 'hint', text: 'Start your public link to share it; the report\'s address is your public link followed by r/<id>.' }) : null,
    reportActions(r, refresh, true),
    h('div', { class: 'pop-actions' }, h('span', { class: 'grow' }), h('button', { class: 'btn', type: 'button', text: 'Close', onclick: () => dlg.close() }))));
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
    if (state.inv && inv) await Promise.all([loadStatus(), loadVerdict(), loadNotes(), inv.load()]);
    else {
      // Hidden panels (the Overview is showing, or focus) load when shown.
      await Promise.all([loadStatus(), loadVerdict(), loadIncidents(), loadNotes(), overviewShown() ? /** @type {Overview} */ (ov).load() : null,
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
  window.scrollTo({ top: Math.max(0, top), behavior: 'smooth' });
}

/** A small "?" button that explains a verdict kind (hover, focus or tap). */
function whyButton(/** @type {string} */ title, /** @type {string} */ text) {
  const b = h('button', { class: 'why', type: 'button', 'aria-label': `What this means: ${text}`, text: '?' });
  const show = () => {
    const r = b.getBoundingClientRect();
    showTip([h('div', { class: 't', text: 'What this means' }), h('div', { class: 'why-title', text: title }), h('div', { text })], r.left, r.bottom - 6);
  };
  b.addEventListener('mouseenter', show);
  b.addEventListener('focus', show);
  b.addEventListener('click', (e) => { e.stopPropagation(); if (tip.hidden) show(); else hideTip(); });
  b.addEventListener('mouseleave', hideTip);
  b.addEventListener('blur', hideTip);
  return b;
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
  el.className = `verdict v-${/^[a-z_]+$/.test(v.kind) ? v.kind : 'other'}`;
  const kids = [h('div', { class: 'v-head' },
    h('span', { class: 'v-dot', 'aria-hidden': 'true' }),
    h('span', { class: 'v-kind', text: info.label }),
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
  // A new verdict usually means a new (or closed) incident.
  if (prev != null && verdict && prev !== verdict.kind) loadIncidents();
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
    h('span', { class: 'o-peak', title: 'Worst one-minute loss', text: `peak ${fmtPct(inc.peak_loss)} loss` }));
    const tt = incidentTraceTarget(inc);
    const tb = tt ? h('button', {
      class: 'tbtn o-trace', type: 'button', title: `Trace ${tt} hop by hop during this incident`, text: `Trace ${tt}`,
      onclick: () => { zoomTo(a - 5 * 60e3, Math.min(Date.now(), b + 5 * 60e3)); openInvestigate(tt); },
    }) : null;
    const rb = reportsOn && canWrite() ? h('button', {
      class: 'tbtn o-trace', type: 'button', title: 'Build an evidence report for this incident (±30 minutes)', text: 'Report',
      onclick: () => openReportDialog({ from: a - 30 * 60e3, to: Math.min(Date.now(), b + 30 * 60e3), title: `${info.label || info.short}: ${fmtWhen(a)}` }),
    }) : null;
    if (!tb && !rb) return h('div', { class: 'o-item' }, row);
    return h('div', { class: 'o-item' }, row, h('span', { class: 'o-acts' }, rb, tb));
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
  if (document.hidden || refreshing) return;
  if (isRelative() && Date.now() - lastRefresh >= refreshMs()) refreshAll();
  // The banner is always "now", even when zoomed; it is cheap to poll.
  else if (Date.now() - lastVerdict >= VERDICT_MS) loadVerdict();
}, 1000);
document.addEventListener('visibilitychange', () => {
  updateRefreshLabel();
  if (document.hidden) return;
  if (isRelative() && Date.now() - lastRefresh >= refreshMs()) refreshAll();
  else if (Date.now() - lastVerdict >= VERDICT_MS) loadVerdict();
  if (inv && state.inv) inv.keepalive();
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
  const icmpOff = status && status.caps && status.caps.icmp === 'unavailable';
  $('#kinds').replaceChildren(...KINDS.map((k) => h('button', {
    type: 'button', text: KIND_LABEL[/** @type {'https'} */ (k)], 'aria-pressed': String(state.kinds.has(k)),
    disabled: k === 'icmp' && icmpOff,
    title: k === 'https' ? 'HTTPS: request sent → first response byte' : k === 'tcp' ? 'TCP connect time to port 443' : 'ICMP echo (ping)',
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

/** Header tools: "+ note", "Create report" (local, writable) and the "normal" band toggle. */
function renderTools() {
  const el = $('#tools');
  const kids = [];
  if (baseOn) {
    kids.push(h('button', {
      class: 'btn ghost', type: 'button', text: 'Normal band', 'aria-pressed': String(state.band),
      title: 'Shade each target\'s normal latency (median to p95 over the past week) on the HTTPS charts',
      onclick: () => { state.band = !state.band; storageSet('fyisp-band', state.band ? '1' : '0'); renderTools(); redrawCharts(); },
    }));
  }
  if (notesOn && canWrite()) {
    kids.push(h('button', {
      class: 'btn', type: 'button', text: '+ note', title: 'Add a note at the current time (or right-click a chart at any time)',
      onclick: (/** @type {MouseEvent} */ e) => { e.stopPropagation(); const r = /** @type {HTMLElement} */ (e.currentTarget).getBoundingClientRect(); openNoteEditor(null, r.left - 150, r.bottom, { at: Date.now() }); },
    }));
  }
  if (reportsOn && canWrite()) {
    kids.push(h('button', { class: 'btn', type: 'button', text: 'Create report', title: 'Build an evidence report for the current range', onclick: () => openReportDialog() }));
  }
  el.replaceChildren(...kids);
  el.hidden = !kids.length;
}

function adminHeader() {
  let t = null;
  try { t = sessionStorage.getItem('fyisp-admin'); } catch { /* ignore */ }
  if (!t) {
    t = window.prompt('Admin token (--admin-token) for sharing, notes and reports:') || '';
    try { if (t) sessionStorage.setItem('fyisp-admin', t); } catch { /* ignore */ }
  }
  return t;
}

async function shareAction(/** @type {'start'|'stop'} */ what) {
  /** @type {Record<string,string>} */
  const headers = { 'X-FYISP-Token': TOKEN };
  if (status && status.controls === 'admin') headers['X-FYISP-Admin'] = adminHeader();
  try {
    const r = await fetchJSON(`api/share/${what}`, { method: 'POST', headers }, 1);
    status.share = r.share;
  } catch (e) {
    const err = /** @type {any} */ (e);
    if (err.status === 403 && status.controls === 'admin') { try { sessionStorage.removeItem('fyisp-admin'); } catch { /* ignore */ } }
    status.share = { ...(status.share || {}), phase: 'error', last_err: err.message };
  }
  renderShare();
  loadStatus();
}

function renderShare() {
  const el = $('#share');
  const badge = $('#mode-badge');
  if (MODE === 'public') {
    badge.hidden = false;
    badge.textContent = 'Public view · read-only';
    el.replaceChildren();
    return;
  }
  if (!status) return;
  const sh = status.share || { phase: 'off' };
  const dot = h('span', { class: 'dot' + (sh.phase === 'connected' ? ' on' : sh.phase === 'error' ? ' err' : sh.phase === 'off' || sh.phase === 'unavailable' ? '' : ' busy') });
  const kids = [dot];
  const disabled = status.controls === 'disabled';
  switch (sh.phase) {
    case 'off':
      kids.push(h('span', { class: 'muted', text: 'Not shared' }));
      if (!disabled) kids.push(h('button', { class: 'btn primary', type: 'button', text: 'Create public link', title: 'Publish a read-only, redacted view through a Cloudflare quick tunnel', onclick: () => shareAction('start') }));
      break;
    case 'connected': {
      const input = /** @type {HTMLInputElement} */ (h('input', { type: 'text', readonly: true, value: sh.url || '', 'aria-label': 'Public link' }));
      input.addEventListener('focus', () => input.select());
      const copy = h('button', {
        class: 'btn', type: 'button', text: 'Copy',
        onclick: async () => {
          try { await navigator.clipboard.writeText(sh.url); copy.textContent = 'Copied'; } catch { input.select(); document.execCommand('copy'); copy.textContent = 'Copied'; }
          setTimeout(() => { copy.textContent = 'Copy'; }, 1500);
        },
      });
      kids.push(h('span', { class: 'muted', text: 'Public link' + (sh.protocol ? ` (${sh.protocol})` : '') }), input, copy);
      if (!disabled) kids.push(h('button', { class: 'btn', type: 'button', text: 'Stop sharing', onclick: () => shareAction('stop') }));
      break;
    }
    case 'error':
      kids.push(h('span', { text: `Sharing failed${sh.last_err ? ': ' + sh.last_err : ''}` }));
      if (!disabled) kids.push(h('button', { class: 'btn', type: 'button', text: 'Retry', onclick: () => shareAction('start') }),
        h('button', { class: 'btn ghost', type: 'button', text: 'Stop', onclick: () => shareAction('stop') }));
      break;
    default:
      kids.push(h('span', { class: 'muted', text: sh.phase === 'starting' ? 'Creating public link…' : sh.phase === 'reconnecting' ? 'Reconnecting public link…' : `Sharing: ${sh.phase}` }));
      if (!disabled) kids.push(h('button', { class: 'btn ghost', type: 'button', text: 'Cancel', onclick: () => shareAction('stop') }));
  }
  if (disabled) kids.push(h('span', { class: 'muted small', text: 'Share controls disabled: the dashboard is reachable from your network and fyisp was started without --admin-token' }));
  el.replaceChildren(...kids);
}

function renderNotices() {
  const el = $('#notices');
  const out = [];
  if (!status) { el.replaceChildren(); return; }
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

let statusTimer = 0;
async function loadStatus() {
  try {
    status = await fetchJSON('api/status');
  } catch {
    return;
  }
  renderShare();
  renderNotices();
  renderTools();
  // Poll faster while the share link is changing state.
  clearTimeout(statusTimer);
  const ph = status.share && status.share.phase;
  if (MODE === 'local' && (ph === 'starting' || ph === 'reconnecting')) statusTimer = setTimeout(loadStatus, 2000);
}

// ---------- boot ----------

async function main() {
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
  readHash();
  const log = /** @type {HTMLDetailsElement} */ ($('#outages'));
  log.open = state.log;
  log.addEventListener('toggle', () => { if (state.log !== log.open) { state.log = log.open; writeHash(); } });
  const notesBox = /** @type {HTMLDetailsElement} */ ($('#notes'));
  const repBox = /** @type {HTMLDetailsElement} */ ($('#reports'));
  notesBox.open = state.notes;
  repBox.open = state.reports;
  notesBox.addEventListener('toggle', () => { if (state.notes !== notesBox.open) { state.notes = notesBox.open; writeHash(); } });
  repBox.addEventListener('toggle', () => { if (state.reports !== repBox.open) { state.reports = repBox.open; writeHash(); } if (repBox.open) loadReports(); });
  document.body.append(notePop);
  // Clicks outside the note popover close it.
  document.addEventListener('mousedown', (e) => { if (!notePop.hidden && !notePop.contains(/** @type {Node} */ (e.target))) closeNoteEditor(); });
  document.addEventListener('keydown', (e) => { if (e.key === 'Escape' && !notePop.hidden) closeNoteEditor(); });
  // Back from focus mode restores the Overview's scroll position itself.
  if ('scrollRestoration' in history) history.scrollRestoration = 'manual';
  const onNav = () => {
    if (location.hash === lastHash) return;
    lastHash = location.hash;
    readHash(); log.open = state.log; notesBox.open = state.notes; repBox.open = state.reports;
    renderControls(); applyView(); showView(); refreshAll();
  };
  window.addEventListener('hashchange', onNav);
  window.addEventListener('popstate', onNav);
  renderShare();
  const [st, prof] = await Promise.all([fetchJSON('api/status').catch(() => null), fetchJSON('api/profile')]);
  status = st;
  traceOn = !!(prof.features && prof.features.trace);
  notesOn = !!(prof.features && prof.features.notes);
  reportsOn = MODE === 'local' && !!(prof.features && prof.features.reports);
  baseOn = !!(prof.features && prof.features.baselines);
  allTargets = prof.targets;
  // Which target profile(s) this run measures, unless the default.
  const pb = $('#profile-badge');
  if (prof.name && prof.name !== 'default') {
    pb.hidden = false;
    pb.textContent = 'Profile: ' + prof.name;
    pb.title = `${prof.targets.length} targets in ${prof.groups.length} panels`;
  }
  readHash();
  if (status && status.caps && status.caps.icmp === 'unavailable') state.kinds.delete('icmp');
  if (!state.kinds.size) state.kinds.add('https');
  notesBox.open = state.notes;
  repBox.open = state.reports;
  renderNotes();
  renderReports();
  if (reportsOn) loadReports();
  renderControls();
  renderShare();
  renderNotices();
  /** @type {TargetInfo[]} */
  const targets = prof.targets;
  // The network path comes first, full width.
  const groups = [...prof.groups].sort((/** @type {Group} */ a, /** @type {Group} */ b) => Number(b.id === PATH_GROUP) - Number(a.id === PATH_GROUP));
  panels = groups.map((/** @type {Group} */ g) => new Panel(g, targets.filter((t) => t.group === g.id)));
  bigProfile = targets.filter((t) => !t.layer).length > OVERVIEW_MIN;
  ov = new Overview();
  // The path panel on top, then the view switch, then the Overview or the charts.
  const path = panels.filter((p) => p.isPath).map((p) => p.el);
  $('#panels').replaceChildren(...path, viewBar, focusBar, ov.el, ...panels.filter((p) => !p.isPath).map((p) => p.el));
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
  applyView();
  showView();
  writeHash();
  await refreshAll();
}

main().catch((e) => {
  $('#notices').replaceChildren(h('div', { class: 'notice danger' }, h('b', { text: 'Could not start: ' }), String(e && e.message || e)));
});
