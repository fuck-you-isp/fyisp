// fyisp dashboard: helpers shared by the views (app.js, home*.js). Plain ES
// module, no build step: DOM building, theme and colour tokens, formatting,
// fetch with retry, the tooltip, the write calls and signing in.
// @ts-check

// ---------- page ----------

export const meta = (/** @type {string} */ n) => document.querySelector(`meta[name="${n}"]`)?.getAttribute('content') || '';

export const TOKEN = meta('fyisp-token');

export const $ = (/** @type {string} */ s) => /** @type {HTMLElement} */ (document.querySelector(s));

/**
 * @param {string} tag
 * @param {Object<string, any>} [props]
 * @param {...(Node|string|null|undefined|false)} kids
 */
export function h(tag, props, ...kids) {
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

// ---------- storage and theme ----------

export function storageGet(/** @type {string} */ k) { try { return localStorage.getItem(k); } catch { return null; } }

export function storageSet(/** @type {string} */ k, /** @type {string} */ v) { try { localStorage.setItem(k, v); } catch { /* private mode */ } }

export const mqLight = window.matchMedia('(prefers-color-scheme: light)');

export function effectiveTheme() {
  const t = document.documentElement.dataset.theme;
  if (t === 'light' || t === 'dark') return t;
  return mqLight.matches ? 'light' : 'dark';
}

/** @type {Map<string,string>} */
export const cssCache = new Map();

export function css(/** @type {string} */ name) {
  let v = cssCache.get(name);
  if (v == null) {
    v = getComputedStyle(document.documentElement).getPropertyValue(name).trim();
    cssCache.set(name, v);
  }
  return v;
}

// ---------- colours ----------

/** @param {string} hex @param {number} a */
export function rgba(hex, a) {
  const m = /^#?([0-9a-f]{6})$/i.exec(hex);
  if (!m) return hex;
  const n = parseInt(m[1], 16);
  return `rgba(${n >> 16},${(n >> 8) & 255},${n & 255},${a})`;
}

/** Mix a colour towards white (t>0) or black (t<0). */
export function shade(/** @type {string} */ hex, /** @type {number} */ t) {
  const m = /^#?([0-9a-f]{6})$/i.exec(hex);
  if (!m) return hex;
  const n = parseInt(m[1], 16);
  const f = (/** @type {number} */ c) => Math.round(t > 0 ? c + (255 - c) * t : c * (1 + t));
  const r = f(n >> 16), g = f((n >> 8) & 255), b = f(n & 255);
  return '#' + ((1 << 24) | (r << 16) | (g << 8) | b).toString(16).slice(1);
}

let hatchPattern = /** @type {CanvasPattern|null} */ (null);
/** The diagonal "not measured" fill for canvas charts, built once per theme. */
export function hatch(/** @type {CanvasRenderingContext2D} */ ctx) {
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
/** Forget the cached hatch fill (the theme changed). */
export function resetHatch() { hatchPattern = null; }

/** Smooth scrolling unless the reader asked for reduced motion. @returns {ScrollBehavior} */
export function smooth() { return window.matchMedia('(prefers-reduced-motion: reduce)').matches ? 'auto' : 'smooth'; }

// ---------- formatting ----------

export function fmtMs(/** @type {number|null|undefined} */ v) {
  if (v == null || !isFinite(v)) return '—';
  if (v < 10) return v.toFixed(2);
  if (v < 100) return v.toFixed(1);
  return v.toFixed(0);
}

export function fmtPct(/** @type {number} */ f) {
  if (!isFinite(f)) return '—';
  if (f === 0) return '0%';
  if (f < 0.001) return '<0.1%';
  return (f * 100).toFixed(f < 0.1 ? 1 : 0) + '%';
}

/**
 * "248 Mbit/s", one decimal under 10, "1.2 Gbit/s" from 1000; "—" when not
 * measured (busy is never 0). bare gives the number in Mbit/s alone ("1234"),
 * for a column headed Mbit/s.
 * @param {number|null|undefined} bps @param {boolean} [bare]
 */
export function fmtMbps(bps, bare = false) {
  if (bps == null) return '—';
  const m = bps / 1e6;
  const n = m < 10 ? m.toFixed(1) : String(Math.round(m));
  if (bare) return n;
  return m < 999.5 ? `${n} Mbit/s` : `${(m / 1000).toFixed(1)} Gbit/s`;
}

export function fmtTime(/** @type {number} */ ms, /** @type {number} */ spanMs) {
  const d = new Date(ms);
  const time = d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: spanMs <= 6 * 3600e3 ? '2-digit' : undefined });
  if (spanMs <= 24 * 3600e3) return time;
  return d.toLocaleDateString([], { month: 'short', day: 'numeric' }) + ' ' + time;
}

