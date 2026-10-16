// Investigate's reports in mtr's formats (ui/report.c in
// github.com/traviscross/mtr): `mtr -rwz` text, `mtr -j` JSON and `mtr -C`
// CSV, built in the browser from /api/trace. The source host is always
// "fyisp": never this machine's name or the user's address.
// @ts-check

/**
 * One hop row of /api/trace (the fields used here).
 * @typedef {{hop:number,ip?:string,no_reply?:boolean,private?:boolean,rdns?:string,asn?:number,
 *   n:number,lost:number,loss:number,min:number|null,mean:number|null,max:number|null,
 *   last?:number|null,stdev?:number|null}} ReportHop
 */
/**
 * @typedef {{dst:string,start:number,end:number,intervalMs:number,hops:ReportHop[]}} ReportInput
 */

export const SRC = 'fyisp';
// The tracer's ICMP echo: a 20-byte IPv4 header, 8 bytes of ICMP header
// and 16 of payload (internal/netinfo/hops).
const PSIZE = '44';
// internal/trace.DefaultRouterRate: probes answered per router per second.
const ROUTER_RATE = 1;

/** One line per hop number (the first row of each, the one with most probes), with every TTL up to the last. @param {ReportHop[]} hops */
function byHop(hops) {
  /** @type {Map<number, ReportHop[]>} */
  const m = new Map();
  for (const x of hops) {
    if (!m.has(x.hop)) m.set(x.hop, []);
    /** @type {ReportHop[]} */ (m.get(x.hop)).push(x);
  }
  const last = hops.reduce((a, x) => Math.max(a, x.hop), 0);
  /** @type {{hop:number,main:ReportHop|null,more:ReportHop[]}[]} */
  const out = [];
  for (let n = 1; n <= last; n++) {
    const rows = m.get(n) || [];
    out.push({ hop: n, main: rows[0] || null, more: rows.slice(1) });
  }
  return out;
}

/** mtr's host name with -b: "name (ip)", the address alone, or ??? for a silent hop. @param {ReportHop|null} x */
export function hostName(x) {
  if (!x || x.no_reply || !x.ip) return '???';
  return x.rdns ? `${x.rdns} (${x.ip})` : x.ip;
}

/** @param {ReportHop|null} x */
export function asnName(x) { return x && x.asn ? `AS${x.asn}` : 'AS???'; }

/** mtr's numbers for one hop: RTTs are 0 without answers (as mtr's); Last and StDev null when not known. @param {ReportHop|null} x */
function nums(x) {
  const snt = x ? x.n + x.lost : 0;
  const has = !!x && x.n > 0;
  const v = (/** @type {number|null|undefined} */ f) => (has && f != null && isFinite(f) ? f : has ? null : 0);
  return {
    loss: snt ? (x ? x.lost : 0) / snt * 100 : 0, snt,
    last: v(x && x.last), avg: v(x && x.mean), best: v(x && x.min), wrst: v(x && x.max), stdev: v(x && x.stdev),
  };
}

const r2 = (/** @type {number|null} */ f) => (f == null ? null : Math.round(f * 100) / 100);
const f1 = (/** @type {number|null} */ f, /** @type {number} */ w) => (f == null ? '-' : f.toFixed(1)).padStart(w);

/** mtr's report time: 2026-10-16T10:53:47+0000. @param {number} ms */
function isoTime(ms) { return new Date(ms).toISOString().replace(/\.\d+Z$/, '+0000'); }

/** The footer: how fyisp measured. @param {ReportInput} r */
function footer(r) {
  const s = Math.round(r.intervalMs / 100) / 10;
  return `fyisp: ICMP echo probes, one round every ${s} s, at most ${ROUTER_RATE} probe a second per router (a busy router misses some rounds); ${isoTime(r.start)} to ${isoTime(r.end)}.`;
}

/** `mtr -rwz` text. @param {ReportInput} r */
export function mtrText(r) {
  const rows = byHop(r.hops);
  const label = (/** @type {ReportHop|null} */ x) => `${asnName(x).padEnd(8)}${hostName(x)}`;
  const w = Math.max(SRC.length + 2, ...rows.map((x) => label(x.main).length + 2)) + 1;
  const head = ('HOST: ' + SRC).padEnd(8 + w) + '   Loss%   Snt   Last   Avg  Best  Wrst StDev';
  const lines = [`Start: ${isoTime(r.start)}`, head];
  for (const { hop, main, more } of rows) {
    const v = nums(main);
    lines.push(`${String(hop).padStart(3)}.|-- ${label(main).padEnd(w)}` +
      `${(v.loss.toFixed(2) + '%').padStart(8)}${String(v.snt).padStart(6)} ${f1(v.last, 6)}${f1(v.avg, 6)}${f1(v.best, 6)}${f1(v.wrst, 6)}${f1(v.stdev, 6)}`);
    for (const m of more) lines.push(`        ${label(m)}`);
  }
  lines.push('', footer(r));
  return lines.join('\n') + '\n';
}

/** `mtr -j` JSON (with -z: each hub's ASN). @param {ReportInput} r */
export function mtrJSON(r) {
  const rows = byHop(r.hops);
  const hubs = rows.map(({ hop, main }) => {
    const v = nums(main);
    return { count: hop, host: hostName(main), ASN: asnName(main), 'Loss%': r2(v.loss), Snt: v.snt,
      Last: r2(v.last), Avg: r2(v.avg), Best: r2(v.best), Wrst: r2(v.wrst), StDev: r2(v.stdev) };
  });
  const tests = rows.reduce((m, x) => Math.max(m, nums(x.main).snt), 0);
  return JSON.stringify({ report: { mtr: { src: SRC, dst: r.dst, tos: 0, tests, psize: PSIZE, bitpattern: 'n/a' }, hubs } }, null, 4) + '\n';
}

/** `mtr -C` CSV (with -z: the Asn column). The empty column is mtr's field separator. @param {ReportInput} r */
export function mtrCSV(r) {
  const q = (/** @type {string} */ s) => (/[",\n]/.test(s) ? `"${s.replace(/"/g, '""')}"` : s);
  const f = (/** @type {number|null} */ v) => (v == null ? '' : v.toFixed(2));
  const lines = ['Mtr_Version,Start_Time,Status,Host,Hop,Ip,Asn,Loss%,Snt,,Last,Avg,Best,Wrst,StDev'];
  const t = Math.floor(r.start / 1000);
  for (const { hop, main } of byHop(r.hops)) {
    const v = nums(main);
    lines.push([SRC, t, 'OK', q(r.dst), hop, q(hostName(main)), asnName(main), f(v.loss), v.snt, '', f(v.last), f(v.avg), f(v.best), f(v.wrst), f(v.stdev)].join(','));
  }
  return lines.join('\n') + '\n';
}

/** The command to run the same trace yourself. @param {string} host */
export function mtrCommand(host) { return `mtr -rwzbc 100 ${host}`; }
/** @param {string} host */
export function pathpingCommand(host) { return `pathping -n ${host}`; }
