// Writes the /api/home2 fixtures the Home specs serve through page.route:
// one part=now answer per state and a few part=tile answers, following the
// pinned v2 contract (docs/HOME-API.md once PR 4 lands). The copy mirrors the
// approved mockup. The clock is 2026-10-09 10:30 am in America/New_York (the
// specs run in that zone), like the home2 goldens.
//   node fixtures/gen.js        # rewrites fixtures/*.json (Dockerfile: uifixtures)
// @ts-check
'use strict';
const fs = require('fs');
const path = require('path');

const T0 = Date.UTC(2026, 9, 9, 14, 30); // 10:30 am EDT
const MIN = 60e3, BAR = 5 * MIN;
const at = (/** @type {string} */ hm) => { // "10:05" (am, today) -> ms
  const [h, m] = hm.split(':').map(Number);
  return T0 + ((h - 10) * 60 + (m - 30)) * MIN;
};

const TILES = [
  ['zoom', 'Zoom', 'call', 18], ['teams', 'Microsoft Teams', 'call', 14], ['meet', 'Google Meet', 'call', 9], ['slack', 'Slack', 'call', 22],
  ['m365', 'Microsoft 365', 'app', 12], ['gws', 'Google Workspace', 'app', 8], ['github', 'GitHub', 'app', 24], ['dropbox', 'Dropbox', 'app', 15],
  ['onedrive', 'OneDrive', 'app', 13], ['youtube', 'YouTube', 'stream', 7], ['netflix', 'Netflix', 'stream', 6],
].map(([id, name, kind, usual]) => ({ id: String(id), name: String(name), kind: String(kind), usual: Number(usual) }));
const ENABLE = { github: 'services-dev', dropbox: 'services-productivity', onedrive: 'services-productivity', youtube: 'streaming', netflix: 'streaming' };

/** 24 good bars, 8:30 to 10:30 am; edit(k, bar) may change one. */
function bars(/** @type {(k:number, b:any)=>void} */ edit = () => {}) {
  return Array.from({ length: 24 }, (_, k) => {
    const b = { from: T0 - (24 - k) * BAR, state: 'good', note: '' };
    edit(k, b);
    return b;
  });
}
const barAt = (/** @type {string} */ hm) => Math.floor((at(hm) - (T0 - 24 * BAR)) / BAR);
const word = (/** @type {any} */ t, /** @type {string} */ s) => ({ good: 'Good', slow: t.kind === 'call' ? 'Choppy' : 'Slow', down: 'Down', none: 'No data', off: 'Not checked' })[s];

function goodTile(/** @type {any} */ t) {
  return {
    id: t.id, name: t.name, kind: t.kind, state: 'good', word: 'Good', since: null, note: '',
    aria: `${t.name}: Good`,
    reason: t.kind === 'call' ? 'Calls should be smooth.' : t.kind === 'stream' ? `Working normally. ${t.name} is reachable. If it’s buffering, test your speed below.` : 'Working normally.',
    bars_summary: 'Last 2 hours: no problems.',
    bars: bars(),
  };
}
function offTile(/** @type {any} */ t) {
  const prof = ENABLE[/** @type {'github'} */ (t.id)] || 'services-productivity';
  return {
    id: t.id, name: t.name, kind: t.kind, state: 'off', word: 'Not checked', since: null, note: '',
    aria: `${t.name}: not checked on this setup`, reason: `fyisp isn’t checking ${t.name} on this setup.`,
    enable_hint: `To check ${t.name}, add ${t.kind === 'call' ? 'calls' : prof} to fyisp’s --profile setting (FYISP_PROFILE in Docker).`,
    bars_summary: '', bars: [],
  };
}
/** A tile that is slow or down since hm, with its bars. */
function badTile(/** @type {any} */ t, /** @type {string} */ state, /** @type {string} */ hm, /** @type {string} */ reason) {
  const g = goodTile(t), w = word(t, state), since = `since ${hm} am`;
  return {
    ...g, state, word: w, since: at(hm), note: since, aria: `${t.name}: ${w}, ${since}`, reason,
    bars: bars((k, b) => { if (k >= barAt(hm)) { b.state = state; b.note = `${w} ${since}`.replace(/^./, (c) => c.toUpperCase()); } }),
    bars_summary: `Last 2 hours: ${state === 'down' ? 'down or dropped' : 'slow'} in ${24 - barAt(hm)} of 24 five-minute periods.`,
  };
}