/** Clock time; with the date when not today. */
export function fmtWhen(/** @type {number} */ ms) {
  const d = new Date(ms);
  const time = d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
  if (d.toDateString() === new Date().toDateString()) return time;
  return d.toLocaleDateString([], { month: 'short', day: 'numeric' }) + ' ' + time;
}

/** A duration in words: "45 s", "13 min", "2 h 5 min", "3 d 4 h". */
export function fmtDur(/** @type {number} */ ms) {
  const s = Math.max(0, Math.round(ms / 1000));
  if (s < 60) return `${s} s`;
  const m = Math.round(s / 60);
  if (m < 60) return `${m} min`;
  const hh = Math.floor(m / 60), mm = m % 60;
  if (hh < 48) return mm ? `${hh} h ${mm} min` : `${hh} h`;
  const dd = Math.floor(hh / 24), rh = hh % 24;
  return rh ? `${dd} d ${rh} h` : `${dd} d`;
}

export function fmtStep(/** @type {number} */ ms) {
  if (ms < 60e3) return `${Math.round(ms / 1e3)}s`;
  if (ms < 3600e3) return `${Math.round(ms / 60e3)}m`;
  if (ms < 86400e3) return `${Math.round(ms / 3600e3)}h`;
  return `${Math.round(ms / 86400e3)}d`;
}

/** Clock time in the viewer's locale (12 or 24 h). */
export function clock(/** @type {number} */ ms) { return new Date(ms).toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' }); }
const usClock = new Intl.DateTimeFormat('en-US', { hour: 'numeric', minute: '2-digit' });
/** "10:14 am" (or "10:14" without the day period) in the viewer's zone: Home's style, as the server writes its copy. */
export function hpClock(/** @type {number} */ ms, ampm = true) {
  const p = Object.fromEntries(usClock.formatToParts(new Date(ms)).map((x) => [x.type, x.value]));
  return `${p.hour}:${p.minute}${ampm ? ' ' + String(p.dayPeriod || '').toLowerCase() : ''}`;
}
/** tz= for the API: the browser's IANA zone, so hours of day stay right across DST; minutes east of UTC when it has none. */
export function tzParam() {
  try {
    const z = Intl.DateTimeFormat().resolvedOptions().timeZone;
    if (z) return z;
  } catch { /* old browser */ }
  return String(-new Date().getTimezoneOffset());
}
/** One request per URL while it's in flight: two views asking for the same summary share it. */
const inflight = new Map();
export function fetchShared(/** @type {string} */ url) {
  let p = inflight.get(url);
  if (!p) {
    p = fetchJSON(url).finally(() => inflight.delete(url));
    inflight.set(url, p);
  }
  return p;
}
export function fmtInt(/** @type {number} */ n) { return n.toLocaleString('en-US'); }

export function median(/** @type {number[]} */ v) {
  if (!v.length) return NaN;
  const s = [...v].sort((a, b) => a - b), m = s.length >> 1;
  return s.length % 2 ? s[m] : (s[m - 1] + s[m]) / 2;
}

// ---------- fetch ----------

/**
 * GET/POST JSON with a small retry on 429/503.
 * @param {string} url @param {RequestInit} [init] @param {number} [tries]
 */
export async function fetchJSON(url, init, tries = 3) {
  for (let i = 0; ; i++) {
    const res = await fetch(url, { credentials: 'same-origin', cache: 'no-store', ...init });
    if ((res.status === 429 || res.status === 503) && i < tries - 1) {
      const wait = Math.min(5, Number(res.headers.get('Retry-After')) || 1) * 1000 * (1 + Math.random());
      await new Promise((r) => setTimeout(r, wait));
      if (init?.signal?.aborted) throw new DOMException('aborted', 'AbortError');
      continue;
    }
    const body = await res.json().catch(() => ({}));
    if (res.status === 401) void signIn();
    if (!res.ok) throw Object.assign(new Error(body.error || `HTTP ${res.status}`), { status: res.status, body, retryAfter: Number(res.headers.get('Retry-After')) || 0 });
    return body;
  }
}

// ---------- tooltip ----------

export const tip = $('#tip');

export function showTip(/** @type {Node[]} */ nodes, /** @type {number} */ x, /** @type {number} */ y) {
  tip.replaceChildren(...nodes);
  tip.hidden = false;
  const r = tip.getBoundingClientRect();
  let left = x + 14, top = y + 14;
  if (left + r.width > window.innerWidth - 8) left = x - r.width - 14;
  if (top + r.height > window.innerHeight - 8) top = y - r.height - 14;
  tip.style.left = Math.max(8, left) + 'px';
  tip.style.top = Math.max(8, top) + 'px';
}

export function hideTip() { tip.hidden = true; }

/** A small "?" button that explains a verdict kind (hover, focus or tap). */
export function whyButton(/** @type {string} */ title, /** @type {string} */ text) {
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

// ---------- dialogs ----------

/**
 * An in-page modal <dialog> (instead of prompt/confirm/alert): a title, an
 * optional message, optional fields and buttons. It resolves with the
 * button's value, or '' when dismissed (Escape, Cancel); fields are read by
 * the caller before it resolves.
 * body: paragraphs the dialog is described by (aria-describedby); safe: focus Cancel first.
 * @param {{title:string, text?:string, body?:Node[], fields?:Node[], buttons:{label:string,value:string,primary?:boolean}[], focus?:HTMLElement, safe?:boolean}} o
 * @returns {Promise<string>}
 */
export function dialog(o) {
  return new Promise((resolve) => {
    const dlg = /** @type {HTMLDialogElement} */ (h('dialog', { class: 'dlg', 'aria-labelledby': 'dlg-t', 'aria-describedby': o.body ? 'dlg-d' : null }));
    const form = h('form', { class: 'dlg-form', method: 'dialog' },
      h('h2', { id: 'dlg-t', text: o.title }),
      o.text ? h('p', { class: 'dlg-text', text: o.text }) : null,
      o.body ? h('div', { id: 'dlg-d', class: 'dlg-body' }, ...o.body) : null,
      ...(o.fields || []),
      // Only the primary button submits: Enter in a field (implicit submission) must pick it, never Cancel.
      h('div', { class: 'pop-actions' }, h('span', { class: 'grow' }),
        ...o.buttons.map((b) => h('button', {
          class: `btn${b.primary ? ' primary' : ' ghost'}`, value: b.value, text: b.label,
          type: b.primary ? 'submit' : 'button', onclick: b.primary ? null : () => dlg.close(b.value),
        }))));
    dlg.append(form);
    dlg.addEventListener('close', () => { resolve(dlg.returnValue || ''); dlg.remove(); });
    document.body.append(dlg);
    dlg.showModal();
    // A field, else the safe choice: Cancel when asked (safe), else the primary.
    const pick = o.focus || /** @type {HTMLElement|null} */ (form.querySelector(o.safe ? 'button[value="cancel"]' : '.btn.primary'));
    (pick || form).focus();
  });
}

// ---------- writes ----------

/** POST/PUT/DELETE JSON with the CSRF token. @param {string} method @param {string} url @param {any} [body] */
export function apiWrite(method, url, body) {
  /** @type {Record<string,string>} */
  const headers = { 'X-FYISP-Token': TOKEN };
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  return fetchJSON(url, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) }, 1);
}

