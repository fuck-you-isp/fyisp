// Storage tests: the words for an attempt's class and its timings
// (docs/STORAGE-API.md lists the classes). Shared by the targets list and
// the campaign views. Never the server's message text: the class, the HTTP
// status and the S3 code only.
// Plain ES module, no build step.
// @ts-check

import { fmtMs } from './core.js';

/** @typedef {{at:number,class:string,group:string,keys:string,hint:string|null,http_status:number|null,s3_code:string|null,dns_ms:number|null,connect_ms:number|null,tls_ms:number|null,ttfb_ms:number|null,during_test:boolean}} StorageCheck */

/** Short words (tables, cells) and a plain sentence (labels, Check) per class. */
const CLASSES = /** @type {Object<string, [string, string]>} */ ({
  ok: ['OK', 'Worked.'],
  dns_fail: ['Name lookup failed', 'Couldn’t look up the endpoint’s address (DNS).'],
  dns_no_ipv4: ['No IPv4 address', 'The endpoint’s name has no IPv4 address.'],
  connect_timeout: ['No answer', 'Couldn’t connect: no answer in time.'],
  connect_refused: ['Refused', 'The endpoint refused the connection.'],
  connect_reset: ['Cut off while connecting', 'The connection was cut off while opening.'],
  connect_unreachable: ['Unreachable', 'There was no route to the endpoint.'],
  tls_timeout: ['TLS handshake timed out', 'Connected, but setting up encryption (TLS) got no answer in time.'],
  tls_reset: ['Cut off during TLS', 'Connected, but the connection was cut off while setting up encryption (TLS).'],
  tls_error: ['Encryption failed', 'Connected, but setting up encryption (TLS) failed.'],
  reset_before_response: ['Cut off before answering', 'The connection was cut off before the endpoint answered.'],
  timeout_response: ['No response', 'The endpoint took the request but didn’t answer in time.'],
  conn_closed: ['Connection closed', 'The endpoint closed the reused connection without answering.'],
  reset_mid_transfer: ['Cut off mid-transfer', 'The connection was cut off in the middle of the transfer.'],
  stall: ['Stalled', 'The transfer stopped: no data for 10 s.'],
  short_body: ['Ended early', 'The download ended before all of it arrived.'],
  timeout_total: ['Took too long', 'The request didn’t finish in time.'],
  http_5xx: ['Server error', 'The storage service answered with an error. The provider’s side.'],
  throttled: ['Slowed down by the provider', 'The provider asked fyisp to slow down.'],
  http_auth: ['Keys refused', 'The keys were refused. A setting to fix, not a network problem.'],
  clock_skew: ['Clock off', 'The endpoint refused the request because this computer’s clock is off.'],
  tls_cert: ['Certificate not trusted', 'Couldn’t set up encryption: the certificate isn’t trusted.'],
  http_redirect: ['Redirected', 'The endpoint answered with a redirect: another endpoint or region.'],
  http_4xx: ['Refused the request', 'The endpoint refused the request.'],
  addr_refused: ['Address not allowed', 'fyisp doesn’t connect to the address the name now resolves to.'],
});

/** "Keys refused" for a class. @param {string} c */
export function classShort(c) { return (CLASSES[c] || [c])[0]; }

/** The class's sentence. @param {string} c */
export function classLong(c) { return (CLASSES[c] || [c, c])[1]; }

/** " (403 AccessDenied)", or "" with neither. @param {number|null} status @param {string|null} code */
export function statusWords(status, code) {
  const s = [status ? String(status) : '', code || ''].filter(Boolean).join(' ');
  return s ? ` (${s})` : '';
}

/** "Name lookup 3 ms, connect 11 ms, encryption 23 ms, first byte 31 ms". @param {StorageCheck} c */
export function timings(c) {
  return /** @type {[string, number|null][]} */ ([['Name lookup', c.dns_ms], ['connect', c.connect_ms], ['encryption', c.tls_ms], ['first byte', c.ttfb_ms]])
    .filter(([, v]) => v != null).map(([k, v]) => `${k} ${fmtMs(v)} ms`).join(', ');
}

/** The fix a hint names, as a sentence. */
const HINTS = /** @type {Object<string, string>} */ ({
  region: 'Use the region your provider shows for this bucket.',
  path_style: 'Bucket names with dots break the certificate check when the bucket is in the host name. Choose “In the path”.',
  endpoint: 'Use the endpoint and region the bucket is in.',
  clock: 'Set this computer’s clock (turn on automatic time).',
  ca: 'If this is your own server, add its certificate authority under “Uses its own certificate authority”.',
});

/** @param {string|null} h */
export function hintWords(h) { return (h && HINTS[h]) || ''; }