const HISTORY = [
  { id: 't-1760017260000-slack', from: at('9:41'), to: at('9:45'), ongoing: false, level: 'slow', when: 'Today 9:41–9:45 am', what: 'Slack calls may have been choppy.', cause: 'Slack’s side', cause_kind: 'app', evidence: 'Your internet and every other app were normal.' },
  { id: 't-1759961640000-m365', from: T0 - 14.27 * 3600e3, to: T0 - 14.13 * 3600e3, ongoing: false, level: 'slow', when: 'Yesterday 8:14–8:22 pm', what: 'Microsoft 365 was slow to load.', cause: 'Microsoft’s side', cause_kind: 'app', evidence: 'Your internet and every other app were normal.' },
  { id: 't-1759955220000-gws,meet,youtube', from: T0 - 18.05 * 3600e3, to: T0 - 18 * 3600e3, ongoing: false, level: 'down', when: 'Yesterday 4:27–4:30 pm', what: 'Google Meet, Gmail, Docs and YouTube didn’t load for 3 minutes.', cause: 'Google’s side', cause_kind: 'app', evidence: 'Your internet and the non-Google apps were normal.' },
  { id: 'c-1759947000000', from: T0 - 20.33 * 3600e3, to: T0 - 20.08 * 3600e3, ongoing: false, level: 'slow', when: 'Yesterday 2:10–2:25 pm', what: 'Calls may have been choppy. Websites were fine.', cause: 'your internet provider', cause_kind: 'provider', evidence: 'Your router was fine, but your provider’s connection was dropping some data.' },
  { id: 'c-1759795320000', from: T0 - 62.5 * 3600e3, to: T0 - 62.47 * 3600e3, ongoing: false, level: 'down', when: 'Thu 8:02–8:04 pm', what: 'Internet dropped for about 2 minutes.', cause: 'your home router', cause_kind: 'home', evidence: 'Your router stopped responding, so fyisp couldn’t reach anything past it.' },
];
// Episode ids carry their start time, as the server's do.
for (const p of HISTORY) p.id = p.id.replace(/\d{13}/, String(p.from));
const SUMMARY5 = '5 problems in the last 7 days: 1 likely from your internet provider, 1 from your home router, 3 from the apps themselves.';
const ROUTER_HINT = 'fyisp checks from the box it runs on, up to your router. It can’t see the Wi-Fi on the device you’re using now — if calls are choppy here, try moving closer to your router or plugging in.';
const fine = (/** @type {string} */ id, /** @type {string} */ label) => ({ id, label, state: 'good', sub: 'Working normally' });

/** The "All good" answer every state starts from. */
function good() {
  const tiles = TILES.map(goodTile);
  // Slack was choppy 9:41–9:45 (in the bars and the problems).
  tiles[3].bars[barAt('9:40')].state = 'slow';
  tiles[3].bars[barAt('9:40')].note = 'Choppy 9:41–9:45 am — likely Slack’s side';
  tiles[3].bars_summary = 'Last 2 hours: slow in 1.';
  return {
    v: 2, at: T0, last_check: T0 - 4000, stale: false, watching_since: Date.UTC(2026, 2, 3, 15), footer: 'Watching your connection since Mar 3',
    learning: null, monitored: 11,
    answer: {
      key: 'good', tone: 'ok', icon: 'good', tab: '✓ All good',
      headline: 'Everything you use is working normally.',
      sub: 'Your router, your internet provider and all 11 apps look healthy. Calls and video should be smooth.',
      day: { text: 'Last 24 hours: 4 short problems, 1 likely from your internet provider.', link: '4 short problems' },
    },
    path: [fine('router', 'Your home router'), fine('provider', 'Your internet provider'), fine('internet', 'The internet')],
    path_hint: ROUTER_HINT,
    tiles,
    speed: {
      available: true,
      latest: { at: at('8:00'), down_mbps: 930, up_mbps: 940, text: 'Your speed: about 930 Mbps down / 940 Mbps up, about your usual.', note: 'Today 8:00 am' },
      empty_text: 'No speed test yet. The first one runs within the hour.',
      usual: { down_mbps: 925, up_mbps: 938 }, running: null, can_control: true,
    },
    problems: { summary: SUMMARY5, items: HISTORY },
  };
}

