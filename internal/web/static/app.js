// fyisp dashboard. Plain ES module, no build step. uPlot is loaded as a
// classic deferred script before this module runs (window.uPlot).
// @ts-check

/** @typedef {{id:string,title:string}} Group */
/** @typedef {{name:string,group:string,kinds:string[],interval_ms:number,layer?:string}} TargetInfo */
/** @typedef {{kind:string,since?:string,summary?:string,targets?:string[],evidence?:Object<string,number>}} Verdict */
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

/** @type {{range:string, from:number|null, to:number|null, kinds:Set<string>, log:boolean}} */
const state = { range: '30m', from: null, to: null, kinds: new Set(['https']), log: false };
/** @type {any} */
let status = null;
/** @type {Panel[]} */
let panels = [];
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
  const k = p.get('k');
  if (k != null) {
    const ks = k.split(',').filter((x) => KINDS.includes(x));
    state.kinds = new Set(ks.length ? ks : ['https']);
  }
}
function writeHash() {
  const p = new URLSearchParams();
  p.set('r', state.range);
  if (state.from != null && state.to != null) { p.set('from', String(state.from)); p.set('to', String(state.to)); }
  p.set('k', [...state.kinds].join(','));
  if (state.log) p.set('o', '1');
  const s = '#' + p.toString();
  if (location.hash !== s) history.replaceState(null, '', s);
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

    this.meta = h('span', { class: 'meta' });
    this.csv = h('a', { href: '#', download: '', title: 'Download this panel as CSV', text: 'CSV' });
    this.chartEl = h('div', { class: 'chart' });
    this.emptyEl = h('div', { class: 'empty', text: 'Loading…' });
    this.chartEl.append(this.emptyEl);
    this.strip = /** @type {HTMLCanvasElement} */ (h('canvas', { class: 'strip', 'aria-hidden': 'true' }));
    this.stripLegend = h('div', { class: 'strip-legend' });
    this.legend = h('ul', { class: 'legend', 'aria-label': `${group.title} targets` });
    this.layers = this.isPath ? h('ol', { class: 'layers', 'aria-label': 'Network path status' }) : null;
    this.el = h('section', { class: this.isPath ? 'panel path' : 'panel', 'aria-label': group.title, id: 'panel-' + group.id },
      h('div', { class: 'panel-head' }, h('h2', { text: group.title }), this.meta, this.csv),
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
    const { from, to } = queryRange();
    const points = Math.min(2000, Math.max(50, Math.round(this.width())));
    const q = new URLSearchParams({ group: this.group.id, from, to, points: String(points) });
    this.csv.setAttribute('href', 'api/panel.csv?' + q);
    this.chartEl.classList.add('loading');
    try {
      /** @type {PanelData} */
      const d = await fetchJSON('api/panel?' + q, { signal: ctl.signal });
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
        setSelect: [(/** @type {any} */ u) => {
          const w = u.select.width;
          if (w > 4) {
            const a = u.posToVal(u.select.left, 'x'), b = u.posToVal(u.select.left + w, 'x');
            zoomTo(Math.round(a * 1000), Math.round(b * 1000));
          }
          u.setSelect({ left: 0, top: 0, width: 0, height: 0 }, false);
        }],
        setCursor: [(/** @type {any} */ u) => self.onCursor(u)],
        draw: [(/** @type {any} */ u) => { self.drawIncidents(u); self.drawStrip(); }],
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
      h('span', { class: 'loss' + (lossF > 0.005 ? ' bad' : ''), text: fmtPct(lossF) + ' loss' }));
      items.push(li);
    }
    this.legend.replaceChildren(...items);
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
  return { ever, n, lost, loss: n + lost ? lost / (n + lost) : NaN, rtt: null };
}

/** Incidents overlapping [from, to) (milliseconds). */
function incidentsAt(/** @type {number} */ from, /** @type {number} */ to) {
  const now = Date.now();
  return incidents.filter((inc) => Date.parse(inc.start) < to && (inc.end ? Date.parse(inc.end) : now) >= from);
}

// ---------- zoom & refresh ----------