// ---------- sign-in ----------

/**
 * Exchanges an admin token for the HttpOnly session cookie: true when the
 * dashboard took it, false when it refused it (403); other failures throw.
 * @param {string} t
 */
export async function adminSession(t) {
  const res = await fetch('api/admin/session', { method: 'POST', credentials: 'same-origin', cache: 'no-store', headers: { 'X-FYISP-Token': TOKEN, 'X-FYISP-Admin': t } });
  if (res.ok) return true;
  if (res.status === 403) return false;
  throw new Error(`HTTP ${res.status}`);
}

/** @type {Promise<void>|null} */
let signingIn = null;
/**
 * Asks for the admin token once the dashboard answered 401 (no session yet,
 * or fyisp restarted with a new token), then reloads the page signed in.
 * Calls while the dialog is open share it. Escape closes it, unless stay
 * (the page has nothing to show without it); the next 401 asks again.
 * @param {string} [why] its first line @param {boolean} [stay]
 */
export function signIn(why = '', stay = false) {
  signingIn ||= (async () => {
    let text = why || 'This dashboard needs its admin token. Open the link fyisp printed when it started, or paste the token here.';
    for (;;) {
      const input = /** @type {HTMLInputElement} */ (h('input', { type: 'password', id: 'dlg-admin', autocomplete: 'current-password' }));
      const v = await dialog({
        title: 'Sign in',
        text,
        fields: [h('label', { class: 'fld', for: 'dlg-admin' }, h('span', { text: 'Admin token' }), input)],
        buttons: [{ label: 'Sign in', value: 'ok', primary: true }],
        focus: input,
      });
      const t = input.value.trim();
      if (v !== 'ok' && !stay) { signingIn = null; return; }
      if (!t) continue;
      try {
        if (await adminSession(t)) { location.reload(); return; }
        text = 'That is not this dashboard’s admin token. Paste the token fyisp was started with, or open the newest link from its log.';
      } catch (e) {
        text = 'Could not sign in: ' + /** @type {any} */ (e).message + '. Try again.';
      }
    }
  })();
  return signingIn;
}

/** 'YYYY-MM-DDTHH:MM' in local time, for <input type="datetime-local">. */
export function toLocalInput(/** @type {number} */ ms) {
  const d = new Date(ms);
  const p = (/** @type {number} */ n) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}`;
}

export function fromLocalInput(/** @type {string} */ s) { const t = s ? new Date(s).getTime() : NaN; return isFinite(t) ? t : NaN; }