/** @type {Record<string, any>} */
const NOW = {};
NOW.good = good();

NOW.blip = (() => {
  const d = good();
  const end = at('10:14') + 40e3;
  d.answer = {
    key: `working_again:${end}`, tone: 'warn', icon: 'slow', tab: '! Dropped at 10:14',
    headline: 'Working again. Your internet dropped for about 40 seconds at 10:14 am.',
    sub: 'Likely cause: your internet provider — your router kept answering, but nothing past it did. If a call cut out around then, this is why.',
    day: { text: 'Last 24 hours: 5 short problems, 2 likely from your internet provider.', link: '5 short problems' },
  };
  d.path[1].sub = 'Dropped for 40 s at 10:14 am, fine now';
  for (const t of d.tiles) {
    t.note = 'Dropped 40 s at 10:14 am';
    t.aria = `${t.name}: Good, dropped 40 s at 10:14 am`;
    t.reason += ' It dropped for about 40 seconds at 10:14 am.';
    t.bars[barAt('10:10')] = { ...t.bars[barAt('10:10')], state: 'down', note: 'Dropped 40 s' };
    t.bars_summary = 'Last 2 hours: down or dropped in 1 of 24 five-minute periods.';
  }
  d.problems = {
    summary: '6 problems in the last 7 days: 2 likely from your internet provider, 1 from your home router, 3 from the apps themselves.',
    items: [{ id: `c-${at('10:14')}`, from: at('10:14'), to: end, ongoing: false, level: 'down', when: 'Today 10:14 am (about 40 seconds)', what: 'Everything dropped for about 40 seconds.', cause: 'your internet provider', cause_kind: 'provider', evidence: 'Your router kept answering, but nothing past it did.' }, ...HISTORY],
  };
  return d;
})();

NOW.isp = (() => {
  const d = good();
  const start = at('10:12');
  d.answer = {
    key: `isp:${start}`, tone: 'bad', icon: 'down', tab: '✕ Provider problem',
    headline: 'Your internet provider is likely having trouble.',
    sub: 'Your router is answering normally, but your provider’s first stop is losing data, so every app is affected at once. Calls may be choppy and some sites won’t load. Started at 10:12 am.',
    day: { text: 'Last 24 hours: 5 short problems, 2 likely from your internet provider.', link: '5 short problems' },
  };
  d.path = [fine('router', 'Your home router'), { id: 'provider', label: 'Your internet provider', state: 'down', sub: 'Losing data since 10:12 am' }, { id: 'internet', label: 'The internet', state: 'slow', sub: 'Hard to reach through your provider' }];
  d.tiles = TILES.map((t) => {
    const down = t.id === 'github' || t.id === 'netflix';
    const s = down ? 'down' : 'slow';
    const r = t.kind === 'call' ? 'Calls may be choppy since 10:12 am — audio is breaking up.' : down ? 'Not loading since 10:12 am.' : 'Slower than usual since 10:12 am.';
    return badTile(t, s, '10:12', r);
  });
  d.speed.latest = { at: at('10:20'), down_mbps: 140, up_mbps: 95, text: 'Your speed: about 140 Mbps down / 95 Mbps up, much slower than usual (usually about 925 / 938).', note: 'Today 10:20 am, during the problem' };
  d.problems = {
    summary: '6 problems in the last 7 days: 2 likely from your internet provider, 1 from your home router, 3 from the apps themselves.',
    items: [{ id: `c-${start}`, from: start, to: 0, ongoing: true, level: 'down', when: 'Today since 10:12 am (still happening)', what: 'Calls choppy, some sites not loading.', cause: 'your internet provider', cause_kind: 'provider', evidence: 'Your router is fine; the problem starts just past it, inside your provider’s network.' }, ...HISTORY],
  };
  return d;
})();