function zoomTo(/** @type {number} */ from, /** @type {number} */ to) {
  if (to - from < 30e3) { const mid = (from + to) / 2; from = mid - 15e3; to = mid + 15e3; }
  state.from = Math.round(from); state.to = Math.round(to);
  writeHash();
  for (const p of panels) { p.view = [from / 1000, to / 1000]; p.u?.setScale('x', { min: p.view[0], max: p.view[1] }); }
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
    await Promise.all([loadStatus(), loadVerdict(), loadIncidents(), ...panels.map((p) => p.load())]);
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
  p.isolate(name);
  const top = p.el.getBoundingClientRect().top + window.scrollY - $('.top').offsetHeight - 8;
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
    meta.push(h('span', { class: 'v-aff', text: 'Affected:' }));
    for (const name of v.targets) {
      meta.push(h('button', { class: 'tchip', type: 'button', title: `Show ${name} on its chart`, text: name, onclick: () => focusTarget(name) }));
    }
  }
  if (meta.length) kids.push(h('div', { class: 'v-meta' }, ...meta));
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
    const info = VERDICTS[inc.kind] || { short: inc.kind };
    const kind = /^[a-z_]+$/.test(inc.kind) ? inc.kind : 'other';
    return h('button', {
      class: 'o-row' + (inc.end ? '' : ' ongoing'), type: 'button',
      title: 'Zoom all charts to this incident',
      onclick: () => zoomTo(a - 5 * 60e3, Math.min(Date.now(), b + 5 * 60e3)),
    },
    h('span', { class: 'o-when' }, fmtWhen(a), ' – ', inc.end ? fmtWhen(b) : h('b', { text: 'ongoing' })),
    h('span', { class: 'o-dur', text: fmtDur(b - a) }),
    h('span', { class: `kbadge v-${kind}`, text: info.short }),
    h('span', { class: 'o-sum', text: inc.summary }),
    h('span', { class: 'o-peak', title: 'Worst one-minute loss', text: `peak ${fmtPct(inc.peak_loss)} loss` }));
  }));
}

function updateRefreshLabel() {
  const el = $('#refresh');
  if (refreshing) el.textContent = 'Updating…';
  else if (!isRelative()) el.textContent = 'Zoomed · auto-refresh off';
  else if (document.hidden) el.textContent = 'Paused';
  else el.textContent = 'Auto-refresh 15s';
}

setInterval(() => {
  if (document.hidden || refreshing) return;
  if (isRelative() && Date.now() - lastRefresh >= REFRESH_MS) refreshAll();
  // The banner is always "now", even when zoomed; it is cheap to poll.
  else if (Date.now() - lastVerdict >= VERDICT_MS) loadVerdict();
}, 1000);
document.addEventListener('visibilitychange', () => {
  updateRefreshLabel();
  if (document.hidden) return;
  if (isRelative() && Date.now() - lastRefresh >= REFRESH_MS) refreshAll();
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
    },
  })));
  updateRefreshLabel();
}

function adminHeader() {
  let t = null;
  try { t = sessionStorage.getItem('fyisp-admin'); } catch { /* ignore */ }
  if (!t) {
    t = window.prompt('Admin token (--admin-token) to control sharing:') || '';
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
  window.addEventListener('hashchange', () => { readHash(); log.open = state.log; renderControls(); refreshAll(); });
  renderShare();
  const [st, prof] = await Promise.all([fetchJSON('api/status').catch(() => null), fetchJSON('api/profile')]);
  status = st;
  if (status && status.caps && status.caps.icmp === 'unavailable') state.kinds.delete('icmp');
  if (!state.kinds.size) state.kinds.add('https');
  renderControls();
  renderShare();
  renderNotices();
  /** @type {TargetInfo[]} */
  const targets = prof.targets;
  // The network path comes first, full width.
  const groups = [...prof.groups].sort((/** @type {Group} */ a, /** @type {Group} */ b) => Number(b.id === PATH_GROUP) - Number(a.id === PATH_GROUP));
  panels = groups.map((/** @type {Group} */ g) => new Panel(g, targets.filter((t) => t.group === g.id)));
  $('#panels').replaceChildren(...panels.map((p) => p.el));
  uPlot.sync(SYNC_KEY);
  writeHash();
  await refreshAll();
}

main().catch((e) => {
  $('#notices').replaceChildren(h('div', { class: 'notice danger' }, h('b', { text: 'Could not start: ' }), String(e && e.message || e)));
});