function zoomDown() {
  const d = good();
  const start = at('10:05');
  d.answer = {
    key: `app:zoom:${start}`, tone: 'ok', icon: 'good', tab: '! Zoom down',
    headline: 'Your internet is fine. Zoom isn’t connecting.',
    sub: 'Likely cause: Zoom’s side — every other app is working normally. Teams and Google Meet are working if you need to switch.',
    day: { text: 'Last 24 hours: 5 short problems, 1 likely from your internet provider.', link: '5 short problems' },
  };
  d.tiles[0] = badTile(TILES[0], 'down', '10:05', 'Not connecting since 10:05 am. Likely cause: Zoom’s side — everything else is normal.');
  d.problems = {
    summary: '6 problems in the last 7 days: 1 likely from your internet provider, 1 from your home router, 4 from the apps themselves.',
    items: [{ id: `t-${start}-zoom`, from: start, to: 0, ongoing: true, level: 'down', when: 'Today since 10:05 am (still happening)', what: 'Zoom not connecting.', cause: 'Zoom’s side', cause_kind: 'app', evidence: 'Your internet and all other apps are normal.' }, ...HISTORY],
  };
  return d;
}
NOW.zoom = zoomDown();

// App-only problem with a connection drop that ended 16 minutes ago: the app wins the headline, the drop is appended.
NOW['zoom-working-again'] = (() => {
  const d = zoomDown();
  d.answer.sub += ' Working again. Your internet dropped for about 40 seconds at 10:14 am.';
  d.path[1].sub = 'Dropped for 40 s at 10:14 am, fine now';
  for (const t of d.tiles.slice(1)) {
    t.note = 'Dropped 40 s at 10:14 am';
    t.aria = `${t.name}: Good, dropped 40 s at 10:14 am`;
    t.bars[barAt('10:10')] = { ...t.bars[barAt('10:10')], state: 'down', note: 'Dropped 40 s' };
  }
  d.problems.items.splice(1, 0, { id: `c-${at('10:14')}`, from: at('10:14'), to: at('10:14') + 40e3, ongoing: false, level: 'down', when: 'Today 10:14 am (about 40 seconds)', what: 'Everything dropped for about 40 seconds.', cause: 'your internet provider', cause_kind: 'provider', evidence: 'Your router kept answering, but nothing past it did.' });
  d.problems.summary = '7 problems in the last 7 days: 2 likely from your internet provider, 1 from your home router, 4 from the apps themselves.';
  return d;
})();

NOW.learning = (() => {
  const d = good();
  const start = at('9:50');
  d.watching_since = start;
  d.footer = 'Watching your connection since today 9:50 am';
  d.learning = { until: start + 24 * 3600e3, text: 'fyisp is still learning what’s usual for your connection. This takes about a day. Until then it only flags big problems, so small slowdowns won’t show yet.' };
  d.answer.sub = 'Your router, your internet provider and all 11 apps look healthy. fyisp started watching at 9:50 am.';
  d.answer.day = { text: 'Last 24 hours: no problems.', link: '' };
  d.tiles = TILES.map((t) => ({ ...goodTile(t), bars: bars((k, b) => { if (k < barAt('9:50')) b.state = 'none'; }), bars_summary: 'Last 2 hours: not watched for 80 minutes.' }));
  d.speed.latest = null;
  d.speed.usual = null;
  d.problems = { summary: 'No problems since fyisp started at 9:50 am.', items: [] };
  return d;
})();

// fyisp says it is stale itself (its last check is 9 minutes old).
NOW.stale = (() => {
  const d = good();
  d.last_check = at('10:21');
  d.stale = true;
  d.answer = { ...d.answer, key: 'stale', tone: 'stale', icon: 'none', tab: '? Not checking', headline: 'fyisp hasn’t checked since 10:21 am.', sub: 'Is the computer it runs on asleep or turned off? Everything below is from 10:21 am and may be out of date.' };
  d.path = d.path.map((p) => ({ ...p, sub: 'Last known: working' }));
  for (const t of d.tiles) for (const k of [22, 23]) t.bars[k] = { ...t.bars[k], state: 'none', note: '' };
  return d;
})();

NOW.checking = (() => {
  const d = good();
  d.last_check = 0;
  d.answer = { key: 'checking', tone: 'checking', icon: 'none', tab: 'Checking', headline: 'Checking your connection…', sub: 'fyisp just started. The first answer takes about a minute.', day: null };
  d.path = d.path.map((p) => ({ ...p, state: 'unknown', sub: 'Checking…' }));
  d.tiles = TILES.map((t) => ({ ...goodTile(t), state: 'none', word: 'No data', aria: `${t.name}: no data yet`, reason: 'fyisp is still checking.', bars_summary: '', bars: bars((k, b) => { b.state = 'none'; }) }));
  d.problems = { summary: 'No problems in the last 7 days.', items: [] };
  return d;
})();

// A custom --config: 4 tiles have no targets.
NOW.off4 = (() => {
  const d = good();
  const off = ['github', 'dropbox', 'youtube', 'netflix'];
  d.monitored = 7;
  d.answer.sub = 'Your router, your internet provider and all 7 apps look healthy. Calls and video should be smooth.';
  d.tiles = d.tiles.map((t, i) => (off.includes(t.id) ? offTile(TILES[i]) : t));
  return d;
})();

// --profile calls: only the call tiles.
NOW['calls-profile'] = (() => {
  const d = good();
  d.monitored = 4;
  d.answer.sub = 'Your router, your internet provider and all 4 apps look healthy. Calls and video should be smooth.';
  d.tiles = d.tiles.map((t, i) => (TILES[i].kind === 'call' ? t : offTile(TILES[i])));
  return d;
})();

// UDP blocked on a network that says so (Quiet): the info card.
NOW['udp-quiet'] = (() => {
  const d = good();
  const start = at('9:00');
  d.answer = {
    key: `udp_blocked:${start}`, tone: 'info', icon: 'slow', tab: '! Calls limited',
    headline: 'Calls can’t use their fast path on this network.',
    sub: 'This network blocks the kind of traffic calls prefer, so calls fall back to a slower route. Websites and apps are fine.',
    day: NOW.good.answer.day,
  };
  return d;
})();

NOW['speed-running'] = (() => {
  const d = good();
  d.speed.running = { entry: 12, expected_end: T0 + 20e3 };
  return d;
})();

NOW['speed-no-usual'] = (() => {
  const d = good();
  d.speed.usual = null;
  d.speed.latest = { ...d.speed.latest, text: 'Your speed: about 930 Mbps down / 940 Mbps up.' };
  return d;
})();

// After "Test speed now" finished.
NOW['speed-done'] = (() => {
  const d = good();
  d.speed.latest = { at: T0, down_mbps: 925, up_mbps: 938, text: 'Your speed: about 925 Mbps down / 938 Mbps up, about your usual.', note: 'Today 10:30 am' };
  return d;
})();

// ---------- part=tile ----------

/** 288 five-minute points over the last 24 h around usual, with fyisp off 3:05–3:50 am. */
function series(/** @type {any} */ t, /** @type {(i:number, v:number|null)=>number|null} */ edit = (_, v) => v) {
  let x = t.id.split('').reduce((a, c) => a + c.charCodeAt(0), 7) * 97;
  const r = () => (x = (x * 16807) % 2147483647) / 2147483647;
  const from = T0 - 288 * BAR;
  return Array.from({ length: 288 }, (_, i) => {
    let v = /** @type {number|null} */ (Math.round(t.usual * (0.92 + r() * 0.16) * 10) / 10);
    if (i >= 199 && i <= 209) v = null;
    return edit(i, v);
  }).map((v, i) => (from + i * BAR < T0 ? v : null));
}
const OFF_BAND = { from: T0 - 288 * BAR + 199 * BAR, to: T0 - 288 * BAR + 210 * BAR, kind: 'off', text: 'fyisp wasn’t running 3:05–3:50 am' };
const CALL_LIMITS = (/** @type {string[]} */ v, /** @type {boolean[]} */ bad) => [
  { label: 'Delay', value: v[0], limit: 'choppy over 150 ms', bad: bad[0] },
  { label: 'Jitter', value: v[1], limit: 'over 30 ms', bad: bad[1] },
  { label: 'Packet loss', value: v[2], limit: '2% or more', bad: bad[2] },
];
const SHARED = 'Call quality is judged from the general call path; fyisp can’t test Zoom’s own servers before a meeting.';

/** @type {Record<string, any>} */
const TILE = {};
const tileBase = (/** @type {any} */ t) => ({ v: 2, at: T0, id: t.id, from: T0 - 288 * BAR, step: BAR, usual_ms: t.usual });

TILE.zoom = {
  ...tileBase(TILES[0]), state: 'good', word: 'Good', reason: 'Calls should be smooth.', coverage: SHARED,
  metrics: CALL_LIMITS(['21 ms', '2 ms', '0%'], [false, false, false]),
  ms: series(TILES[0]), bands: [OFF_BAND],
  text: 'Usually about 18 ms. fyisp wasn’t running 3:05–3:50 am.',
};
TILE['zoom-down'] = {
  ...TILE.zoom, state: 'down', word: 'Down', reason: 'Not connecting since 10:05 am. Likely cause: Zoom’s side — everything else is normal.',
  metrics: CALL_LIMITS(['—', '—', '100%'], [false, false, true]),
  ms: series(TILES[0], (i, v) => (i >= 283 ? null : v)),
  bands: [OFF_BAND, { from: at('10:05'), to: T0, kind: 'problem', text: 'Down since 10:05 am — likely Zoom’s side' }],
  text: 'Usually about 18 ms. fyisp wasn’t running 3:05–3:50 am. Down since 10:05 am — likely Zoom’s side.',
};
TILE.teams = {
  ...tileBase(TILES[1]), state: 'good', word: 'Good', reason: 'Calls should be smooth.', coverage: '',
  metrics: CALL_LIMITS(['16 ms', '1 ms', '0%'], [false, false, false]),
  ms: series(TILES[1], (i, v) => (i >= 44 && i <= 47 && v != null ? v + 14 : v)),
  bands: [{ from: T0 - 288 * BAR + 44 * BAR, to: T0 - 288 * BAR + 48 * BAR, kind: 'problem', text: 'Choppy 2:10–2:25 pm — likely your internet provider' }, OFF_BAND],
  text: 'Usually about 14 ms. Choppy 2:10–2:25 pm — likely your internet provider. fyisp wasn’t running 3:05–3:50 am.',
};
TILE.m365 = {
  ...tileBase(TILES[4]), state: 'good', word: 'Good', reason: 'Working normally.', coverage: '',
  metrics: [{ label: 'Usual', value: '12 ms' }, { label: 'Slow above', value: '42 ms' }],
  ms: series(TILES[4], (i, v) => (i >= 117 && i <= 118 && v != null ? 34 : v)),
  bands: [{ from: T0 - 288 * BAR + 117 * BAR, to: T0 - 288 * BAR + 119 * BAR, kind: 'problem', text: 'Slow 8:14–8:22 pm — likely Microsoft’s side' }, OFF_BAND],
  text: 'Usually about 12 ms. Slow 8:14–8:22 pm — likely Microsoft’s side. fyisp wasn’t running 3:05–3:50 am.',
};
TILE.youtube = {
  ...tileBase(TILES[9]), state: 'good', word: 'Good', reason: NOW.good.tiles[9].reason, coverage: '',
  metrics: [{ label: 'Usual', value: '7 ms' }, { label: 'Only marked Down if it stops loading', value: '' }],
  ms: series(TILES[9]), bands: [OFF_BAND],
  text: 'Usually about 7 ms. fyisp wasn’t running 3:05–3:50 am.',
};
TILE['gws-learning'] = {
  ...tileBase(TILES[5]), state: 'good', word: 'Good', reason: 'Working normally.', coverage: '', usual_ms: null, metrics: [],
  ms: series(TILES[5], (i, v) => (i < 280 ? null : v)),
  bands: [{ from: T0 - 288 * BAR, to: at('9:50'), kind: 'off', text: 'Not watched before 9:50 am' }],
  text: 'Still learning what’s usual. Not watched before 9:50 am.',
};

// ---------- write ----------

/** Pretty JSON with each bars/ms array on one line, so a fixture reads top to bottom. */
function stringify(/** @type {any} */ o) {
  /** @type {string[]} */
  const keep = [];
  const s = JSON.stringify(o, (k, v) => ((k === 'bars' || k === 'ms') && Array.isArray(v) ? `@@${keep.push(JSON.stringify(v)) - 1}@@` : v), 2);
  return s.replace(/"@@(\d+)@@"/g, (_, i) => keep[Number(i)]) + '\n';
}
const dir = process.argv[2] || __dirname;
fs.mkdirSync(dir, { recursive: true });
for (const [name, d] of Object.entries(NOW)) fs.writeFileSync(path.join(dir, `now-${name}.json`), stringify(d));
for (const [name, d] of Object.entries(TILE)) fs.writeFileSync(path.join(dir, `tile-${name}.json`), stringify(d));
console.log(`${Object.keys(NOW).length} part=now and ${Object.keys(TILE).length} part=tile fixtures in ${dir}`);
